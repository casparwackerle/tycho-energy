package analysismetrics

import (
	"math"
	"sort"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"k8s.io/klog/v2"
)

const (
	MetricSystemResidualEnergyMJ  analysis.MetricID = "system_residual_energy_mj"
	MetricSystemEnergyMJ          analysis.MetricID = "system_energy_mj"
	MetricSystemAccountedEnergyMJ analysis.MetricID = "system_accounted_energy_mj"
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

	// 1) Pick redfish chassis energy (prefer Self).
	rfPts := c.Store.ListByID(MetricRedfishSystemEnergyMJ)
	rfMJ, rfChassis, okRF := selectRedfishEnergyMJ(rfPts)
	if !okRF {
		// Without system energy, residual is undefined.
		klog.V(2).Infof("[analysis] residual: missing redfish_system_energy_mj for window=%s", c.Window.String())
		return nil
	}

	// 2) RAPL total = pkg + dram (explicitly NOT summing core/uncore).
	raplPkgMJ := getRAPLDomainMJ(c, "pkg")
	raplDramMJ := getRAPLDomainMJ(c, "dram")
	raplTotalMJ := raplPkgMJ + raplDramMJ

	// 3) GPU total: sum across UUIDs.
	gpuMJ := sumByID(c.Store.ListByID(MetricGpuEnergyMJ))

	accounted := raplTotalMJ + gpuMJ
	residual := rfMJ - accounted

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
		Key:    analysis.Key(MetricSystemEnergyMJ, analysis.Labels{"chassis": rfChassis}),
		Window: c.Window,
		Unit:   "mJ",
		Value:  rfMJ,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricSystemAccountedEnergyMJ, nil),
		Window: c.Window,
		Unit:   "mJ",
		Value:  accounted,
	})

	// Per-cycle breakdown log (fast validation).
	klog.Infof(
		"[analysis] residual window=%s chassis=%q redfish_mj=%.3f rapl_pkg_mj=%.3f rapl_dram_mj=%.3f gpu_mj=%.3f residual_mj=%.3f",
		c.Window.String(),
		rfChassis,
		rfMJ,
		raplPkgMJ,
		raplDramMJ,
		gpuMJ,
		residual,
	)

	// Warn if residual is strongly negative (do not fail).
	tol := math.Max(0.02*rfMJ, 50000.0) // 2% of system energy or 50 J
	if residual < -tol {
		klog.Warningf(
			"[analysis] residual negative beyond tolerance window=%s residual_mj=%.3f tol_mj=%.3f (redfish_mj=%.3f accounted_mj=%.3f)",
			c.Window.String(),
			residual,
			tol,
			rfMJ,
			accounted,
		)
	}

	return nil
}

func sumByID(ps []analysis.Point) float64 {
	var s float64
	for _, p := range ps {
		// Assume unit correctness (mJ). This slice keeps it minimal.
		s += p.Value
	}
	return s
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

func selectRedfishEnergyMJ(ps []analysis.Point) (val float64, chassis string, ok bool) {
	if len(ps) == 0 {
		return 0, "", false
	}

	// Prefer chassis="Self"
	for _, p := range ps {
		ch := ""
		if p.Key.Labels != nil {
			ch = p.Key.Labels["chassis"]
		}
		if ch == "Self" {
			return p.Value, "Self", true
		}
	}

	// Else pick deterministically: lexicographically smallest non-empty chassis label,
	// falling back to first point if labels are missing.
	type cand struct {
		ch string
		v  float64
	}
	cands := make([]cand, 0, len(ps))
	for _, p := range ps {
		ch := ""
		if p.Key.Labels != nil {
			ch = p.Key.Labels["chassis"]
		}
		cands = append(cands, cand{ch: ch, v: p.Value})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ch < cands[j].ch })

	return cands[0].v, cands[0].ch, true
}
