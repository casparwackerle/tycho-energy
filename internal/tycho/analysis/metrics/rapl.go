// // file: internal/tycho/analysis/metrics/rapl_unified.go
// package analysismetrics

// import (
// 	"time"

// 	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
// 	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
// 	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
// 	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
// 	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
// 	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
// 	"github.com/casparwackerle/tycho-energy/pkg/config"
// )

// // -----------------------------------------------------------------------------
// // Unified metric family IDs (Prometheus sink prefix "tycho" yields
// // tycho_energy_mj and tycho_power_mw).
// // -----------------------------------------------------------------------------

// const (
// 	MetricWindowRaplEnergyMJ analysis.MetricID = "rapl_window_energy_mj"
// 	MetricWindowRaplPowerMW  analysis.MetricID = "rapl_window_power_mw"
// )

// // -----------------------------------------------------------------------------
// // RAPL totals (kind="total")
// // - energy: exported as cumulative (monotonic) by accumulating per-window deltas
// // - power: exported as window-average (delta / dt)
// // -----------------------------------------------------------------------------

// // RaplTotals exports unified family metrics for RAPL totals:
// //
// //	tycho_energy_mj{component="rapl",domain="pkg|core|dram|uncore",kind="total"} (cumulative)
// //	tycho_power_mw {component="rapl",domain="pkg|core|dram|uncore",kind="total"} (window-average)
// type RaplTotals struct {
// 	mono       *clock.Mono
// 	delayTicks uint64
// }

// func NewRaplTotals(mono *clock.Mono) *RaplTotals {
// 	return &RaplTotals{
// 		mono:       mono,
// 		delayTicks: mono.TicksForMsCeil(config.RaplDelayMs()),
// 	}
// }

// // ID identifies the plugin. Emitted metric IDs are MetricEnergyMJ / MetricPowerMW.
// func (m *RaplTotals) ID() analysis.MetricID { return "rapl_totals" }

// func (m *RaplTotals) IsEnabled(c *analysis.Cycle) bool {
// 	return c != nil && c.Rapl() != nil && c.Sink != nil && c.State != nil && c.Mono != nil
// }

// func (m *RaplTotals) Run(c *analysis.Cycle) error {
// 	r := c.Rapl()
// 	if r == nil || c.Sink == nil || c.State == nil {
// 		return nil
// 	}

// 	// Compute effective window ONCE for this source.
// 	w := c.EffectiveWindowTicks(m.delayTicks)

// 	// Read only in-window samples (best-effort).
// 	samples := analysisctx.FilterWindowChrono[ring.RaplTick](
// 		r,
// 		w.StartMono,
// 		w.EndMono,
// 		func(t ring.RaplTick) uint64 { return t.SampleMeta.Mono },
// 	)
// 	if len(samples) < 2 {
// 		return nil
// 	}

// 	// For each socket: first/last counters.
// 	type firstLast struct {
// 		first ring.RaplDomainCounters
// 		last  ring.RaplDomainCounters
// 		ok    bool
// 	}
// 	perSocket := map[int]*firstLast{}

// 	for _, tick := range samples {
// 		for socketID, ctrs := range tick.Sockets {
// 			fl, ok := perSocket[socketID]
// 			if !ok {
// 				perSocket[socketID] = &firstLast{first: ctrs, last: ctrs, ok: true}
// 				continue
// 			}
// 			fl.last = ctrs
// 		}
// 	}

// 	// Aggregate deltas across sockets (non-wrapping monotonic semantics).
// 	var pkgDeltaMJ, coreDeltaMJ, uncoreDeltaMJ, dramDeltaMJ uint64
// 	for _, fl := range perSocket {
// 		if fl == nil || !fl.ok {
// 			continue
// 		}
// 		pkgDeltaMJ += analysisops.DeltaU64(fl.first.Pkg, fl.last.Pkg)
// 		coreDeltaMJ += analysisops.DeltaU64(fl.first.Core, fl.last.Core)
// 		uncoreDeltaMJ += analysisops.DeltaU64(fl.first.Uncore, fl.last.Uncore)
// 		dramDeltaMJ += analysisops.DeltaU64(fl.first.DRAM, fl.last.DRAM)
// 	}

