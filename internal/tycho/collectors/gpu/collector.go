// file internal/tycho/collectors/gpu/collector.go:
package gpuCollector

import (
	"context"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator/devices"
	"k8s.io/klog/v2"
)

// Config holds dependencies/settings for the GPU collector.
type Config struct {
	Buf  *ring.Sync[ring.GpuTick]
	Mono *clock.Mono
}

// New returns a GPU collector.
func New(cfg Config) *Collector {
	return &Collector{
		buf:  cfg.Buf,
		mono: cfg.Mono,
	}
}

// NewForCalibration returns a collector *without* a ring buffer, suitable
// for short-lived probes like PollProbeGPU. The caller must call Init/Close.
func NewForCalibration(m *clock.Mono) *Collector {
	return &Collector{
		mono: m,
		// buf intentionally nil
	}
}

// DerivePhaseAwareFromEngineInterval derives phase-aware polling from the engine cadence Teng.
// Target scenario: device publishes roughly every Teng (e.g., 500ms). We:
//   - seed T̂ := Teng (not 2×),
//   - allow EMA to adapt in [~0.75·Teng, 4·Teng],
//   - ensure BaseInterval ≤ Teng (never slower than engine),
//   - keep a tight burst window with a faster burst interval near edges.
func DerivePhaseAwareFromEngineInterval(Teng time.Duration) PhaseAwarePollConfig {
	if Teng <= 0 {
		Teng = 100 * time.Millisecond // guard
	}

	mul := func(d time.Duration, num, den int) time.Duration {
		return d * time.Duration(num) / time.Duration(den)
	}
	clamp := func(v, lo, hi time.Duration) time.Duration {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	maxDur := func(a, b time.Duration) time.Duration {
		if a > b {
			return a
		}
		return b
	}
	minDur := func(a, b time.Duration) time.Duration {
		if a < b {
			return a
		}
		return b
	}

	// Seed T̂ to the engine cadence (align-by-default).
	TdevGuess := Teng

	// Let EMA explore from ~0.75·Teng up to 4·Teng.
	Tmin := mul(Teng, 3, 4) // 0.75·Teng
	Tmax := 4 * Teng

	// Base polling: a tad faster than the engine to help catch edges without hyperpolling.
	// Choose 0.8·T̂ capped at Teng (never slower than engine).
	base := minDur(mul(TdevGuess, 4, 5), Teng) // min(0.8·T̂, Teng)
	// Keep base within [0.5·Teng, Teng] to avoid extremes.
	base = clamp(base, mul(Teng, 1, 2), Teng)

	// Burst polling: near predicted edges. Use 0.25·Teng (or 100ms floor).
	burst := maxDur(mul(Teng, 1, 4), 100*time.Millisecond)
	// And clamp within [Teng/6, Teng/2].
	burst = clamp(burst, mul(Teng, 1, 6), mul(Teng, 1, 2))

	// Window half-width: about a third of T̂ so we tighten around edges.
	half := mul(TdevGuess, 1, 3) // ~0.33·T̂
	// Keep within [0.2·T̂, 0.5·T̂].
	half = clamp(half, mul(TdevGuess, 1, 5), mul(TdevGuess, 1, 2))

	return PhaseAwarePollConfig{
		Enabled: true,

		// Cadence model
		InitialPublishPeriod:  TdevGuess,
		MinPublishPeriodClamp: Tmin,
		MaxPublishPeriodClamp: Tmax,
		PhaseEMAAlpha:         0.20, // phase EMA
		PeriodEMAAlpha:        0.10, // period EMA

		// Polling schedule
		BaseInterval:   base,
		BurstInterval:  burst,
		BurstHalfWidth: half,

		// Dedupe threshold (power)
		EpsilonMilliW: 2,
	}
}

// Init discovers the active accelerator and enumerates GPU devices/handles.
// It returns nil if no GPUs are available so the caller can proceed without failing startup.
// After a successful init, c.inited is set and Collect() can sample devices.
func (c *Collector) Init(ctx context.Context) error {
	if c.inited {
		return nil
	}

	// Find active accelerator (Kepler side). If GPU disabled or unavailable, noop gracefully.
	accel := accelerator.GetActiveAcceleratorByType(config.GPU)
	if accel == nil {
		klog.V(2).Info("gpuCollector: no active accelerator found; GPU collection disabled")
		return nil
	}
	devIf := accel.Device()
	c.devIf = devIf
	c.backendStr = devIf.DevType().String()

	byID := devIf.DevicesByID()
	if len(byID) == 0 {
		klog.V(2).Info("gpuCollector: accelerator reports no devices; GPU collection disabled")
		return nil
	}

	c.devs = make([]gpuDeviceState, 0, len(byID))
	for id, anyDev := range byID {
		gd, ok := anyDev.(devices.GPUDevice)
		if !ok || gd.DeviceHandler == nil {
			continue
		}
		nv, ok := gd.DeviceHandler.(nvml.Device)
		if !ok {
			continue
		}

		// Static metadata
		uuid, _ := nv.GetUUID()
		name, _ := nv.GetName()
		pci, _ := nv.GetPciInfo()
		bus := cChar32ToString(pci.BusId) // convert [32]int8 -> string

		// Capability probe: cumulative energy (mJ)
		hasCum := false
		if _, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
			hasCum = true
		}

		c.devs = append(c.devs, gpuDeviceState{
			Index:               id,
			Nvml:                nv,
			UUID:                uuid,
			Name:                name,
			PCIBus:              bus,
			HasCumulativeEnergy: hasCum,
		})
	}

	if len(c.devs) == 0 {
		klog.V(2).Info("gpuCollector: no NVML-capable devices; GPU collection disabled")
		return nil
	}

	c.inited = true
	klog.V(3).Infof("gpuCollector: initialized %d device(s) via backend=%s", len(c.devs), c.backendStr)
	// Initialize lastMono so first dt is sane
	c.lastMono = c.mono.From(time.Now())
	c.lastProcQueryMono = make(map[int]uint64)
	if c.lastProcQueryTime == nil {
		c.lastProcQueryTime = make(map[int]time.Time)
	}
	return nil
}

