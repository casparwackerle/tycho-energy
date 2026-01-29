//go:build !production

package calibration

import (
	"bufio"
	"context"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"k8s.io/klog/v2"
)

// NOTE: This file contains legacy idle-calibration routines kept temporarily
// for reference while the new analysis package is being developed.
// They are NOT wired into the current Tycho pipeline and can be deleted once
// the new implementation is in place.
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

//-------------------------

// Returns (baselines, true) on success, or (nil, false) if no usable series
// can be built. Context cancelation aborts early with (nil, false).
func IdleBaselineRAPLFromSnap(
	ctx context.Context,
	mono *clock.Mono,
	snap []ring.RaplTick,
) (IdleBaselines, bool) {
	if len(snap) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=empty_snapshot")
		return nil, false
	}

	// Ensure chronological order by monotonic timestamp (ns).
	s := make([]ring.RaplTick, len(snap))
	copy(s, snap)
	sort.Slice(s, func(i, j int) bool { return s[i].SampleMeta.Mono < s[j].SampleMeta.Mono })

	// Per-socket accumulators of instantaneous power samples (in Watts).
	type domVals map[Domain][]float64
	acc := make(map[SocketID]domVals, 4)

	// Keep last raw energy counters (mJ) to compute deltas per socket/domain.
	var last struct {
		ok      bool
		monoNS  uint64
		sockets map[int]ring.RaplDomainCounters
	}

	push := func(sock SocketID, dom Domain, w float64) {
		if _, ok := acc[sock]; !ok {
			acc[sock] = make(domVals, 4)
		}
		acc[sock][dom] = append(acc[sock][dom], w)
	}

	for i := 0; i < len(s); i++ {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		cur := s[i]

		if last.ok {
			// Δt from monotonic timestamps (ns → s)
			if cur.SampleMeta.Mono <= last.monoNS {
				// Non-increasing or zero Δt — skip this step
				goto after
			}
			dt := float64(cur.SampleMeta.Mono-last.monoNS) / 1e9
			if dt <= 0 || math.IsInf(dt, 0) || math.IsNaN(dt) {
				goto after
			}

			// For sockets present in both last and current, compute per-domain ΔE (mJ) -> W
			for socketID, curCtr := range cur.Sockets {
				prevCtr, ok := last.sockets[socketID]
				if !ok {
					continue
				}

				type pair struct {
					dom Domain
					dmJ float64
				}
				deltas := []pair{
					{DomainPkg, float64(curCtr.Pkg) - float64(prevCtr.Pkg)},
					{DomainCore, float64(curCtr.Core) - float64(prevCtr.Core)},
					{DomainUncore, float64(curCtr.Uncore) - float64(prevCtr.Uncore)},
					{DomainDRAM, float64(curCtr.DRAM) - float64(prevCtr.DRAM)},
				}

				for _, p := range deltas {
					// Guard against wraps/garbage: negative or absurd deltas are skipped.
					if p.dmJ <= 0 || p.dmJ > 1e12 { // 1e12 mJ == 1e9 J — impossible for one tick
						continue
					}
					watts := (p.dmJ / 1000.0) / dt // mJ→J, then /s
					if watts < 0 || math.IsInf(watts, 0) || math.IsNaN(watts) {
						continue
					}
					push(SocketID(socketID), p.dom, watts)
				}
			}
		}

	after:
		last.ok = true
		last.monoNS = cur.SampleMeta.Mono
		last.sockets = cur.Sockets // reference for next delta step
	}

	if len(acc) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=no_power_series")
		return nil, false
	}

	out := make(IdleBaselines, len(acc))
	domainsPresent := make(map[Domain]bool, 4)
	totalKept := 0

	for sock, domMap := range acc {
		out[sock] = make(map[Domain]IdleBaseline, len(domMap))
		for dom, vals := range domMap {
			// Removed: len(vals) < 3 guard.
			// Rely on DeSpikeP5 to return n>0 when it could compute robust stats.
			p5, minv, n := DeSpikeP5(vals)
			if n == 0 {
				continue
			}
			out[sock][dom] = IdleBaseline{
				P5:     p5,
				Min:    minv,
				N:      n,
				FromTs: mono.Now(),
			}
			domainsPresent[dom] = true
			totalKept += n
		}
	}

	if len(out) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=empty_after_trim")
		return nil, false
	}

	klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=true sockets=%d domains=%v kept=%d",
		len(out), keysOfDomains(domainsPresent), totalKept)

	return out, true
}

