// file: internal/tycho/analysis/metrics/idle_rapl.go
package analysismetrics

import (
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	MetricRaplIdlePowerMW  analysis.MetricID = "rapl_idle_power_mw"
	MetricRaplDynPowerMW   analysis.MetricID = "rapl_dyn_power_mw"
	MetricRaplIdleEnergyMJ analysis.MetricID = "rapl_idle_energy_mj"
	MetricRaplDynEnergyMJ  analysis.MetricID = "rapl_dyn_energy_mj"

	MetricIdleModelReady  analysis.MetricID = "idle_model_ready"
	MetricIdleModelBins   analysis.MetricID = "idle_model_bins"
	MetricIdleModelPoints analysis.MetricID = "idle_model_points"
	MetricIdleModelMode   analysis.MetricID = "idle_model_mode"
	MetricIdleModelBetaMW analysis.MetricID = "idle_model_beta_mw"
)

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

	// Read proxy rates from pointstore (if missing, treat as 0 and model will stay baseline).
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

	// Domains to model:
	// - pkg/core/uncore: use uCPU proxy (same for v1)
	// - dram: use uDRAM proxy
	domainsCPU := []string{"pkg", "core", "uncore"}
	domainsDRAM := []string{"dram"}

	for _, d := range domainsCPU {
		m.handleDomain(c, d, uCPU, dtSec, now)
	}
	for _, d := range domainsDRAM {
		m.handleDomain(c, d, uDRAM, dtSec, now)
	}

	return nil
}

func (m *RaplIdleDynamic) handleDomain(c *analysis.Cycle, domain string, u float64, dtSec float64, now time.Time) {
	// Read energy (mJ) from pointstore.
	eKey := analysis.Key(analysis.MetricID("rapl_energy_mj"), analysis.Labels{"domain": domain})
	eMJ := getPointValue(c.Store, eKey)

	// Convert to window-average power in mW: mW = mJ / s
	pMW := 0.0
	if dtSec > 0 {
		pMW = eMJ / dtSec
	}

	modelName := "rapl_" + domain
	mod := idle.GetOrInitScalar(c.State, modelName, m.cfg)
	if mod == nil {
		return
	}
	_ = mod.Observe(u, pMW, now)
	betaMW, q := mod.Estimate()

	idleMW := betaMW
	dynMW := pMW - betaMW
	if dynMW < 0 {
		dynMW = 0
	}

	idleMJ := idleMW * dtSec
	dynMJ := dynMW * dtSec

	labels := analysis.Labels{"domain": domain}

	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplIdlePowerMW, labels), Window: c.Window, Unit: "mW", Value: idleMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplDynPowerMW, labels), Window: c.Window, Unit: "mW", Value: dynMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplIdleEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: idleMJ})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricRaplDynEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: dynMJ})

	diag := config.GetIdleDiagnosticsEnabled()
	if diag {
		// Quality gauges.
		ql := analysis.Labels{"name": modelName, "domain": domain, "mode": q.Mode}
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelReady, ql), Window: c.Window, Unit: "bool", Value: boolToFloat(q.Ready)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBins, ql), Window: c.Window, Unit: "count", Value: float64(q.BinsPopulated)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelPoints, ql), Window: c.Window, Unit: "count", Value: float64(q.TotalPoints)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelMode, ql), Window: c.Window, Unit: "enum", Value: modeToFloat(q.Mode)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBetaMW, ql), Window: c.Window, Unit: "mW", Value: q.Beta})
	}
}

func windowDurSec(c *analysis.Cycle) float64 {
	if c == nil || c.Mono == nil {
		return 0
	}
	if c.Window.EndMono <= c.Window.StartMono {
		return 0
	}
	q := c.Mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	dtTicks := c.Window.EndMono - c.Window.StartMono
	return float64(time.Duration(dtTicks)*q) / float64(time.Second)
}

func getPointValue(store *analysis.PointStore, key analysis.MetricKey) float64 {
	if store == nil {
		return 0
	}
	p, ok := store.GetExact(key)
	if !ok {
		return 0
	}
	return p.Value
}

func normalizeRate(rate, amax float64) float64 {
	if amax <= 0 {
		return 0
	}
	u := rate / amax
	if u < 0 {
		return 0
	}
	if u > 1 {
		return 1
	}
	return u
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func modeToFloat(mode string) float64 {
	// Prometheus prefers labels over enums, but keep a numeric for quick dashboards.
	if mode == "model" {
		return 1
	}
	return 0
}