// SetCumEnergyDiag ingests per-UUID validity flags for cumulative energy.
// NOTE: This intentionally avoids importing the calibration package to prevent cycles.
func (c *Collector) SetCumEnergyDiag(valid map[string]bool) {
	if c == nil {
		return
	}
	c.cumMu.Lock()
	defer c.cumMu.Unlock()
	if c.cumValid == nil {
		c.cumValid = make(map[string]bool, len(valid))
	}
	for uuid, ok := range valid {
		c.cumValid[uuid] = ok
	}
}

// cumEnergyOK reports if cumulative energy is considered usable for a given uuid.
func (c *Collector) cumEnergyOK(uuid string) bool {
	c.cumMu.RLock()
	defer c.cumMu.RUnlock()
	if c.cumValid == nil {
		return false
	}
	return c.cumValid[uuid]
}

// -----------------------------------------------------------------------------
// 1) Public configuration extensions (minimal surface)
// -----------------------------------------------------------------------------

// PhaseAwarePollConfig controls the internal sampler's phase-aware burst behavior.
// All durations are engine-agnostic and use monotonic time as provided by c.mono.
type PhaseAwarePollConfig struct {
	Enabled bool // if false, sampler runs in fixed-base mode

	// Device cadence model (estimates)
	InitialPublishPeriod  time.Duration // e.g., 500 * time.Millisecond
	MinPublishPeriodClamp time.Duration // e.g., 450ms
	MaxPublishPeriodClamp time.Duration // e.g., 600ms
	PhaseEMAAlpha         float64       // e.g., 0.2
	PeriodEMAAlpha        float64       // e.g., 0.1 (set 0 to disable period EMA)

	// Polling schedule
	BaseInterval   time.Duration // e.g., 300ms outside burst
	BurstInterval  time.Duration // e.g., 50ms during burst
	BurstHalfWidth time.Duration // e.g., 150ms around predicted edge

	// New sample detection
	EpsilonMilliW int // e.g., 3–5 mW (power change threshold if CumEnergy unreliable)
}

// SamplerLimits caps allocations and CPU usage for the sampler.
type SamplerLimits struct {
	MaxEventQueue int // bounded queue length (e.g., 4–8)
}

// -----------------------------------------------------------------------------
// 2) Internal event model & queue
// -----------------------------------------------------------------------------

// gpuSamplerEvent is produced by the sampler and consumed by Collect().
type gpuSamplerEvent struct {
	// tObsMono is the observed monotonic timestamp for the updated device snapshot.
	tObsMono uint64
	Devices  []ring.GpuSample     // per-device instantaneous snapshot
	Procs    []ring.GpuProcSample // optional per-process slice (if enabled)
	// changed indicates this event reflects a fresh device publish (vs. synthetic/no-change).
	Changed bool
}

// gpuEventQueue is a minimal bounded, non-blocking queue abstraction.
// Overwrite-on-full policy to keep freshest events.
type gpuEventQueue struct {
	mu   sync.Mutex
	buf  []gpuSamplerEvent
	head int // next index to read
	tail int // next index to write
	size int // current number of items
	cap  int // fixed capacity
}

// newGpuEventQueue constructs a bounded queue.
func newGpuEventQueue(capacity int) *gpuEventQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &gpuEventQueue{
		buf:  make([]gpuSamplerEvent, capacity),
		head: 0,
		tail: 0,
		size: 0,
		cap:  capacity,
	}
}

// Enqueue tries to add an event; overwrites oldest entry if the queue is full.
func (q *gpuEventQueue) Enqueue(ev gpuSamplerEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.size == q.cap {
		// Queue full → overwrite oldest (advance head).
		q.head = (q.head + 1) % q.cap
		q.size--
	}

	q.buf[q.tail] = ev
	q.tail = (q.tail + 1) % q.cap
	q.size++
	klog.V(5).Infof("GPU-QUEUE: enqueue ts=%d changed=%v size=%d/%d", ev.tObsMono, ev.Changed, q.size, q.cap)
}

// TryDequeue returns (event, ok) without blocking.
// If the queue is empty, ok=false and an empty event is returned.
func (q *gpuEventQueue) TryDequeue() (gpuSamplerEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.size == 0 {
		return gpuSamplerEvent{}, false
	}

	ev := q.buf[q.head]
	q.head = (q.head + 1) % q.cap
	q.size--

	return ev, true
}

