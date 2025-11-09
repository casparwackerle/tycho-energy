package calibration

import (
	"context"
	"math"
	"unsafe"

	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	gpuCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/gpu"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// Public API
// -----------------------------------------------------------------------------

// PollProbeGPU measures per-device GPU metric publish cadence before normal
// collection, then returns a node-wide recommended poll period (bestMS).
// It uses a short-lived GPU collector with no ring buffer and shuts it down.
func PollProbeGPU(ctx context.Context, mono *clock.Mono, budgetSec int, minMs int) (bestMS int, ok bool) {
	klog.V(5).Infof("GPU-POLL: starting probe (budgetSec=%d, minMs=%d)", budgetSec, minMs)

	// Create a short-lived collector with no buffer; reuse your lifecycle
	col := gpuCollector.New(gpuCollector.Config{
		Buf:  nil,  // important: no ring in calibration
		Mono: mono, // use shared monotonic clock
	})
	if col == nil {
		klog.V(2).Info("GPU-POLL: failed to create GPU collector")
		return 0, false
	}
	defer func() {
		if err := col.Close(); err != nil {
			klog.V(5).Infof("GPU-POLL: close error: %v", err)
		}
	}()

	if err := col.Init(ctx); err != nil {
		klog.V(2).Infof("GPU-POLL: init error: %v", err)
		return 0, false
	}

	raw := col.DevicesInternal() // []gpuDeviceState (unexported type; exported fields)
	if len(raw) == 0 {
		klog.V(2).Info("GPU-POLL: no NVML-capable devices discovered; skip probe")
		return 0, false
	}

	cfg := defaultGPUPollProbeConfig(minMs)
	st := newGPUPollState(ctx, mono, budgetSec, cfg)

	// Snapshot exported metadata + keep NVML handles aligned by index in st
	for _, d := range raw {
		st.devs = append(st.devs, gpuDevMeta{
			Index: d.Index, UUID: d.UUID, Name: d.Name, PCIBus: d.PCIBus,
			Nvml: d.Nvml, // handle used for reads
		})
		st.out = append(st.out, &deviceProbeResult{
			DeviceIndex: d.Index,
			UUID:        d.UUID,
			Name:        d.Name,
			PCIBus:      d.PCIBus,
		})
	}

	// hyper-poll refine (tight loop around suspected cadence)
	if err := st.hyperPollRefineAll(); err != nil {
		klog.V(5).Infof("GPU-POLL: refine finished with error: %v", err)
	}

	// Summarize → per-device recommendation → node min
	best, ok := st.summarizeAndRecommend()
	if ok {
		klog.V(2).Infof("GPU-POLL: summary bestMS=%d ms across %d device(s)", best, len(st.devs))
	}
	return best, ok
}

// -----------------------------------------------------------------------------
// Config / constants
// -----------------------------------------------------------------------------

const (
	nvmlFieldInstantPower = 186

	defaultEdgesTarget    = 50
	defaultRefineGuardSec = 8
	defaultHyperPollMs    = 1
)

type GPUPollProbeConfig struct {
	MinRecommendedMs int

	EdgesTarget    int
	RefineGuardSec int
	HyperPollMs    int
}

func defaultGPUPollProbeConfig(minMs int) GPUPollProbeConfig {
	return GPUPollProbeConfig{
		MinRecommendedMs: minMs,
		EdgesTarget:      defaultEdgesTarget,
		RefineGuardSec:   defaultRefineGuardSec,
		HyperPollMs:      defaultHyperPollMs,
	}
}

// -----------------------------------------------------------------------------
// State & data models
// -----------------------------------------------------------------------------

type edgeMetric int

const (
	edgeMetricUnknown edgeMetric = iota
	edgeMetricPowerUsage
	edgeMetricField186
)

type deviceEdge struct {
	DeviceIndex int
	WhenWall    time.Time // used for robust Δt computation
	Source      edgeMetric
}

type callLatency struct {
	PowerUsageDur  []time.Duration
	FieldValuesDur []time.Duration
	BundleSpanDur  []time.Duration
}

type deviceProbeResult struct {
	DeviceIndex int
	UUID        string
	Name        string
	PCIBus      string

	DeviceEdges []deviceEdge

	Notes []string

	Latency callLatency

	MeanPeriodMs int
}

type gpuDevMeta struct {
	Index  int
	UUID   string
	Name   string
	PCIBus string
	Nvml   nvml.Device
}

type gpuPollState struct {
	ctx      context.Context
	mono     *clock.Mono
	deadline time.Time
	cfg      GPUPollProbeConfig

	devs []gpuDevMeta
	out  []*deviceProbeResult
}

func newGPUPollState(ctx context.Context, mono *clock.Mono, budgetSec int, cfg GPUPollProbeConfig) *gpuPollState {
	var dl time.Time
	if budgetSec > 0 {
		dl = time.Now().Add(time.Duration(budgetSec) * time.Second)
	}
	return &gpuPollState{
		ctx:      ctx,
		mono:     mono,
		deadline: dl,
		cfg:      cfg,
	}
}

func (s *gpuPollState) deadlineExceeded() bool {
	return !s.deadline.IsZero() && time.Now().After(s.deadline)
}

// deviceEdgeDetector tracks plateau state. If field186 is ever observed,
// we ignore power edges entirely to avoid double-counting.
type deviceEdgeDetector struct {
	// availability flag: once we see any field186 value, we stick to it exclusively
	have186 bool

	// field 186 plateau tracking
	last186       *uint64
	stable186Val  *uint64
	stable186Hits int

	// power plateau tracking (used ONLY when have186==false)
	lastPower       *uint64
	stablePowerVal  *uint64
	stablePowerHits int
}

// detectEdge returns true when a NEW plateau is committed for the chosen metric.
// Energy is intentionally ignored elsewhere.
func (d *deviceEdgeDetector) detectEdge(b readBundleOut) (bool, edgeMetric) {
	const needHits = 2 // N identical reads in a row to confirm plateau

	// If we ever observe field186, lock to it and never emit power edges anymore.
	if b.Field186 != nil {
		d.have186 = true
	}

	if d.have186 {
		// ---- Use ONLY field186 ----
		if b.Field186 == nil {
			return false, edgeMetricUnknown
		}
		cur := *b.Field186
		if d.last186 == nil || *d.last186 != cur {
			d.last186 = &cur
			d.stable186Hits = 1
		} else {
			d.stable186Hits++
		}
		if d.stable186Hits >= needHits {
			if d.stable186Val == nil {
				d.stable186Val = &cur // initialize plateau; no edge
				return false, edgeMetricUnknown
			}
			if *d.stable186Val != cur {
				d.stable186Val = &cur
				return true, edgeMetricField186
			}
		}
		return false, edgeMetricUnknown
	}

	// ---- Fallback: use ONLY power when 186 never showed up ----
	if b.PowerMw == nil {
		return false, edgeMetricUnknown
	}
	cur := *b.PowerMw
	if d.lastPower == nil || *d.lastPower != cur {
		d.lastPower = &cur
		d.stablePowerHits = 1
	} else {
		d.stablePowerHits++
	}
	if d.stablePowerHits >= needHits {
		if d.stablePowerVal == nil {
			d.stablePowerVal = &cur // initialize plateau; no edge
			return false, edgeMetricUnknown
		}
		if *d.stablePowerVal != cur {
			d.stablePowerVal = &cur
			return true, edgeMetricPowerUsage
		}
	}
	return false, edgeMetricUnknown
}

func (s *gpuPollState) hyperPollRefineAll() error {
	for i := range s.devs {
		if s.deadlineExceeded() {
			return context.DeadlineExceeded
		}
		if err := s.hyperPollRefineDevice(i); err != nil {
			// propagate or just log; using _ = ... hides real errors
			return err
		}
	}
	return nil
}
func (s *gpuPollState) hyperPollRefineDevice(i int) error {
	meta := s.devs[i]
	out := s.out[i]

	// Configurable tiny poll interval (default 1ms).
	pollStep := time.Duration(s.cfg.HyperPollMs) * time.Millisecond
	perDevDeadline := time.Now().Add(time.Duration(s.cfg.RefineGuardSec) * time.Second)

	// This phase keeps its own edges
	out.DeviceEdges = nil

	// Seed the plateau detector so first commit isn't treated as an edge
	det := deviceEdgeDetector{}
	if b0, _ := s.readBundle(meta.Nvml); true {
		_, _ = det.detectEdge(b0)
		if b0.DurBundle > 0 {
			out.Latency.BundleSpanDur = append(out.Latency.BundleSpanDur, b0.DurBundle)
		}
		if b0.DurPower > 0 {
			out.Latency.PowerUsageDur = append(out.Latency.PowerUsageDur, b0.DurPower)
		}
		if b0.DurField > 0 {
			out.Latency.FieldValuesDur = append(out.Latency.FieldValuesDur, b0.DurField)
		}
	}

	// Anchor on first real edge
	var (
		startWall time.Time
		lastWall  time.Time
		edges     int
	)

	nextTick := time.Now() // drift-free loop: sleep until nextTick, then advance by pollStep

waitFirst:
	for {
		// Stop conditions
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}
		if time.Now().After(perDevDeadline) {
			return context.DeadlineExceeded
		}

		// Sleep until the scheduled tick (handles oversleep gracefully)
		now := time.Now()
		if d := nextTick.Sub(now); d > 0 {
			time.Sleep(d)
		}
		nextTick = nextTick.Add(pollStep)

		// Read+detect
		b, _ := s.readBundle(meta.Nvml)
		if ok, src := det.detectEdge(b); ok {
			t := time.Now()
			// de-dup identical wall instants (extremely unlikely)
			if n := len(out.DeviceEdges); n == 0 || !out.DeviceEdges[n-1].WhenWall.Equal(t) {
				out.DeviceEdges = append(out.DeviceEdges, deviceEdge{
					DeviceIndex: meta.Index,
					WhenWall:    t,
					Source:      src,
				})
				startWall = t
				lastWall = t
				edges = 1
				break waitFirst
			}
		}
	}

	// Collect more edges at 1ms cadence
	target := s.cfg.EdgesTarget
	if target < 20 {
		target = 20
	}

	for edges < target {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}
		if time.Now().After(perDevDeadline) {
			break
		}

		now := time.Now()
		if d := nextTick.Sub(now); d > 0 {
			time.Sleep(d)
		}
		nextTick = nextTick.Add(pollStep)

		b, _ := s.readBundle(meta.Nvml)
		if ok, src := det.detectEdge(b); ok {
			t := time.Now()
			if n := len(out.DeviceEdges); n == 0 || !out.DeviceEdges[n-1].WhenWall.Equal(t) {
				out.DeviceEdges = append(out.DeviceEdges, deviceEdge{
					DeviceIndex: meta.Index,
					WhenWall:    t,
					Source:      src,
				})
				lastWall = t
				edges++
			}
		}
	}

	if edges < 2 {
		klog.V(5).Infof("GPU-POLL: refine dev=%d collected <2 edges; cannot compute mean", meta.Index)
		return nil
	}

	span := lastWall.Sub(startWall)
	out.MeanPeriodMs = int(float64(span.Milliseconds())/float64(edges-1) + 0.5)

	klog.V(5).Infof("GPU-POLL: refine dev=%d edges=%d span=%s mean≈%dms (poll=%s)",
		meta.Index, edges, span.String(), out.MeanPeriodMs, pollStep)

	return nil
}

