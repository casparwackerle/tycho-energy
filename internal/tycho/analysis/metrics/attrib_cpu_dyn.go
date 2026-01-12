// file: internal/tycho/analysis/metrics/attrib_cpu_dyn.go
package analysismetrics

import (
	"fmt"
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/attribution"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

// Output metric family (Prometheus sink prefix "tycho" yields tycho_workload_energy_mj).
const (
	MetricWorkloadEnergyMJ analysis.MetricID = "workload_energy_mj"

	// Diag-only (low cardinality).
	MetricAttribBinsTotal      analysis.MetricID = "attrib_bins_total"
	MetricAttribWeightSum      analysis.MetricID = "attrib_weight_sum"
	MetricAttribSystemFraction analysis.MetricID = "attrib_system_fraction"

	// Optional diag counters (no workload labels).
	MetricAttribResolvedProcID analysis.MetricID = "attrib_resolved_procid_total"
	MetricAttribResolvedCgroup analysis.MetricID = "attrib_resolved_cgroup_total"
	MetricAttribFallbackSystem analysis.MetricID = "attrib_fallback_system_total"
)

const (
	attribKindDynamic = "dynamic"
	componentRaplPkg  = "rapl_pkg"
	componentRaplDram = "rapl_dram"
)

type wk = attribution.WorkloadKey

type attribBin struct {
	total float64
	w     map[wk]float64
}

type attribBins []attribBin

type attribSeriesRef struct {
	Component string
	Kind      string
	Namespace string
	Pod       string
	Container string
}

type CpuDynamicAttributionPerTick struct {
	// No config yet; keep slice minimal.
}

func NewCpuDynamicAttributionPerTick() *CpuDynamicAttributionPerTick {
	return &CpuDynamicAttributionPerTick{}
}

func (m *CpuDynamicAttributionPerTick) ID() analysis.MetricID { return "attrib_cpu_dyn_per_tick_v1" }

func (m *CpuDynamicAttributionPerTick) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Meta != nil && c.Bpf() != nil && c.Mono != nil
}

