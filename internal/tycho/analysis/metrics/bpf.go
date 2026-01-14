package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

// MetricIDs (Slice 1 output contract).
const (
	// CPU time-share ratios (gauges, unit="ratio", value in [0,1]).
	MetricBpfCPUIDLERatio    analysis.MetricID = "bpf_cpu_idle_ratio"
	MetricBpfCPUIRQRatio     analysis.MetricID = "bpf_cpu_irq_ratio"
	MetricBpfCPUSoftIRQRatio analysis.MetricID = "bpf_cpu_softirq_ratio"
	MetricBpfCPUActiveRatio  analysis.MetricID = "bpf_cpu_active_ratio"

	// System-level cumulative counters aggregated across all PIDs (counters, unit="count").
	MetricBpfCPUInstructions analysis.MetricID = "bpf_cpu_instructions"
	MetricBpfCPUCycles       analysis.MetricID = "bpf_cpu_cycles"
	MetricBpfCacheMisses     analysis.MetricID = "bpf_cache_misses"
	MetricBpfPageCacheHits   analysis.MetricID = "bpf_page_cache_hits"
	MetricBpfIRQNetTX        analysis.MetricID = "bpf_irq_net_tx"
	MetricBpfIRQNetRX        analysis.MetricID = "bpf_irq_net_rx"
	MetricBpfIRQBlock        analysis.MetricID = "bpf_irq_block"
)

const bpfSource = "ebpf"

// BpfSystemMetrics emits eBPF-derived system metrics:
//   - CPU time-share ratios (idle/irq/softirq/active) as window-relative gauges (unit="ratio").
//     Important: BpfTick.{IdleNS,IRQNS,SoftirqNS} are CPU-time-like and typically aggregated across logical CPUs,
//     so ratios are normalized by (window_ns * logical_cores).
//   - Aggregated process counters (summed across PIDs and accumulated across cycles) as true counters (unit="count").
//
// Notes:
//   - BpfTick.{IdleNS,IRQNS,SoftirqNS} are assumed to be per-tick deltas (validated from collector semantics).
//   - BpfProcDelta.* fields used here are assumed to be per-tick deltas.
//   - Window-edge overlap is handled conservatively using IntegrateDeltaWindow, which can yield fractional counts.
//     We keep float64 accumulators to preserve conservation across variable window boundaries.
type BpfSystemMetrics struct {
	delayTicks uint64

	// Collected once.
	cores int

	// Persistent accumulators (true counters, reset on process restart is acceptable).
	cpuInstr      float64
	cpuCycles     float64
	cacheMisses   float64
	pageCacheHits float64
	irqNetTX      float64
	irqNetRX      float64
	irqBlock      float64
}

func NewBpfSystemMetrics() *BpfSystemMetrics {
	return &BpfSystemMetrics{
		cores: getCPUCores(),
	}
}

func (m *BpfSystemMetrics) ID() analysis.MetricID { return "bpf_system_metrics" }

func (m *BpfSystemMetrics) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Bpf() != nil && c.Mono != nil
}