// -----------------------------------------------------------------------------
// 3) Phase estimator state (PhaseAwarePolling core)
// -----------------------------------------------------------------------------

// phaseEstimator tracks the device publish period and phase vs. Tycho's monotonic clock.
type phaseEstimator struct {
	Enabled bool

	// Estimated device cadence
	Period   time.Duration // T̂
	Phase    time.Duration // ϕ̂
	AlphaPhi float64       // EMA alpha for phase correction
	AlphaT   float64       // EMA alpha for period correction
	MinT     time.Duration // clamp lower bound for period updates
	MaxT     time.Duration // clamp upper bound for period updates
	quantum  time.Duration

	// Last observation bookkeeping for drift and jitter handling
	lastObsMono uint64
}

// nextEdgeMono predicts the next publish edge in monotonic units (ns) at or after nowMono.
func (p *phaseEstimator) nextEdgeMono(nowMono uint64) uint64 {
	if !p.Enabled || p.Period <= 0 || p.quantum <= 0 {
		return nowMono
	}
	// Convert Duration → mono ticks (round down)
	T := uint64(p.Period / p.quantum)
	if T == 0 { // avoid degenerate zero
		return nowMono
	}

	// Normalize phase into [0, T)
	phiTicks := int64((p.Phase % p.Period) / p.quantum)
	if phiTicks < 0 {
		// wrap into positive range
		mod := int64(T)
		phiTicks = ((phiTicks % mod) + mod) % mod
	}
	phi := uint64(phiTicks)

	// If we have a last observation, step forward in multiples of T
	if p.lastObsMono != 0 {
		last := p.lastObsMono
		next := last + T
		if next >= nowMono {
			return next
		}
		delta := nowMono - last
		steps := delta / T
		return last + (steps+1)*T
	}

	// Align by phase so that k*T + phi >= nowMono
	if nowMono <= phi {
		return phi
	}
	off := nowMono - phi
	rem := off % T
	if rem == 0 {
		return nowMono
	}
	return nowMono + (T - rem)
}

// correctPhase updates the phase estimate using the observed delta from the predicted edge.
// It applies a simple EMA step and normalizes Phase into [0, Period).
func (p *phaseEstimator) correctPhase(delta time.Duration) {
	if !p.Enabled || p.Period <= 0 || p.AlphaPhi <= 0 {
		return
	}

	// EMA: Phase += α * delta
	adj := time.Duration(p.AlphaPhi * float64(delta))
	p.Phase += adj

	// Normalize Phase into [0, Period)
	if p.Phase >= p.Period || p.Phase < 0 {
		// Go's % keeps the sign of the dividend, so re-normalize to positive
		p.Phase = p.Phase % p.Period
		if p.Phase < 0 {
			p.Phase += p.Period
		}
	}
}

// correctPeriod updates the estimated publish period (T̂) using EMA smoothing
// on the observed inter-sample duration. The result is clamped within
// [MinT, MaxT] to avoid runaway drift.
func (p *phaseEstimator) correctPeriod(dt time.Duration) {
	if !p.Enabled || p.AlphaT <= 0 || dt <= 0 {
		return
	}

	newT := (1-p.AlphaT)*float64(p.Period) + p.AlphaT*float64(dt)
	p.Period = time.Duration(newT)

	// Clamp to configured bounds, if any
	if p.MinT > 0 && p.Period < p.MinT {
		p.Period = p.MinT
	}
	if p.MaxT > 0 && p.Period > p.MaxT {
		p.Period = p.MaxT
	}
}

// -----------------------------------------------------------------------------
// 4) Sampler: internal mini-scheduler (runs in its own goroutine)
// -----------------------------------------------------------------------------

// sampler encapsulates the variable-rate polling loop and dedupe logic.
type sampler struct {
	mono    *clock.Mono
	cfgB    PhaseAwarePollConfig
	limits  SamplerLimits
	quantum time.Duration

	// Hooks into the existing collector for device/process reads (no code duplication).
	parent *Collector

	// Phase-aware model
	phase phaseEstimator

	// Output queue consumed by Collect()
	q *gpuEventQueue

	// Sticky copy of the last-known snapshot for no-change ticks
	lastDevices []ring.GpuSample
	lastProcs   []ring.GpuProcSample

	// Control
	stopCh  chan struct{}
	doneCh  chan struct{}
	running bool
}

// start launches the sampler loop in its own goroutine.
// It is safe to call once after initialization; subsequent calls are ignored.
func (s *sampler) start(ctx context.Context) {
	if s == nil || s.running {
		return
	}

	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	s.running = true

	go func() {
		defer close(s.doneCh)
		func() { s.running = false }()
		s.loop(ctx)
	}()
}

// stop requests termination and waits for exit (best-effort).
func (s *sampler) stop() {
	if s == nil || !s.running {
		return
	}

	// Signal the sampler loop to stop.
	close(s.stopCh)
	select {
	case <-s.doneCh:
	case <-time.After(500 * time.Millisecond):
	}
	s.running = false

	// Give the loop a short grace period to exit; no hard synchronization required.
	// This avoids deadlocks while ensuring background goroutine has time to terminate.
	timeout := time.NewTimer(200 * time.Millisecond)
	defer timeout.Stop()

	select {
	case <-timeout.C:
		// Timed out waiting for graceful stop — mark as not running anyway.
		s.running = false
	default:
		// In normal operation, loop() should observe <-s.stopCh and return quickly.
	}
}

