package analysisexport

import (
	"context"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"k8s.io/klog/v2"
)

type LogSink struct{}

func NewLogSink() *LogSink { return &LogSink{} }

func (s *LogSink) Emit(_ context.Context, p analysis.Point) {
	if p.Quality != nil {
		klog.Infof("[analysis] emit %s value=%v unit=%s window=%s samples=%d notes=%q",
			p.Key.String(), p.Value, p.Unit, p.Window.String(), p.Quality.SamplesUsed, p.Quality.Notes)
		return
	}
	klog.Infof("[analysis] emit %s value=%v unit=%s window=%s",
		p.Key.String(), p.Value, p.Unit, p.Window.String())
}
