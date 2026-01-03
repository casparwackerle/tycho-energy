package analysismetrics

import (
	"math"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

const (
	MetricSystemResidualEnergyMJ  analysis.MetricID = "system_residual_energy_mj"
	MetricSystemEnergyMJ          analysis.MetricID = "system_energy_mj"
	MetricSystemAccountedEnergyMJ analysis.MetricID = "system_accounted_energy_mj"
	MetricRedfishEnergyHatMJ      analysis.MetricID = "redfish_energy_hat_mj"
	MetricRedfishPowerHatMW       analysis.MetricID = "redfish_power_hat_mw"
	MetricFusionTheta             analysis.MetricID = "fusion_theta" // state key family
)

type SystemResidualEnergy struct{}

func NewSystemResidualEnergy() *SystemResidualEnergy { return &SystemResidualEnergy{} }

func (m *SystemResidualEnergy) ID() analysis.MetricID { return "system_residual_energy" }

func (m *SystemResidualEnergy) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil
}

func (m *SystemResidualEnergy) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil {
		return nil
	}

	// 1) Pick system energy:
	// Prefer fused hat energy (Slice 6b), else fall back to raw Redfish window energy.
	var (
		sysMJ      float64
		sysChassis string
		sysSrc     string
		okSys      bool
	)

	// Prefer fused estimate if present.
	if hatPts := c.Store.ListByID(analysis.MetricID("redfish_energy_hat_mj")); len(hatPts) > 0 {
		sysMJ, sysChassis, okSys = selectRedfishEnergyMJ(hatPts)
		if okSys {
			sysSrc = "hat"
		}
	}

	// Fall back to raw Redfish integration.
	if !okSys {
		rfPts := c.Store.ListByID(MetricRedfishSystemEnergyMJ)
		sysMJ, sysChassis, okSys = selectRedfishEnergyMJ(rfPts)
		if !okSys {
			// Without system energy, residual is undefined.
			klog.V(2).Infof("[analysis] residual: missing system energy for window=%s", c.Window.String())
			return nil
		}
		sysSrc = "redfish"
	}

	// 2) RAPL total = pkg + dram (explicitly NOT summing core/uncore).
	raplPkgMJ := getRAPLDomainMJ(c, "pkg")
	raplDramMJ := getRAPLDomainMJ(c, "dram")
	raplTotalMJ := raplPkgMJ + raplDramMJ

	// 3) GPU total: sum across UUIDs.
	gpuMJ := sumEnergyBy(
		"gpu",
		"total",
		c.Store.ListByID(MetricGpuEnergyMJ),
	)

	accounted := raplTotalMJ + gpuMJ
	residual := sysMJ - accounted

	if config.GetFusionDiagnosticsEnabled() {
		// Emit residual + optional debug helpers.
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemResidualEnergyMJ, nil),
			Window: c.Window,
			Unit:   "mJ",
			Value:  residual,
			Quality: &analysis.Quality{
				DelayTicks: 0, // derived metric: no additional delay; components carry their own
			},
		})
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemEnergyMJ, analysis.Labels{"chassis": sysChassis, "source": sysSrc}),
			Window: c.Window,
			Unit:   "mJ",
			Value:  sysMJ,
		})
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemAccountedEnergyMJ, nil),
			Window: c.Window,
			Unit:   "mJ",
			Value:  accounted,
		})
	}
	// Per-cycle breakdown log (fast validation).
	klog.Infof(
		"[analysis] residual window=%s chassis=%q source=%s sys_mj=%.3f rapl_pkg_mj=%.3f rapl_dram_mj=%.3f gpu_mj=%.3f residual_mj=%.3f",
		c.Window.String(),
		sysChassis,
		sysSrc,
		sysMJ,
		raplPkgMJ,
		raplDramMJ,
		gpuMJ,
		residual,
	)

	// Warn if residual is strongly negative (do not fail).
	tol := math.Max(0.02*sysMJ, 50000.0) // 2% of system energy or 50 J
	if residual < -tol {
		klog.Warningf(
			"[analysis] residual negative beyond tolerance window=%s residual_mj=%.3f tol_mj=%.3f (sys_mj=%.3f accounted_mj=%.3f source=%s)",
			c.Window.String(),
			residual,
			tol,
			sysMJ,
			accounted,
			sysSrc,
		)
	}

	return nil
}

func getRAPLDomainMJ(c *analysis.Cycle, domain string) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	key := analysis.Key(analysis.MetricID("rapl_energy_mj"), analysis.Labels{"domain": domain})
	p, ok := c.Store.GetExact(key)
	if !ok {
		// If RAPL is disabled or domain not present, treat as 0 but keep logs in residual line.
		return 0
	}
	return p.Value
}