func (m *BpfSystemMetrics) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Mono == nil || c.Bpf() == nil {
		return nil
	}

	// Delay is configurable (often 0). Still use the same code path.
	m.delayTicks = c.Mono.TicksForMsCeil(config.BpfDelayMs())

	// Raw-sample selection window (forward-shifted).
	wEff := c.EffectiveWindowTicks(m.delayTicks)

	// Load ticks in [start,end] plus one predecessor tick < start.
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		c.Bpf(),
		wEff.StartMono,
		wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)
	if len(ticks) < 2 {
		// Not enough to attribute intervals conservatively.
		return nil
	}

	labels := analysis.Labels{
		"source": bpfSource,
	}

	// ----------------------------------------------------------------
	// (1) CPU time-share ratios (gauges, unit="ratio").
	// ----------------------------------------------------------------

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

	// Window duration from corrected window (shared across all emitted metrics).
	dtTicks := uint64(0)
	if c.Window.EndMono > c.Window.StartMono {
		dtTicks = c.Window.EndMono - c.Window.StartMono
	}
	q := c.Mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	windowDur := time.Duration(dtTicks) * q
	windowNS := float64(windowDur) / float64(time.Nanosecond)
	if windowNS <= 0 {
		return nil
	}

	cores := m.cores
	if cores <= 0 {
		cores = 1
	}
	capacityNS := windowNS * float64(cores)
	if capacityNS <= 0 {
		return nil
	}

	// Compute ratios in [0,1] and clamp conservatively.
	sumNS := idleNS + irqNS + softirqNS
	activeNS := capacityNS - sumNS
	if activeNS < 0 {
		activeNS = 0
	}

	idleRatio := idleNS / capacityNS
	irqRatio := irqNS / capacityNS
	softirqRatio := softirqNS / capacityNS
	activeRatio := activeNS / capacityNS

	clamp01 := func(x float64) float64 {
		if x < 0 {
			return 0
		}
		if x > 1 {
			return 1
		}
		return x
	}
	idleRatio = clamp01(idleRatio)
	irqRatio = clamp01(irqRatio)
	softirqRatio = clamp01(softirqRatio)
	activeRatio = clamp01(activeRatio)

	// Ratios: unit="ratio"
	// Quality: SamplesUsed refers to intervals used by each integration.
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUIDLERatio, labels),
		Window:  c.Window,
		Unit:    "ratio",
		Value:   idleRatio,
		Quality: &analysis.Quality{SamplesUsed: idleIntervals, DelayTicks: m.delayTicks},
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUIRQRatio, labels),
		Window:  c.Window,
		Unit:    "ratio",
		Value:   irqRatio,
		Quality: &analysis.Quality{SamplesUsed: irqIntervals, DelayTicks: m.delayTicks},
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUSoftIRQRatio, labels),
		Window:  c.Window,
		Unit:    "ratio",
		Value:   softirqRatio,
		Quality: &analysis.Quality{SamplesUsed: softirqIntervals, DelayTicks: m.delayTicks},
	})
	// For active, reuse the max of the three interval counts as a simple proxy.
	usedActive := idleIntervals
	if irqIntervals > usedActive {
		usedActive = irqIntervals
	}
	if softirqIntervals > usedActive {
		usedActive = softirqIntervals
	}
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUActiveRatio, labels),
		Window:  c.Window,
		Unit:    "ratio",
		Value:   activeRatio,
		Quality: &analysis.Quality{SamplesUsed: usedActive, DelayTicks: m.delayTicks},
	})

	// ----------------------------------------------------------------
	// (2) Aggregated process counters (true counters, unit="count").
	// ----------------------------------------------------------------

	sumU64 := func(t ring.BpfTick, f func(p ring.BpfProcDelta) uint64) uint64 {
		var s uint64
		for i := range t.Procs {
			s += f(t.Procs[i])
		}
		return s
	}

	// Instructions
	dInstr, usedInstr := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.CPUInstr }))
		},
	)
	m.cpuInstr += dInstr
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUInstructions, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.cpuInstr),
		Quality: &analysis.Quality{SamplesUsed: usedInstr, DelayTicks: m.delayTicks},
	})

	// Cycles
	dCycles, usedCycles := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.CPUCycles }))
		},
	)
	m.cpuCycles += dCycles
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCPUCycles, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.cpuCycles),
		Quality: &analysis.Quality{SamplesUsed: usedCycles, DelayTicks: m.delayTicks},
	})

	// Cache misses
	dMiss, usedMiss := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.CacheMiss }))
		},
	)
	m.cacheMisses += dMiss
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfCacheMisses, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.cacheMisses),
		Quality: &analysis.Quality{SamplesUsed: usedMiss, DelayTicks: m.delayTicks},
	})

	// Page cache hits
	dPCHit, usedPCHit := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.PageCacheHit }))
		},
	)
	m.pageCacheHits += dPCHit
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfPageCacheHits, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.pageCacheHits),
		Quality: &analysis.Quality{SamplesUsed: usedPCHit, DelayTicks: m.delayTicks},
	})

	// IRQ net TX
	dTX, usedTX := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.IRQNetTX }))
		},
	)
	m.irqNetTX += dTX
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIRQNetTX, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.irqNetTX),
		Quality: &analysis.Quality{SamplesUsed: usedTX, DelayTicks: m.delayTicks},
	})

	// IRQ net RX
	dRX, usedRX := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.IRQNetRX }))
		},
	)
	m.irqNetRX += dRX
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIRQNetRX, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.irqNetRX),
		Quality: &analysis.Quality{SamplesUsed: usedRX, DelayTicks: m.delayTicks},
	})

	// IRQ block
	dBlk, usedBlk := analysisops.IntegrateDeltaWindow(
		ticks, wEff.StartMono, wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
		func(t ring.BpfTick) float64 {
			return float64(sumU64(t, func(p ring.BpfProcDelta) uint64 { return p.IRQBlock }))
		},
	)
	m.irqBlock += dBlk
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:     analysis.Key(MetricBpfIRQBlock, labels),
		Window:  c.Window,
		Unit:    "count",
		Value:   math.Floor(m.irqBlock),
		Quality: &analysis.Quality{SamplesUsed: usedBlk, DelayTicks: m.delayTicks},
	})

	return nil
}