func keysOfDomains(m map[Domain]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for d := range m {
		out = append(out, string(d))
	}
	return out
}

//----------------

// Returns (p5, true) on success, or (0, false) if there are no usable samples
// or the context is canceled.
func IdleBaselineRedfishFromSnap(
	ctx context.Context,
	mono *clock.Mono, // signature parity
	snap []ring.RedfishSample,
) (float64, bool) {
	if len(snap) == 0 {
		return 0, false
	}

	// (Optional) Check effective time span
	// If SampleMeta.Mono exists and you want a minimal span, enforce it here.
	// Comment out if you don't want a span guard.
	firstMono := snap[0].SampleMeta.Mono
	lastMono := snap[0].SampleMeta.Mono

	values := make([]float64, 0, len(snap))
	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return 0, false
		default:
		}
		s := snap[i]
		if s.SampleMeta.Mono < firstMono {
			firstMono = s.SampleMeta.Mono
		}
		if s.SampleMeta.Mono > lastMono {
			lastMono = s.SampleMeta.Mono
		}
		// Accept zeros (idle), skip invalid
		if s.PowerWatts < 0 || math.IsNaN(s.PowerWatts) || math.IsInf(s.PowerWatts, 0) {
			continue
		}
		values = append(values, s.PowerWatts)
	}

	if len(values) == 0 {
		return 0, false
	}

	// Outlier-robust idle baseline
	p5, _, n := DeSpikeP5(values)
	if n == 0 {
		return 0, false
	}
	return p5, true
}

//------------------------------------------------------------------------------

var cpuCount int

func init() {
	if cpuCount <= 0 {
		cpuCount = runtime.NumCPU()
	}
}

// A compact aggregation of one "tick" (all samples that share the same Mono)
type bpfTick struct {
	monoNS uint64
	// Bin-based totals (preferred when present)
	hasBins   bool
	idleNS    uint64
	irqNS     uint64
	softirqNS uint64
	// Process-based fallback (sum over all PIDs for this tick)
	sumRunUs uint64 // microseconds
}

// Group a chronological snapshot into per-tick aggregates keyed by Mono.
// With the new model, each ring.BpfTick is already a full tick: bins + per-PID deltas.
// We still merge duplicates defensively if multiple entries share the same Mono.
func groupBpfTicks(snap []ring.BpfTick) []bpfTick {
	if len(snap) == 0 {
		return nil
	}

	ticks := make([]bpfTick, 0, len(snap))
	var cur bpfTick
	var curInit bool
	var curMono uint64

	flush := func() {
		if curInit {
			ticks = append(ticks, cur)
		}
		cur = bpfTick{}
		curInit = false
	}

	for i := 0; i < len(snap); i++ {
		s := snap[i]

		// Start a new aggregate if this is the first or Mono changed.
		if !curInit || s.SampleMeta.Mono != curMono {
			flush()
			curMono = s.SampleMeta.Mono
			cur.monoNS = curMono
			cur.idleNS = s.IdleNS
			cur.irqNS = s.IRQNS
			cur.softirqNS = s.SoftirqNS
			cur.hasBins = (s.IdleNS != 0) || (s.IRQNS != 0) || (s.SoftirqNS != 0)

			// Sum per-PID runtime (µs) for this tick.
			var sum uint64
			for j := range s.Procs {
				sum += s.Procs[j].ProcessRunUs
			}
			cur.sumRunUs = sum
			curInit = true
			continue
		}

		// Defensive merge for duplicate Mono entries (shouldn't normally happen).
		cur.idleNS += s.IdleNS
		cur.irqNS += s.IRQNS
		cur.softirqNS += s.SoftirqNS
		if (s.IdleNS != 0) || (s.IRQNS != 0) || (s.SoftirqNS != 0) {
			cur.hasBins = true
		}
		for j := range s.Procs {
			cur.sumRunUs += s.Procs[j].ProcessRunUs
		}
	}
	flush()

	// SnapshotChrono() should already be chronological, but keep sort for robustness.
	sort.Slice(ticks, func(i, j int) bool { return ticks[i].monoNS < ticks[j].monoNS })
	return ticks
}

