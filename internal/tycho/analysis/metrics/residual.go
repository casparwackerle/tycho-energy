// file: internal/tycho/analysis/metrics/residual.go
package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
	"k8s.io/klog/v2"
)

const (
	MetricResidualPowerMW  analysis.MetricID = "residual_power_mw"
	MetricResidualEnergyMJ analysis.MetricID = "residual_energy_mj"

	MetricResidualNegativePowerMW analysis.MetricID = "residual_negative_mw" // diag only
	MetricFusionTheta             analysis.MetricID = "fusion_theta"         // state key family

	residualKindTotal   = "total"
	residualKindIdle    = "idle"
	residualKindDynamic = "dynamic"
)

type Residual struct{}

func NewResidual() *Residual { return &Residual{} }

func (m *Residual) ID() analysis.MetricID { return "residual" }

func (m *Residual) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil
}

func (m *Residual) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil {
		return nil
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()
	if monoQuantumSec <= 0 {
		return nil
	}
	winSec := float64(c.Window.EndMono-c.Window.StartMono+1) * monoQuantumSec
	if winSec <= 0 {
		return nil
	}

	// Prefer corrected system power, else raw (warmup).
	sysP, chassis, source, ok := getSystemPower(c)
	if !ok {
		return nil
	}

	// System window energy from average power.
	sysEWinMJ := sysP * winSec

	// Accounted window energy from average power (aligned semantics).
	raplPW := raplTotalPowerMW(c) // pkg + dram
	gpuPW := gpuTotalPowerMW(c)   // sum over gpu_uuid

	accEWinMJ := (raplPW + gpuPW) * winSec

	// Residual window energy and power.
	resWinMJ := sysEWinMJ - accEWinMJ
	resMW := resWinMJ / winSec

	// For counters and idle split, clamp at 0 (no negative energy).
	resWinMJClamp := math.Max(0, resWinMJ)
	resMWClamp := resWinMJClamp / winSec

	// Idle model on clamped residual power.
	cfg := idle.DefaultConfig()
	model := idle.GetOrInitScalar(c.State, "residual", cfg)
	if model == nil {
		return nil
	}
	now := time.Now()
	model.Observe(0.0, resMWClamp, now)
	idleMW, _ := model.Estimate()

	dynMW := math.Max(0, resMWClamp-idleMW)

	idleWinMJ := idleMW * winSec
	dynWinMJ := dynMW * winSec

	labelsBase := analysis.Labels{
		"chassis": chassis,
		"source":  source, // follows active system source; raw during warmup, corrected afterwards
	}

	emitPower := func(kind string, v float64) {
		labels := copyLabels(labelsBase)
		labels["kind"] = kind
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricResidualPowerMW, labels),
			Window: c.Window,
			Unit:   "mW",
			Value:  v,
		})
	}

	emitPower(residualKindTotal, resMW)   // may be negative (diagnostic value)
	emitPower(residualKindIdle, idleMW)   // >= 0
	emitPower(residualKindDynamic, dynMW) // >= 0

	emitEnergyCounter := func(kind string, winAddMJ float64) {
		labels := copyLabels(labelsBase)
		labels["kind"] = kind
		if winAddMJ < 0 || math.IsNaN(winAddMJ) || math.IsInf(winAddMJ, 0) {
			winAddMJ = 0
		}

		key := analysis.Key(MetricResidualEnergyMJ, labels)
		var prev float64
		if v, ok := c.State.Get(key); ok {
			if f, ok2 := v.(float64); ok2 {
				prev = f
			}
		}
		next := prev + winAddMJ
		c.State.Set(key, next)

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricResidualEnergyMJ, labels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  next,
		})
	}

	emitEnergyCounter(residualKindTotal, resWinMJClamp)
	emitEnergyCounter(residualKindIdle, idleWinMJ)
	emitEnergyCounter(residualKindDynamic, dynWinMJ)

	if resMW < 0 {
		labels := copyLabels(labelsBase)
		labels["kind"] = residualKindTotal
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricResidualNegativePowerMW, labels),
			Window: c.Window,
			Unit:   "mW",
			Value:  resMW,
		})
	}

	// Optional: log if residual is persistently negative, helps debug conservation issues.
	if resMW < -1e3 {
		klog.V(4).Infof("[analysis] residual negative chassis=%q source=%q resMW=%.3f sysP=%.3f raplP=%.3f gpuP=%.3f",
			chassis, source, resMW, sysP, raplPW, gpuPW)
	}

	return nil
}

func getSystemPower(c *analysis.Cycle) (pMW float64, chassis, source string, ok bool) {
	if c == nil || c.Store == nil {
		return 0, "", "", false
	}

	pts := c.Store.ListByID(MetricSystemPowerMW)
	if len(pts) == 0 {
		return 0, "", "", false
	}

	// Prefer corrected.
	for _, p := range pts {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["source"] == redfishSourceCorrected && p.Key.Labels["kind"] == systemKindTotal {
			ch := p.Key.Labels["chassis"]
			if ch == "" {
				ch = "Self"
			}
			return p.Value, ch, redfishSourceCorrected, true
		}
	}
	// Else raw.
	for _, p := range pts {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["source"] == redfishSourceRaw && p.Key.Labels["kind"] == systemKindTotal {
			ch := p.Key.Labels["chassis"]
			if ch == "" {
				ch = "Self"
			}
			return p.Value, ch, redfishSourceRaw, true
		}
	}

	return 0, "", "", false
}

func raplTotalPowerMW(c *analysis.Cycle) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	sum := 0.0
	for _, domain := range []string{"pkg", "dram"} {
		key := analysis.Key(
			analysis.MetricID("rapl_power_mw"),
			analysis.Labels{
				"domain": domain,
				"kind":   "total",
				"source": "rapl",
			},
		)
		p, ok := c.Store.GetExact(key)
		if !ok {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		sum += v
	}
	return sum
}

func gpuTotalPowerMW(c *analysis.Cycle) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	// Try using the canonical GPU power metric ID if it exists in your package.
	// If MetricGpuPowerMW is not defined, replace it with analysis.MetricID(MetricGpuPowerMW).
	ps := c.Store.ListByID(analysis.MetricID(MetricGpuPowerMW))
	sum := 0.0
	for _, p := range ps {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["source"] != "nvml_corrected" {
			continue
		}
		if k := p.Key.Labels["kind"]; k != "" && k != "total" {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		sum += v
	}
	return sum
}

func copyLabels(in analysis.Labels) analysis.Labels {
	out := analysis.Labels{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