func (m *CpuDynamicAttributionPerTick) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Meta == nil || c.Bpf() == nil || c.Mono == nil {
		return nil
	}

	// -------------------------------------------------------------------------
	// Task 1: Define bins from BPF ticks (ticks are bin ends; bins are (prev, curr])
	// Use same delay + window-with-prev selection logic as bpf.go.
	// -------------------------------------------------------------------------
	delayTicks := c.Mono.TicksForMsCeil(config.BpfDelayMs())
	wEff := c.EffectiveWindowTicks(delayTicks)

	// Load ticks in [start,end] plus predecessor tick < start.
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		c.Bpf(),
		wEff.StartMono,
		wEff.EndMono,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)
	if len(ticks) < 2 {
		return nil
	}

	// -------------------------------------------------------------------------
	// Task 0: Refresh cgroup index once per window (explicit dependency).
	// Use only in-window ticks for the join (exclude the predecessor tick).
	// -------------------------------------------------------------------------
	nowWall := time.Now()
	inWindow := ticks[1:] // ticks[0] is predecessor; bins start at ticks[1]
	attribution.BuildCgroupIndexFromBpfWindow(c.Meta, nowWall, c.NowMono, inWindow)

	// -------------------------------------------------------------------------
	// Task 3: Obtain per-window dynamic energy budgets (authoritative).
	// We read the already-emitted cumulative dynamic energy and difference it here.
	// Domains: pkg and dram. Source label: raplSource. Kind: dynamic.
	// -------------------------------------------------------------------------
	pkgBudgetMJ, pkgBudgetOK := raplDynamicDeltaBudgetMJ(c, "pkg")
	dramBudgetMJ, dramBudgetOK := raplDynamicDeltaBudgetMJ(c, "dram")
	if !pkgBudgetOK && !dramBudgetOK {
		// Nothing to do if budgets missing this cycle.
		return nil
	}

	// -------------------------------------------------------------------------
	// Task 2: Build per-bin workload weight maps (pkg=instr, dram=miss or cycles).
	// Also gather window-level sums for DRAM fallback decision.
	// Apply conservative overlap scaling for first/last bin if window edges cut bins.
	// -------------------------------------------------------------------------
	sys := attribution.SystemWorkloadKey()

	binPkg := make(attribBins, 0, len(inWindow))
	binDram := make(attribBins, 0, len(inWindow))

	var sumMiss float64
	var sumCycles float64

	// Optional diag counters (kept per-run then accumulated as monotonic in state).
	var nProcID, nCgroup, nSystem uint64

	// Diagnostics-only: window-level weight aggregation + small PID samples for audit logs.
	diagEnabled := config.GetAttributionDiagnosticsEnabled()
	var wSumPkg float64
	var wSumDram float64
	var wSysPkg float64
	var wSysDram float64

	// Top-K audit helpers (kept low, logs only).
	wByWkPkg := map[wk]float64{}
	wByWkDram := map[wk]float64{}
	examplePIDs := map[wk][]uint32{} // cap to 3 PIDs per workload

	// Helper: overlap factor for bin (prevMono, currMono] intersected with [start,end].
	overlapFactor := func(prev, curr, start, end uint64) float64 {
		if curr <= prev {
			return 0
		}
		// bin interval (prev, curr]
		b0 := prev
		b1 := curr
		// window interval [start, end]
		w0 := start
		w1 := end
		// intersection length in ticks
		lo := b0
		if w0 > lo {
			lo = w0
		}
		hi := b1
		if w1 < hi {
			hi = w1
		}
		if hi <= lo {
			return 0
		}
		num := float64(hi - lo)
		den := float64(b1 - b0)
		if den <= 0 {
			return 0
		}
		f := num / den
		if f < 0 {
			return 0
		}
		if f > 1 {
			return 1
		}
		return f
	}

	// Iterate bins: for tick i (inWindow index k), predecessor is ticks[k] in the original slice.
	for k := 0; k < len(inWindow); k++ {
		curr := inWindow[k]
		prev := ticks[k] // aligns because inWindow = ticks[1:], so prev for inWindow[0] is ticks[0]
		f := overlapFactor(prev.Mono, curr.Mono, wEff.StartMono, wEff.EndMono)
		if f <= 0 {
			// Bin has no overlap with effective window.
			binPkg = append(binPkg, attribBin{total: 0, w: map[wk]float64{sys: 0}})
			binDram = append(binDram, attribBin{total: 0, w: map[wk]float64{sys: 0}})
			continue
		}

		mp := attribBin{w: make(map[wk]float64, 64)}
		md := attribBin{w: make(map[wk]float64, 64)}

		// Ensure system bucket exists if needed later.
		mp.w[sys] = 0
		md.w[sys] = 0

		for i := range curr.Procs {
			d := &curr.Procs[i]

			// Resolve ProcID readiness via Cycle metadata (StartJiffies required).
			procID, _, okProc := attribution.ResolveProcFromCycle(c, d.PID)
			procPtr := attribution.ProcIDPtr(procID, okProc)

			// Resolve workload via Decision B (ProcID preferred, else cgroup fallback).
			wkKey, okWk := attribution.ResolveWorkload(c.Meta, procPtr, d.CgroupID)

			if diagEnabled {
				// store a few example PIDs per workload (cap 3)
				if len(examplePIDs[wkKey]) < 3 {
					examplePIDs[wkKey] = append(examplePIDs[wkKey], uint32(d.PID))
				}
			}

			if !okWk {
				// System fallback.
				wkKey = sys
				nSystem++
			} else {
				// Count which path likely resolved it (diag-only).
				if procPtr != nil {
					nProcID++
				} else if d.CgroupID != 0 {
					nCgroup++
				}
			}

			// pkg weight: CPUInstr
			instr := float64(d.CPUInstr) * f
			if instr > 0 {
				mp.total += instr
				mp.w[wkKey] += instr
			}

			if diagEnabled && instr > 0 {
				wSumPkg += instr
				wByWkPkg[wkKey] += instr
				if wkKey.IsSystem() {
					wSysPkg += instr
				}
			}

			// dram base weights: CacheMiss (fallback later decided window-level)
			miss := float64(d.CacheMiss) * f
			cyc := float64(d.CPUCycles) * f
			if miss > 0 {
				sumMiss += miss
				md.total += miss
				md.w[wkKey] += miss
			}
			if diagEnabled && miss > 0 {
				wSumDram += miss
				wByWkDram[wkKey] += miss
				if wkKey.IsSystem() {
					wSysDram += miss
				}
			}

			if cyc > 0 {
				sumCycles += cyc
			}
		}

		binPkg = append(binPkg, mp)
		binDram = append(binDram, md)
	}

	// DRAM fallback rule (window-level): if miss sum is 0 but cycles > 0, use cycles per bin.
	useCyclesForDram := (sumMiss == 0 && sumCycles > 0)
	if useCyclesForDram {
		// Rebuild DRAM bin maps using CPUCycles as weight (same mapping).
		binDram = binDram[:0]

		if diagEnabled {
			// Replace DRAM diag aggregates with cycle-based values.
			wSumDram = 0
			wSysDram = 0
			wByWkDram = map[wk]float64{}
			// examplePIDs map remains valid; mapping decisions are the same.
		}

		for k := 0; k < len(inWindow); k++ {
			curr := inWindow[k]
			prev := ticks[k]
			f := overlapFactor(prev.Mono, curr.Mono, wEff.StartMono, wEff.EndMono)
			if f <= 0 {
				binDram = append(binDram, attribBin{total: 0, w: map[wk]float64{sys: 0}})
				continue
			}

			md := attribBin{w: make(map[wk]float64, 64)}
			md.w[sys] = 0

			for i := range curr.Procs {
				d := &curr.Procs[i]
				procID, _, okProc := attribution.ResolveProcFromCycle(c, d.PID)
				procPtr := attribution.ProcIDPtr(procID, okProc)
				wkKey, okWk := attribution.ResolveWorkload(c.Meta, procPtr, d.CgroupID)
				if !okWk {
					wkKey = sys
				}
				cyc := float64(d.CPUCycles) * f
				if cyc > 0 {
					md.total += cyc
					md.w[wkKey] += cyc
				}

				if diagEnabled && cyc > 0 {
					wSumDram += cyc
					wByWkDram[wkKey] += cyc
					if wkKey.IsSystem() {
						wSysDram += cyc
					}
				}
			}

			binDram = append(binDram, md)
		}
	}

	// -------------------------------------------------------------------------
	// Task 4: Allocate budgets across bins, then workloads (two-stage).
	// Remainders always routed to system.
	// -------------------------------------------------------------------------
	// Emit diag: bins count (from inWindow length; bins correspond to intervals).
	if diagEnabled {
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricAttribBinsTotal, nil),
			Window: c.Window,
			Unit:   "count",
			Value:  float64(len(inWindow)),
		})
	}

	// Allocate pkg.
	pkgSystemMJ := allocateComponent(
		c,
		componentRaplPkg,
		pkgBudgetMJ,
		binPkg,
	)
	// Allocate dram.
	dramSystemMJ := allocateComponent(
		c,
		componentRaplDram,
		dramBudgetMJ,
		binDram,
	)

	// -------------------------------------------------------------------------
	// Task 5: Export workload dynamic energy counters.
	// Done inside allocateComponent via StateStore-backed counters + Sink.Emit.
	// Ensure system bucket always emitted (allocateComponent guarantees).
	// -------------------------------------------------------------------------

	if diagEnabled {
		// Diagnostics: weight sums + system fraction.
		emitWeightDiag(c, componentRaplPkg, binPkg, pkgBudgetMJ, pkgSystemMJ)
		emitWeightDiag(c, componentRaplDram, binDram, dramBudgetMJ, dramSystemMJ)

		// Optional diag counters: accumulate as monotonic counters in state and emit without workload labels.
		emitDiagCounter(c, MetricAttribResolvedProcID, "count", nProcID)
		emitDiagCounter(c, MetricAttribResolvedCgroup, "count", nCgroup)
		emitDiagCounter(c, MetricAttribFallbackSystem, "count", nSystem)
	}
	if diagEnabled {
		logAttributionAudit(
			c,
			componentRaplPkg, componentRaplDram,
			pkgBudgetMJ, dramBudgetMJ,
			wSumPkg, wSumDram,
			wSysPkg, wSysDram,
			wByWkPkg, wByWkDram,
			examplePIDs,
			nProcID, nCgroup, nSystem,
		)
	}

	return nil
}

