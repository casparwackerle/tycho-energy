// file: internal/tycho/analysis/metrics/bpf_procs.go
package analysismetrics

import (
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	MetricBpfCPUInstrRate  analysis.MetricID = "bpf_cpu_instr_per_s"
	MetricBpfCacheMissRate analysis.MetricID = "bpf_cache_miss_per_s"

	// Also emit deltas (sometimes useful for debugging).
	MetricBpfCPUInstrDelta  analysis.MetricID = "bpf_cpu_instr_delta"
	MetricBpfCacheMissDelta analysis.MetricID = "bpf_cache_miss_delta"
)

// BpfProcAggWindow aggregates per-tick process deltas over the window.
// Assumption (from ring/samples.go): BpfProcDelta.{CPUInstr,CacheMiss} are deltas for the tick.
type BpfProcAggWindow struct {
	delayTicks uint64
}

func NewBpfProcAggWindow() *BpfProcAggWindow { return &BpfProcAggWindow{} }

func (m *BpfProcAggWindow) ID() analysis.MetricID { return "bpf_proc_agg_window" }

func (m *BpfProcAggWindow) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Bpf() != nil && c.Mono != nil
}

func (m *BpfProcAggWindow) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Mono == nil || c.Bpf() == nil {
		return nil
	}

	// Delay is configurable (often 0). Still use same code path.
	m.delayTicks = c.Mono.TicksForMsCeil(config.BpfDelayMs())
	wEff := c.EffectiveWindowTicks(m.delayTicks)

	// Include predecessor tick for interval attribution.
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		c.Bpf(),
		wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)
	if len(ticks) < 2 {
		return nil
	}

	// Integrate per-tick deltas over window by overlap.
	// deltaOf(ticks[i]) belongs to (mono[i-1], mono[i]] just like IntegrateDeltaWindow assumes.
	dInstr, usedInstr := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			var s uint64
			for i := range t.Procs {
				s += t.Procs[i].CPUInstr
			}
			return float64(s)
		},
	)

	dMiss, usedMiss := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			var s uint64
			for i := range t.Procs {
				s += t.Procs[i].CacheMiss
			}
			return float64(s)
		},
	)

	// Compute dtSec from corrected window (not wEff).
	dtTicks := uint64(0)
	if c.Window.EndMono > c.Window.StartMono {
		dtTicks = c.Window.EndMono - c.Window.StartMono
	}
	q := c.Mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	dtSec := float64(time.Duration(dtTicks)*q) / float64(time.Second)
	if dtSec <= 0 {
		return nil
	}

	instrRate := dInstr / dtSec
	missRate := dMiss / dtSec

	labels := analysis.Labels{}
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUInstrDelta, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   dInstr,
		Quality: &analysis.Quality{SamplesUsed: usedInstr, DelayTicks: m.delayTicks},
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCacheMissDelta, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   dMiss,
		Quality: &analysis.Quality{SamplesUsed: usedMiss, DelayTicks: m.delayTicks},
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUInstrRate, labels),
		Window:  c.Window,
		Unit:    "1/s",
		Value:   instrRate,
		Quality: &analysis.Quality{SamplesUsed: usedInstr, DelayTicks: m.delayTicks},
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCacheMissRate, labels),
		Window:  c.Window,
		Unit:    "1/s",
		Value:   missRate,
		Quality: &analysis.Quality{SamplesUsed: usedMiss, DelayTicks: m.delayTicks},
	})

	return nil
}
