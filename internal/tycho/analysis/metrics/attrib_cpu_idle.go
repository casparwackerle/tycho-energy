// file: internal/tycho/analysis/metrics/attrib_cpu_idle.go
package analysismetrics

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jaypipes/ghw"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/attribution"
	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	attribKindIdle = "idle"

	MetricIdleAllocBeta           analysis.MetricID = "idle_alloc_beta"
	MetricIdleAllocPoolReqMJ      analysis.MetricID = "idle_alloc_pool_req_mj"
	MetricIdleAllocPoolOppMJ      analysis.MetricID = "idle_alloc_pool_opp_mj"
	MetricIdleAllocSystemFraction analysis.MetricID = "idle_alloc_system_fraction"
	MetricIdleAllocWeightSumReq   analysis.MetricID = "idle_alloc_weight_sum_req"
	MetricIdleAllocWeightSumOpp   analysis.MetricID = "idle_alloc_weight_sum_opp"
)

type CpuIdleAllocationWindowV1 struct{}

func NewCpuIdleAllocationWindowV1() *CpuIdleAllocationWindowV1 { return &CpuIdleAllocationWindowV1{} }

func (m *CpuIdleAllocationWindowV1) ID() analysis.MetricID { return "attrib_cpu_idle_window_v1" }

func (m *CpuIdleAllocationWindowV1) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Meta != nil && c.Mono != nil
}

func (m *CpuIdleAllocationWindowV1) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Meta == nil {
		return nil
	}

	// Slice 13E uses conservative idle budgets (from RaplIdleDynamic outputs).
	pkgIdleMJ, pkgOK := raplIdleDeltaBudgetMJ(c, "pkg")
	coreIdleMJ, coreOK := raplIdleDeltaBudgetMJ(c, "core")
	uncoreIdleMJ, uncoreOK := raplIdleDeltaBudgetMJ(c, "uncore")
	dramIdleMJ, dramOK := raplIdleDeltaBudgetMJ(c, "dram")

	// If none are available, nothing to do.
	if !pkgOK && !coreOK && !uncoreOK && !dramOK {
		return nil
	}

	// Read dynamic alloc maps (window-sticky activity signal) from Slice 13D.
	dynPkg := getDynAllocMap(c, "pkg")
	dynCore := getDynAllocMap(c, "core")
	dynUncore := getDynAllocMap(c, "uncore")
	dynDram := getDynAllocMap(c, "dram")

	// Build request maps and beta denominators from metadata (declarative reservation).
	reqCPUByWk, sumReqCPU := buildRequestsCPU(c.Meta)
	reqMemByWk, sumReqMem := buildRequestsMem(c.Meta)

	cpuCapMillis, memCapBytes := getOrInitCapacities(c)

	betaCPU := 0.0
	if cpuCapMillis > 0 && sumReqCPU > 0 {
		betaCPU = clamp01(float64(sumReqCPU) / float64(cpuCapMillis))
	}
	betaMem := 0.0
	if memCapBytes > 0 && sumReqMem > 0 {
		betaMem = clamp01(float64(sumReqMem) / float64(memCapBytes))
	}

	// Recipients: union of window-sticky (dyn maps) + metadata containers.
	// This is stable for churn: dyn maps capture short-lived, metadata captures idle-but-running.
	wkSet := make(map[attribution.WorkloadKey]struct{}, 256)
	addWkSetFromAlloc(wkSet, dynPkg)
	addWkSetFromAlloc(wkSet, dynCore)
	addWkSetFromAlloc(wkSet, dynUncore)
	addWkSetFromAlloc(wkSet, dynDram)
	addWkSetFromMeta(wkSet, c.Meta)

	sys := attribution.SystemWorkloadKey()
	wkSet[sys] = struct{}{}

	// Allocate and emit:
	// - pkg/core/uncore use CPU reservation + CPU activity (domain-specific dynamic alloc maps)
	// - dram uses MEM reservation + MEM activity (domain-specific dynamic alloc map)
	if pkgOK {
		m.allocAndEmitDomain(c, "pkg", pkgIdleMJ, betaCPU, wkSet, reqCPUByWk, dynPkg)
	}
	if coreOK {
		m.allocAndEmitDomain(c, "core", coreIdleMJ, betaCPU, wkSet, reqCPUByWk, dynCore)
	}
	if uncoreOK {
		m.allocAndEmitDomain(c, "uncore", uncoreIdleMJ, betaCPU, wkSet, reqCPUByWk, dynUncore)
	}
	if dramOK {
		m.allocAndEmitDomain(c, "dram", dramIdleMJ, betaMem, wkSet, reqMemByWk, dynDram)
	}
	return nil
}

