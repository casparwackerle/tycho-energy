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
)

// -----------------------------------------------------------------------------
// Unified metric family IDs (Prometheus sink prefix "tycho" yields
// tycho_energy_mj and tycho_power_mw).
// -----------------------------------------------------------------------------

const (
	MetricRaplEnergyMJ analysis.MetricID = "rapl_energy_mj"
	MetricRaplPowerMW  analysis.MetricID = "rapl_power_mw"
)

// Slice 10A: fixed provenance label
const raplSource = "rapl"

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

	// Compute effective window ONCE for this source.
	w := c.EffectiveWindowTicks(m.delayTicks)

	samples := analysisctx.FilterWindowChrono[ring.RaplTick](
		r,
		w.StartMono,
		w.EndMono,
		func(t ring.RaplTick) uint64 { return t.SampleMeta.Mono },
	)
	if len(samples) < 2 {
		return nil
	}

	dtSec := windowDurSec(c)
	if dtSec <= 0 {
		return nil
	}

	// Minimal, structured quality.
	q := &analysis.Quality{
		SamplesUsed: len(samples),
		SocketsUsed: 0,
		DelayTicks:  m.delayTicks,
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
			"source": raplSource,
		}

		// Fixed startup offset (per domain). This makes the exported counter start at 0.
		offKey := stateKeyOffsetU64("rapl", domain, "native_total_mj")
		offsetMJ, ok := stateGetU64(c.State, offKey)
		if !ok {
			offsetMJ = endNativeMJ
			stateSetU64(c.State, offKey, offsetMJ)
		}

		adjEndMJ := uint64(0)
		if endNativeMJ >= offsetMJ {
			adjEndMJ = endNativeMJ - offsetMJ
		}

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

	// Proxy rates from raw ebpf ticks (no dependency on bpf metrics in pointstore).
	cpuRate, dramRate, _, ok := bpfProcRatesInWindow(c)
	if !ok {
		cpuRate = 0
		dramRate = 0
	}

	now := time.Now()

	// Continue exactly as before:
	cpuP95 := idle.GetOrInitP95(c.State, "cpu_instr_rate", 240, 0.02)
	dramP95 := idle.GetOrInitP95(c.State, "dram_cachemiss_rate", 240, 0.02)

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

	stableCPU := cpuP95 != nil && cpuP95.Ready()
	stableDRAM := dramP95 != nil && dramP95.Ready()

	for _, d := range []string{"pkg", "core", "uncore"} {
		m.handleDomain(c, d, uCPU, dtSec, now, stableCPU)
	}
	m.handleDomain(c, "dram", uDRAM, dtSec, now, stableDRAM)

	return nil
}