// raplDynamicDeltaBudgetMJ returns the per-window dynamic delta (mJ) for the given RAPL domain
// by differencing the already-exported cumulative dynamic energy series from PointStore.
func raplDynamicDeltaBudgetMJ(c *analysis.Cycle, domain string) (uint64, bool) {
	if c == nil || c.Store == nil || c.State == nil {
		return 0, false
	}
	key := analysis.Key(MetricRaplEnergyMJ, analysis.Labels{
		"domain": domain,
		"kind":   "dynamic",
		"source": raplSource,
	})
	curr, ok := getPointValueOk(c.Store, key)
	if !ok || curr < 0 {
		return 0, false
	}

	lastKey := stateKeyLastSeen("attrib13d", domain, "rapl_dyn_cum_mj")
	prev, prevOK := stateGetF64(c.State, lastKey)
	stateSetF64(c.State, lastKey, curr)
	if !prevOK {
		// First observation: initialize and skip emitting budgets.
		return 0, false
	}

	d := curr - prev
	if d < 0 {
		d = 0
	}
	return uint64(d + 0.5), true
}

// allocateComponent performs the two-stage allocation for one component budget.
// It guarantees conservation exactly in integer mJ:
//
//	sum_alloc == budgetMJ, with all remainder routed to __system__.
//
// It updates monotonic workload counters in state and emits tycho_workload_energy_mj points
// ONCE per workload per cycle (not per bin).
//
// Returns the system-allocated amount (mJ) for system fraction diagnostics.
func allocateComponent(
	c *analysis.Cycle,
	component string,
	budgetMJ uint64,
	bins attribBins,
) uint64 {
	if c == nil || c.Sink == nil || c.State == nil {
		return 0
	}

	sys := attribution.SystemWorkloadKey()

	// Always emit system series even if budget is 0.
	if budgetMJ == 0 {
		applyWorkloadAllocs(c, component, map[attribution.WorkloadKey]uint64{sys: 0})
		return 0
	}

	// Window total weight.
	sumW := 0.0
	for i := range bins {
		sumW += bins[i].total
	}

	// If no weight, allocate full budget to system.
	if sumW <= 0 {
		applyWorkloadAllocs(c, component, map[attribution.WorkloadKey]uint64{sys: budgetMJ})
		return budgetMJ
	}

	// Window accumulator: per-workload allocations across all bins.
	windowAlloc := make(map[attribution.WorkloadKey]uint64, 128)

	// Stage 1: allocate window budget to bins (integer floors), remainder to system at end.
	var binsAllocated uint64

	for i := range bins {
		bt := bins[i].total
		if bt <= 0 {
			continue
		}

		// Floor share, do NOT round. Remainder will go to system.
		binMJ := uint64(float64(budgetMJ) * (bt / sumW))
		if binMJ == 0 {
			continue
		}

		binsAllocated += binMJ

		// Stage 2: allocate this binMJ to workloads (integer floors), remainder to system.
		var binAllocated uint64

		// Ensure system exists as sink.
		if _, ok := bins[i].w[sys]; !ok {
			bins[i].w[sys] = 0
		}

		// Allocate per workload.
		for wk, ww := range bins[i].w {
			if ww <= 0 {
				continue
			}
			shareMJ := uint64(float64(binMJ) * (ww / bt)) // floor
			if shareMJ == 0 {
				continue
			}
			if wk.IsZero() {
				wk = sys
			}
			windowAlloc[wk] += shareMJ
			binAllocated += shareMJ
		}

		// Bin remainder -> system
		if binAllocated < binMJ {
			windowAlloc[sys] += (binMJ - binAllocated)
		}
	}

	// Window remainder -> system
	if binsAllocated < budgetMJ {
		windowAlloc[sys] += (budgetMJ - binsAllocated)
	}

	// Apply + emit once per workload
	systemAdded := windowAlloc[sys]
	applyWorkloadAllocs(c, component, windowAlloc)
	return systemAdded
}

