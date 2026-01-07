package analysisexport

import (
	"context"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
)

// MultiSink fan-outs points to multiple sinks.
// Useful to keep LogSink while also exporting Prometheus.
type MultiSink struct {
	sinks []analysis.Sink
}

func NewMultiSink(sinks ...analysis.Sink) *MultiSink {
	out := make([]analysis.Sink, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			out = append(out, s)
		}
	}
	return &MultiSink{sinks: out}
}

func (m *MultiSink) Emit(ctx context.Context, p analysis.Point) {
	for _, s := range m.sinks {
		s.Emit(ctx, p)
	}
}

func (m *MultiSink) Delete(ctx context.Context, key analysis.MetricKey) {
	for _, s := range m.sinks {
		s.Delete(ctx, key)
	}
}