// 	dtSec := windowDurSec(c)
// 	if dtSec <= 0 {
// 		return nil
// 	}

// 	// Minimal, structured quality.
// 	q := &analysis.Quality{
// 		SamplesUsed: len(samples),
// 		SocketsUsed: len(perSocket),
// 		DelayTicks:  m.delayTicks,
// 		Notes:       "",
// 	}

// 	emitTotal := func(domain string, deltaMJ uint64) {
// 		lbl := analysis.Labels{
// 			"domain": domain,
// 			"kind":   "total",
// 		}

// 		// Cumulative energy (monotonic): cum += delta
// 		cum := stateAddU64(c.State, stateKeyCumEnergyU64("rapl", domain, "total"), deltaMJ)

// 		// Window-average power in mW: mW = (mJ / s)
// 		pMW := float64(deltaMJ) / dtSec

// 		c.Sink.Emit(c.Ctx, analysis.Point{
// 			Key:     analysis.Key(MetricWindowRaplEnergyMJ, lbl),
// 			Window:  c.Window,
// 			Unit:    "mJ",
// 			Value:   float64(cum),
// 			Quality: q,
// 		})
// 		c.Sink.Emit(c.Ctx, analysis.Point{
// 			Key:     analysis.Key(MetricWindowRaplPowerMW, lbl),
// 			Window:  c.Window,
// 			Unit:    "mW",
// 			Value:   pMW,
// 			Quality: q,
// 		})
// 	}

// 	emitTotal("pkg", pkgDeltaMJ)
// 	emitTotal("core", coreDeltaMJ)
// 	emitTotal("uncore", uncoreDeltaMJ)
// 	emitTotal("dram", dramDeltaMJ)
// 	return nil
// }

// // -----------------------------------------------------------------------------
// // RAPL idle/dynamic split (kind="idle" | "dynamic")
// // - reads total energy from the unified family (component=rapl, kind=total)
// // - computes window-average total power and estimates idle baseline (beta)
// // - clamps to preserve invariants and emits power + cumulative energy
// // -----------------------------------------------------------------------------

// type RaplIdleDynamic struct {
// 	cfg idle.Config
// }

// func NewRaplIdleDynamic() *RaplIdleDynamic {
// 	return &RaplIdleDynamic{cfg: idle.DefaultConfig()}
// }

// func (m *RaplIdleDynamic) ID() analysis.MetricID { return "rapl_idle_dynamic" }

// func (m *RaplIdleDynamic) IsEnabled(c *analysis.Cycle) bool {
// 	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil
// }

// func (m *RaplIdleDynamic) Run(c *analysis.Cycle) error {
// 	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil {
// 		return nil
// 	}

// 	dtSec := windowDurSec(c)
// 	if dtSec <= 0 {
// 		return nil
// 	}

// 	// Proxy rates from pointstore (if missing, treat as 0 and model stays baseline).
// 	cpuRate := getPointValue(c.Store, analysis.Key(MetricBpfCPUInstrRate, nil))   // 1/s
// 	dramRate := getPointValue(c.Store, analysis.Key(MetricBpfCacheMissRate, nil)) // 1/s

// 	now := time.Now()

// 	// Track decayed p95 maxima.
// 	cpuP95 := idle.GetOrInitP95(c.State, "cpu_instr_rate", 240, 0.02)
// 	dramP95 := idle.GetOrInitP95(c.State, "dram_cachemiss_rate", 240, 0.02)
// 	if cpuP95 != nil {
// 		cpuP95.Observe(cpuRate, now)
// 	}
// 	if dramP95 != nil {
// 		dramP95.Observe(dramRate, now)
// 	}