// applyWorkloadAllocs increments monotonic counters in state and emits them once per workload.
// alloc is the per-window delta allocation in mJ.
func applyWorkloadAllocs(
	c *analysis.Cycle,
	component string,
	alloc map[attribution.WorkloadKey]uint64,
) {
	if c == nil || c.Sink == nil || c.State == nil || c.Mono == nil {
		return
	}

	sys := attribution.SystemWorkloadKey()

	// Ensure system always present.
	if _, ok := alloc[sys]; !ok {
		alloc[sys] = 0
	}

	now := c.NowMono
	if now == 0 {
		now = c.Window.EndMono
	}

	// TTL: start conservative, tune later.
	ttlTicks := c.Mono.TicksForDurationCeil(2 * time.Minute)

	// Load / init index.
	idxKey := analysis.Key("__attrib_series_index", analysis.Labels{
		"component": component,
		"kind":      attribKindDynamic,
	})
	var index []attribSeriesRef
	if v, ok := c.State.Get(idxKey); ok {
		if vv, ok2 := v.([]attribSeriesRef); ok2 {
			index = vv
		}
	}
	if index == nil {
		index = []attribSeriesRef{}
	}

	// Track active series this cycle + ensure index contains them.
	active := make(map[attribSeriesRef]struct{}, len(alloc))

	for wk, deltaMJ := range alloc {
		if wk.IsZero() {
			wk = sys
		}

		ref := attribSeriesRef{
			Component: component,
			Kind:      attribKindDynamic,
			Namespace: wk.Namespace,
			Pod:       wk.Pod,
			Container: wk.Container,
		}
		active[ref] = struct{}{}

		// Add to index if missing (linear scan is OK at your scale; can optimize later).
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

		// Monotonic accumulator in state (per workload + component).
		stateKey := analysis.Key("__attrib_workload_energy_u64", analysis.Labels{
			"component": component,
			"kind":      attribKindDynamic,
			"ns":        wk.Namespace,
			"pod":       wk.Pod,
			"ctr":       wk.Container,
		})

		var prev uint64
		if prevAny, ok := c.State.Get(stateKey); ok {
			if u, ok2 := prevAny.(uint64); ok2 {
				prev = u
			}
		}
		next := prev + deltaMJ
		c.State.Set(stateKey, next)

		// Update last-seen for this series.
		lastSeenKey := analysis.Key("__attrib_last_seen_u64", analysis.Labels{
			"component": component,
			"kind":      attribKindDynamic,
			"ns":        wk.Namespace,
			"pod":       wk.Pod,
			"ctr":       wk.Container,
		})
		c.State.Set(lastSeenKey, now)

		// Emit user-facing series.
		labels := analysis.Labels{
			"component": component,
			"kind":      attribKindDynamic,
			"namespace": wk.Namespace,
			"pod":       wk.Pod,
			"container": wk.Container,
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricWorkloadEnergyMJ, labels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  float64(next),
		})
	}

	// GC pass: remove stale series (not active, lastSeen too old).
	if ttlTicks > 0 {
		nextIndex := make([]attribSeriesRef, 0, len(index))
		for _, ref := range index {
			if _, ok := active[ref]; ok {
				nextIndex = append(nextIndex, ref)
				continue
			}

			lastSeenKey := analysis.Key("__attrib_last_seen_u64", analysis.Labels{
				"component": ref.Component,
				"kind":      ref.Kind,
				"ns":        ref.Namespace,
				"pod":       ref.Pod,
				"ctr":       ref.Container,
			})
			v, ok := c.State.Get(lastSeenKey)
			if !ok {
				// No timestamp: keep for now (conservative).
				nextIndex = append(nextIndex, ref)
				continue
			}
			last, ok := v.(uint64)
			if !ok {
				nextIndex = append(nextIndex, ref)
				continue
			}
			if now > last && (now-last) > ttlTicks {
				// Delete user-facing series.
				lbl := analysis.Labels{
					"component": ref.Component,
					"kind":      ref.Kind,
					"namespace": ref.Namespace,
					"pod":       ref.Pod,
					"container": ref.Container,
				}
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadEnergyMJ, lbl))

				// Delete internal state.
				energyKey := analysis.Key("__attrib_workload_energy_u64", analysis.Labels{
					"component": ref.Component,
					"kind":      ref.Kind,
					"ns":        ref.Namespace,
					"pod":       ref.Pod,
					"ctr":       ref.Container,
				})
				c.State.Delete(energyKey)
				c.State.Delete(lastSeenKey)

				// Do not keep in index.
				continue
			}

			// Not stale yet.
			nextIndex = append(nextIndex, ref)
		}
		index = nextIndex
	}

	// Persist updated index.
	c.State.Set(idxKey, index)
}

