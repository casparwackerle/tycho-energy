// file: internal/tycho/analysis/metrics/system_residual_idle_dynamic.go
package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

const (
	// residual = system minus (RAPL pkg+dram + GPU sum)
	MetricResidualPowerMW         analysis.MetricID = "residual_power_mw"
	MetricResidualEnergyMJ        analysis.MetricID = "residual_energy_mj"
	MetricResidualIdlePowerMW     analysis.MetricID = "residual_idle_power_mw"
	MetricResidualIdleEnergyMJ    analysis.MetricID = "residual_idle_energy_mj"
	MetricResidualDynPowerMW      analysis.MetricID = "residual_dyn_power_mw"
	MetricResidualDynEnergyMJ     analysis.MetricID = "residual_dyn_energy_mj"
	MetricResidualNegativePowerMW analysis.MetricID = "residual_negative_mw" // diag only
	MetricIdleResidualFitReady    analysis.MetricID = "idle_residual_fit_ready"
)

// fixed provenance
const residualSourceRaw = "redfish_raw"

type SystemResidualIdleDynamic struct{}

func NewSystemResidualIdleDynamic() *SystemResidualIdleDynamic {
	return &SystemResidualIdleDynamic{}
}

func (m *SystemResidualIdleDynamic) ID() analysis.MetricID {
	return "system_residual_idle_dynamic"
}

func (m *SystemResidualIdleDynamic) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil
}

func (m *SystemResidualIdleDynamic) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil {
		return nil
	}

	// Window duration (seconds).
	winTicks := uint64(0)
	if c.Window.EndMono > c.Window.StartMono {
		winTicks = c.Window.EndMono - c.Window.StartMono
	}
	q := c.Mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	winSec := float64(time.Duration(winTicks)*q) / float64(time.Second)
	if winSec <= 0 {
		return nil
	}

	diag := config.GetIdleDiagnosticsEnabled()

	// --- system energy (prefer hat, else raw redfish) ---
	sysMJ, chassis, _, ok := selectSystemEnergyMJ(c)
	if !ok {
		klog.V(3).Infof("missing system energy window=%s", c.Window.String())
		return nil
	}

	labels := analysis.Labels{
		"chassis": chassis,
		"source":  residualSourceRaw,
	}

	// --- accounted (rapl pkg+dram + gpu sum) ---
	raplPkgMJ := getPointValue(c.Store, analysis.Key(
		analysis.MetricID("rapl_energy_mj"),
		analysis.Labels{"domain": "pkg"},
	))
	raplDramMJ := getPointValue(c.Store, analysis.Key(
		analysis.MetricID("rapl_energy_mj"),
		analysis.Labels{"domain": "dram"},
	))
	raplTotalMJ := raplPkgMJ + raplDramMJ

	gpuMJ := sumEnergyBy(
		"gpu",
		"total",
		c.Store.ListByID(MetricGpuEnergyMJ),
	)

	accountedMJ := raplTotalMJ + gpuMJ

	// --- residual ---
	residualMJ := sysMJ - accountedMJ
	residualMJClamp := math.Max(0, residualMJ)

	residualMW := residualMJ / winSec
	residualMWClamp := residualMJClamp / winSec

	// --- idle model on clamped residual power ---
	cfg := idle.DefaultConfig()
	model := idle.GetOrInitScalar(c.State, "residual", cfg)
	if model == nil {
		return nil
	}

	now := time.Now()
	model.Observe(0.0, residualMWClamp, now)
	idleMW, fitQ := model.Estimate()

	dynMW := math.Max(0, residualMWClamp-idleMW)

	idleMJ := idleMW * winSec
	dynMJ := dynMW * winSec

	// --- emit residual series ---
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricResidualPowerMW, labels),
		Window: c.Window,
		Unit:   "mW",
		Value:  residualMW,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricResidualEnergyMJ, labels),
		Window: c.Window,
		Unit:   "mJ",
		Value:  residualMJ,
	})

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricResidualIdlePowerMW, labels),
		Window: c.Window,
		Unit:   "mW",
		Value:  idleMW,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricResidualIdleEnergyMJ, labels),
		Window: c.Window,
		Unit:   "mJ",
		Value:  idleMJ,
	})

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricResidualDynPowerMW, labels),
		Window: c.Window,
		Unit:   "mW",
		Value:  dynMW,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricResidualDynEnergyMJ, labels),
		Window: c.Window,
		Unit:   "mJ",
		Value:  dynMJ,
	})

	// --- diagnostics ---
	if diag {
		if residualMW < 0 {
			c.Sink.Emit(c.Ctx, analysis.Point{
				Key:    analysis.Key(MetricResidualNegativePowerMW, labels),
				Window: c.Window,
				Unit:   "mW",
				Value:  residualMW,
			})
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleResidualFitReady, labels),
			Window: c.Window,
			Unit:   "1",
			Value:  boolTo01(fitQ.Ready),
		})
	}

	return nil
}

func selectSystemEnergyMJ(c *analysis.Cycle) (float64, string, string, bool) {
	if c == nil || c.Store == nil {
		return 0, "", "", false
	}

	// Prefer fused hat energy.
	if hat := c.Store.ListByID(MetricRedfishEnergyHatMJ); len(hat) > 0 {
		v, ch, ok := selectRedfishEnergyMJ(hat)
		if ok {
			return v, ch, "hat", true
		}
	}

	// Fallback to raw Redfish integration.
	if raw := c.Store.ListByID(MetricRedfishSystemEnergyMJ); len(raw) > 0 {
		v, ch, ok := selectRedfishEnergyMJ(raw)
		if ok {
			return v, ch, "redfish", true
		}
	}

	return 0, "", "", false
}
