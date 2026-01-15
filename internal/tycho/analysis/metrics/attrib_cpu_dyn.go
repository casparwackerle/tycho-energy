// file: internal/tycho/analysis/metrics/attrib_cpu_dyn.go
package analysismetrics

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/attribution"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

// Output metric family (Prometheus sink prefix "tycho" yields tycho_*).
const (
	// NEW (schema-aligned with tycho_rapl_energy_mj):
	// tycho_workload_rapl_energy_mj{domain,kind,source,namespace,pod,container}
	MetricWorkloadRaplEnergyMJ analysis.MetricID = "workload_rapl_energy_mj"

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

	raplDomainPkg    = "pkg"
	raplDomainCore   = "core"
	raplDomainUncore = "uncore"
	raplDomainDram   = "dram"
)

type wk = attribution.WorkloadKey

type attribBin struct {
	total float64
	w     map[wk]float64
}

type attribBins []attribBin

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

// Drop-in replacement for (*CpuDynamicAttributionPerTick).Run
// Assumptions:
// - tick.Procs[*].CPUInstr/CPUCycles/CacheMiss are MONOTONIC COUNTERS (Tycho lifetime)
// - We reconstruct per-tick deltas by differencing against the predecessor tick (PID-keyed).
// - No overlap/partial-bin scaling. Bins are simply consecutive tick intervals.
// - Budgets are already emitted as cumulative dynamic energy; we difference them here.
// NOTE: Requires allocateComponent to be the "single rounding" version (largest remainder) provided earlier.

