// file: internal/tycho/analysis/metrics/rapl.go
package analysismetrics

import (
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

// -----------------------------------------------------------------------------
// Unified metric family IDs (Prometheus sink prefix "tycho" yields
// tycho_energy_mj and tycho_power_mw).
// -----------------------------------------------------------------------------

const (
	MetricRaplEnergyMJ analysis.MetricID = "rapl_energy_mj"
	MetricRaplPowerMW  analysis.MetricID = "rapl_power_mw"
)

// -----------------------------------------------------------------------------
// RAPL totals (kind="total")
// - energy: exported as cumulative (monotonic) by accumulating per-window deltas
// - power: exported as window-average (delta / dt)
// -----------------------------------------------------------------------------

// RaplTotals exports unified family metrics for RAPL totals:
//
//	tycho_energy_mj{component="rapl",domain="pkg|core|dram|uncore",kind="total"} (cumulative)
//	tycho_power_mw {component="rapl",domain="pkg|core|dram|uncore",kind="total"} (window-average)
type RaplTotals struct {
	mono       *clock.Mono
	delayTicks uint64
}

func NewRaplTotals(mono *clock.Mono) *RaplTotals {
	return &RaplTotals{
		mono:       mono,
		delayTicks: mono.TicksForMsCeil(config.RaplDelayMs()),
	}
}

// ID identifies the plugin. Emitted metric IDs are MetricEnergyMJ / MetricPowerMW.
func (m *RaplTotals) ID() analysis.MetricID { return "rapl_totals" }

func (m *RaplTotals) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Rapl() != nil && c.Sink != nil && c.State != nil && c.Mono != nil
}
func (m *RaplTotals) Run(c *analysis.Cycle) error {
	r := c.Rapl()
	if r == nil || c.Sink == nil || c.State == nil {
		return nil
	}
	klog.Infof("[RAPL] ring len=%d", r.Len())
	// Compute effective window ONCE for this source.
	w := c.EffectiveWindowTicks(m.delayTicks)

	klog.Infof(
		"[RAPL] window analysis=[%d,%d] effective=[%d,%d] delayTicks=%d",
		c.Window.StartMono,
		c.Window.EndMono,
		w.StartMono,
		w.EndMono,
		m.delayTicks,
	)

	// Read only in-window samples (best-effort).
	samples := analysisctx.FilterWindowChrono[ring.RaplTick](
		r,
		w.StartMono,
		w.EndMono,
		func(t ring.RaplTick) uint64 { return t.SampleMeta.Mono },
	)
	if len(samples) < 2 {
		return nil
	}
	klog.Infof("[RAPL] filtered samples=%d", len(samples))

	dtSec := windowDurSec(c)
	if dtSec <= 0 {
		return nil
	}

	// Minimal, structured quality.
	q := &analysis.Quality{
		SamplesUsed: len(samples),
		SocketsUsed: 0, // filled below
		DelayTicks:  m.delayTicks,
		Notes:       "",
	}

	// For each socket, track the first and last counters within the window.
	type firstLast struct {
		first ring.RaplDomainCounters
		last  ring.RaplDomainCounters
		ok    bool
	}
	perSocket := map[int]*firstLast{}

	for _, tick := range samples {
		for socketID, ctrs := range tick.Sockets {
			fl, ok := perSocket[socketID]
			if !ok {
				perSocket[socketID] = &firstLast{first: ctrs, last: ctrs, ok: true}
				continue
			}
			fl.last = ctrs
		}
	}
	q.SocketsUsed = len(perSocket)
	klog.Infof("[RAPL] sockets used=%d", q.SocketsUsed)

	// Aggregate across sockets:
	// - end-of-window native cumulative counters: sum(last)
	// - window increments: sum(last - first) wrap-safe
	var pkgEndMJ, coreEndMJ, uncoreEndMJ, dramEndMJ uint64
	var pkgIncMJ, coreIncMJ, uncoreIncMJ, dramIncMJ uint64

	for _, fl := range perSocket {
		if fl == nil || !fl.ok {
			continue
		}

		pkgEndMJ += fl.last.Pkg
		coreEndMJ += fl.last.Core
		uncoreEndMJ += fl.last.Uncore
		dramEndMJ += fl.last.DRAM

		pkgIncMJ += analysisops.DeltaU64(fl.first.Pkg, fl.last.Pkg)
		coreIncMJ += analysisops.DeltaU64(fl.first.Core, fl.last.Core)
		uncoreIncMJ += analysisops.DeltaU64(fl.first.Uncore, fl.last.Uncore)
		dramIncMJ += analysisops.DeltaU64(fl.first.DRAM, fl.last.DRAM)
	}

	emitTotal := func(domain string, endNativeMJ uint64, incMJ uint64) {
		lbl := analysis.Labels{
			"domain": domain,
			"kind":   "total",
		}

		klog.Infof(
			"[RAPL] domain=%s native_end=%d native_inc=%d",
			domain,
			endNativeMJ,
			incMJ,
		)
		// Fixed startup offset (per domain). This makes the exported counter start at 0.
		offKey := stateKeyOffsetU64("rapl", domain, "native_total_mj")
		offsetMJ, ok := stateGetU64(c.State, offKey)
		if !ok {
			offsetMJ = endNativeMJ
			stateSetU64(c.State, offKey, offsetMJ)
		}

		klog.Infof(
			"[RAPL] domain=%s offset set=%v offset=%d",
			domain,
			!ok,
			offsetMJ,
		)

		// Adjusted cumulative energy since Tycho start.
		adjEndMJ := uint64(0)
		if endNativeMJ >= offsetMJ {
			adjEndMJ = endNativeMJ - offsetMJ
		} else {
			// Defensive against underflow; resets/wrap handling is out of scope by your request.
			adjEndMJ = 0
		}

		klog.Infof(
			"[RAPL] domain=%s adj_cum=%d",
			domain,
			adjEndMJ,
		)

		// Energy is a true counter starting at 0.
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:     analysis.Key(MetricRaplEnergyMJ, lbl),
			Window:  c.Window,
			Unit:    "mJ",
			Value:   float64(adjEndMJ),
			Quality: q,
		})

		// Power remains window-average from the in-window increment.
		pMW := 0.0
		if dtSec > 0 {
			pMW = float64(incMJ) / dtSec
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:     analysis.Key(MetricRaplPowerMW, lbl),
			Window:  c.Window,
			Unit:    "mW",
			Value:   pMW,
			Quality: q,
		})
	}

	emitTotal("pkg", pkgEndMJ, pkgIncMJ)
	emitTotal("core", coreEndMJ, coreIncMJ)
	emitTotal("uncore", uncoreEndMJ, uncoreIncMJ)
	emitTotal("dram", dramEndMJ, dramIncMJ)

	return nil
}

