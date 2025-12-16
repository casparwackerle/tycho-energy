package analysisregistry

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
)

type Registry struct {
	metrics []analysis.Metric
}

func New() *Registry { return &Registry{} }

func (r *Registry) Register(m analysis.Metric) {
	if m == nil {
		return
	}
	r.metrics = append(r.metrics, m)
}

// BuildPlan returns an ordered list of metrics.
// Slice 0: flat list, no dependencies, no DAG solver.
func (r *Registry) BuildPlan(_ *analysis.Cycle) analysis.Plan {
	// static plan in slice 0
	return &Plan{metrics: append([]analysis.Metric(nil), r.metrics...)}
}