// Drop-in replacement for (*CpuDynamicAttributionPerTick).Run
func (m *CpuDynamicAttributionPerTick) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Meta == nil || c.Bpf() == nil || c.Mono == nil {
		return nil
	}

	// -------------------------------------------------------------------------
	// Define bins from BPF ticks (ticks are bin ends; bins are (prev, curr])
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
	inWindow := ticks[1:]

	// -------------------------------------------------------------------------
	// Refresh cgroup index once per cycle.
	// Keep this call, but make sure the underlying implementation is PID-reuse safe
	// (see replacement in section B below).
	// -------------------------------------------------------------------------
	nowWall := time.Now()
	attribution.BuildCgroupIndexFromBpfWindow(c.Meta, nowWall, c.NowMono, inWindow)

	// -------------------------------------------------------------------------
	// Obtain per-window dynamic energy budgets (authoritative).
	// -------------------------------------------------------------------------
	pkgBudgetMJ, pkgBudgetOK := raplDynamicDeltaBudgetMJ(c, "pkg")
	coreBudgetMJ, coreBudgetOK := raplDynamicDeltaBudgetMJ(c, "core")
	uncoreBudgetMJ, uncoreBudgetOK := raplDynamicDeltaBudgetMJ(c, "uncore")
	dramBudgetMJ, dramBudgetOK := raplDynamicDeltaBudgetMJ(c, "dram")

	// If none of the dynamic RAPL series exist / are available, nothing to do.
	// (When some domains are missing, budgets stay 0 and we still emit 0 for __system__.)
	if !pkgBudgetOK && !coreBudgetOK && !uncoreBudgetOK && !dramBudgetOK {
		return nil
	}
	// -------------------------------------------------------------------------
	// State-based PMU baseline (NOT prev-tick-only).
	// This prevents weight loss for first-seen/churning PIDs and handles PID reuse safely
	// when ProcID (PID+StartJiffies) is available.
	// -------------------------------------------------------------------------
	sys := attribution.SystemWorkloadKey()

	baselines := getOrInitPmuBaselines(c)
	nowMono := c.NowMono
	if nowMono == 0 {
		nowMono = c.Window.EndMono
	}
	ttlTicks := c.Mono.TicksForDurationCeil(2 * time.Minute)

	// -------------------------------------------------------------------------
	// Build per-bin workload weight maps using deltas vs state baselines.
	// pkg weight: CPUInstr delta
	// dram weight: CacheMiss delta, with fallback to CPUCycles delta if miss mass is zero.
	// -------------------------------------------------------------------------
	binPkg := make(attribBins, 0, len(inWindow))
	binDram := make(attribBins, 0, len(inWindow))

	var sumMiss float64
	var sumCycles float64

	// Optional diag counters (per-run, later accumulated as monotonic in state).
	var nProcID, nCgroup, nSystem uint64

	diagEnabled := config.GetAttributionDiagnosticsEnabled()

	var wSumPkg float64
	var wSumDram float64
	var wSysPkg float64
	var wSysDram float64
	wByWkPkg := map[wk]float64{}
	wByWkDram := map[wk]float64{}
	examplePIDs := map[wk][]uint32{} // cap to 3 PIDs per workload

	for k := 0; k < len(inWindow); k++ {
		curr := inWindow[k]

		mp := attribBin{w: make(map[wk]float64, 64)}
		md := attribBin{w: make(map[wk]float64, 64)}
		mp.w[sys] = 0
		md.w[sys] = 0

		for i := range curr.Procs {
			d := &curr.Procs[i]

			// Resolve ProcID (PID+StartJiffies) if available. This is the key to PID-reuse safety.
			procID, _, okProc := attribution.ResolveProcFromCycle(c, d.PID)
			procPtr := attribution.ProcIDPtr(procID, okProc)

			key := pmuBaselineKey(d.PID, procID, okProc)

			// Load baseline. If first time seen, init baseline and skip crediting this tick.
			prev, ok := baselines[key]
			if !ok {
				baselines[key] = pmuBaseline{
					Instr:    d.CPUInstr,
					Cycles:   d.CPUCycles,
					Misses:   d.CacheMiss,
					LastSeen: nowMono,
				}
				continue
			}

			// Compute deltas with underflow protection. Underflow => treat as reset/PID reuse; rebase.
			var dInstr, dCyc, dMiss uint64
			underflow := false

			if d.CPUInstr >= prev.Instr {
				dInstr = d.CPUInstr - prev.Instr
			} else {
				underflow = true
			}
			if d.CPUCycles >= prev.Cycles {
				dCyc = d.CPUCycles - prev.Cycles
			} else {
				underflow = true
			}
			if d.CacheMiss >= prev.Misses {
				dMiss = d.CacheMiss - prev.Misses
			} else {
				underflow = true
			}

			// Update baseline to current regardless.
			baselines[key] = pmuBaseline{
				Instr:    d.CPUInstr,
				Cycles:   d.CPUCycles,
				Misses:   d.CacheMiss,
				LastSeen: nowMono,
			}

			// If reset/reuse detected, do not credit this tick (avoid biasing allocation elsewhere).
			if underflow {
				continue
			}

			// If no activity, skip.
			if dInstr == 0 && dCyc == 0 && dMiss == 0 {
				continue
			}

			// Resolve workload (ProcID preferred, else cgroup fallback).
			wkKey, okWk := attribution.ResolveWorkload(c.Meta, procPtr, d.CgroupID)
			if !okWk {
				wkKey = sys
				nSystem++
			} else {
				if procPtr != nil {
					nProcID++
				} else if d.CgroupID != 0 {
					nCgroup++
				}
			}

			if diagEnabled {
				if len(examplePIDs[wkKey]) < 3 {
					examplePIDs[wkKey] = append(examplePIDs[wkKey], uint32(d.PID))
				}
			}

			// pkg weight: CPUInstr delta
			if dInstr > 0 {
				instr := float64(dInstr)
				mp.total += instr
				mp.w[wkKey] += instr

				if diagEnabled {
					wSumPkg += instr
					wByWkPkg[wkKey] += instr
					if wkKey.IsSystem() {
						wSysPkg += instr
					}
				}
			}

			// dram base weight: CacheMiss delta
			if dMiss > 0 {
				miss := float64(dMiss)
				sumMiss += miss
				md.total += miss
				md.w[wkKey] += miss

				if diagEnabled {
					wSumDram += miss
					wByWkDram[wkKey] += miss
					if wkKey.IsSystem() {
						wSysDram += miss
					}
				}
			}

			// cycles tracked for fallback decision
			if dCyc > 0 {
				sumCycles += float64(dCyc)
			}
		}

		binPkg = append(binPkg, mp)
		binDram = append(binDram, md)
	}

	// Persist updated baselines + GC old entries.
	gcPmuBaselines(baselines, nowMono, ttlTicks)
	setPmuBaselines(c, baselines)

	// DRAM fallback (window-level): if miss mass is zero but cycles > 0, rebuild DRAM bins using cycles deltas.
	// With state-based baselines, "rebuild" is unnecessary. Instead, we just interpret md as misses, and if sumMiss==0,
	// we should have built md from cycles in the first place. So: rebuild using cycles deltas by re-walking ticks again
	// is expensive and redundant.
	//
	// Minimal safe approach: keep your existing rebuild logic, but it would need to re-compute deltas from baselines too.
	// To keep this drop-in small: we do NOT rebuild here. We instead choose the DRAM bins to be cycles-based from the start
	// only when misses appear to be absent. That requires a second pass over the same data.
	//
	// For now, keep your window-level fallback behavior, but implement it without prev-tick maps:
	useCyclesForDram := (sumMiss == 0 && sumCycles > 0)
	if useCyclesForDram {
		// Recompute binDram using cycles deltas only, reusing the baselines already updated above is not correct
		// (it would produce zero). Therefore, we do a second pass using a *separate* baseline map for cycles-only.
		cycBaselines := getOrInitPmuCycleBaselines(c)
		binDram = binDram[:0]

		if diagEnabled {
			wSumDram = 0
			wSysDram = 0
			wByWkDram = map[wk]float64{}
		}

		for k := 0; k < len(inWindow); k++ {
			curr := inWindow[k]

			md := attribBin{w: make(map[wk]float64, 64)}
			md.w[sys] = 0

			for i := range curr.Procs {
				d := &curr.Procs[i]

				procID, _, okProc := attribution.ResolveProcFromCycle(c, d.PID)
				procPtr := attribution.ProcIDPtr(procID, okProc)
				key := pmuBaselineKey(d.PID, procID, okProc)

				prev, ok := cycBaselines[key]
				if !ok {
					cycBaselines[key] = pmuCycleBaseline{Cycles: d.CPUCycles, LastSeen: nowMono}
					continue
				}

				if d.CPUCycles < prev.Cycles {
					// reset/reuse
					cycBaselines[key] = pmuCycleBaseline{Cycles: d.CPUCycles, LastSeen: nowMono}
					continue
				}

				dCyc := d.CPUCycles - prev.Cycles
				cycBaselines[key] = pmuCycleBaseline{Cycles: d.CPUCycles, LastSeen: nowMono}

				if dCyc == 0 {
					continue
				}

				wkKey, okWk := attribution.ResolveWorkload(c.Meta, procPtr, d.CgroupID)
				if !okWk {
					wkKey = sys
				}

				cyc := float64(dCyc)
				md.total += cyc
				md.w[wkKey] += cyc

				if diagEnabled {
					wSumDram += cyc
					wByWkDram[wkKey] += cyc
					if wkKey.IsSystem() {
						wSysDram += cyc
					}
				}
			}

			binDram = append(binDram, md)
		}

		gcPmuCycleBaselines(cycBaselines, nowMono, ttlTicks)
		setPmuCycleBaselines(c, cycBaselines)
	}

	// -------------------------------------------------------------------------
	// Diagnostics: bins count (low-cardinality).
	// -------------------------------------------------------------------------
	if diagEnabled {
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricAttribBinsTotal, nil),
			Window: c.Window,
			Unit:   "count",
			Value:  float64(len(inWindow)),
		})
	}

	// -------------------------------------------------------------------------
	// Allocate budgets and emit workload monotonic energy counters.
	// -------------------------------------------------------------------------
	allocPkg := applyWorkloadRaplAllocs(c, raplDomainPkg, pkgBudgetMJ, binPkg)
	allocCore := applyWorkloadRaplAllocs(c, raplDomainCore, coreBudgetMJ, binPkg)
	allocUncore := applyWorkloadRaplAllocs(c, raplDomainUncore, uncoreBudgetMJ, binPkg)
	allocDram := applyWorkloadRaplAllocs(c, raplDomainDram, dramBudgetMJ, binDram)

	// -------------------------------------------------------------------------
	// Persist per-window dynamic alloc maps for downstream slices (Slice 13E+).
	// These maps are "window-sticky" and already PID-reuse safe via Slice 13D.
	// -------------------------------------------------------------------------
	if c.State != nil {
		c.State.Set(analysis.Key("__attrib13d_dyn_alloc_u64", analysis.Labels{"domain": raplDomainPkg}), allocPkg)
		c.State.Set(analysis.Key("__attrib13d_dyn_alloc_u64", analysis.Labels{"domain": raplDomainCore}), allocCore)
		c.State.Set(analysis.Key("__attrib13d_dyn_alloc_u64", analysis.Labels{"domain": raplDomainUncore}), allocUncore)
		c.State.Set(analysis.Key("__attrib13d_dyn_alloc_u64", analysis.Labels{"domain": raplDomainDram}), allocDram)
	}

	// -------------------------------------------------------------------------
	// Diagnostics.
	// -------------------------------------------------------------------------
	if diagEnabled {
		emitWeightDiagDomain(c, raplDomainPkg, binPkg, pkgBudgetMJ, allocPkg)
		emitWeightDiagDomain(c, raplDomainCore, binPkg, coreBudgetMJ, allocCore)
		emitWeightDiagDomain(c, raplDomainUncore, binPkg, uncoreBudgetMJ, allocUncore)
		emitWeightDiagDomain(c, raplDomainDram, binDram, dramBudgetMJ, allocDram)

		emitDiagCounter(c, MetricAttribResolvedProcID, "count", nProcID)
		emitDiagCounter(c, MetricAttribResolvedCgroup, "count", nCgroup)
		emitDiagCounter(c, MetricAttribFallbackSystem, "count", nSystem)
	}

	// logAttributionAudit(
	// 	c,
	// 	"rapl_pkg", "rapl_dram",
	// 	pkgBudgetMJ, dramBudgetMJ,
	// 	wSumPkg, wSumDram,
	// 	wSysPkg, wSysDram,
	// 	wByWkPkg, wByWkDram,
	// 	examplePIDs,
	// 	nProcID, nCgroup, nSystem,
	// )

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