// 	amaxCPU := 0.0
// 	if cpuP95 != nil {
// 		amaxCPU = cpuP95.Value()
// 	}
// 	amaxDRAM := 0.0
// 	if dramP95 != nil {
// 		amaxDRAM = dramP95.Value()
// 	}

// 	uCPU := normalizeRate(cpuRate, amaxCPU)
// 	uDRAM := normalizeRate(dramRate, amaxDRAM)

// 	// Domains:
// 	// - pkg/core/uncore: use uCPU proxy
// 	// - dram: use uDRAM proxy
// 	for _, d := range []string{"pkg", "core", "uncore"} {
// 		m.handleDomain(c, d, uCPU, dtSec, now)
// 	}
// 	m.handleDomain(c, "dram", uDRAM, dtSec, now)

// 	return nil
// }

// func (m *RaplIdleDynamic) handleDomain(c *analysis.Cycle, domain string, u float64, dtSec float64, now time.Time) {
// 	// Read TOTAL cumulative energy (mJ) from pointstore:
// 	// tycho_energy_mj{component="rapl",domain="<domain>",kind="total"}
// 	totalKey := analysis.Key(MetricWindowRaplEnergyMJ, analysis.Labels{
// 		"domain": domain,
// 		"kind":   "total",
// 	})
// 	totalCumMJ := getPointValue(c.Store, totalKey)

// 	// Convert cumulative energy to window delta (mJ) by differencing against last seen.
// 	deltaTotalMJ := deltaFromCumulative(c.State, stateKeyLastSeen("rapl", domain, "total_energy_seen_mj"), totalCumMJ)

// 	// Window-average total power in mW: mW = (mJ / s)
// 	totalMW := 0.0
// 	if dtSec > 0 {
// 		totalMW = deltaTotalMJ / dtSec
// 	}

// 	// Idle model per domain.
// 	modelName := "rapl_" + domain
// 	mod := idle.GetOrInitScalar(c.State, modelName, m.cfg)
// 	if mod == nil {
// 		return
// 	}
// 	_ = mod.Observe(u, totalMW, now)
// 	betaMW, q := mod.Estimate()

// 	// Enforce invariants on the split:
// 	// idle <= total, dynamic = total - idle, both non-negative.
// 	idleMW := betaMW
// 	if idleMW < 0 {
// 		idleMW = 0
// 	}
// 	if idleMW > totalMW {
// 		idleMW = totalMW
// 	}
// 	dynMW := totalMW - idleMW

// 	// Window deltas (mJ).
// 	idleDeltaMJ := idleMW * dtSec
// 	dynDeltaMJ := dynMW * dtSec

// 	// Cumulative (monotonic) idle/dynamic energies.
// 	idleCumMJ := stateAddF64(c.State, stateKeyCumEnergyF64("rapl", domain, "idle"), idleDeltaMJ)
// 	dynCumMJ := stateAddF64(c.State, stateKeyCumEnergyF64("rapl", domain, "dynamic"), dynDeltaMJ)

// 	labelsIdle := analysis.Labels{"domain": domain, "kind": "idle"}
// 	labelsDyn := analysis.Labels{"domain": domain, "kind": "dynamic"}

// 	// Unified family emissions.
// 	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplPowerMW, labelsIdle), Window: c.Window, Unit: "mW", Value: idleMW})
// 	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplPowerMW, labelsDyn), Window: c.Window, Unit: "mW", Value: dynMW})
// 	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplEnergyMJ, labelsIdle), Window: c.Window, Unit: "mJ", Value: idleCumMJ})
// 	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplEnergyMJ, labelsDyn), Window: c.Window, Unit: "mJ", Value: dynCumMJ})