func (s *gpuPollState) summarizeAndRecommend() (best int, ok bool) {
	if len(s.devs) == 0 {
		return 0, false
	}

	nodeBest := 0
	numOK := 0

	for i := range s.devs {
		out := s.out[i]

		// need at least 2 edges to form a mean
		if len(out.DeviceEdges) < 2 || out.MeanPeriodMs <= 0 {
			continue
		}

		meanMs := out.MeanPeriodMs
		klog.V(2).Infof("GPU-POLL: dev=%d mean_publish=%dms edges=%d",
			out.DeviceIndex, meanMs, len(out.DeviceEdges))

		if nodeBest == 0 || meanMs < nodeBest {
			nodeBest = meanMs
		}
		numOK++
	}

	if numOK == 0 {
		return 0, false
	}
	return nodeBest, true
}

type readBundleOut struct {
	PowerMw  *uint64
	Field186 *uint64

	DurPower  time.Duration
	DurField  time.Duration
	DurBundle time.Duration
}

func (s *gpuPollState) readBundle(dev nvml.Device) (readBundleOut, error) {
	var out readBundleOut

	preWall := time.Now()

	// Field 186
	{
		start := time.Now()
		values := []nvml.FieldValue{{FieldId: nvmlFieldInstantPower}}
		if ret := dev.GetFieldValues(values); ret == nvml.SUCCESS && values[0].NvmlReturn == uint32(nvml.SUCCESS) {
			if mw, ok := decodeNVMLUint(values[0]); ok {
				//klog.V(5).Infof("InstantPower: %d", mw)
				val := uint64(mw)
				out.Field186 = &val
			}
		}
		out.DurField = time.Since(start)
	}

	// Power usage (classic)
	{
		start := time.Now()
		if mwU32, ret := dev.GetPowerUsage(); ret == nvml.SUCCESS {
			//klog.V(5).Infof("Power: %d", mwU32)
			val := uint64(mwU32)
			out.PowerMw = &val
		}
		out.DurPower = time.Since(start)
	}

	postWall := time.Now()
	out.DurBundle = postWall.Sub(preWall)

	return out, nil
}