func emitWeightDiag(
	c *analysis.Cycle,
	component string,
	bins attribBins,
	budgetMJ uint64,
	systemAllocMJ uint64,
) {
	if c == nil || c.Sink == nil {
		return
	}
	if !config.GetAttributionDiagnosticsEnabled() {
		return
	}

	sumW := 0.0
	for i := range bins {
		sumW += bins[i].total
	}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricAttribWeightSum, analysis.Labels{"component": component}),
		Window: c.Window,
		Unit:   "count",
		Value:  sumW,
	})

	frac := 0.0
	if budgetMJ > 0 {
		frac = float64(systemAllocMJ) / float64(budgetMJ)
	}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricAttribSystemFraction, analysis.Labels{"component": component}),
		Window: c.Window,
		Unit:   "ratio",
		Value:  clamp01(frac),
	})
}

func emitDiagCounter(c *analysis.Cycle, id analysis.MetricID, unit string, delta uint64) {
	if c == nil || c.Sink == nil || c.State == nil {
		return
	}
	if !config.GetAttributionDiagnosticsEnabled() {
		return
	}

	key := analysis.Key("__attrib_diag_u64", analysis.Labels{"id": string(id)})
	prevAny, ok := c.State.Get(key)
	var prev uint64
	if ok {
		if u, ok2 := prevAny.(uint64); ok2 {
			prev = u
		}
	}
	next := prev + delta
	c.State.Set(key, next)

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(id, nil),
		Window: c.Window,
		Unit:   unit,
		Value:  float64(next),
	})
}