// 	// Optional idle diagnostics unchanged (still separate metrics).
// 	if config.GetIdleDiagnosticsEnabled() {
// 		ql := analysis.Labels{"name": modelName, "domain": domain, "mode": q.Mode}
// 		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelReady, ql), Window: c.Window, Unit: "bool", Value: boolToFloat(q.Ready)})
// 		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBins, ql), Window: c.Window, Unit: "count", Value: float64(q.BinsPopulated)})
// 		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelPoints, ql), Window: c.Window, Unit: "count", Value: float64(q.TotalPoints)})
// 		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelMode, ql), Window: c.Window, Unit: "enum", Value: modeToFloat(q.Mode)})
// 		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBetaMW, ql), Window: c.Window, Unit: "mW", Value: q.Beta})
// 	}
// }

// file: internal/tycho/analysis/metrics/rapl_unified.go
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
// Contract-aligned RAPL metrics:
//
// 1) Total energy counter (native cumulative, per domain):
//    tycho_rapl_total_energy_mj{domain="pkg|core|dram|uncore"}  (counter)
//
// 2) Window decomposition (per-window increments and window-average power):
//    tycho_rapl_window_energy_mj{domain="...", kind="total|idle|dynamic"} (gauge; this window energy)
//    tycho_rapl_window_power_mw {domain="...", kind="total|idle|dynamic"} (gauge; window-average power)
//
// Note: idle/dynamic are model-based and intentionally NOT exported as cumulative counters.
// -----------------------------------------------------------------------------

const (
	MetricRaplTotalEnergyMJ  analysis.MetricID = "rapl_total_energy_mj"
	MetricWindowRaplEnergyMJ analysis.MetricID = "rapl_window_energy_mj"
	MetricWindowRaplPowerMW  analysis.MetricID = "rapl_window_power_mw"
)

// -----------------------------------------------------------------------------
// RAPL totals
// - total energy: native cumulative counters (end-of-window snapshot; summed across sockets)
// - window energy: per-window increment (first/last differencing inside effective window)
// - window power: window-average based on the same increment
// -----------------------------------------------------------------------------

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

func (m *RaplTotals) ID() analysis.MetricID { return "rapl_totals" }

func (m *RaplTotals) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Rapl() != nil && c.Sink != nil && c.Mono != nil
}

func (m *RaplTotals) Run(c *analysis.Cycle) error {
	r := c.Rapl()
	if r == nil || c.Sink == nil {
		return nil
	}

	// Compute effective window ONCE for this source.
	w := c.EffectiveWindowTicks(m.delayTicks)

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

	dtSec := windowDurSec(c)
	if dtSec <= 0 {
		return nil
	}

	// For each socket: first/last counters within the window.
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

	// Aggregate across sockets.
	// End-of-window total counter values (native cumulative):
	var pkgEndMJ, coreEndMJ, uncoreEndMJ, dramEndMJ uint64
	// Per-window energy increments (first/last differencing):
	var pkgWindowMJ, coreWindowMJ, uncoreWindowMJ, dramWindowMJ uint64

	for _, fl := range perSocket {
		if fl == nil || !fl.ok {
			continue
		}

		// Native counters at end of window (approx: last in-window sample).
		pkgEndMJ += fl.last.Pkg
		coreEndMJ += fl.last.Core
		uncoreEndMJ += fl.last.Uncore
		dramEndMJ += fl.last.DRAM

		// Window energy increments by differencing.
		pkgWindowMJ += analysisops.DeltaU64(fl.first.Pkg, fl.last.Pkg)
		coreWindowMJ += analysisops.DeltaU64(fl.first.Core, fl.last.Core)
		uncoreWindowMJ += analysisops.DeltaU64(fl.first.Uncore, fl.last.Uncore)
		dramWindowMJ += analysisops.DeltaU64(fl.first.DRAM, fl.last.DRAM)
	}

	// Minimal, structured quality.
	q := &analysis.Quality{
		SamplesUsed: len(samples),
		SocketsUsed: len(perSocket),
		DelayTicks:  m.delayTicks,
		Notes:       "",
	}

	emitDomain := func(domain string, endCumMJ uint64, windowMJ uint64) {
		// 1) Total energy counter (native cumulative).
		lblTotal := analysis.Labels{
			"domain": domain,
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:     analysis.Key(MetricRaplTotalEnergyMJ, lblTotal),
			Window:  c.Window,
			Unit:    "mJ",
			Value:   float64(endCumMJ),
			Quality: q,
		})

		// 2) Window total energy increment (gauge).
		lblWinTotal := analysis.Labels{
			"domain": domain,
			"kind":   "total",
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:     analysis.Key(MetricWindowRaplEnergyMJ, lblWinTotal),
			Window:  c.Window,
			Unit:    "mJ",
			Value:   float64(windowMJ),
			Quality: q,
		})

		// 3) Window-average total power (gauge).
		totalMW := 0.0
		if dtSec > 0 {
			totalMW = float64(windowMJ) / dtSec
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:     analysis.Key(MetricWindowRaplPowerMW, lblWinTotal),
			Window:  c.Window,
			Unit:    "mW",
			Value:   totalMW,
			Quality: q,
		})
	}

	emitDomain("pkg", pkgEndMJ, pkgWindowMJ)
	emitDomain("core", coreEndMJ, coreWindowMJ)
	emitDomain("uncore", uncoreEndMJ, uncoreWindowMJ)
	emitDomain("dram", dramEndMJ, dramWindowMJ)

	return nil
}