// decodeNVMLUint mirrors your helper semantics (unsigned or double → uint64).
// If you already have this function in a shared place, reuse it instead.
func decodeNVMLUint(v nvml.FieldValue) (uint64, bool) {
	const (
		nvmlValueTypeDouble           = 0
		nvmlValueTypeUnsignedInt      = 1
		nvmlValueTypeUnsignedLong     = 2
		nvmlValueTypeUnsignedLongLong = 3
		nvmlValueTypeSignedLongLong   = 4
	)
	switch v.ValueType {
	case nvmlValueTypeUnsignedInt:
		// NVML packs little-endian; power fits in 32 bits
		return uint64(*(*uint32)(unsafe.Pointer(&v.Value[0]))), true
	case nvmlValueTypeUnsignedLong, nvmlValueTypeUnsignedLongLong:
		return *(*uint64)(unsafe.Pointer(&v.Value[0])), true
	case nvmlValueTypeDouble:
		bits := *(*uint64)(unsafe.Pointer(&v.Value[0]))
		f := math.Float64frombits(bits)
		if f < 0 {
			return 0, false
		}
		return uint64(f + 0.5), true
	default:
		return 0, false
	}
}

// IdleBaselineGPUPerDeviceFromSnap returns a per-device idle baseline (5th percentile, in Watts)
// for devices that have enough "quiet" samples within the snapshot. Devices that do not meet
// the quietness/min-sample criteria are omitted from the result map.
// Returns (map, true) if at least one device produced a baseline; otherwise (nil, false).
func IdleBaselineGPUPerDeviceFromSnap(
	ctx context.Context,
	mono *clock.Mono, // kept for signature symmetry; not used
	snap []ring.GpuTick,
) (map[string]float64, bool) {
	if len(snap) == 0 {
		return nil, false
	}

	// Tunables
	const (
		smQuietMaxPct   = 3.0 // SM/core util threshold for "quiet"
		memQuietMaxPct  = 5.0 // Memory controller util threshold
		encQuietMaxPct  = 1.0 // NVENC util threshold
		decQuietMaxPct  = 1.0 // NVDEC util threshold
		procQuietMaxPct = 3.0 // Sum of per-process compute util on that GPU
	)

	perDevQuietWatts := make(map[string][]float64)

	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		tick := snap[i]

		// Per-tick process util grouped by GPU index
		procComputeByGPU := make(map[int]float64)
		for _, p := range tick.Processes {
			u := float64(p.ComputeUtil)
			if u < 0 {
				u = 0
			} else if u > 100 {
				u = 100
			}
			procComputeByGPU[p.GpuIndex] += u
		}

		// Check each device
		for _, dev := range tick.Devices {
			if dev.PowerMilliW < 0 {
				continue
			}
			if !isGpuDeviceQuiet(dev, procComputeByGPU[dev.DeviceIndex],
				smQuietMaxPct, memQuietMaxPct, encQuietMaxPct, decQuietMaxPct, procQuietMaxPct) {
				continue
			}
			perDevQuietWatts[dev.UUID] = append(perDevQuietWatts[dev.UUID], float64(dev.PowerMilliW)/1000.0)
		}
	}

	if len(perDevQuietWatts) == 0 {
		return nil, false
	}

	out := make(map[string]float64, len(perDevQuietWatts))
	for uuid, watts := range perDevQuietWatts {
		if len(watts) == 0 {
			continue
		}
		// Compute P5 directly, no enforced sample count
		out[uuid] = P5(watts)
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// Quietness check for a single device sample within a tick.
// procComputeTotal is the sum of per-process GPU compute util for this device's GPU index in this tick.
func isGpuDeviceQuiet(
	dev ring.GpuSample,
	procComputeTotal float64,
	smMax, memMax, encMax, decMax, procMax float64,
) bool {
	// Clamp/normalize optional enc/dec pointers.
	enc := 0.0
	if dev.EncUtilPct != nil {
		enc = *dev.EncUtilPct
		if enc < 0 {
			enc = 0
		} else if enc > 100 {
			enc = 100
		}
	}
	dec := 0.0
	if dev.DecUtilPct != nil {
		dec = *dev.DecUtilPct
		if dec < 0 {
			dec = 0
		} else if dec > 100 {
			dec = 100
		}
	}

	// Clamp primary utils as well (defensive).
	sm := dev.SMUtilPct
	if sm < 0 {
		sm = 0
	} else if sm > 100 {
		sm = 100
	}
	mem := dev.MemUtilPct
	if mem < 0 {
		mem = 0
	} else if mem > 100 {
		mem = 100
	}

	// Processes sum clamp.
	if procComputeTotal < 0 {
		procComputeTotal = 0
	} else if procComputeTotal > 100 {
		procComputeTotal = 100
	}

	return sm <= smMax &&
		mem <= memMax &&
		enc <= encMax &&
		dec <= decMax &&
		procComputeTotal <= procMax
}

// CumEnergyValidationPerDeviceFromSnap validates NVML cumulative energy per device
// using a snapshot of GPU ticks. For each device that has enough data, it checks:
//   - cumulative counter is monotonic (non-decreasing)
//   - slope matches integrated InstantPowerMilliW within a tolerance
//
// Returns (map[uuid]CumEnergyDiag, true) if at least one device produced a verdict;
// otherwise (nil, false).
func CumEnergyValidationPerDeviceFromSnap(
	ctx context.Context,
	mono *clock.Mono,
	snap []ring.GpuTick,
) (map[string]CumEnergyDiag, bool) {
	if len(snap) < 2 {
		return nil, false
	}

	// ---- Tunables (consider moving to config) ----
	const (
		minEnergyJ  = 0.25 // require at least 0.25 J over window to be meaningful
		maxRelError = 0.15 // 15% relative error tolerance
	)

	// Per-device time series accumulators
	type dp struct {
		tMono uint64 // ns (from tick.SampleMeta.Mono)
		pMW   uint64 // instant power (mW)
		eMJ   uint64 // cumulative energy (mJ); only valid if okE
		okE   bool   // whether cumulative reading was present
	}

	series := make(map[string][]dp) // uuid -> []dp

	// Build per-device series from the snapshot
	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		tick := snap[i]
		// NOTE: This assumes your tick carries per-device slices (like your idle function).
		// If your current tick is flattened, adapt this loop accordingly.
		for _, dev := range tick.Devices {
			d := dp{
				tMono: tick.SampleMeta.Mono,
				pMW:   dev.InstantPowerMilliW, // integrate the *instant* signal
			}
			if dev.CumEnergyMilliJ != nil {
				d.eMJ = *dev.CumEnergyMilliJ
				d.okE = true
			}

			series[dev.UUID] = append(series[dev.UUID], d)
		}
	}

	out := make(map[string]CumEnergyDiag, len(series))
	haveAny := false

	for uuid, s := range series {
		// Count available cumulative reads
		cumReads := 0
		for _, x := range s {
			if x.okE {
				cumReads++
			}
		}
		if cumReads < 2 {
			continue
		}

		// Compute window span
		t0 := s[0].tMono
		t1 := s[len(s)-1].tMono
		winSec := float64(t1-t0) / 1e9

		// Monotonicity check (non-decreasing on the subsequence where okE==true)
		monoViol := 0
		var prev uint64
		prevSet := false
		for _, x := range s {
			if !x.okE {
				continue
			}
			if prevSet && x.eMJ < prev {
				monoViol++
			}
			prev = x.eMJ
			prevSet = true
		}
		if monoViol > 0 {
			out[uuid] = CumEnergyDiag{
				Valid:               false,
				Samples:             len(s),
				CumReads:            cumReads,
				MonotonicViolations: monoViol,
				WindowSeconds:       winSec,
			}
			haveAny = true
			continue
		}

		// Integrate instant power with trapezoidal rule
		var eintJ float64
		for i := 1; i < len(s); i++ {
			dt := float64(s[i].tMono-s[i-1].tMono) / 1e9 // seconds
			p0 := float64(s[i-1].pMW) / 1000.0           // W
			p1 := float64(s[i].pMW) / 1000.0             // W
			eintJ += 0.5 * (p0 + p1) * dt
		}

		// Delta of cumulative over first/last valid cum sample
		// (scan from both ends to find valid endpoints)
		var e0, e1 uint64
		found0 := false
		for i := 0; i < len(s); i++ {
			if s[i].okE {
				e0 = s[i].eMJ
				found0 = true
				break
			}
		}
		if !found0 {
			continue
		}
		for i := len(s) - 1; i >= 0; i-- {
			if s[i].okE {
				e1 = s[i].eMJ
				break
			}
		}
		ecumJ := float64(e1-e0) / 1000.0 // mJ → J

		// Require a meaningful amount of energy change
		if eintJ < minEnergyJ {
			out[uuid] = CumEnergyDiag{
				Valid:            false,
				Samples:          len(s),
				CumReads:         cumReads,
				IntegratedJ:      eintJ,
				CumulativeDeltaJ: ecumJ,
				RelativeError:    1.0,
				WindowSeconds:    winSec,
			}
			haveAny = true
			continue
		}

		rel := math.Abs(ecumJ-eintJ) / math.Max(eintJ, 1e-6)
		valid := rel <= maxRelError

		out[uuid] = CumEnergyDiag{
			Valid:               valid,
			Samples:             len(s),
			CumReads:            cumReads,
			MonotonicViolations: monoViol,
			IntegratedJ:         eintJ,
			CumulativeDeltaJ:    ecumJ,
			RelativeError:       rel,
			WindowSeconds:       winSec,
		}
		haveAny = true
	}

	if !haveAny {
		return nil, false
	}
	return out, true
}