// loop is the phase-aware polling state machine (Base vs Burst).
func (s *sampler) loop(ctx context.Context) {
	if s == nil || s.mono == nil {
		return
	}

	for {
		// Determine current mode and interval.
		nowMono := s.mono.From(time.Now())
		var interval time.Duration
		if s.shouldBurst(nowMono) && s.cfgB.Enabled {
			interval = s.cfgB.BurstInterval
		} else {
			interval = s.cfgB.BaseInterval
		}
		if interval <= 0 {
			interval = 200 * time.Millisecond // conservative fallback
		}

		// Sleep until next poll or stop.
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			// proceed
		}

		// Read a fresh snapshot.
		devs, procs, ok := s.readSnapshot()
		if !ok {
			// Could not read -> skip this cycle
			continue
		}

		// Dedupe: detect whether device values reflect a new publish.
		changed := s.detectNewSample(devs)
		tObs := s.mono.From(time.Now())

		if changed {
			// Emit event and refine phase/period estimates.
			s.onNewObservation(tObs, devs, procs)
		} else {
			// No new device publish -> skip this cycle
		}
	}
}

// shouldBurst decides whether to enter burst mode at nowMono.
// Returns true if the current monotonic time lies within ±BurstHalfWidth
// of the predicted next device publish edge.
func (s *sampler) shouldBurst(nowMono uint64) bool {
	if !s.cfgB.Enabled || s.phase.Period <= 0 || s.quantum <= 0 {
		return false
	}
	nextEdge := s.phase.nextEdgeMono(nowMono)
	if nextEdge == 0 {
		return false
	}
	var dist uint64
	if nowMono > nextEdge {
		dist = nowMono - nextEdge
	} else {
		dist = nextEdge - nowMono
	}
	bw := uint64(s.cfgB.BurstHalfWidth / s.quantum) // convert Duration to mono ticks
	if bw == 0 {
		return false
	}
	return dist <= bw
}

// readSnapshot reads devices (+optional per-process) and returns a snapshot.
// It succeeds if device sampling succeeds; per-process sampling is optional.
func (s *sampler) readSnapshot() (devs []ring.GpuSample, procs []ring.GpuProcSample, ok bool) {
	if s == nil || s.parent == nil || s.mono == nil {
		return nil, nil, false
	}

	// Devices are mandatory for a valid snapshot.
	dv, okDev := s.parent.readAllDevicesSnapshot()
	if !okDev || len(dv) == 0 {
		return nil, nil, false
	}

	// Per-process is optional; failure yields an empty slice but overall ok=true.
	nowMono := s.mono.From(time.Now())
	pr, okProc := s.parent.readPerProcessSnapshot(nowMono)
	if !okProc {
		pr = nil
	}

	return dv, pr, true
}

// detectNewSample compares against the last snapshot (preferring cumulative energy when present)
// and returns true if ANY device indicates a fresh publish.
// Priority:
//  1. If both current/previous have CumEnergyMilliJ: any increase => fresh
//  2. Else power delta > EpsilonMilliW => fresh
//  3. Else SM or MEM util delta > utilEps (pp) => fresh
func (s *sampler) detectNewSample(curr []ring.GpuSample) bool {
	if len(curr) == 0 {
		return false
	}
	if len(s.lastDevices) == 0 {
		s.lastDevices = curr
		return true
	}

	// thresholds
	epsMW := s.cfgB.EpsilonMilliW
	if epsMW <= 0 {
		epsMW = 3
	}
	//utilEps := 1.0 // percentage points; small but filters jitter

	// Build a quick lookup for previous by UUID (handles reorder)
	prevByUUID := make(map[string]*ring.GpuSample, len(s.lastDevices))
	for i := range s.lastDevices {
		prevByUUID[s.lastDevices[i].UUID] = &s.lastDevices[i]
	}

	for i := range curr {
		c := curr[i]
		prev := prevByUUID[c.UUID]
		if prev == nil {
			// New device appeared -> treat as fresh
			s.lastDevices = curr
			return true
		}

		// (1) Cumulative energy preferred when available on both AND validated as usable
		useCum := c.CumEnergyMilliJ != nil &&
			prev.CumEnergyMilliJ != nil &&
			s.parent != nil &&
			s.parent.cumEnergyOK(c.UUID)

		if useCum {
			// any increase = fresh publish
			if *c.CumEnergyMilliJ > *prev.CumEnergyMilliJ {
				klog.V(5).Infof("GPU-DEDUPE: uuid=%s cumE mJ prev=%d curr=%d (validated) -> changed=true",
					c.UUID, *prev.CumEnergyMilliJ, *c.CumEnergyMilliJ)
				s.lastDevices = curr
				return true
			}
			// equal or decrease: fall through to power/util checks
		}

		// (2) Power delta
		dp := c.PowerMilliW - prev.PowerMilliW
		if dp < 0 {
			dp = -dp
		}
		if dp > epsMW {
			klog.V(5).Infof("GPU-DEDUPE: uuid=%s power %d -> %d Δ=%d mW (>%d) -> changed=true",
				c.UUID, prev.PowerMilliW, c.PowerMilliW, dp, epsMW)
			s.lastDevices = curr
			return true
		}

	}

	// If we reach here, none of the devices indicate freshness
	return false
}