// -----------------------------------------------------------------------------
// RAPL idle/dynamic split (kind="idle" | "dynamic")
// - reads total energy from the unified family (component=rapl, kind=total)
// - computes window-average total power and estimates idle baseline (beta)
// - clamps to preserve invariants and emits power + cumulative energy
// -----------------------------------------------------------------------------

type RaplIdleDynamic struct {
	cfg idle.Config
}

func NewRaplIdleDynamic() *RaplIdleDynamic {
	return &RaplIdleDynamic{cfg: idle.DefaultConfig()}
}

func (m *RaplIdleDynamic) ID() analysis.MetricID { return "rapl_idle_dynamic" }

func (m *RaplIdleDynamic) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil
}

func (m *RaplIdleDynamic) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil {
		return nil
	}

	dtSec := windowDurSec(c)
	if dtSec <= 0 {
		return nil
	}

	// Proxy rates from pointstore (if missing, treat as 0 and model stays baseline).
	cpuRate := getPointValue(c.Store, analysis.Key(MetricBpfCPUInstrRate, nil))   // 1/s
	dramRate := getPointValue(c.Store, analysis.Key(MetricBpfCacheMissRate, nil)) // 1/s

	now := time.Now()

	// Track decayed p95 maxima.
	cpuP95 := idle.GetOrInitP95(c.State, "cpu_instr_rate", 240, 0.02)
	dramP95 := idle.GetOrInitP95(c.State, "dram_cachemiss_rate", 240, 0.02)
	if cpuP95 != nil {
		cpuP95.Observe(cpuRate, now)
	}
	if dramP95 != nil {
		dramP95.Observe(dramRate, now)
	}

	amaxCPU := 0.0
	if cpuP95 != nil {
		amaxCPU = cpuP95.Value()
	}
	amaxDRAM := 0.0
	if dramP95 != nil {
		amaxDRAM = dramP95.Value()
	}

	uCPU := normalizeRate(cpuRate, amaxCPU)
	uDRAM := normalizeRate(dramRate, amaxDRAM)

	klog.Infof(
		"[RAPL-IDLE] proxies cpuRate=%g dramRate=%g amaxCPU=%g readyCPU=%v amaxDRAM=%g readyDRAM=%v uCPU=%g uDRAM=%g",
		cpuRate, dramRate,
		amaxCPU, func() bool {
			if cpuP95 == nil {
				return false
			}
			return cpuP95.Ready()
		}(),
		amaxDRAM, func() bool {
			if dramP95 == nil {
				return false
			}
			return dramP95.Ready()
		}(),
		uCPU, uDRAM,
	)

	// Domains:
	// - pkg/core/uncore: use uCPU proxy
	// - dram: use uDRAM proxy
	stableCPU := cpuP95 != nil && cpuP95.Ready()
	stableDRAM := dramP95 != nil && dramP95.Ready()

	for _, d := range []string{"pkg", "core", "uncore"} {
		m.handleDomain(c, d, uCPU, dtSec, now, stableCPU)
	}
	m.handleDomain(c, "dram", uDRAM, dtSec, now, stableDRAM)

	return nil
}
func (m *RaplIdleDynamic) handleDomain(c *analysis.Cycle, domain string, u float64, dtSec float64, now time.Time, stable bool) {
	// NOTE: stable is currently unused. If you later want to disable stability gating
	// until p95 is ready, thread it into the model config or Observe logic.

	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil {
		return
	}
	if dtSec <= 0 {
		return
	}

	// --- Keys we need ---
	totalEnergyKey := analysis.Key(MetricRaplEnergyMJ, analysis.Labels{
		"domain": domain,
		"kind":   "total",
	})
	totalPowerKey := analysis.Key(MetricRaplPowerMW, analysis.Labels{
		"domain": domain,
		"kind":   "total",
	})

	// --- Prefer the exported total power series for clamping (Prometheus-visible guarantee) ---
	// If available, we clamp idle <= THIS value, so users never observe idle > total in Prometheus.
	clampTotalMW := 0.0
	if exportTotalMW, ok := getPointValueOk(c.Store, totalPowerKey); ok && exportTotalMW >= 0 {
		clampTotalMW = exportTotalMW
	}

	// --- We still need window energy to keep conservation exact (idleDelta + dynDelta = totalDelta) ---
	totalCumMJf, ok := getPointValueOk(c.Store, totalEnergyKey)
	if !ok {
		// Total energy not available. Avoid poisoning / inconsistent state.
		return
	}

	// Convert cumulative energy to window delta (mJ) by differencing against last seen.
	// IMPORTANT: on first sighting, store curr and return without observing/emitting.
	lastKey := stateKeyLastSeen("rapl", domain, "total_energy_seen_mj")
	prevCum, prevOK := stateGetF64(c.State, lastKey)
	stateSetF64(c.State, lastKey, totalCumMJf)
	if !prevOK {
		return
	}

	deltaTotalMJf := totalCumMJf - prevCum
	if deltaTotalMJf < 0 {
		deltaTotalMJf = 0
	}
	// Integer total delta in mJ (rounded once).
	deltaTotalMJ := uint64(deltaTotalMJf + 0.5)

	// Window-average total power derived from energy delta.
	totalMWFromEnergy := float64(deltaTotalMJ) / dtSec

	// Choose the total power we use for *clamping*.
	// If exported total power exists, use that (guarantees idle<=total in Prometheus).
	// Otherwise fall back to energy-derived total.
	totalMWForClamp := totalMWFromEnergy
	if clampTotalMW > 0 {
		totalMWForClamp = clampTotalMW
	}

	// --- Idle model per domain ---
	modelName := "rapl_" + domain
	mod := idle.GetOrInitScalar(c.State, modelName, m.cfg)
	if mod == nil {
		return
	}

	// Observe the measured total power (energy-derived) against utilization proxy.
	// This keeps the model consistent with the same energy source used for conservation.
	_ = mod.Observe(u, totalMWFromEnergy, now)
	betaMW, q := mod.Estimate()

	// Asymmetric smoothing to reduce outlier sensitivity.
	// During warmup (stable=false), we do not allow baseline to rise yet.
	upPerSec := 0.02
	downPerSec := 0.20
	if !stable {
		upPerSec = 0.0
	}
	betaMW = smoothBaselineMW(c.State, "rapl", domain, betaMW, now, upPerSec, downPerSec)

	// Clamp idle power estimate to [0, totalMWForClamp] (Prometheus-visible physical constraint).
	idleMW := betaMW
	if idleMW < 0 {
		idleMW = 0
	}
	if idleMW > totalMWForClamp {
		idleMW = totalMWForClamp
	}

	// Convert idle power to integer window energy (mJ), then enforce conservation exactly.
	// IMPORTANT: conservation is with respect to deltaTotalMJ (energy-derived total).
	idleDeltaMJ := uint64(idleMW*dtSec + 0.5)
	if idleDeltaMJ > deltaTotalMJ {
		idleDeltaMJ = deltaTotalMJ
	}
	dynDeltaMJ := deltaTotalMJ - idleDeltaMJ

	// Derive emitted powers from integer energies (keeps invariants exact).
	idleMW = float64(idleDeltaMJ) / dtSec
	dynMW := float64(dynDeltaMJ) / dtSec

	// Cumulative (monotonic) idle/dynamic energies as uint64, starting at 0.
	idleCumMJ := stateAddU64(c.State, stateKeyCumEnergyU64("rapl", domain, "idle"), idleDeltaMJ)
	dynCumMJ := stateAddU64(c.State, stateKeyCumEnergyU64("rapl", domain, "dynamic"), dynDeltaMJ)

	labelsIdle := analysis.Labels{"domain": domain, "kind": "idle"}
	labelsDyn := analysis.Labels{"domain": domain, "kind": "dynamic"}

	// Unified family emissions.
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplPowerMW, labelsIdle), Window: c.Window, Unit: "mW", Value: idleMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplPowerMW, labelsDyn), Window: c.Window, Unit: "mW", Value: dynMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplEnergyMJ, labelsIdle), Window: c.Window, Unit: "mJ", Value: float64(idleCumMJ)})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplEnergyMJ, labelsDyn), Window: c.Window, Unit: "mJ", Value: float64(dynCumMJ)})

	// Optional idle diagnostics unchanged (still separate metrics).
	if config.GetIdleDiagnosticsEnabled() {
		ql := analysis.Labels{"name": modelName, "domain": domain, "mode": q.Mode}
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelReady, ql), Window: c.Window, Unit: "bool", Value: boolToFloat(q.Ready)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBins, ql), Window: c.Window, Unit: "count", Value: float64(q.BinsPopulated)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelPoints, ql), Window: c.Window, Unit: "count", Value: float64(q.TotalPoints)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelMode, ql), Window: c.Window, Unit: "enum", Value: modeToFloat(q.Mode)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBetaMW, ql), Window: c.Window, Unit: "mW", Value: q.Beta})
	}
}