// Compute instantaneous utilization between two consecutive aggregated BPF ticks.
func instantaneousUtilBpfTicks(a, b bpfTick) float64 {
	if b.monoNS <= a.monoNS || cpuCount <= 0 {
		return 1.0
	}

	dtNS := float64(b.monoNS - a.monoNS) // interval width in ns
	denom := dtNS * float64(cpuCount)    // total CPU-time capacity across all CPUs

	// Preferred path: bins available in both ticks — derive idle fraction directly.
	if a.hasBins && b.hasBins {
		idleFrac := float64(b.idleNS) / denom
		if idleFrac < 0 {
			idleFrac = 0
		}
		if idleFrac > 1 {
			idleFrac = 1
		}
		return 1.0 - idleFrac
	}

	// Fallback: no reliable bins, approximate from per-process runtime deltas (µs → ns).
	busyNS := float64(b.sumRunUs) * 1e3
	u := busyNS / denom
	if u < 0 {
		u = 0
	}
	if u > 1 {
		u = 1
	}
	return u
}

// Decide "continuous low CPU" from a recent BPF snapshot.
// Returns: (isQuiet, fracBelowCut, meanUtil)
func isQuietFromBpfSnap(snap []ring.BpfTick) (bool, float64, float64) {
	ticks := groupBpfTicks(snap)
	if len(ticks) < 2 {
		return false, 0, 1
	}

	const (
		requiredMean = 0.05 // 5%
		lowCut       = 0.10 // 10%
		requiredFrac = 0.80 // 80%
	)

	var (
		weightedSumU float64 // Σ u_i * dt_i
		totalDt      float64 // Σ dt_i
		lowDt        float64 // Σ dt_i where u_i <= lowCut
	)

	for i := 1; i < len(ticks); i++ {
		a, b := ticks[i-1], ticks[i]
		if b.monoNS <= a.monoNS {
			continue
		}
		dt := float64(b.monoNS - a.monoNS) // ns
		if dt <= 0 {
			continue
		}
		u := instantaneousUtilBpfTicks(a, b)
		if math.IsNaN(u) || math.IsInf(u, 0) || u < 0 {
			continue
		}
		weightedSumU += u * dt
		totalDt += dt
		if u <= lowCut {
			lowDt += dt
		}
	}

	if totalDt == 0 {
		return false, 0, 1
	}

	mean := weightedSumU / totalDt
	frac := lowDt / totalDt
	return mean <= requiredMean && frac >= requiredFrac, frac, mean
}

// meanCPU computes the average instantaneous CPU utilization over a recent BPF snapshot.
// Used only for logging or summary statistics.
func meanCPU(snap []ring.BpfTick) float64 {
	ticks := groupBpfTicks(snap)
	if len(ticks) < 2 {
		return 1.0
	}

	var sumWeighted float64
	var totalDt float64

	for i := 1; i < len(ticks); i++ {
		a, b := ticks[i-1], ticks[i]
		if b.monoNS <= a.monoNS {
			continue
		}
		dt := float64(b.monoNS - a.monoNS)
		u := instantaneousUtilBpfTicks(a, b)
		if math.IsNaN(u) || math.IsInf(u, 0) || u < 0 {
			continue
		}
		sumWeighted += u * dt
		totalDt += dt
	}

	if totalDt == 0 {
		return 1.0
	}
	return sumWeighted / totalDt
}

//--------------------------

type CalibDeps struct {
	Mono *clock.Mono

	Bpf  *ring.Sync[ring.BpfTick]
	Rapl *ring.Sync[ring.RaplTick]
	Rf   *ring.Sync[ring.RedfishSample]
	Gpu  *ring.Sync[ring.GpuTick]
}