// onNewObservation enqueues event and refines phase/period estimates.
func (s *sampler) onNewObservation(tObsMono uint64, devs []ring.GpuSample, procs []ring.GpuProcSample) {
	if s == nil || s.q == nil {
		return
	}

	// ---- Phase/period refinement --------------------------------------------
	if s.phase.Enabled && s.phase.Period > 0 {
		// Predict the edge nearest to tObsMono (choose between next and previous).
		nextEdge := s.phase.nextEdgeMono(tObsMono)
		T := uint64(s.phase.Period)

		// Candidate previous edge (handle potential underflow carefully).
		var prevEdge uint64
		if nextEdge >= T {
			prevEdge = nextEdge - T
		} else {
			prevEdge = nextEdge // fallback: treat nextEdge as closest if underflow risk
		}

		// Compute deltas to both candidates and choose the smaller magnitude.
		dNext := int64(0)
		dPrev := int64(0)
		if tObsMono >= nextEdge {
			dNext = int64(tObsMono - nextEdge)
		} else {
			dNext = -int64(nextEdge - tObsMono)
		}
		if tObsMono >= prevEdge {
			dPrev = int64(tObsMono - prevEdge)
		} else {
			dPrev = -int64(prevEdge - tObsMono)
		}

		// Pick the delta with smaller absolute value.
		delta := dNext
		if abs64(dPrev) < abs64(dNext) {
			delta = dPrev
		}

		// Apply phase correction.
		klog.V(6).Infof("GPU-OBS: tObs=%d, T̂=%d ticks (~%v), nextEdge=%d", tObsMono, uint64(s.phase.Period/s.quantum), s.phase.Period, s.phase.nextEdgeMono(tObsMono))

		s.phase.correctPhase(time.Duration(delta) * s.quantum)

		// Update period estimate from inter-observation interval.
		if s.phase.lastObsMono != 0 && tObsMono > s.phase.lastObsMono {
			dtMono := tObsMono - s.phase.lastObsMono
			s.phase.correctPeriod(time.Duration(dtMono) * s.quantum)
		}
		s.phase.lastObsMono = tObsMono
	}

	// ---- Update last snapshots ----------------------------------------------
	s.lastDevices = devs
	s.lastProcs = procs

	// ---- Enqueue the new observation event ----------------------------------
	s.q.Enqueue(gpuSamplerEvent{
		tObsMono: tObsMono,
		Devices:  devs,
		Procs:    procs,
		Changed:  true,
	})
}

// abs64 returns the absolute value of an int64 without overflow.
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// -----------------------------------------------------------------------------
// 5) Collector additions for sampler integration
// -----------------------------------------------------------------------------

// CollectorSamplerDeps groups the knobs to enable PhaseAware Sampling from the outside.
type CollectorSamplerDeps struct {
	PhaseAware PhaseAwarePollConfig
	Limits     SamplerLimits
}

// EnablePhaseAware configures and wires the phase-aware sampler.
// All timing is derived from the user-defined engine interval (GpuPollMs).
// If GpuPhaseAwareSamplingEnabled() is false, this is a no-op.
func (c *Collector) EnablePhaseAware(ctx context.Context, deps CollectorSamplerDeps) {
	if c == nil || c.PhaseAwareEnabled || c.mono == nil {
		return
	}
	if !config.GpuPhaseAwareSamplingEnabled() {
		klog.V(3).Info("gpuCollector: phase-aware disabled by config")
		return
	}

	quantum := time.Duration(config.TimebaseQuantumMs()) * time.Millisecond
	if quantum <= 0 {
		quantum = time.Millisecond
	}

	Teng := time.Duration(config.GpuPollMs()) * time.Millisecond
	cfg := DerivePhaseAwareFromEngineInterval(Teng)
	cfg.Enabled = true
	cfg.EpsilonMilliW = 2

	lim := deps.Limits
	if lim.MaxEventQueue <= 0 {
		lim.MaxEventQueue = 8
	}

	q := newGpuEventQueue(lim.MaxEventQueue)
	ph := phaseEstimator{
		Enabled:  true,
		Period:   cfg.InitialPublishPeriod,
		Phase:    0,
		AlphaPhi: cfg.PhaseEMAAlpha,
		AlphaT:   cfg.PeriodEMAAlpha,
		MinT:     cfg.MinPublishPeriodClamp,
		MaxT:     cfg.MaxPublishPeriodClamp,
	}

	s := &sampler{
		mono:    c.mono,
		cfgB:    cfg,
		limits:  lim,
		parent:  c,
		phase:   ph,
		q:       q,
		quantum: quantum,
	}
	s.phase.quantum = quantum

	c.PhaseAwareEnabled = true
	c.events = q
	c.samp = s

	if !c.inited || c.disabled {
		klog.V(3).Info("gpuCollector: defer sampler start until Init() completes")
	} else {
		c.samp.start(ctx)
	}

	klog.V(3).Infof("gpuCollector: phase-aware enabled+started; Teng=%v T̂=%v base=%v burst=%v ±%v eps=%dmW queue=%d",
		Teng, cfg.InitialPublishPeriod, cfg.BaseInterval, cfg.BurstInterval, cfg.BurstHalfWidth,
		cfg.EpsilonMilliW, lim.MaxEventQueue,
	)
}