// --- churn control ------------------------------------------------------------

// We maintain a small index of known workload series keys in StateStore so we can GC
// without requiring StateStore iteration.
func indexKey(component string) analysis.MetricKey {
	return analysis.Key("__attrib_workload_index", analysis.Labels{
		"component": component,
		"kind":      attribKindDynamic,
	})
}

func gcAttributionWorkloads(
	c *analysis.Cycle,
	component string,
	ttlTicks uint64,
	now uint64,
	activeKeys map[string]struct{},
) {
	if c == nil || c.State == nil || c.Sink == nil {
		return
	}
	if ttlTicks == 0 {
		return
	}

	// Load index: []string of canonical state keys for "__attrib_workload_energy_u64"
	idxK := indexKey(component)
	var idx []string
	if v, ok := c.State.Get(idxK); ok {
		if vv, ok2 := v.([]string); ok2 {
			// shallow copy to avoid accidental mutation
			idx = append([]string(nil), vv...)
		}
	}
	if idx == nil {
		idx = []string{}
	}

	// Rebuild index in-place to drop stale entries.
	nextIdx := make([]string, 0, len(idx))

	for _, canon := range idx {
		// Keep if it is active this cycle (avoid flapping deletes)
		if _, ok := activeKeys[canon]; ok {
			nextIdx = append(nextIdx, canon)
			continue
		}

		// Parse back labels: we can’t reliably parse CanonicalString, so we also store lastSeen
		// under the same dimensions and keep a parallel key in the index.
		//
		// Simpler approach: index stores the LAST-SEEN key canonical string, not energy key.
		// To avoid breaking your current deployment, we implement a dual-mode:
		// - If canon starts with "__attrib_workload_last_seen_u64", treat as lastSeen key.
		// - Else, treat as energy key and derive lastSeen key by swapping MetricID.
		isLastSeen := false
		if len(canon) >= len("__attrib_workload_last_seen_u64") && canon[:len("__attrib_workload_last_seen_u64")] == "__attrib_workload_last_seen_u64" {
			isLastSeen = true
		}

		var lastSeenKey analysis.MetricKey
		var energyKey analysis.MetricKey

		if isLastSeen {
			// This case requires us to reconstitute MetricKey, which we can’t from canon.
			// Therefore: we do not support this mode without changing index format.
			// Keep entry to avoid accidental deletions.
			nextIdx = append(nextIdx, canon)
			continue
		}

		// We can’t reconstruct labels from canon either, so we must store labels explicitly.
		// => To keep this drop-in minimal and safe, we implement the index in a reliable way:
		// store structured entries instead of strings.

		nextIdx = append(nextIdx, canon)
		_ = lastSeenKey
		_ = energyKey
	}

	// Write back unchanged for now.
	// (See below: the robust index implementation replacement.)
	c.State.Set(idxK, nextIdx)
}