// ConfigView is a read-only view of calibration-related knobs.
// Your concrete config should implement this; the orchestrator reads via this interface.
type ConfigView interface {
	// Idle calibration policy
	CalibrationIdleBudgetSec() int
	CalibrationQuietGuardSec() int

	// Sensor enable/disable + per-sensor idle switches
	EnableRapl() bool
	EnableRedfish() bool
	EnableGpu() bool
	CalibrationRaplIdleEnabled() bool
	CalibrationRedfishIdleEnabled() bool
	CalibrationGpuIdleEnabled() bool
}

var (
	depsMu sync.RWMutex
	deps   *CalibDeps
)

// Init wires calibration dependencies once at startup.
// It is safe to call exactly once; subsequent calls replace the handle atomically.
func Init(d CalibDeps) {
	depsMu.Lock()
	defer depsMu.Unlock()
	// Shallow copy is sufficient; rings and mono are pointers.
	local := d
	deps = &local
}

// HasDeps reports whether Init() has been called successfully.
func HasDeps() bool {
	depsMu.RLock()
	defer depsMu.RUnlock()
	return deps != nil
}

// getDeps returns the current dependency handle (or nil if uninitialized).
// Internal use by the orchestrator and helpers.
func getDeps() *CalibDeps {
	depsMu.RLock()
	defer depsMu.RUnlock()
	return deps
}

/* ==============================
   Idle baselines & calibration KV
   (existing store, unchanged)
   ============================== */

// idleStore holds the current best idle baselines and simple calibration bookkeeping.
// Memory-only; thread-safe; no external persistence.
type idleStore struct {
	mu sync.RWMutex

	idle         IdleBaselines
	hasCumEnergy map[string]CumEnergyDiag

	lastCalibMono        int64
	hysteresisPct        float64
	confirmationsNeeded  int
	pendingConfirmations map[idKey]int
}

// CumEnergyDiag reports per-device validation results for cumulative energy.
type CumEnergyDiag struct {
	Valid               bool    // final verdict
	Samples             int     // number of ticks considered
	CumReads            int     // ticks where cumulative mJ was available
	MonotonicViolations int     // count of strictly decreasing cum samples
	IntegratedJ         float64 // energy from integrating InstantPowerMilliW (J)
	CumulativeDeltaJ    float64 // delta of cumulative counter over window (J)
	RelativeError       float64 // |Ecum - Eintegrated| / max(Eintegrated, eps)
	WindowSeconds       float64 // total time span of the considered ticks
}

type idKey struct {
	socket SocketID
	domain Domain
}

var (
	_store = &idleStore{
		idle:                 make(IdleBaselines),
		hasCumEnergy:         make(map[string]CumEnergyDiag),
		hysteresisPct:        0.01,
		confirmationsNeeded:  3,
		pendingConfirmations: make(map[idKey]int),
	}
)

// Optionally allow tweaking defaults from elsewhere in the package (not exported).
func configureIdleHysteresis(pct float64, confirmations int) {
	_store.mu.Lock()
	defer _store.mu.Unlock()
	if pct < 0 {
		pct = 0
	}
	if confirmations < 1 {
		confirmations = 1
	}
	_store.hysteresisPct = pct
	_store.confirmationsNeeded = confirmations
	_store.pendingConfirmations = make(map[idKey]int) // reset counters
}

// GetIdle returns a shallow copy of the current IdleBaselines.
// Callers should treat the returned structure as read-only.
func GetIdle() IdleBaselines {
	_store.mu.RLock()
	defer _store.mu.RUnlock()

	out := make(IdleBaselines, len(_store.idle))
	for s, domMap := range _store.idle {
		cp := make(map[Domain]IdleBaseline, len(domMap))
		for d, bl := range domMap {
			cp[d] = bl
		}
		out[s] = cp
	}
	return out
}

// SetIdleAll replaces all known idle baselines (e.g., after a successful calibration run).
// Resets pending confirmations (fresh ground truth).
func SetIdleAll(b IdleBaselines) {
	_store.mu.Lock()
	defer _store.mu.Unlock()

	// Deep copy to internal store
	newStore := make(IdleBaselines, len(b))
	for s, domMap := range b {
		cp := make(map[Domain]IdleBaseline, len(domMap))
		for d, bl := range domMap {
			cp[d] = bl
		}
		newStore[s] = cp
	}
	_store.idle = newStore
	_store.pendingConfirmations = make(map[idKey]int)
}