func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	c.StopPhaseAwareSampler()
	return nil
}

// StartPhaseAwareSampler starts the background sampler.
func (c *Collector) StartPhaseAwareSampler(ctx context.Context) {
	if c == nil || !c.PhaseAwareEnabled || c.samp == nil {
		return
	}
	// Optionally ensure collector has been initialized.
	if !c.inited || c.disabled {
		return
	}
	c.samp.start(ctx)
}

// StopPhaseAwareSampler stops the background sampler.
func (c *Collector) StopPhaseAwareSampler() {
	if c == nil || c.samp == nil {
		return
	}
	c.samp.stop()
}

// -----------------------------------------------------------------------------
// 6) Collector (existing) — only minimal field additions needed
// -----------------------------------------------------------------------------
// NOTE: This extends your existing Collector with just a few fields.
// Keep your existing fields as-is; only the delta is shown here for clarity.
type Collector struct {
	// existing fields …
	buf  *ring.Sync[ring.GpuTick]
	mono *clock.Mono

	inited     bool
	disabled   bool
	backendStr string // "NVML" | "DCGM" | etc.
	devIf      devices.Device

	cumMu    sync.RWMutex
	cumValid map[string]bool

	devs              []gpuDeviceState
	lastProcQueryMono map[int]uint64
	lastMono          uint64

	// --- NEW: PhaseAware sampler integration ---
	PhaseAwareEnabled bool
	samp              *sampler
	events            *gpuEventQueue
	lastProcQueryTime map[int]time.Time // per-owner (GPU or MIG) last successful query wall time
	lastDeviceObsTime time.Time         // wall time when we last pushed a device snapshot to the ring
}

// Collect remains your engine-driven per-tick entry point.
// It should drain at most one sampler event and push exactly one ring tick.
// If no event is available, it can push a "no-change" tick using last snapshot.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	if c == nil || c.buf == nil || c.mono == nil || !c.inited || c.disabled {
		return
	}

	// --------------------------
	// Mode A: Phase-aware sampler
	// --------------------------
	if c.PhaseAwareEnabled && c.events != nil {
		if ev, ok := c.events.TryDequeue(); ok {
			c.buf.Push(ring.GpuTick{
				SampleMeta: ring.SampleMeta{Mono: ev.tObsMono},
				Devices:    ev.Devices,
				Processes:  ev.Procs,
			})
			klog.V(5).Infof("GPU-COLLECT: pushed tick mono=%d changed=%v devs=%d procs=%d",
				ev.tObsMono, ev.Changed, len(ev.Devices), len(ev.Procs))
			c.lastMono = ev.tObsMono
			c.lastDeviceObsTime = time.Now()
			c.logTickDebug(ev.Changed, len(ev.Devices), len(ev.Procs))
		}
		return
	}

	// -----------------------------------------
	// Mode B: Non-phase-aware immediate sampling
	// -----------------------------------------
	// Collect immediately on engine tick.
	nowMono := c.mono.From(time.Now())

	devs, okDev := c.readAllDevicesSnapshot()
	if !okDev || len(devs) == 0 {
		return
	}

	procs, okProc := c.readPerProcessSnapshot(nowMono)
	if !okProc {
		procs = nil
	}

	c.buf.Push(ring.GpuTick{
		SampleMeta: ring.SampleMeta{Mono: nowMono},
		Devices:    devs,
		Processes:  procs,
	})
	klog.V(5).Infof("GPU-COLLECT: immediate push mono=%d devs=%d procs=%d",
		nowMono, len(devs), len(procs))

	c.lastMono = nowMono
	c.lastDeviceObsTime = time.Now()
	c.logTickDebug(true, len(devs), len(procs))
}

// -----------------------------------------------------------------------------
// 7) Minimal device state (unchanged from your version, included for completeness)
// -----------------------------------------------------------------------------

type gpuDeviceState struct {
	Index int
	// Keep NVML/DCGM handles and static metadata as in your current code.
	// (Placeholders here to avoid importing vendors in this skeleton.)
	UUID   string
	Name   string
	PCIBus string

	Nvml nvml.Device

	HasCumulativeEnergy bool
}

// DevicesInternal returns a shallow copy of the internal device slice.
// Use this only within the gpuCollector package.
func (c *Collector) DevicesInternal() []gpuDeviceState {
	if c == nil || !c.inited || len(c.devs) == 0 {
		return nil
	}
	out := make([]gpuDeviceState, len(c.devs))
	copy(out, c.devs)
	return out
}

// -----------------------------------------------------------------------------
// 8) Wiring helpers used by sampler to reuse Collector's existing reads
// -----------------------------------------------------------------------------