// allocAndEmitDomain implements two-pool idle allocation for one RAPL domain:
//   - pool1 (reserved): beta * idleBudget, allocated by requests (only workloads with request>0)
//   - pool2 (opportunistic): remaining idleBudget, allocated by activity (dynamic alloc shares),
//     only among workloads with request==0, plus __system__.
//
// Any remainder or degenerate case routes to __system__.
func (m *CpuIdleAllocationWindowV1) allocAndEmitDomain(
	c *analysis.Cycle,
	domain string,
	idleBudgetMJ uint64,
	beta float64,
	wkSet map[attribution.WorkloadKey]struct{},
	reqByWk map[attribution.WorkloadKey]int64,
	dynAlloc map[attribution.WorkloadKey]uint64,
) {
	sys := attribution.SystemWorkloadKey()

	// Ensure maps exist.
	if reqByWk == nil {
		reqByWk = map[attribution.WorkloadKey]int64{}
	}
	if dynAlloc == nil {
		dynAlloc = map[attribution.WorkloadKey]uint64{}
	}
	if _, ok := dynAlloc[sys]; !ok {
		dynAlloc[sys] = 0
	}

	// Pool sizes (exact conservation): poolReq + poolOpp == idleBudgetMJ.
	poolReq := uint64(0)
	if idleBudgetMJ > 0 && beta > 0 {
		poolReq = uint64(float64(idleBudgetMJ)*beta + 0.5)
		if poolReq > idleBudgetMJ {
			poolReq = idleBudgetMJ
		}
	}
	poolOpp := idleBudgetMJ - poolReq

	// ---------------------------------------------------------------------
	// Pool 1: requests-weighted among workloads with request > 0.
	// ---------------------------------------------------------------------
	reqWeights := make(map[attribution.WorkloadKey]uint64, 128)
	var sumReqW uint64
	for wk := range wkSet {
		if wk.IsZero() || wk.IsSystem() {
			continue
		}
		r := reqByWk[wk]
		if r <= 0 {
			continue
		}
		w := uint64(r)
		reqWeights[wk] = w
		sumReqW += w
	}
	allocReq := allocateByWeights(poolReq, reqWeights)

	// ---------------------------------------------------------------------
	// Pool 2: activity-weighted among workloads with request == 0, PLUS __system__.
	//
	// IMPORTANT FIX:
	// - __system__ must participate with a real weight, otherwise it will frequently get 0,
	//   and your __system__ counter will not move -> rate() stays ~0.
	// - We use dynAlloc[sys] as the window-sticky activity signal for system.
	// ---------------------------------------------------------------------
	oppWeights := make(map[attribution.WorkloadKey]uint64, 128)
	var sumOppW uint64

	for wk := range wkSet {
		if wk.IsZero() {
			continue
		}

		// Opportunistic pool participants are "no request known".
		// __system__ always belongs here (no request).
		if !wk.IsSystem() && reqByWk[wk] > 0 {
			continue
		}

		w := dynAlloc[wk]

		// Keep zero weights out (except system is handled explicitly below).
		if w == 0 {
			continue
		}

		oppWeights[wk] = w
		sumOppW += w
	}

	// Ensure system participates even if its weight is 0 (it is also the remainder sink).
	// But when dynAlloc[sys] > 0, it now properly receives an activity share too.
	if _, ok := oppWeights[sys]; !ok {
		oppWeights[sys] = dynAlloc[sys] // may be 0
		// Only add to sum if non-zero (it might already be zero).
		if dynAlloc[sys] > 0 {
			sumOppW += dynAlloc[sys]
		}
	}

	allocOpp := allocateByWeights(poolOpp, oppWeights)

	// ---------------------------------------------------------------------
	// Combine allocations.
	// ---------------------------------------------------------------------
	alloc := make(map[attribution.WorkloadKey]uint64, 256)
	for wk, v := range allocReq {
		if wk.IsZero() {
			wk = sys
		}
		alloc[wk] += v
	}
	for wk, v := range allocOpp {
		if wk.IsZero() {
			wk = sys
		}
		alloc[wk] += v
	}
	if _, ok := alloc[sys]; !ok {
		alloc[sys] = 0
	}

	// ---------------------------------------------------------------------
	// HARD CONSERVATION FIXUP (IMPORTANT)
	// Force: sum(alloc) == idleBudgetMJ by adjusting __system__.
	// This makes "sum(workload idle) == rapl idle" true (modulo Grafana rate smoothing).
	// ---------------------------------------------------------------------
	var sumAlloc uint64
	for _, v := range alloc {
		sumAlloc += v
	}
	if sumAlloc < idleBudgetMJ {
		alloc[sys] += (idleBudgetMJ - sumAlloc)
	} else if sumAlloc > idleBudgetMJ {
		over := sumAlloc - idleBudgetMJ
		// Prefer subtracting from system first.
		if alloc[sys] >= over {
			alloc[sys] -= over
		} else {
			// If system isn't enough (should not happen), clamp system to 0 and accept minor overshoot correction
			// by removing remainder from arbitrary non-zero workloads deterministically.
			over -= alloc[sys]
			alloc[sys] = 0

			// Deterministic removal order: lexicographic by ns/pod/ctr string.
			type kv struct {
				wk attribution.WorkloadKey
			}
			var keys []kv
			for wk, v := range alloc {
				if wk.IsSystem() || v == 0 {
					continue
				}
				keys = append(keys, kv{wk: wk})
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := keys[i].wk, keys[j].wk
				if a.Namespace != b.Namespace {
					return a.Namespace < b.Namespace
				}
				if a.Pod != b.Pod {
					return a.Pod < b.Pod
				}
				return a.Container < b.Container
			})
			for _, e := range keys {
				if over == 0 {
					break
				}
				wk := e.wk
				v := alloc[wk]
				if v == 0 {
					continue
				}
				if v >= over {
					alloc[wk] = v - over
					over = 0
					break
				}
				alloc[wk] = 0
				over -= v
			}
		}
	}

	// Emit monotonic workload counters for kind="idle".
	applyWorkloadRaplAllocsKind(c, domain, attribKindIdle, alloc)

	// Diagnostics (low-cardinality).
	if config.GetAttributionDiagnosticsEnabled() {
		lbl := analysis.Labels{"domain": domain}

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleAllocBeta, lbl),
			Window: c.Window,
			Unit:   "ratio",
			Value:  clamp01(beta),
		})
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleAllocPoolReqMJ, lbl),
			Window: c.Window,
			Unit:   "mJ",
			Value:  float64(poolReq),
		})
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleAllocPoolOppMJ, lbl),
			Window: c.Window,
			Unit:   "mJ",
			Value:  float64(poolOpp),
		})

		sysMJ := alloc[sys]
		frac := 0.0
		if idleBudgetMJ > 0 {
			frac = float64(sysMJ) / float64(idleBudgetMJ)
		}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleAllocSystemFraction, lbl),
			Window: c.Window,
			Unit:   "ratio",
			Value:  clamp01(frac),
		})

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleAllocWeightSumReq, lbl),
			Window: c.Window,
			Unit:   "count",
			Value:  float64(sumReqW),
		})
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricIdleAllocWeightSumOpp, lbl),
			Window: c.Window,
			Unit:   "count",
			Value:  float64(sumOppW),
		})
	}
}

