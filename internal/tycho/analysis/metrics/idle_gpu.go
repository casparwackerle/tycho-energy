// file: internal/tycho/analysis/metrics/idle_gpu.go
package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	MetricGpuIdlePowerMW  analysis.MetricID = "gpu_idle_power_mw"
	MetricGpuDynPowerMW   analysis.MetricID = "gpu_dyn_power_mw"
	MetricGpuIdleEnergyMJ analysis.MetricID = "gpu_idle_energy_mj"
	MetricGpuDynEnergyMJ  analysis.MetricID = "gpu_dyn_energy_mj"
)

type GpuIdleDynamic struct {
	cfg idle.Config
}

func NewGpuIdleDynamic() *GpuIdleDynamic {
	return &GpuIdleDynamic{cfg: idle.DefaultConfig()}
}

func (m *GpuIdleDynamic) ID() analysis.MetricID { return "gpu_idle_dynamic" }

func (m *GpuIdleDynamic) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil && c.Gpu() != nil
}

func (m *GpuIdleDynamic) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil || c.Gpu() == nil {
		return nil
	}

	dtSec := windowDurSec(c)
	if dtSec <= 0 {
		return nil
	}

	// We need per-GPU (u_sm,u_mem). We'll take the latest corrected sample within the analysis window.
	// Raw GPU ticks are delayed by config.GpuDelayMs(), same as your GPU window energy plugin.
	delayTicks := c.Mono.TicksForMsCeil(config.GpuDelayMs())

	// Build map uuid -> (u1,u2) from latest tick in raw window that maps into corrected window.
	uMap := latestGpuUtilInWindow(c, delayTicks)

	// For each GPU energy point emitted earlier:
	ps := c.Store.ListByID(MetricGpuEnergyMJ)
	if len(ps) == 0 {
		return nil
	}

	now := time.Now()

	for _, p := range ps {
		uuid := ""
		if p.Key.Labels != nil {
			uuid = p.Key.Labels["gpu_uuid"]
		}
		if uuid == "" {
			continue
		}
		eMJ := p.Value
		pMW := eMJ / dtSec

		u1, u2 := uMap[uuid][0], uMap[uuid][1] // default 0,0 if missing

		modelName := "gpu_" + uuid
		mod := idle.GetOrInitVec2(c.State, modelName, m.cfg)
		if mod == nil {
			continue
		}
		_ = mod.Observe(u1, u2, pMW, now)
		betaMW, q := mod.Estimate()

		idleMW := betaMW
		dynMW := pMW - betaMW
		if dynMW < 0 {
			dynMW = 0
		}

		labels := analysis.Labels{"gpu_uuid": uuid}

		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuIdlePowerMW, labels), Window: c.Window, Unit: "mW", Value: idleMW})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuDynPowerMW, labels), Window: c.Window, Unit: "mW", Value: dynMW})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuIdleEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: idleMW * dtSec})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuDynEnergyMJ, labels), Window: c.Window, Unit: "mJ", Value: dynMW * dtSec})

		ql := analysis.Labels{"name": modelName, "gpu_uuid": uuid, "mode": q.Mode}
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelReady, ql), Window: c.Window, Unit: "bool", Value: boolToFloat(q.Ready)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBins, ql), Window: c.Window, Unit: "count", Value: float64(q.BinsPopulated)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelPoints, ql), Window: c.Window, Unit: "count", Value: float64(q.TotalPoints)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelMode, ql), Window: c.Window, Unit: "enum", Value: modeToFloat(q.Mode)})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBetaMW, ql), Window: c.Window, Unit: "mW", Value: q.Beta})
	}

	return nil
}

func latestGpuUtilInWindow(c *analysis.Cycle, delayTicks uint64) map[string][2]float64 {
	out := make(map[string][2]float64, 8)
	if c == nil || c.Gpu() == nil {
		return out
	}

	// Raw selection window: we want corrected time in [Window.StartMono,Window.EndMono]
	// corrected = raw - delayTicks => raw in window shifted forward
	rawW := c.Window.ShiftForward(delayTicks)

	seg1, seg2 := c.Gpu().ViewChrono()

	// Walk chronological segments, keep last per uuid.
	ingest := func(seg []ring.GpuTick) {
		for _, t := range seg {
			tRaw := t.SampleMeta.Mono
			if tRaw < rawW.StartMono || tRaw > rawW.EndMono {
				continue
			}
			for i := range t.Devices {
				d := t.Devices[i]
				uuid := d.UUID
				if uuid == "" {
					continue
				}
				u1 := clamp01(d.SMUtilPct / 100.0)
				u2 := clamp01(d.MemUtilPct / 100.0)
				out[uuid] = [2]float64{u1, u2}
			}
		}
	}

	ingest(seg1)
	ingest(seg2)
	return out
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}