// -----------------------------------------------------------------------------
// RAPL idle/dynamic split (kind="idle" | "dynamic")
// - reads window total energy from window metric (kind="total")
// - converts to window-average total power
// - estimates idle baseline (beta) and clamps to preserve invariants
// - emits window-average power and window energy increments (NOT cumulative)
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

	// Domains:
	// - pkg/core/uncore: use uCPU proxy
	// - dram: use uDRAM proxy
	for _, d := range []string{"pkg", "core", "uncore"} {
		m.handleDomain(c, d, uCPU, dtSec, now)
	}
	m.handleDomain(c, "dram", uDRAM, dtSec, now)

	return nil
}

func (m *RaplIdleDynamic) handleDomain(c *analysis.Cycle, domain string, u float64, dtSec float64, now time.Time) {
	// Read TOTAL window energy increment (mJ) from pointstore:
	// tycho_rapl_window_energy_mj{domain="<domain>",kind="total"}
	totalKey := analysis.Key(MetricWindowRaplEnergyMJ, analysis.Labels{
		"domain": domain,
		"kind":   "total",
	})
	totalWindowMJ := getPointValue(c.Store, totalKey)

	// Window-average total power in mW: mW = (mJ / s)
	totalMW := 0.0
	if dtSec > 0 {
		totalMW = totalWindowMJ / dtSec
	}

	// Idle model per domain.
	modelName := "rapl_" + domain
	mod := idle.GetOrInitScalar(c.State, modelName, m.cfg)
	if mod == nil {
		return
	}
	_ = mod.Observe(u, totalMW, now)
	betaMW, q := mod.Estimate()

	// Enforce invariants on the split:
	// idle <= total, dynamic = total - idle, both non-negative.
	idleMW := betaMW
	if idleMW < 0 {
		idleMW = 0
	}
	if idleMW > totalMW {
		idleMW = totalMW
	}
	dynMW := totalMW - idleMW

	// Window energy increments (mJ).
	idleWindowMJ := idleMW * dtSec
	dynWindowMJ := dynMW * dtSec

	labelsIdle := analysis.Labels{"domain": domain, "kind": "idle"}
	labelsDyn := analysis.Labels{"domain": domain, "kind": "dynamic"}

	// Window power (gauges).
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplPowerMW, labelsIdle), Window: c.Window, Unit: "mW", Value: idleMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplPowerMW, labelsDyn), Window: c.Window, Unit: "mW", Value: dynMW})

	// Window energy increments (gauges).
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplEnergyMJ, labelsIdle), Window: c.Window, Unit: "mJ", Value: idleWindowMJ})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricWindowRaplEnergyMJ, labelsDyn), Window: c.Window, Unit: "mJ", Value: dynWindowMJ})

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