func (m *RaplIdleDynamic) handleDomain(
	c *analysis.Cycle,
	domain string,
	u float64,
	dtSec float64,
	now time.Time,
	stable bool,
) {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || dtSec <= 0 {
		return
	}

	totalEnergyKey := analysis.Key(MetricRaplEnergyMJ, analysis.Labels{
		"domain": domain,
		"kind":   "total",
		"source": raplSource,
	})
	totalPowerKey := analysis.Key(MetricRaplPowerMW, analysis.Labels{
		"domain": domain,
		"kind":   "total",
		"source": raplSource,
	})

	// --- Prefer the exported total power series for clamping (Prometheus-visible guarantee) ---
	// If available, we clamp idle <= THIS value, so users never observe idle > total in Prometheus.
	clampTotalMW := 0.0
	if exportTotalMW, ok := getPointValueOk(c.Store, totalPowerKey); ok && exportTotalMW >= 0 {
		clampTotalMW = exportTotalMW
	}

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
	deltaTotalMJ := uint64(deltaTotalMJf + 0.5)

	totalMWFromEnergy := float64(deltaTotalMJ) / dtSec
	totalMWForClamp := totalMWFromEnergy
	if clampTotalMW > 0 {
		totalMWForClamp = clampTotalMW
	}

	modelName := "rapl_" + domain
	mod := idle.GetOrInitScalar(c.State, modelName, m.cfg)
	if mod == nil {
		return
	}

	_ = mod.Observe(u, totalMWFromEnergy, now)
	betaMW, q := mod.Estimate()

	upPerSec := 0.02
	downPerSec := 0.20
	if !stable {
		upPerSec = 0.0
	}
	betaMW = smoothBaselineMW(c.State, "rapl", domain, betaMW, now, upPerSec, downPerSec)

	idleMW := betaMW
	if idleMW < 0 {
		idleMW = 0
	}
	if idleMW > totalMWForClamp {
		idleMW = totalMWForClamp
	}

	idleDeltaMJ := uint64(idleMW*dtSec + 0.5)
	if idleDeltaMJ > deltaTotalMJ {
		idleDeltaMJ = deltaTotalMJ
	}
	dynDeltaMJ := deltaTotalMJ - idleDeltaMJ

	idleMW = float64(idleDeltaMJ) / dtSec
	dynMW := float64(dynDeltaMJ) / dtSec

	idleCumMJ := stateAddU64(c.State, stateKeyCumEnergyU64("rapl", domain, "idle"), idleDeltaMJ)
	dynCumMJ := stateAddU64(c.State, stateKeyCumEnergyU64("rapl", domain, "dynamic"), dynDeltaMJ)

	labelsIdle := analysis.Labels{
		"domain": domain,
		"kind":   "idle",
		"source": raplSource,
	}
	labelsDyn := analysis.Labels{
		"domain": domain,
		"kind":   "dynamic",
		"source": raplSource,
	}

	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplPowerMW, labelsIdle), Window: c.Window, Unit: "mW", Value: idleMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplPowerMW, labelsDyn), Window: c.Window, Unit: "mW", Value: dynMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplEnergyMJ, labelsIdle), Window: c.Window, Unit: "mJ", Value: float64(idleCumMJ)})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplEnergyMJ, labelsDyn), Window: c.Window, Unit: "mJ", Value: float64(dynCumMJ)})

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

// bpfProcRatesInWindow computes the same proxy rates as BpfProcAggWindow
// (bpf_cpu_instr_per_s and bpf_cache_miss_per_s), but directly from raw ebpf ticks.
// This removes the dependency on the bpf metric outputs being present in c.Store.
func bpfProcRatesInWindow(c *analysis.Cycle) (cpuInstrPerSec float64, cacheMissPerSec float64, used int, ok bool) {
	if c == nil || c.Mono == nil || c.Bpf() == nil {
		return 0, 0, 0, false
	}

	// Match the ebpf metric logic: apply configurable delay, and integrate over effective window.
	delayTicks := c.Mono.TicksForMsCeil(config.BpfDelayMs())
	wEff := c.EffectiveWindowTicks(delayTicks)

	// Include predecessor tick for interval attribution (same as bpf.go).
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		c.Bpf(),
		wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)
	if len(ticks) < 2 {
		return 0, 0, 0, false
	}

	// Integrate sum of per-proc deltas, conservatively weighted by overlap.
	dInstr, usedInstr := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			var s uint64
			for i := range t.Procs {
				s += t.Procs[i].CPUInstr
			}
			return float64(s)
		},
	)

	dMiss, usedMiss := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			var s uint64
			for i := range t.Procs {
				s += t.Procs[i].CacheMiss
			}
			return float64(s)
		},
	)

	// Compute dtSec from the cycle's corrected window (not wEff), same as bpf.go.
	dtTicks := uint64(0)
	if c.Window.EndMono > c.Window.StartMono {
		dtTicks = c.Window.EndMono - c.Window.StartMono
	}
	q := c.Mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	dtSec := float64(time.Duration(dtTicks)*q) / float64(time.Second)
	if dtSec <= 0 {
		return 0, 0, 0, false
	}

	// Rates per second.
	cpuRate := dInstr / dtSec
	dramRate := dMiss / dtSec

	// For "used", pick the limiting one (conservative).
	used = usedInstr
	if usedMiss < used {
		used = usedMiss
	}

	return cpuRate, dramRate, used, true
}