type raplSeriesRef struct {
	Domain    string
	Kind      string
	Source    string
	Namespace string
	Pod       string
	Container string
}

// applyWorkloadRaplAllocs allocates a domain budget using bins and emits
// tycho_workload_rapl_energy_mj{domain,kind,source,namespace,pod,container} as a monotonic counter.
//
// Missing domains are handled by emitting a single __system__ series with value 0 (or the allocated amount).
func applyWorkloadRaplAllocs(c *analysis.Cycle, domain string, budgetMJ uint64, bins attribBins) map[attribution.WorkloadKey]uint64 {
	sys := attribution.SystemWorkloadKey()

	// Always return a valid map (even on nil cycle).
	if c == nil || c.Sink == nil || c.State == nil {
		return map[attribution.WorkloadKey]uint64{sys: 0}
	}

	// Allocate (conserving exactly).
	alloc := allocateComponentToMap(budgetMJ, bins)

	// Ensure system always present.
	if _, ok := alloc[sys]; !ok {
		alloc[sys] = 0
	}

	now := c.NowMono
	if now == 0 {
		now = c.Window.EndMono
	}

	ttlTicks := uint64(0)
	if c.Mono != nil {
		ttlTicks = c.Mono.TicksForDurationCeil(2 * time.Minute)
	}

	// Index in state so we can GC without StateStore iteration.
	idxKey := analysis.Key("__attrib_rapl_series_index", analysis.Labels{
		"domain": domain,
		"kind":   attribKindDynamic,
		"source": raplSource,
	})
	var index []raplSeriesRef
	if v, ok := c.State.Get(idxKey); ok {
		if vv, ok2 := v.([]raplSeriesRef); ok2 {
			index = vv
		}
	}
	if index == nil {
		index = []raplSeriesRef{}
	}

	active := make(map[raplSeriesRef]struct{}, len(alloc))

	for wk, deltaMJ := range alloc {
		if wk.IsZero() {
			wk = sys
		}

		ref := raplSeriesRef{
			Domain:    domain,
			Kind:      attribKindDynamic,
			Source:    raplSource,
			Namespace: wk.Namespace,
			Pod:       wk.Pod,
			Container: wk.Container,
		}
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

		// Monotonic accumulator in state (per workload + domain).
		stateKey := analysis.Key("__attrib_workload_rapl_energy_u64", analysis.Labels{
			"domain": domain,
			"kind":   attribKindDynamic,
			"source": raplSource,
			"ns":     wk.Namespace,
			"pod":    wk.Pod,
			"ctr":    wk.Container,
		})

		var prev uint64
		if prevAny, ok := c.State.Get(stateKey); ok {
			if u, ok2 := prevAny.(uint64); ok2 {
				prev = u
			}
		}
		next := prev + deltaMJ
		c.State.Set(stateKey, next)

		lastSeenKey := analysis.Key("__attrib_workload_rapl_last_seen_u64", analysis.Labels{
			"domain": domain,
			"kind":   attribKindDynamic,
			"source": raplSource,
			"ns":     wk.Namespace,
			"pod":    wk.Pod,
			"ctr":    wk.Container,
		})
		c.State.Set(lastSeenKey, now)

		labels := analysis.Labels{
			"domain":    domain,
			"kind":      attribKindDynamic,
			"source":    raplSource,
			"namespace": wk.Namespace,
			"pod":       wk.Pod,
			"container": wk.Container,
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricWorkloadRaplEnergyMJ, labels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  float64(next),
		})
	}

	// GC stale series.
	if ttlTicks > 0 && now > 0 {
		nextIndex := make([]raplSeriesRef, 0, len(index))
		for _, ref := range index {
			if _, ok := active[ref]; ok {
				nextIndex = append(nextIndex, ref)
				continue
			}

			lastSeenKey := analysis.Key("__attrib_workload_rapl_last_seen_u64", analysis.Labels{
				"domain": ref.Domain,
				"kind":   ref.Kind,
				"source": ref.Source,
				"ns":     ref.Namespace,
				"pod":    ref.Pod,
				"ctr":    ref.Container,
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
			if now > last && (now-last) > ttlTicks {
				lbl := analysis.Labels{
					"domain":    ref.Domain,
					"kind":      ref.Kind,
					"source":    ref.Source,
					"namespace": ref.Namespace,
					"pod":       ref.Pod,
					"container": ref.Container,
				}
				c.Sink.Delete(c.Ctx, analysis.Key(MetricWorkloadRaplEnergyMJ, lbl))

				energyKey := analysis.Key("__attrib_workload_rapl_energy_u64", analysis.Labels{
					"domain": ref.Domain,
					"kind":   ref.Kind,
					"source": ref.Source,
					"ns":     ref.Namespace,
					"pod":    ref.Pod,
					"ctr":    ref.Container,
				})
				c.State.Delete(energyKey)
				c.State.Delete(lastSeenKey)
				continue
			}

			nextIndex = append(nextIndex, ref)
		}
		index = nextIndex
	}

	c.State.Set(idxKey, index)
	return alloc
}

// allocateComponentToMap is the allocator core from allocateComponent, but returns alloc map.
// It guarantees sum(alloc)==budgetMJ with remainder routed to system.
func allocateComponentToMap(budgetMJ uint64, bins attribBins) map[attribution.WorkloadKey]uint64 {
	sys := attribution.SystemWorkloadKey()

	alloc := make(map[attribution.WorkloadKey]uint64, 128)
	alloc[sys] = 0

	if budgetMJ == 0 {
		return alloc
	}

	sumW := 0.0
	for i := range bins {
		sumW += bins[i].total
	}
	if sumW <= 0 {
		alloc[sys] = budgetMJ
		return alloc
	}

	wByWk := make(map[attribution.WorkloadKey]float64, 128)
	for i := range bins {
		for wk, ww := range bins[i].w {
			if ww <= 0 {
				continue
			}
			if wk.IsZero() {
				wk = sys
			}
			wByWk[wk] += ww
		}
	}
	if _, ok := wByWk[sys]; !ok {
		wByWk[sys] = 0
	}

	type fracEntry struct {
		wk   attribution.WorkloadKey
		frac float64
	}
	fracs := make([]fracEntry, 0, len(wByWk))

	var sumFloor uint64
	for wk, ww := range wByWk {
		if ww <= 0 {
			continue
		}
		exact := float64(budgetMJ) * (ww / sumW)
		floor := uint64(exact)
		alloc[wk] = floor
		sumFloor += floor
		if frac := exact - float64(floor); frac > 0 {
			fracs = append(fracs, fracEntry{wk: wk, frac: frac})
		}
	}

	if sumFloor < budgetMJ && len(fracs) > 0 {
		rem := budgetMJ - sumFloor
		sort.Slice(fracs, func(i, j int) bool { return fracs[i].frac > fracs[j].frac })
		for rem > 0 && len(fracs) > 0 {
			for i := 0; i < len(fracs) && rem > 0; i++ {
				alloc[fracs[i].wk] += 1
				rem--
			}
		}
	}

	var sumAlloc uint64
	for _, v := range alloc {
		sumAlloc += v
	}
	if sumAlloc < budgetMJ {
		alloc[sys] += (budgetMJ - sumAlloc)
	}
	return alloc
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

// --- PMU baselines (state-based) ---------------------------------------------

type pmuBaseline struct {
	Instr    uint64
	Cycles   uint64
	Misses   uint64
	LastSeen uint64
}

type pmuCycleBaseline struct {
	Cycles   uint64
	LastSeen uint64
}

func pmuBaselineKey(pid uint64, procID attribution.ProcID, okProc bool) string {
	// Prefer stable ProcID (PID + StartJiffies) to avoid PID reuse poisoning.
	if okProc {
		// Explicitly construct a stable key.
		// This MUST uniquely identify a process instance.
		return "pid:" + u64toa(uint64(procID.PID)) +
			":start:" + u64toa(procID.StartJiffies)
	}

	// Fallback: PID only (less safe, but still better than prev-tick-only).
	return "pid:" + u64toa(pid)
}
func getOrInitPmuBaselines(c *analysis.Cycle) map[string]pmuBaseline {
	key := analysis.Key("__attrib_pmu_baselines", nil)
	if v, ok := c.State.Get(key); ok {
		if m, ok2 := v.(map[string]pmuBaseline); ok2 && m != nil {
			return m
		}
	}
	m := make(map[string]pmuBaseline, 4096)
	c.State.Set(key, m)
	return m
}

func setPmuBaselines(c *analysis.Cycle, m map[string]pmuBaseline) {
	key := analysis.Key("__attrib_pmu_baselines", nil)
	c.State.Set(key, m)
}

func gcPmuBaselines(m map[string]pmuBaseline, nowMono uint64, ttlTicks uint64) {
	if ttlTicks == 0 || nowMono == 0 {
		return
	}
	for k, v := range m {
		if v.LastSeen == 0 {
			continue
		}
		if nowMono > v.LastSeen && (nowMono-v.LastSeen) > ttlTicks {
			delete(m, k)
		}
	}
}

// cycles-only baselines for DRAM fallback pass
func getOrInitPmuCycleBaselines(c *analysis.Cycle) map[string]pmuCycleBaseline {
	key := analysis.Key("__attrib_pmu_cycle_baselines", nil)
	if v, ok := c.State.Get(key); ok {
		if m, ok2 := v.(map[string]pmuCycleBaseline); ok2 && m != nil {
			return m
		}
	}
	m := make(map[string]pmuCycleBaseline, 4096)
	c.State.Set(key, m)
	return m
}

func setPmuCycleBaselines(c *analysis.Cycle, m map[string]pmuCycleBaseline) {
	key := analysis.Key("__attrib_pmu_cycle_baselines", nil)
	c.State.Set(key, m)
}

func gcPmuCycleBaselines(m map[string]pmuCycleBaseline, nowMono uint64, ttlTicks uint64) {
	if ttlTicks == 0 || nowMono == 0 {
		return
	}
	for k, v := range m {
		if v.LastSeen == 0 {
			continue
		}
		if nowMono > v.LastSeen && (nowMono-v.LastSeen) > ttlTicks {
			delete(m, k)
		}
	}
}

// tiny u64 -> string without fmt import (keep file lean)
func u64toa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [32]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + (v % 10))
		v /= 10
	}
	return string(buf[i:])
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
func emitWeightDiagDomain(
	c *analysis.Cycle,
	domain string,
	bins attribBins,
	budgetMJ uint64,
	alloc map[attribution.WorkloadKey]uint64,
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

	// IMPORTANT:
	// Keep label KEY "component" to avoid schema change for existing diag metric IDs.
	// Only change the label VALUE.
	lbl := analysis.Labels{"component": "rapl_" + domain}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricAttribWeightSum, lbl),
		Window: c.Window,
		Unit:   "count",
		Value:  math.Floor(sumW),
	})

	sys := attribution.SystemWorkloadKey()
	systemAllocMJ := uint64(0)
	if alloc != nil {
		systemAllocMJ = alloc[sys]
	}

	frac := 0.0
	if budgetMJ > 0 {
		frac = float64(systemAllocMJ) / float64(budgetMJ)
	}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricAttribSystemFraction, lbl),
		Window: c.Window,
		Unit:   "ratio",
		Value:  clamp01(frac),
	})
}
