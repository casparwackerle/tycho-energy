package analysisexport

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"k8s.io/klog/v2"
)

type LogSink struct{}

func NewLogSink() *LogSink { return &LogSink{} }

func (s *LogSink) Emit(_ context.Context, p analysis.Point) {
	labels := formatLabels(p.Key.Labels)

	// Quality fields are optional; keep logging stable even if Quality is nil.
	samples := 0
	sockets := 0
	delayTicks := uint64(0)
	notes := ""
	if p.Quality != nil {
		samples = p.Quality.SamplesUsed
		sockets = p.Quality.SocketsUsed
		delayTicks = p.Quality.DelayTicks
		notes = p.Quality.Notes
	}

	klog.Infof(
		"[analysis] emit %s%s value=%v unit=%s window=[start=%d end=%d] samples=%d sockets=%d delayTicks=%d notes=%q",
		p.Key.ID, labels,
		p.Value, p.Unit,
		p.Window.StartMono, p.Window.EndMono,
		samples, sockets, delayTicks, notes,
	)
}

func formatLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%s", k, m[k])
	}
	b.WriteByte('}')
	return b.String()
}
