// file: internal/tycho/analysis/metrics/attrib_bpf.go
package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/attribution"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

// Slice 16: Pattern B (one metric per signal).
//
// Prometheus sink prefix "tycho" yields:
//
//	tycho_workload_bpf_cpu_instructions_total{source,namespace,pod,container}
//	tycho_workload_bpf_cpu_cycles_total{source,namespace,pod,container}
//	tycho_workload_bpf_cache_misses_total{source,namespace,pod,container}
//	tycho_workload_bpf_page_cache_hits_total{source,namespace,pod,container}
//	tycho_workload_bpf_irq_net_tx_total{source,namespace,pod,container}
//	tycho_workload_bpf_irq_net_rx_total{source,namespace,pod,container}
//	tycho_workload_bpf_irq_block_total{source,namespace,pod,container}
//	tycho_workload_bpf_process_run_us_total{source,namespace,pod,container}
//
// Units:
//   - *_total counters: "count" except process_run_us_total uses "us".
const (
	MetricWorkloadBpfCPUInstructionsTotal analysis.MetricID = "workload_bpf_cpu_instructions_total"
	MetricWorkloadBpfCPUCyclesTotal       analysis.MetricID = "workload_bpf_cpu_cycles_total"
	MetricWorkloadBpfCacheMissesTotal     analysis.MetricID = "workload_bpf_cache_misses_total"
	MetricWorkloadBpfPageCacheHitsTotal   analysis.MetricID = "workload_bpf_page_cache_hits_total"
	MetricWorkloadBpfIRQNetTXTotal        analysis.MetricID = "workload_bpf_irq_net_tx_total"
	MetricWorkloadBpfIRQNetRXTotal        analysis.MetricID = "workload_bpf_irq_net_rx_total"
	MetricWorkloadBpfIRQBlockTotal        analysis.MetricID = "workload_bpf_irq_block_total"
	MetricWorkloadBpfProcessRunUsTotal    analysis.MetricID = "workload_bpf_process_run_us_total"
)

type bpfWorkloadSeriesRef struct {
	Namespace string
	Pod       string
	Container string
}

type BpfWorkloadCounters struct {
	// no config (slice minimal)
}

func NewBpfWorkloadCounters() *BpfWorkloadCounters { return &BpfWorkloadCounters{} }

func (m *BpfWorkloadCounters) ID() analysis.MetricID { return "attrib_bpf_workload_counters_v1" }

func (m *BpfWorkloadCounters) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.State != nil && c.Meta != nil && c.Bpf() != nil && c.Mono != nil
}