// --- diagnostics-only audit logging (no Prometheus series) --------------------

type wkWeight struct {
	wk wk
	v  float64
}

func topKWeights(m map[wk]float64, k int) []wkWeight {
	if k <= 0 {
		return nil
	}
	out := make([]wkWeight, 0, len(m))
	for kk, vv := range m {
		if vv <= 0 {
			continue
		}
		out = append(out, wkWeight{wk: kk, v: vv})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].v > out[j].v })
	if len(out) > k {
		out = out[:k]
	}
	return out
}

func wkString(wk wk) string {
	if wk.IsSystem() || (wk.Namespace == "" && wk.Pod == "" && wk.Container == "") {
		return "__system__"
	}
	return wk.Namespace + "/" + wk.Pod + "/" + wk.Container
}

func logAttributionAudit(
	c *analysis.Cycle,
	compPkg string,
	compDram string,
	budgetPkgMJ uint64,
	budgetDramMJ uint64,
	sumWPkg float64,
	sumWDram float64,
	sysWPkg float64,
	sysWDram float64,
	wByWkPkg map[wk]float64,
	wByWkDram map[wk]float64,
	examplePIDs map[wk][]uint32,
	nProcID, nCgroup, nSystem uint64,
) {
	if c == nil {
		return
	}

	sysFracPkg := 0.0
	if sumWPkg > 0 {
		sysFracPkg = sysWPkg / sumWPkg
	}
	sysFracDram := 0.0
	if sumWDram > 0 {
		sysFracDram = sysWDram / sumWDram
	}

	topPkg := topKWeights(wByWkPkg, 5)
	topDram := topKWeights(wByWkDram, 5)

	formatTop := func(xs []wkWeight) string {
		if len(xs) == 0 {
			return "[]"
		}
		s := "["
		for i := range xs {
			if i > 0 {
				s += " "
			}
			wk := xs[i].wk
			pids := examplePIDs[wk]
			s += fmt.Sprintf("%s:%.1f", wkString(wk), xs[i].v)
			if len(pids) > 0 {
				s += ":pids="
				for j := range pids {
					if j > 0 {
						s += ","
					}
					s += fmt.Sprintf("%d", pids[j])
				}
			}
		}
		s += "]"
		return s
	}

	klog.Infof(
		"[attrib/audit] win=%s budgets_mj{pkg=%d dram=%d} weights{sum_pkg=%.1f sys_frac_pkg=%.3f sum_dram=%.1f sys_frac_dram=%.3f} resolved{procid=%d cgroup=%d system=%d} top_pkg=%s top_dram=%s",
		c.Window.String(),
		budgetPkgMJ, budgetDramMJ,
		sumWPkg, sysFracPkg,
		sumWDram, sysFracDram,
		nProcID, nCgroup, nSystem,
		formatTop(topPkg),
		formatTop(topDram),
	)
}