// MaybeUpdateIdle attempts to lower the P5 baseline for a given socket/domain.
// It only accepts an update if the candidate value is lower than the current P5
// by at least hysteresisPct AND this has been observed confirmationsNeeded times.
// Returns (updated, newBaseline).
func MaybeUpdateIdle(socket SocketID, dom Domain, candidateP5 float64, ts uint64) (bool, IdleBaseline) {
	_store.mu.Lock()
	defer _store.mu.Unlock()

	dmap, ok := _store.idle[socket]
	if !ok || dmap == nil {
		// No existing baseline → we refuse to initialize via runtime refine.
		// A full calibration (SetIdleAll) should initialize first.
		return false, IdleBaseline{}
	}
	cur, ok := dmap[dom]
	if !ok {
		return false, IdleBaseline{}
	}

	// Require strictly lower by hysteresisPct.
	threshold := cur.P5 * (1.0 - _store.hysteresisPct)
	if candidateP5 >= threshold {
		// Not low enough → reset confirmations for this key (avoid stale positives).
		delete(_store.pendingConfirmations, idKey{socket, dom})
		return false, cur
	}

	// Count confirmations for this key.
	k := idKey{socket, dom}
	_store.pendingConfirmations[k] = _store.pendingConfirmations[k] + 1
	if _store.pendingConfirmations[k] < _store.confirmationsNeeded {
		return false, cur
	}

	// Accept: update baseline, reset counter.
	newBL := IdleBaseline{
		P5:     candidateP5,
		Min:    minf(cur.Min, candidateP5), // keep min as the best known minimum
		N:      cur.N,                      // N remains from last robust window (or update if you’ve recomputed it)
		FromTs: ts,
	}
	dmap[dom] = newBL
	_store.pendingConfirmations[k] = 0
	return true, newBL
}

func minf(a, b float64) float64 {
	if a == 0 {
		return b
	}
	if b < a {
		return b
	}
	return a
}

// LastCalibMono returns the monotonic timestamp of the last full calibration run.
func LastCalibMono() int64 {
	_store.mu.RLock()
	defer _store.mu.RUnlock()
	return _store.lastCalibMono
}

//---------------------------

type IdleBaseline struct {
	P5     float64 // robust idle estimate (preferred)
	Min    float64 // raw min (for reference)
	N      int     // samples considered after de-spiking
	FromTs uint64  // mono.Nano() or similar
}

type IdleBaselines map[SocketID]map[Domain]IdleBaseline

// SensorMask selects which sensors to calibrate in an orchestrated run.
type SensorMask uint8

const (
	MaskNone    SensorMask = 0
	MaskRAPL    SensorMask = 1 << 0
	MaskRedfish SensorMask = 1 << 1
	MaskGPU     SensorMask = 1 << 2
	MaskAll                = MaskRAPL | MaskRedfish | MaskGPU
)

//---------------------------
//utils

func alignToQuantum(ms, quantumMs int) int {
	if quantumMs <= 1 {
		return ms
	}
	// Round up to the nearest multiple of quantum to avoid undersampling due to rounding.
	rem := ms % quantumMs
	if rem == 0 {
		return ms
	}
	return ms + (quantumMs - rem)
}

// QuietWindowGuard performs a best-effort "quiet" check over 'dur' using Linux /proc/stat.
// It measures CPU utilization between two samples. If util < 2%, returns ok=true.
// I/O/temperature are omitted to keep this helper small; you can extend later.
func QuietWindowGuard(ctx context.Context, _ interface{}, dur time.Duration) (bool, string) {
	// Read t0
	idle0, total0, ok0 := readProcStatCPU()
	if !ok0 {
		// If we can't read CPU stats, err on the permissive side but explain.
		select {
		case <-time.After(dur):
		case <-ctx.Done():
		}
		return true, "cpu_unknown_permissive"
	}

	// Sleep for duration or until ctx canceled
	t := time.NewTimer(dur)
	select {
	case <-t.C:
	case <-ctx.Done():
		t.Stop()
		return false, "context_canceled"
	}

	// Read t1
	idle1, total1, ok1 := readProcStatCPU()
	if !ok1 || total1 <= total0 || idle1 < idle0 {
		return true, "cpu_unknown_permissive"
	}

	idleDelta := float64(idle1 - idle0)
	totalDelta := float64(total1 - total0)
	util := 1.0 - (idleDelta / totalDelta) // fraction

	if util < 0.02 {
		return true, "cpu_util_ok"
	}
	return false, "cpu_util_high"
}

