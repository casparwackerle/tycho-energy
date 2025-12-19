package analysismetrics

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// MetricIDs (Slice 1 output contract).
const (
	MetricBpfIdleMS    analysis.MetricID = "bpf_idle_ms"
	MetricBpfIRQMS     analysis.MetricID = "bpf_irq_ms"
	MetricBpfSoftIRQMS analysis.MetricID = "bpf_softirq_ms"
)

// BpfWindowCounters sums per-tick deltas across the analysis window.
// Assumption (validated from collector semantics): BpfTick.{IdleNS,IRQNS,SoftirqNS} are deltas per poll.
type BpfWindowCounters struct{}

func NewBpfWindowCounters() *BpfWindowCounters { return &BpfWindowCounters{} }

func (m *BpfWindowCounters) ID() analysis.MetricID { return "bpf_window_counters" }

func (m *BpfWindowCounters) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Bpf() != nil
}

func (m *BpfWindowCounters) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil {
		return nil
	}
	r := c.Bpf()
	if r == nil {
		return nil
	}

	w := c.EffectiveWindowTicks(0)

	// Load ticks in [start,end] plus one predecessor tick < start.
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		r,
		w.StartMono,
		w.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)

	// Weighted window sums (still “conservative” and integral-preserving).
	idleNS, idleIntervals := analysisops.IntegrateDeltaWindow(
		ticks, w.StartMono, w.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 { return float64(t.IdleNS) },
	)
	irqNS, irqIntervals := analysisops.IntegrateDeltaWindow(
		ticks, w.StartMono, w.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 { return float64(t.IRQNS) },
	)
	softirqNS, softirqIntervals := analysisops.IntegrateDeltaWindow(
		ticks, w.StartMono, w.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 { return float64(t.SoftirqNS) },
	)

	// Emit in seconds for usability.
	const nsPerMS = 1e6
	idleMS := idleNS / nsPerMS
	irqMS := irqNS / nsPerMS
	softirqMS := softirqNS / nsPerMS

	// Quality: SamplesUsed now refers to intervals used (not raw ticks).
	// Keep it simple; you can refine later.
	qIdle := &analysis.Quality{SamplesUsed: idleIntervals, DelayTicks: 0}
	qIRQ := &analysis.Quality{SamplesUsed: irqIntervals, DelayTicks: 0}
	qSoft := &analysis.Quality{SamplesUsed: softirqIntervals, DelayTicks: 0}

	// labels stay empty (Slice 1)
	labels := analysis.Labels{}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIdleMS, labels),
		Window:  w,
		Unit:    "ms",
		Value:   idleMS,
		Quality: qIdle,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIRQMS, labels),
		Window:  w,
		Unit:    "ms",
		Value:   irqMS,
		Quality: qIRQ,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfSoftIRQMS, labels),
		Window:  w,
		Unit:    "ms",
		Value:   softirqMS,
		Quality: qSoft,
	})

	return nil
}
