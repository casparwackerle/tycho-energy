package analysisregistry

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"k8s.io/klog/v2"
)

type Plan struct {
	metrics []analysis.Metric
}

// Run executes each enabled metric plugin in order.
// Engine remains metric-free; errors do not stop other metrics in Slice 0.
func (p *Plan) Run(c *analysis.Cycle) error {
	if p == nil || c == nil {
		return nil
	}
	for _, m := range p.metrics {
		if m == nil {
			continue
		}
		if !m.IsEnabled(c) {
			continue
		}
		if err := m.Run(c); err != nil {
			klog.Warningf("[analysis] metric %s failed: %v", m.ID(), err)
			// continue
		}
	}
	return nil
}