// raplIdleDeltaBudgetMJ returns the per-window idle delta (mJ) for the given domain
// by differencing the already-exported cumulative idle energy series from PointStore.
func raplIdleDeltaBudgetMJ(c *analysis.Cycle, domain string) (uint64, bool) {
	if c == nil || c.Store == nil || c.State == nil {
		return 0, false
	}
	key := analysis.Key(MetricRaplEnergyMJ, analysis.Labels{
		"domain": domain,
		"kind":   "idle",
		"source": raplSource,
	})
	curr, ok := getPointValueOk(c.Store, key)
	if !ok || curr < 0 {
		return 0, false
	}

	lastKey := stateKeyLastSeen("attrib13e", domain, "rapl_idle_cum_mj")
	prev, prevOK := stateGetF64(c.State, lastKey)
	stateSetF64(c.State, lastKey, curr)
	if !prevOK {
		return 0, false
	}

	d := curr - prev
	if d < 0 {
		d = 0
	}
	return uint64(d + 0.5), true
}

func getDynAllocMap(c *analysis.Cycle, domain string) map[attribution.WorkloadKey]uint64 {
	sys := attribution.SystemWorkloadKey()
	if c == nil || c.State == nil {
		return map[attribution.WorkloadKey]uint64{sys: 0}
	}
	v, ok := c.State.Get(analysis.Key("__attrib13d_dyn_alloc_u64", analysis.Labels{"domain": domain}))
	if !ok || v == nil {
		return map[attribution.WorkloadKey]uint64{sys: 0}
	}
	m, ok := v.(map[attribution.WorkloadKey]uint64)
	if !ok || m == nil {
		return map[attribution.WorkloadKey]uint64{sys: 0}
	}
	if _, ok := m[sys]; !ok {
		m[sys] = 0
	}
	return m
}