func (m *BpfWorkloadCounters) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.State == nil || c.Meta == nil || c.Bpf() == nil || c.Mono == nil {
		return nil
	}

	// -------------------------------------------------------------------------
	// Window extraction (same pattern as Slice 13D and bpf.go)
	// -------------------------------------------------------------------------
	delayTicks := c.Mono.TicksForMsCeil(config.BpfDelayMs())
	wEff := c.EffectiveWindowTicks(delayTicks)

	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		c.Bpf(),
		wEff.StartMono,
		wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)
	if len(ticks) < 2 {
		return nil
	}
	inWindow := ticks[1:] // intervals are (ticks[i-1], ticks[i]]

	// Refresh cgroup index once per window (Decision B cgroup fallback).
	nowWall := time.Now()
	nowMono := c.NowMono
	if nowMono == 0 {
		nowMono = c.Window.EndMono
	}
	attribution.BuildCgroupIndexFromBpfWindow(c.Meta, nowWall, nowMono, inWindow)

	// -------------------------------------------------------------------------
	// Accumulate per-workload delta sums over the effective window.
	//
	// IMPORTANT BEHAVIOR CHANGE (fixing __system__ semantics):
	//   - We always compute "total" deltas directly from the raw tick.Procs (all PIDs we saw).
	//   - We only add deltas to non-system workloads when they are resolved to a k8s workload.
	//   - __system__ is then set as the residual:
	//       system = total - sum(non-system workloads)
	//     per signal, clamped >= 0.
	//
	// This guarantees full conservation:
	//   total == sum(workloads) + system
	// even if some PIDs cannot be resolved to Kubernetes.
	// -------------------------------------------------------------------------
	sys := attribution.SystemWorkloadKey()

	// Window integration uses float64 to preserve conservation under partial overlap.
	type agg struct {
		instr  float64
		cycles float64
		misses float64
		pchit  float64
		irqTX  float64
		irqRX  float64
		irqBlk float64
		runUs  float64
	}

	byWK := make(map[attribution.WorkloadKey]*agg, 256)
	byWK[sys] = &agg{} // always present

	// Totals over all observed PIDs (raw truth for conservation).
	var tot agg

	// Iterate intervals: interval i corresponds to curr := ticks[i], prev := ticks[i-1].
	for i := 1; i < len(ticks); i++ {
		prev := ticks[i-1]
		curr := ticks[i]

		f := overlapFactor(prev.Mono, curr.Mono, wEff.StartMono, wEff.EndMono)
		if f <= 0 {
			continue
		}

		for j := range curr.Procs {
			p := &curr.Procs[j]

			// 1) Always add to totals (all PIDs we observed in the raw buffer).
			if p.CPUInstr != 0 {
				tot.instr += f * float64(p.CPUInstr)
			}
			if p.CPUCycles != 0 {
				tot.cycles += f * float64(p.CPUCycles)
			}
			if p.CacheMiss != 0 {
				tot.misses += f * float64(p.CacheMiss)
			}
			if p.PageCacheHit != 0 {
				tot.pchit += f * float64(p.PageCacheHit)
			}
			if p.IRQNetTX != 0 {
				tot.irqTX += f * float64(p.IRQNetTX)
			}
			if p.IRQNetRX != 0 {
				tot.irqRX += f * float64(p.IRQNetRX)
			}
			if p.IRQBlock != 0 {
				tot.irqBlk += f * float64(p.IRQBlock)
			}
			if p.ProcessRunUs != 0 {
				tot.runUs += f * float64(p.ProcessRunUs)
			}

			// 2) Only add to workload buckets if we can resolve to a k8s workload.
			//
			// If resolution fails, do not add anywhere here; it will end up in __system__
			// via the residual step below.
			procID, _, okProc := attribution.ResolveProcFromCycle(c, p.PID)
			procPtr := attribution.ProcIDPtr(procID, okProc)

			wkKey, okWk := attribution.ResolveWorkload(c.Meta, procPtr, p.CgroupID)
			if !okWk || wkKey.IsZero() || wkKey.IsSystem() {
				continue
			}

			a := byWK[wkKey]
			if a == nil {
				a = &agg{}
				byWK[wkKey] = a
			}

			// All fields in ring.BpfProcDelta are per-tick deltas.
			// Apply overlap scaling factor f (can be fractional).
			if p.CPUInstr != 0 {
				a.instr += f * float64(p.CPUInstr)
			}
			if p.CPUCycles != 0 {
				a.cycles += f * float64(p.CPUCycles)
			}
			if p.CacheMiss != 0 {
				a.misses += f * float64(p.CacheMiss)
			}
			if p.PageCacheHit != 0 {
				a.pchit += f * float64(p.PageCacheHit)
			}
			if p.IRQNetTX != 0 {
				a.irqTX += f * float64(p.IRQNetTX)
			}
			if p.IRQNetRX != 0 {
				a.irqRX += f * float64(p.IRQNetRX)
			}
			if p.IRQBlock != 0 {
				a.irqBlk += f * float64(p.IRQBlock)
			}
			if p.ProcessRunUs != 0 {
				a.runUs += f * float64(p.ProcessRunUs)
			}
		}
	}

	// Residual enforcement: __system__ := total - sum(non-system workloads)
	var sumNonSys agg
	for wk, a := range byWK {
		if wk.IsZero() || wk.IsSystem() {
			continue
		}
		if a == nil {
			continue
		}
		sumNonSys.instr += a.instr
		sumNonSys.cycles += a.cycles
		sumNonSys.misses += a.misses
		sumNonSys.pchit += a.pchit
		sumNonSys.irqTX += a.irqTX
		sumNonSys.irqRX += a.irqRX
		sumNonSys.irqBlk += a.irqBlk
		sumNonSys.runUs += a.runUs
	}

	clamp0 := func(x float64) float64 {
		if x < 0 {
			return 0
		}
		return x
	}

	sysAgg := byWK[sys]
	if sysAgg == nil {
		sysAgg = &agg{}
		byWK[sys] = sysAgg
	}
	sysAgg.instr = clamp0(tot.instr - sumNonSys.instr)
	sysAgg.cycles = clamp0(tot.cycles - sumNonSys.cycles)
	sysAgg.misses = clamp0(tot.misses - sumNonSys.misses)
	sysAgg.pchit = clamp0(tot.pchit - sumNonSys.pchit)
	sysAgg.irqTX = clamp0(tot.irqTX - sumNonSys.irqTX)
	sysAgg.irqRX = clamp0(tot.irqRX - sumNonSys.irqRX)
	sysAgg.irqBlk = clamp0(tot.irqBlk - sumNonSys.irqBlk)
	sysAgg.runUs = clamp0(tot.runUs - sumNonSys.runUs)

	// -------------------------------------------------------------------------
	// Emit monotonic per-workload counters with TTL lifecycle/GC.
	// -------------------------------------------------------------------------
	ttlTicks := c.Mono.TicksForDurationCeil(2 * time.Minute)

	// Shared index for GC without iterating state.
	idxKey := analysis.Key("__attrib_bpf_workload_series_index", nil)
	var index []bpfWorkloadSeriesRef
	if v, ok := c.State.Get(idxKey); ok {
		if vv, ok2 := v.([]bpfWorkloadSeriesRef); ok2 {
			index = vv
		}
	}
	if index == nil {
		index = []bpfWorkloadSeriesRef{}
	}

	active := make(map[bpfWorkloadSeriesRef]struct{}, len(byWK))

	emitOne := func(id analysis.MetricID, unit string, wk attribution.WorkloadKey, delta float64) {
		// Monotonic accumulator stored as float64 to preserve fractional window-edge scaling.
		stateKey := analysis.Key("__attrib_workload_bpf_f64", analysis.Labels{
			"id":  string(id),
			"ns":  wk.Namespace,
			"pod": wk.Pod,
			"ctr": wk.Container,
		})

		var prev float64
		if v, ok := c.State.Get(stateKey); ok {
			if f, ok2 := v.(float64); ok2 {
				prev = f
			}
		}

		next := prev + delta
		if next < prev {
			// Defensive: should not happen, but never regress.
			next = prev
		}
		c.State.Set(stateKey, next)

		lbl := analysis.Labels{
			"source":    bpfSource,
			"namespace": wk.Namespace,
			"pod":       wk.Pod,
			"container": wk.Container,
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(id, lbl),
			Window: c.Window,
			Unit:   unit,
			Value:  math.Floor(next),
		})
	}

	// Emit all workloads observed in this window (including __system__).
	for wk, a := range byWK {
		if wk.IsZero() {
			wk = sys
		}
		if a == nil {
			a = &agg{}
			byWK[wk] = a
		}

		ref := bpfWorkloadSeriesRef{Namespace: wk.Namespace, Pod: wk.Pod, Container: wk.Container}
		active[ref] = struct{}{}

		// Add to index if missing.
		found := false
		for i := range index {
			if index[i] == ref {
				found = true
				break
			}
		}
		if !found {
			index = append(index, ref)
		}

		// Update last-seen (shared across all BPF workload counters).
		lastSeenKey := analysis.Key("__attrib_workload_bpf_last_seen_u64", analysis.Labels{
			"ns":  wk.Namespace,
			"pod": wk.Pod,
			"ctr": wk.Container,
		})
		c.State.Set(lastSeenKey, nowMono)

		// Emit per-metric totals for this workload.
		emitOne(MetricWorkloadBpfCPUInstructionsTotal, "count", wk, a.instr)
		emitOne(MetricWorkloadBpfCPUCyclesTotal, "count", wk, a.cycles)
		emitOne(MetricWorkloadBpfCacheMissesTotal, "count", wk, a.misses)
		emitOne(MetricWorkloadBpfPageCacheHitsTotal, "count", wk, a.pchit)
		emitOne(MetricWorkloadBpfIRQNetTXTotal, "count", wk, a.irqTX)
		emitOne(MetricWorkloadBpfIRQNetRXTotal, "count", wk, a.irqRX)
		emitOne(MetricWorkloadBpfIRQBlockTotal, "count", wk, a.irqBlk)
		emitOne(MetricWorkloadBpfProcessRunUsTotal, "us", wk, a.runUs)
	}

	// -------------------------------------------------------------------------
	// GC stale workload series (never delete __system__).
	// -------------------------------------------------------------------------
	if ttlTicks > 0 && nowMono > 0 {
		nextIndex := make([]bpfWorkloadSeriesRef, 0, len(index))
		for _, ref := range index {
			// never delete __system__
			if ref.Namespace == attribution.SystemNamespace &&
				ref.Pod == attribution.SystemPod &&
				ref.Container == attribution.SystemContainer {
				nextIndex = append(nextIndex, ref)
				continue
			}

			if _, ok := active[ref]; ok {
				nextIndex = append(nextIndex, ref)
				continue
			}

			lastSeenKey := analysis.Key("__attrib_workload_bpf_last_seen_u64", analysis.Labels{
				"ns":  ref.Namespace,
				"pod": ref.Pod,
				"ctr": ref.Container,
			})
			v, ok := c.State.Get(lastSeenKey)
			if !ok {
				nextIndex = append(nextIndex, ref)
				continue
			}
			last, ok := v.(uint64)
			if !ok {
				nextIndex = append(nextIndex, ref)
				continue
			}

			if nowMono > last && (nowMono-last) > ttlTicks {
				// Delete all emitted workload BPF series for this workload.
				lbl := analysis.Labels{
					"source":    bpfSource,
					"namespace": ref.Namespace,
					"pod":       ref.Pod,
					"container": ref.Container,
				}
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfCPUInstructionsTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfCPUCyclesTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfCacheMissesTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfPageCacheHitsTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfIRQNetTXTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfIRQNetRXTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfIRQBlockTotal, lbl))
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadBpfProcessRunUsTotal, lbl))

				// Delete state accumulators.
				deleteAcc := func(id analysis.MetricID) {
					accKey := analysis.Key("__attrib_workload_bpf_f64", analysis.Labels{
						"id":  string(id),
						"ns":  ref.Namespace,
						"pod": ref.Pod,
						"ctr": ref.Container,
					})
					c.State.Delete(accKey)
				}
				deleteAcc(MetricWorkloadBpfCPUInstructionsTotal)
				deleteAcc(MetricWorkloadBpfCPUCyclesTotal)
				deleteAcc(MetricWorkloadBpfCacheMissesTotal)
				deleteAcc(MetricWorkloadBpfPageCacheHitsTotal)
				deleteAcc(MetricWorkloadBpfIRQNetTXTotal)
				deleteAcc(MetricWorkloadBpfIRQNetRXTotal)
				deleteAcc(MetricWorkloadBpfIRQBlockTotal)
				deleteAcc(MetricWorkloadBpfProcessRunUsTotal)

				c.State.Delete(lastSeenKey)
				continue
			}

			nextIndex = append(nextIndex, ref)
		}
		index = nextIndex
	}

	c.State.Set(idxKey, index)
	return nil
}

// overlapFactor returns the fraction of the interval (prev, curr] that overlaps [winStart, winEnd].
// All values are in monotonic ticks. Returns 0 if no overlap or interval degenerate.
func overlapFactor(prev, curr, winStart, winEnd uint64) float64 {
	if curr <= prev {
		return 0
	}
	// interval (prev, curr] treated as [prev, curr] for overlap math in tick-space
	a0 := prev
	a1 := curr
	b0 := winStart
	b1 := winEnd

	// overlap of [a0,a1] with [b0,b1]
	start := a0
	if b0 > start {
		start = b0
	}
	end := a1
	if b1 < end {
		end = b1
	}
	if end <= start {
		return 0
	}
	olen := float64(end - start)
	ilen := float64(curr - prev)
	if ilen <= 0 {
		return 0
	}
	return olen / ilen
}
