// file: internal/tycho/analysis/metrics/system_other_idle_dynamic.go
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
	// Slice 9: "other" is system minus (RAPL pkg+dram + GPU sum).
	MetricOtherPowerMW         analysis.MetricID = "other_power_mw"
	MetricOtherEnergyMJ        analysis.MetricID = "other_energy_mj"
	MetricOtherPowerClampedMW  analysis.MetricID = "other_power_clamped_mw"
	MetricOtherEnergyClampMJ   analysis.MetricID = "other_energy_clamped_mj"
	MetricOtherIdlePowerMW     analysis.MetricID = "other_idle_power_mw"
	MetricOtherIdleEnergyMJ    analysis.MetricID = "other_idle_energy_mj"
	MetricOtherDynPowerMW      analysis.MetricID = "other_dyn_power_mw"
	MetricOtherDynEnergyMJ     analysis.MetricID = "other_dyn_energy_mj"
	MetricOtherNegativePowerMW analysis.MetricID = "other_residual_negative_mw" // diag only
	MetricIdleOtherFitReady    analysis.MetricID = "idle_other_fit_ready"       // diag only
)

type SystemOtherIdleDynamic struct{}

func NewSystemOtherIdleDynamic() *SystemOtherIdleDynamic { return &SystemOtherIdleDynamic{} }

func (m *SystemOtherIdleDynamic) ID() analysis.MetricID { return "system_other_idle_dynamic" }

func (m *SystemOtherIdleDynamic) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil
}

func (m *SystemOtherIdleDynamic) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil {
		return nil
	}

	// Window duration (seconds) from ticks.
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

	// --- system energy (prefer fusion hat, else raw redfish window energy) ---
	sysMJ, chassis, src, ok := selectSystemEnergyMJ(c)
	if !ok {
		klog.V(3).Infof("[analysis] slice9: missing system energy window=%s", c.Window.String())
		return nil
	}
	labels := analysis.Labels{"chassis": chassis, "source": src}

	// --- accounted (rapl pkg+dram + gpu sum) ---
	raplPkgMJ := getPointValue(c.Store, analysis.Key(analysis.MetricID("rapl_energy_mj"), analysis.Labels{"domain": "pkg"}))
	raplDramMJ := getPointValue(c.Store, analysis.Key(analysis.MetricID("rapl_energy_mj"), analysis.Labels{"domain": "dram"}))
	raplTotalMJ := raplPkgMJ + raplDramMJ

	gpuMJ := sumEnergyBy(
		"gpu",
		"total",
		c.Store.ListByID(MetricGpuEnergyMJ),
	)

	accountedMJ := raplTotalMJ + gpuMJ

	// --- other ---
	otherMJ := sysMJ - accountedMJ
	otherMJClamp := math.Max(0, otherMJ)

	otherMW := otherMJ / winSec
	otherMWClamp := otherMJClamp / winSec

	// --- idle model on clamped other power ---
	cfg := idle.DefaultConfig()
	model := idle.GetOrInitScalar(c.State, "other", cfg)
	if model == nil {
		return nil
	}

	now := time.Now()
	model.Observe(0.0, otherMWClamp, now)
	idleMW, fitQ := model.Estimate()

	dynMW := math.Max(0, otherMWClamp-idleMW)

	idleMJ := idleMW * winSec
	dynMJ := dynMW * winSec

	// Emit other series (raw + clamped + idle + dyn).
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherPowerMW, labels), Window: c.Window, Unit: "mW", Value: otherMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: otherMJ})
	//c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherPowerClampedMW, labels), Window: c.Window, Unit: "mW", Value: otherMWClamp})
	//c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherEnergyClampMJ, labels), Window: c.Window, Unit: "mJ", Value: otherMJClamp})

	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherIdlePowerMW, labels), Window: c.Window, Unit: "mW", Value: idleMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherIdleEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: idleMJ})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherDynPowerMW, labels), Window: c.Window, Unit: "mW", Value: dynMW})
	c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherDynEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: dynMJ})

	// Diagnostics
	if diag {
		if otherMW < 0 {
			c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricOtherNegativePowerMW, labels), Window: c.Window, Unit: "mW", Value: otherMW})
		}
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleOtherFitReady, labels), Window: c.Window, Unit: "1", Value: boolTo01(fitQ.Ready)})

		klog.V(2).Infof("[analysis] slice9 window=%s chassis=%q src=%s sys_mj=%.3f accounted_mj=%.3f other_mj=%.3f other_clamp_mj=%.3f other_idle_mw=%.3f",
			c.Window.String(), chassis, src, sysMJ, accountedMJ, otherMJ, otherMJClamp, idleMW)
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