func addWkSetFromAlloc(dst map[attribution.WorkloadKey]struct{}, alloc map[attribution.WorkloadKey]uint64) {
	if dst == nil || alloc == nil {
		return
	}
	sys := attribution.SystemWorkloadKey()
	for wk := range alloc {
		if wk.IsZero() {
			continue
		}
		if wk == sys {
			continue
		}
		dst[wk] = struct{}{}
	}
}

func addWkSetFromMeta(dst map[attribution.WorkloadKey]struct{}, store *metadata.Store) {
	if dst == nil || store == nil {
		return
	}
	store.RangeContainers(func(_ string, cm *metadata.ContainerMeta) bool {
		if cm == nil {
			return true
		}
		// Conservative: only treat Running containers as current reservation participants/recipients.
		if cm.State != metadata.ContainerStateRunning {
			return true
		}
		if cm.Namespace == "" || cm.PodName == "" || cm.ContainerName == "" {
			return true
		}
		dst[attribution.WorkloadKey{Namespace: cm.Namespace, Pod: cm.PodName, Container: cm.ContainerName}] = struct{}{}
		return true
	})
}

func buildRequestsCPU(store *metadata.Store) (map[attribution.WorkloadKey]int64, int64) {
	out := make(map[attribution.WorkloadKey]int64, 256)
	var sum int64
	if store == nil {
		return out, 0
	}
	store.RangeContainers(func(_ string, cm *metadata.ContainerMeta) bool {
		if cm == nil {
			return true
		}
		if cm.State != metadata.ContainerStateRunning {
			return true
		}
		if cm.Namespace == "" || cm.PodName == "" || cm.ContainerName == "" {
			return true
		}
		r := cm.RequestsCPUMillis
		if r <= 0 {
			return true
		}
		wk := attribution.WorkloadKey{Namespace: cm.Namespace, Pod: cm.PodName, Container: cm.ContainerName}
		out[wk] += r
		sum += r
		return true
	})
	if sum < 0 {
		sum = 0
	}
	return out, sum
}

func buildRequestsMem(store *metadata.Store) (map[attribution.WorkloadKey]int64, int64) {
	out := make(map[attribution.WorkloadKey]int64, 256)
	var sum int64
	if store == nil {
		return out, 0
	}
	store.RangeContainers(func(_ string, cm *metadata.ContainerMeta) bool {
		if cm == nil {
			return true
		}
		if cm.State != metadata.ContainerStateRunning {
			return true
		}
		if cm.Namespace == "" || cm.PodName == "" || cm.ContainerName == "" {
			return true
		}
		r := cm.RequestsMemBytes
		if r <= 0 {
			return true
		}
		wk := attribution.WorkloadKey{Namespace: cm.Namespace, Pod: cm.PodName, Container: cm.ContainerName}
		out[wk] += r
		sum += r
		return true
	})
	if sum < 0 {
		sum = 0
	}
	return out, sum
}