// cChar32ToString converts a C-style fixed-size [32]int8 array to a Go string.
func cChar32ToString(a [32]int8) string {
	b := make([]byte, 0, len(a))
	for _, v := range a {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}

// Adjust these value-type codes if your binding defines different constants.
const (
	nvmlValueTypeDouble           = 0
	nvmlValueTypeUnsignedInt      = 1
	nvmlValueTypeUnsignedLong     = 2
	nvmlValueTypeUnsignedLongLong = 3
	nvmlValueTypeSignedLongLong   = 4
)

// decodeNVMLUint converts an nvml.FieldValue into a uint64 (little-endian).
// Handles the common numeric NVML types we may see for power fields.
func decodeNVMLUint(v nvml.FieldValue) (uint64, bool) {
	switch v.ValueType {
	case nvmlValueTypeUnsignedInt:
		return uint64(binary.LittleEndian.Uint32(v.Value[:4])), true
	case nvmlValueTypeUnsignedLong, nvmlValueTypeUnsignedLongLong:
		return binary.LittleEndian.Uint64(v.Value[:]), true
	case nvmlValueTypeDouble:
		f := math.Float64frombits(binary.LittleEndian.Uint64(v.Value[:]))
		if f < 0 {
			return 0, false
		}
		return uint64(f + 0.5), true
	default:
		return 0, false
	}
}

// readAllDevicesSnapshot reuses the Collector's fast-path reads (NVML/DCGM) to
// construct []ring.GpuSample for the sampler. This avoids code duplication.
func (c *Collector) readAllDevicesSnapshot() (devs []ring.GpuSample, ok bool) {
	// Basic guards
	if c == nil || !c.inited || c.disabled {
		return nil, false
	}
	if len(c.devs) == 0 {
		return nil, false
	}

	// --- Physical GPU device samples (NVML fast-path) ------------------------
	out := make([]ring.GpuSample, 0, len(c.devs))
	for i := range c.devs {
		state := &c.devs[i]
		nv := state.Nvml

		powerMwU32, _ := nv.GetPowerUsage()
		powerMw := int(powerMwU32)

		util, _ := nv.GetUtilizationRates()
		mem, _ := nv.GetMemoryInfo()
		smClk, _ := nv.GetClockInfo(nvml.CLOCK_SM)
		memClk, _ := nv.GetClockInfo(nvml.CLOCK_MEM)
		tempCU32, _ := nv.GetTemperature(nvml.TEMPERATURE_GPU)
		tempC := int(tempCU32)

		// Optional engines
		var encPtr, decPtr *float64
		if u, _, ret := nv.GetEncoderUtilization(); ret == nvml.SUCCESS {
			v := float64(u)
			encPtr = &v
		}
		if u, _, ret := nv.GetDecoderUtilization(); ret == nvml.SUCCESS {
			v := float64(u)
			decPtr = &v
		}

		// Optional cumulative energy (mJ).
		// IMPORTANT: Only expose the counter if the validator deemed it usable.
		// If it is NOT valid (or not yet validated), we intentionally do not collect it and
		// instead publish a zero value. Downstream analysis can treat 0 as "not valid".
		var cumMJPtr *uint64
		if c.cumEnergyOK(state.UUID) {
			if mJ, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
				cum := mJ
				cumMJPtr = &cum
			}
		} else {
			z := uint64(0)
			cumMJPtr = &z
		}

		// Instant power via NVML field API (NVML_FI_DEV_POWER_INSTANT = 186).
		instPowerMw := uint64(0)
		values := []nvml.FieldValue{{FieldId: 186}}
		if ret := nv.GetFieldValues(values); ret == nvml.SUCCESS && values[0].NvmlReturn == uint32(nvml.SUCCESS) {
			if mw, ok := decodeNVMLUint(values[0]); ok {
				instPowerMw = mw
			}
		}

		out = append(out, ring.GpuSample{
			DeviceIndex:        state.Index,
			UUID:               state.UUID,
			PCIBusID:           state.PCIBus,
			Name:               state.Name,
			PowerMilliW:        powerMw,
			InstantPowerMilliW: instPowerMw,
			CumEnergyMilliJ:    cumMJPtr,
			SMUtilPct:          float64(util.Gpu),
			MemUtilPct:         float64(util.Memory),
			EncUtilPct:         encPtr,
			DecUtilPct:         decPtr,
			MemUsedBytes:       mem.Used,
			MemTotalBytes:      mem.Total,
			SMClockMHz:         uint32(smClk),
			MemClockMHz:        uint32(memClk),
			TempC:              tempC,

			Backend: "nvml",
			IsMIG:   false,
		})
	}

	// MIG device sampling (DCGM path or NVML MIG path) is left unchanged below.
	// -----------------------------------------------------------------------
	// (keep your existing MIG code exactly as it is)
	// -----------------------------------------------------------------------

	// Diagnostic: average power across sampled physical devices.
	if len(out) > 0 {
		total := 0
		for _, d := range out {
			total += d.PowerMilliW
		}
		avg := float64(total) / float64(len(out))
		klog.V(5).Infof("GPU-SNAPSHOT: read %d devices, avgPower=%.1f mW", len(out), avg)
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// readPerProcessSnapshot reuses the Collector's per-process path, if enabled.
// nowMono is the current monotonic timestamp (ns). Returns ok=true on successful
// execution of the per-process path (even if it yields zero samples). If per-process
// is disabled or devIf is nil, it returns (nil, true).
func (c *Collector) readPerProcessSnapshot(nowMono uint64) (procs []ring.GpuProcSample, ok bool) {
	// Optional feature: if disabled or unavailable, succeed with empty slice.
	if c == nil || !c.inited || c.disabled {
		return nil, false
	}
	if c.devIf == nil || !config.GpuEnablePerProcess() {
		return nil, true
	}
	if c.lastProcQueryTime == nil {
		c.lastProcQueryTime = make(map[int]time.Time)
	}
	if c.lastProcQueryMono == nil { // keep existing mono map alive (not used here)
		c.lastProcQueryMono = make(map[int]uint64)
	}

	procs = make([]ring.GpuProcSample, 0, 32)

	// Fixed wall-clock "now" for this entire call, so all owners use the same origin.
	nowWall := time.Now()

	// Bounds for the NVML/DCGM "since" window
	const minSince = time.Millisecond // never pass 0
	const maxSince = 5 * time.Second  // cap to avoid huge windows
	roundUpToMS := func(d time.Duration) time.Duration {
		if d <= 0 {
			return minSince
		}
		q := time.Millisecond
		// ceil(d / 1ms) * 1ms
		return ((d + q - 1) / q) * q
	}

	// Compute a per-owner "since" Duration from wall-clock watermarks.
	// Priority:
	//  1) lastProcQueryTime[owner]
	//  2) lastDeviceObsTime (when we last pushed a device snapshot)
	//  3) fallback to nowWall - minSince (=> pass minSince)
	ownerSinceMs := func(ownerIdx int) time.Duration {
		var from time.Time
		if t0, ok := c.lastProcQueryTime[ownerIdx]; ok && !t0.IsZero() {
			from = t0
		} else if !c.lastDeviceObsTime.IsZero() {
			from = c.lastDeviceObsTime
		} else {
			// First ever call very early: simulate a minimal non-zero lookback.
			from = nowWall.Add(-minSince)
		}

		d := nowWall.Sub(from)
		if d < minSince {
			d = minSince
		}
		if d > maxSince {
			d = maxSince
		}
		return roundUpToMS(d)
	}

	appendProc := func(ownerIdx int, ownerUUID string, m map[uint32]any) {
		if len(m) == 0 {
			// Advance wall-clock watermark even if no procs returned (prevents tight loops).
			c.lastProcQueryTime[ownerIdx] = nowWall
			// Keep mono watermark behavior as before:
			c.lastProcQueryMono[ownerIdx] = nowMono
			return
		}
		for _, v := range m {
			p, ok := v.(devices.GPUProcessUtilizationSample)
			if !ok {
				continue
			}
			procs = append(procs, ring.GpuProcSample{
				Pid:               p.Pid,
				TimeStampUS:       p.TimeStamp,   // backend-provided timestamp (opaque)
				ComputeUtil:       p.ComputeUtil, // %
				MemUtil:           p.MemUtil,     // %
				EncUtil:           p.EncUtil,     // %
				DecUtil:           p.DecUtil,     // %
				GpuIndex:          ownerIdx,
				GpuUUID:           ownerUUID,
				GpuInstanceID:     nil,
				ComputeInstanceID: nil,
			})
		}
		// Successful read → advance watermarks
		c.lastProcQueryTime[ownerIdx] = nowWall
		c.lastProcQueryMono[ownerIdx] = nowMono
	}

	// (a) Physical devices via NVML/DCGM
	for i := range c.devs {
		state := &c.devs[i]
		since := ownerSinceMs(state.Index)
		klog.V(6).Infof("gpu-proc: idx=%d uuid=%s since=%s", state.Index, state.UUID, since)

		gd := devices.GPUDevice{
			DeviceHandler: state.Nvml,
			ID:            state.Index,
			IsSubdevice:   false,
		}
		m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
		if err != nil {
			// Advance watermarks to avoid tight loop on repeated errors.
			c.lastProcQueryTime[state.Index] = nowWall
			c.lastProcQueryMono[state.Index] = nowMono
			continue
		}
		appendProc(state.Index, state.UUID, m)
	}

	// (b) MIG instances (DCGM only)
	if c.backendStr == "DCGM" && config.GpuEnableMigDiscovery() {
		inst := c.devIf.DeviceInstances() // map[parentID]map[migID]any
		for parentID, migMap := range inst {
			parentUUID := ""
			for i := range c.devs {
				if c.devs[i].Index == parentID {
					parentUUID = c.devs[i].UUID
					break
				}
			}
			for _, anyDev := range migMap {
				gd, ok := anyDev.(devices.GPUDevice)
				if !ok {
					continue
				}
				ownerIdx := gd.ID // DCGM entity id
				since := ownerSinceMs(ownerIdx)
				klog.V(6).Infof("gpu-proc[MIG]: owner=%d parent=%d since=%s", ownerIdx, parentID, since)

				m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
				if err != nil {
					c.lastProcQueryTime[ownerIdx] = nowWall
					c.lastProcQueryMono[ownerIdx] = nowMono
					continue
				}
				appendProc(ownerIdx, parentUUID, m)
			}
		}
	}

	return procs, true
}

// -----------------------------------------------------------------------------
// 9) Lightweight logging hooks (optional but handy for validation)
// -----------------------------------------------------------------------------

// logTickDebug emits a periodic debug line for ring push verification.
// It logs at verbosity level 3 to avoid cluttering normal output.
func (c *Collector) logTickDebug(changed bool, numDev, numProc int) {
	if c == nil {
		return
	}

	status := "no-change"
	if changed {
		status = "changed"
	}

	klog.V(3).Infof("gpuCollector: pushed tick (%s, %d devices, %d procs)", status, numDev, numProc)
}