// smoothBaselineMW applies asymmetric smoothing to a baseline estimate:
// - decreases are allowed faster than increases, but not instant (to resist outliers)
// - increases are allowed slowly (to avoid snapping up after a one-off dip)
//
// State keys are stored in c.State via stateGetF64/stateSetF64.
func smoothBaselineMW(state any, component, id string, candidateMW float64, now time.Time, upPerSec, downPerSec float64) float64 {
	if candidateMW <= 0 {
		return candidateMW
	}

	// Keys for baseline + last timestamp (as Unix seconds).
	baseKey := stateKeyLastSeen(component, id, "baseline_smooth_mw")
	tKey := stateKeyLastSeen(component, id, "baseline_smooth_t_unix")

	prev, okPrev := stateGetF64(state, baseKey)
	lastTUnix, okT := stateGetF64(state, tKey)

	// Initialize on first use.
	if !okPrev || prev <= 0 || !okT || lastTUnix <= 0 || now.IsZero() {
		stateSetF64(state, baseKey, candidateMW)
		if !now.IsZero() {
			stateSetF64(state, tKey, float64(now.UnixNano())/1e9)
		}
		return candidateMW
	}

	nowUnix := float64(now.UnixNano()) / 1e9
	dt := nowUnix - lastTUnix
	if dt < 0 {
		dt = 0
	}
	stateSetF64(state, tKey, nowUnix)

	// No time elapsed -> return previous.
	if dt <= 0 {
		return prev
	}

	// Convert per-second rates to a bounded interpolation factor r in [0, 0.25].
	// r means: move prev toward candidate by fraction r this step.
	rUp := upPerSec * dt
	if rUp > 0.25 {
		rUp = 0.25
	} else if rUp < 0 {
		rUp = 0
	}

	rDown := downPerSec * dt
	if rDown > 0.25 {
		rDown = 0.25
	} else if rDown < 0 {
		rDown = 0
	}

	next := prev
	if candidateMW >= prev {
		// Increase slowly.
		next = prev + rUp*(candidateMW-prev)
	} else {
		// Decrease faster (but still bounded, so one outlier won't dominate).
		next = prev + rDown*(candidateMW-prev)
	}

	stateSetF64(state, baseKey, next)
	return next
}
