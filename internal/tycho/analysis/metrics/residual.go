// file: internal/tycho/analysis/metrics/residual.go
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

const (
	//raplSource          = "rapl"
	//gpuSourceCorrected  = "nvml_corrected"
	//redfishSourceRaw    = "redfish_raw"
	fusionSourceHat     = "fusion_hat"
	residualSource      = redfishSourceRaw // residual is defined relative to the raw system observation at this stage
	systemEnergyKindHat = "hat"
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
	// Prefer fused hat energy, else fall back to raw Redfish window energy.
	var (
		sysMJ      float64
		sysChassis string
		sysSrc     string // for logging only ("hat" or "redfish")
		okSys      bool
	)

	// Prefer fused estimate if present.
	if hatPts := c.Store.ListByID(MetricRedfishEnergyHatMJ); len(hatPts) > 0 {
		sysMJ, sysChassis, okSys = selectRedfishEnergyMJ(hatPts)
		if okSys {
			sysSrc = systemEnergyKindHat
		}
	}

	// Fall back to raw Redfish integration.
	if !okSys {
		rfPts := c.Store.ListByID(MetricRedfishSystemEnergyMJ)
		sysMJ, sysChassis, okSys = selectRedfishEnergyMJ(rfPts)
		if !okSys {
			klog.V(2).Infof("[analysis] residual: missing system energy for window=%s", c.Window.String())
			return nil
		}
		sysSrc = "redfish"
	}

	// Map to Slice 10A-style provenance label for the emitted system energy debug series.
	sysSourceLabel := redfishSourceRaw
	if sysSrc == systemEnergyKindHat {
		sysSourceLabel = fusionSourceHat
	}

	// 2) RAPL total = pkg + dram (explicitly NOT summing core/uncore).
	raplPkgMJ := getRAPLDomainTotalMJ(c, "pkg")
	raplDramMJ := getRAPLDomainTotalMJ(c, "dram")
	raplTotalMJ := raplPkgMJ + raplDramMJ

	// 3) GPU total: sum across UUIDs for kind="total" and source="nvml_corrected".
	gpuMJ := sumGpuTotalEnergyMJCorrected(c)

	accounted := raplTotalMJ + gpuMJ
	residual := sysMJ - accounted

	if config.GetFusionDiagnosticsEnabled() {
		// Emit residual + optional debug helpers.
		//
		// Slice 10A: all emitted series carry a source label.
		// Residual is defined relative to the raw system observation in this slice.
		resLabels := analysis.Labels{
			"chassis": sysChassis,
			"source":  residualSource,
		}

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemResidualEnergyMJ, resLabels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  residual,
			Quality: &analysis.Quality{
				DelayTicks: 0, // derived metric: components carry their own
			},
		})

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemEnergyMJ, analysis.Labels{"chassis": sysChassis, "source": sysSourceLabel}),
			Window: c.Window,
			Unit:   "mJ",
			Value:  sysMJ,
		})

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemAccountedEnergyMJ, analysis.Labels{"chassis": sysChassis, "source": residualSource}),
			Window: c.Window,
			Unit:   "mJ",
			Value:  accounted,
		})
	}
	return nil
}

// Slice 10A: RAPL energy metrics now include source="rapl" and kind="total".
func getRAPLDomainTotalMJ(c *analysis.Cycle, domain string) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	key := analysis.Key(
		analysis.MetricID("rapl_energy_mj"),
		analysis.Labels{
			"domain": domain,
			"kind":   "total",
			"source": raplSource,
		},
	)
	p, ok := c.Store.GetExact(key)
	if !ok {
		return 0
	}
	return p.Value
}

// Slice 10A: GPU energy points do not have a "component" label, so we sum by labels.
// We also lock to the corrected provenance for contract consistency.
func sumGpuTotalEnergyMJCorrected(c *analysis.Cycle) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	ps := c.Store.ListByID(MetricGpuEnergyMJ)
	sum := 0.0
	for _, p := range ps {
		if p.Key.Labels == nil {
			continue
		}
		if k := p.Key.Labels["kind"]; k != "" && k != "total" {
			continue
		}
		if s := p.Key.Labels["source"]; s != gpuSourceCorrected {
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