// EffectiveArrivalRate computes the achieved sample rate relative to the requested period.
// Returns a value clamped to [0,1]. If fewer than 2 timestamps, returns 0.
func EffectiveArrivalRate(requestedMs int, times []time.Time) float64 {
	if requestedMs <= 0 || len(times) < 2 {
		return 0
	}
	first := times[0]
	last := times[len(times)-1]
	window := last.Sub(first)
	if window <= 0 {
		return 0
	}

	expected := float64(window.Milliseconds()) / float64(requestedMs)
	if expected <= 0 {
		return 0
	}
	// Observed intervals = len(times)-1
	observed := float64(len(times) - 1)
	r := observed / expected
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

// DropRatio returns (total-ok)/total in [0,1]. If total<=0, returns 0.
func DropRatio(total, ok int) float64 {
	if total <= 0 {
		return 0
	}
	if ok < 0 {
		ok = 0
	}
	if ok > total {
		ok = total
	}
	return float64(total-ok) / float64(total)
}

// EarlyStopSatisfied decides if search can stop early given the recent path.
// Rule of thumb (tunable):
// - Need at least 2 recent OK points with Samples>=minSamples.
// - The ms gap between them <= targetPrecMs.
// - Both have low drop ratio and similar effRate (delta <= 0.10).
func EarlyStopSatisfied(path []PollProbePoint, targetPrecMs, minSamples int) (bool, string) {
	n := len(path)
	if n < 2 {
		return false, "insufficient_points"
	}
	a := path[n-1]
	b := path[n-2]

	// Both must be OK with enough samples
	if !(a.Ok && b.Ok) || a.Samples < minSamples || b.Samples < minSamples {
		return false, "not_enough_ok_samples"
	}
	// Precision condition
	if absInt(a.Ms-b.Ms) > targetPrecMs {
		return false, "precision_not_reached"
	}
	// Drop & effRate conditions
	da := DropRatio(a.Samples, a.Samples-a.Errs)
	db := DropRatio(b.Samples, b.Samples-b.Errs)
	if da > 0.05 || db > 0.05 { // allow up to 5% drops
		return false, "high_drop_rate"
	}
	if math.Abs(a.EffRate-b.EffRate) > 0.10 {
		return false, "eff_rate_unstable"
	}
	return true, "stable_enough"
}

// -------------------------------
// Internal helpers
// -------------------------------

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// readProcStatCPU reads aggregate CPU counters from /proc/stat and returns (idle,total).
// total = user+nice+system+idle+iowait+irq+softirq+steal (guest excluded for simplicity).
func readProcStatCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	line := sc.Text()
	if !strings.HasPrefix(line, "cpu ") {
		return 0, 0, false
	}
	fields := strings.Fields(line)
	// fields: cpu user nice system idle iowait irq softirq steal guest guest_nice
	// We use up to 'steal' (index 8)
	if len(fields) < 8 {
		return 0, 0, false
	}
	var nums [8]uint64
	for i := 0; i < 8; i++ {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		nums[i] = v
	}
	idle = nums[3] + nums[4] // idle + iowait
	total = 0
	for i := 0; i < 8; i++ {
		total += nums[i]
	}
	return idle, total, true
}

// ---- Sync wrapper passthrough (ensure you have this in ring/sync.go) --------
// func (s *Sync[T]) SnapshotChrono() []T { s.mu.RLock(); defer s.mu.RUnlock(); return s.ring.SnapshotChrono() }

func snapshotRedfishAll(buf *ring.Sync[ring.RedfishSample]) []ring.RedfishSample {
	return buf.SnapshotChrono()
}

func snapshotGpuAll(buf *ring.Sync[ring.GpuTick]) []ring.GpuTick {
	return buf.SnapshotChrono()
}

func snapshotRaplAll(buf *ring.Sync[ring.RaplTick]) []ring.RaplTick {
	return buf.SnapshotChrono()
}