// applyWorkloadRaplAllocsKind emits tycho_workload_rapl_energy_mj with kind=kind
// using a monotonic accumulator and TTL GC, mirroring Slice 13D behavior.
func applyWorkloadRaplAllocsKind(c *analysis.Cycle, domain string, kind string, alloc map[attribution.WorkloadKey]uint64) {
	sys := attribution.SystemWorkloadKey()
	if c == nil || c.Sink == nil || c.State == nil {
		return
	}
	if alloc == nil {
		alloc = map[attribution.WorkloadKey]uint64{}
	}
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
		"kind":   kind,
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
			Kind:      kind,
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

		// Monotonic accumulator in state (per workload + domain + kind).
		stateKey := analysis.Key("__attrib_workload_rapl_energy_u64", analysis.Labels{
			"domain": domain,
			"kind":   kind,
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
			"kind":   kind,
			"source": raplSource,
			"ns":     wk.Namespace,
			"pod":    wk.Pod,
			"ctr":    wk.Container,
		})
		c.State.Set(lastSeenKey, now)

		labels := analysis.Labels{
			"domain":    domain,
			"kind":      kind,
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
}

// allocateByWeights allocates budgetMJ proportional to weights.
// It guarantees sum(alloc)==budgetMJ by routing remainder to __system__.
func allocateByWeights(budgetMJ uint64, weights map[attribution.WorkloadKey]uint64) map[attribution.WorkloadKey]uint64 {
	sys := attribution.SystemWorkloadKey()
	alloc := make(map[attribution.WorkloadKey]uint64, 128)
	alloc[sys] = 0

	if budgetMJ == 0 {
		return alloc
	}

	var sumW uint64
	for _, w := range weights {
		sumW += w
	}
	if sumW == 0 {
		alloc[sys] = budgetMJ
		return alloc
	}

	type fracEntry struct {
		wk   attribution.WorkloadKey
		frac float64
	}
	fracs := make([]fracEntry, 0, len(weights))

	var sumFloor uint64
	for wk, w := range weights {
		if w == 0 {
			continue
		}
		if wk.IsZero() {
			wk = sys
		}
		exact := float64(budgetMJ) * (float64(w) / float64(sumW))
		floor := uint64(exact)
		alloc[wk] += floor
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

// Capacity caching ------------------------------------------------------------

func getOrInitCapacities(c *analysis.Cycle) (cpuCapMillis int64, memCapBytes int64) {
	// Defaults: never return <=0.
	cpuCapMillis = 1000
	memCapBytes = 1024 * 1024 * 1024

	if c == nil || c.State == nil {
		return cpuCapMillis, memCapBytes
	}

	cpuKey := analysis.Key("__attrib13e_cpu_cap_millis", nil)
	memKey := analysis.Key("__attrib13e_mem_cap_bytes", nil)

	if v, ok := c.State.Get(cpuKey); ok {
		if vv, ok2 := v.(int64); ok2 && vv > 0 {
			cpuCapMillis = vv
		}
	} else {
		cc := int64(getCPUCores()) * 1000
		if cc <= 0 {
			cc = 1000
		}
		c.State.Set(cpuKey, cc)
		cpuCapMillis = cc
	}

	if v, ok := c.State.Get(memKey); ok {
		if vv, ok2 := v.(int64); ok2 && vv > 0 {
			memCapBytes = vv
		}
	} else {
		mm := int64(getMemBytes())
		if mm <= 0 {
			mm = 1024 * 1024 * 1024
		}
		c.State.Set(memKey, mm)
		memCapBytes = mm
	}

	return cpuCapMillis, memCapBytes
}

// getMemBytes returns total memory bytes (prefer "usable", fallback to physical, then /proc/meminfo).
func getMemBytes() uint64 {
	// Try ghw first (preferred, matches your CPU helper style).
	if mem, err := ghw.Memory(ghw.WithDisableWarnings()); err == nil && mem != nil {
		// ghw uses int64 for these.
		if mem.TotalUsableBytes > 0 {
			return uint64(mem.TotalUsableBytes)
		}
		if mem.TotalPhysicalBytes > 0 {
			return uint64(mem.TotalPhysicalBytes)
		}
	}

	// Fallback: /proc/meminfo MemTotal (kB).
	f, err := os.Open("/proc/meminfo")
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "MemTotal:") {
				parts := strings.Fields(line)
				// Expect: MemTotal: <kB> kB
				if len(parts) >= 2 {
					kb, err2 := strconv.ParseUint(parts[1], 10, 64)
					if err2 == nil && kb > 0 {
						return kb * 1024
					}
				}
				break
			}
		}
	}

	// Last resort: 1 GiB (never 0).
	return 1024 * 1024 * 1024
}
