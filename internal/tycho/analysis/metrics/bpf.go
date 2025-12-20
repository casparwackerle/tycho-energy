package analysismetrics

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

// MetricIDs (Slice 1 output contract).
const (
	MetricBpfIdleMS    analysis.MetricID = "bpf_idle_ms"
	MetricBpfIRQMS     analysis.MetricID = "bpf_irq_ms"
	MetricBpfSoftIRQMS analysis.MetricID = "bpf_softirq_ms"
)

// BpfWindowCounters sums per-tick deltas across the analysis window.
// Assumption (validated from collector semantics): BpfTick.{IdleNS,IRQNS,SoftirqNS} are deltas per poll.
type BpfWindowCounters struct {
	delayTicks uint64
}

func NewBpfWindowCounters() *BpfWindowCounters { return &BpfWindowCounters{} }

func (m *BpfWindowCounters) ID() analysis.MetricID { return "bpf_window_counters" }

func (m *BpfWindowCounters) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Bpf() != nil && c.Mono != nil
}

func (m *BpfWindowCounters) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Mono == nil {
		return nil
	}
	r := c.Bpf()
	if r == nil {
		return nil
	}

	// Delay is configurable (often 0). Still use the same code path.
	m.delayTicks = c.Mono.TicksForMsCeil(config.BpfDelayMs())

	// Raw-sample selection window (forward-shifted).
	wEff := c.EffectiveWindowTicks(m.delayTicks)

	// Load ticks in [start,end] plus one predecessor tick < start.
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		r,
		wEff.StartMono,
		wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)

	// Weighted window sums (conservative and integral-preserving).
	idleNS, idleIntervals := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 { return float64(t.IdleNS) },
	)
	irqNS, irqIntervals := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 { return float64(t.IRQNS) },
	)
	softirqNS, softirqIntervals := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 { return float64(t.SoftirqNS) },
	)

	// Emit in milliseconds for usability.
	const nsPerMS = 1e6
	idleMS := idleNS / nsPerMS
	irqMS := irqNS / nsPerMS
	softirqMS := softirqNS / nsPerMS

	// Quality: SamplesUsed refers to intervals used.
	qIdle := &analysis.Quality{SamplesUsed: idleIntervals, DelayTicks: m.delayTicks}
	qIRQ := &analysis.Quality{SamplesUsed: irqIntervals, DelayTicks: m.delayTicks}
	qSoft := &analysis.Quality{SamplesUsed: softirqIntervals, DelayTicks: m.delayTicks}

	labels := analysis.Labels{}

	// Emit with the cycle's corrected window (shared across metrics).
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIdleMS, labels),
		Window:  c.Window,
		Unit:    "ms",
		Value:   idleMS,
		Quality: qIdle,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIRQMS, labels),
		Window:  c.Window,
		Unit:    "ms",
		Value:   irqMS,
		Quality: qIRQ,
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfSoftIRQMS, labels),
		Window:  c.Window,
		Unit:    "ms",
		Value:   softirqMS,
		Quality: qSoft,
	})

	return nil
}
