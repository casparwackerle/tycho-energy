// file: internal/tycho/analysis/metrics/fusion_substrate.go
package analysismetrics

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/fusion"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

const (
	MetricFusionCache analysis.MetricID = "fusion_cache" // state key family
	MetricFusionReady analysis.MetricID = "fusion_ready" // state key family

	// diagnostics (Prometheus-visible, diag-only intent)
	MetricFusionRedfishDelaySelectedMS analysis.MetricID = "fusion_redfish_delay_selected_ms"
	MetricFusionRedfishDeficitSumMW    analysis.MetricID = "fusion_redfish_deficit_mw"
	MetricFusionRedfishDeficitMaxMW    analysis.MetricID = "fusion_redfish_deficit_max_mw"

	// state-only (selected delay persistence)
	MetricFusionRedfishDelayState           analysis.MetricID = "fusion_redfish_delay_state_ms" // state-only
	MetricFusionRedfishDelaySelectedMsState analysis.MetricID = "fusion_redfish_delay_selected_ms_state"
)

type FusionSubstrate struct {
	// no persistent fields; everything persists in analysis.StateStore
}

func NewFusionSubstrate() *FusionSubstrate { return &FusionSubstrate{} }

func (m *FusionSubstrate) ID() analysis.MetricID { return "fusion_substrate" }

func (m *FusionSubstrate) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.State != nil && c.Mono != nil && c.Sink != nil
}

func (m *FusionSubstrate) Run(c *analysis.Cycle) error {
	if c == nil || c.Mono == nil || c.State == nil {
		return nil
	}

	// --- Config ---
	qMs := config.GetFusionQuantumMs()
	if qMs <= 0 {
		qMs = 50
	}
	qTicks := c.Mono.TicksForMsCeil(qMs)
	if qTicks == 0 {
		// Extremely defensive; mono quantum should prevent this.
		return nil
	}

	hSec := config.GetFusionHorizonSec()
	if hSec <= 0 {
		// default: match BufferWindowSec if present; else 90
		if bw := config.BufferWindowSec(); bw > 0 {
			hSec = bw
		} else {
			hSec = 90
		}
	}
	hTicks := c.Mono.TicksForDurationCeil(time.Duration(hSec) * time.Second)
	hBins := int(hTicks / qTicks)
	if hBins < 1 {
		hBins = 1
	}

	kernelStr := config.GetFusionRedfishKernel()
	kernel := fusion.KernelAvg1sTrailing
	switch kernelStr {
	case string(fusion.KernelInstant):
		kernel = fusion.KernelInstant
	case string(fusion.KernelAvg1sTrailing), "":
		kernel = fusion.KernelAvg1sTrailing
	default:
		// Unknown -> default
		kernel = fusion.KernelAvg1sTrailing
	}
	kernelMs := config.GetFusionRedfishKernelMs()
	if kernelMs <= 0 {
		kernelMs = 1000
	}

	diag := config.GetFusionDiagnosticsEnabled()

	// --- Chassis selection policy (prefer Self) ---
	chassis := selectFusionChassis(c)

	// --- Desired horizon alignment ---
	// k_now uses corrected time domain (Cycle.Window is corrected)
	kNow := fusion.BinIndex(int64(c.Window.EndMono / qTicks))
	startBin := kNow - fusion.BinIndex(hBins) + 1

	// --- Load/Create state ---
	key := analysis.Key(MetricFusionCache, analysis.Labels{"chassis": chassis})
	cache, ok := getCache(c.State, key)
	if !ok || cache == nil {
		cache = fusion.NewCache(chassis, qTicks, hBins, startBin)
	} else {
		// Reset on grid param changes.
		if cache.QuantumTicks != qTicks || cache.HorizonBins != hBins {
			klog.V(2).Infof("[analysis] fusion: reset cache due to config change chassis=%q qTicks=%d->%d hBins=%d->%d",
				chassis, cache.QuantumTicks, qTicks, cache.HorizonBins, hBins,
			)
			cache = fusion.NewCache(chassis, qTicks, hBins, startBin)
		} else {
			// Shift horizon.
			cache.EnsureHorizon(startBin)
		}
	}

	// --- Determine incremental update range ---
	kLast := cache.LastBin
	kStart := kLast + 1
	if kStart < startBin {
		kStart = startBin
	}
	cache.ZeroRange(kStart, kNow, true, true, true, true)
	if kStart > kNow {
		// Nothing new to compute; still refresh redfish obs (cheap) if diagnostics enabled.
		if diag {
			m.refreshRedfishObs(c, cache, chassis, kernel, kernelMs)
			putCache(c.State, key, cache)
		}
		return nil
	}

	// Raw window for feature extraction:
	// We need raw samples that could contribute to bins [kStart..kNow], but each metric has its own delay.
	// We will compute per-metric effective raw windows using Cycle.EffectiveWindowTicks and then clamp to horizon bounds.
	binStartMono := uint64(kStart) * qTicks
	binEndMono := uint64(kNow+1) * qTicks // exclusive end of last bin

	// --- Extract RAPL pkg/dram into bins ---
	raplDelay := c.Mono.TicksForMsCeil(config.RaplDelayMs())
	raplRawW := analysis.Window{StartMono: binStartMono, EndMono: binEndMono}.ShiftForward(raplDelay)
	raplSamples, raplIntervals := 0, 0
	if c.Rapl() != nil {
		raplSamples, raplIntervals = fusion.ExtractRAPLIntoCache(
			c, cache, raplDelay,
			raplRawW.StartMono, raplRawW.EndMono,
			kStart, kNow,
		)
	}

	// --- Extract BPF CPUInstr into bins ---
	bpfDelay := c.Mono.TicksForMsCeil(config.BpfDelayMs())
	bpfRawW := analysis.Window{StartMono: binStartMono, EndMono: binEndMono}.ShiftForward(bpfDelay)
	bpfSamples, bpfIntervals := 0, 0
	if c.Bpf() != nil {
		bpfSamples, bpfIntervals = fusion.ExtractBpfCPUInstrIntoCache(
			c, cache,
			bpfRawW.StartMono, bpfRawW.EndMono,
			kStart, kNow,
		)
	}

	// --- Extract GPU corrected-series energy into bins ---
	// GPU corrected series is already in corrected time; we do not apply GPU delay here.
	// We simply use the cache horizon bins and integrate the series over each bin time window.
	gpuRawW := analysis.Window{StartMono: binStartMono, EndMono: binEndMono}
	gpuUUIDs, gpuBins := 0, 0
	if c.Gpu() != nil {
		// Only fill the newly required bins (incremental update horizon).
		gpuUUIDs, gpuBins = fusion.ExtractGPUIntoCache(
			c, cache,
			gpuRawW.StartMono, gpuRawW.EndMono,
			kStart, kNow,
		)
	}

	// Update LastBin and persist.
	cache.SetLastBin(kNow)

	// Redfish observations (refresh every cycle; cheap and simplifies correctness).
	selDelayMs, defSumMW, defMaxMW := m.refreshRedfishObs(c, cache, chassis, kernel, kernelMs)

	// Readiness flag: corrected system/residual should take over when RedfishObs are available.
	readyKey := analysis.Key(MetricFusionReady, analysis.Labels{"chassis": chassis})
	c.State.Set(readyKey, len(cache.RedfishObs) > 0)

	putCache(c.State, key, cache)

	// --- Diagnostics ---
	if diag {
		epkg, edram, egpu, instr := cache.WindowSums(c.Window.StartMono, c.Window.EndMono)

		// Emit debug metrics as analysis points (these are per-window scalars).
		emit := func(id analysis.MetricID, unit string, val float64) {
			c.Sink.Emit(c.Ctx, analysis.Point{
				Key:    analysis.Key(id, analysis.Labels{"chassis": chassis}),
				Window: c.Window,
				Unit:   unit,
				Value:  val,
			})
		}
		emit(MetricFusionRedfishDelaySelectedMS, "ms", float64(selDelayMs))
		emit(MetricFusionRedfishDeficitSumMW, "mW", defSumMW)
		emit(MetricFusionRedfishDeficitMaxMW, "mW", defMaxMW)
		emit("fusion_rapl_pkg_energy_mj_window", "mJ", epkg)
		emit("fusion_rapl_dram_energy_mj_window", "mJ", edram)
		emit("fusion_gpu_energy_mj_window", "mJ", egpu)
		emit("fusion_cpuinstr_sum_window", "count", instr)
		emit("fusion_redfish_obs_count_horizon", "count", float64(len(cache.RedfishObs)))

		// Lightweight log.
		klog.V(2).Infof("[analysis] fusion diag window=%s chassis=%q epkg_mj=%.3f edram_mj=%.3f egpu_mj=%.3f instr=%.0f redfish_obs=%d",
			c.Window.String(), chassis, epkg, edram, egpu, instr, len(cache.RedfishObs))
	}

	// Minimal deep-debug, limited count.
	klog.V(6).Infof("[analysis] fusion update chassis=%q kStart=%s kNow=%s qTicks=%d hBins=%d rapl(samples=%d intervals=%d) bpf(samples=%d intervals=%d) gpu(uuids=%d bins=%d)",
		chassis, kStart.String(), kNow.String(), qTicks, hBins,
		raplSamples, raplIntervals, bpfSamples, bpfIntervals, gpuUUIDs, gpuBins,
	)

	return nil
}

// func (m *FusionSubstrate) refreshRedfishObs(
// 	c *analysis.Cycle,
// 	cache *fusion.Cache,
// 	chassis string,
// 	kernel fusion.RedfishKernel,
// 	kernelMs int,
// ) (selectedDelayMs int, deficitSumMW float64, deficitMaxMW float64) {
// 	if c == nil || cache == nil || c.Mono == nil || cache.QuantumTicks == 0 {
// 		return 0, 0, 0
// 	}

// 	// AUDIT (Slice 12C):
// 	// - "redfish_corrected" is NOT a delay-shifted Redfish series. It is the canonical corrected system
// 	//   series emitted by FusionModel as an estimate from parts (RAPL/GPU/BPF) after fitting theta to
// 	//   Redfish observations.
// 	// - Redfish is not binned into cache arrays. FusionSubstrate extracts RedfishObs into cache.RedfishObs
// 	//   via fusion.ExtractRedfishObs over a raw-time window derived from the cache horizon and a delay.
// 	// - FusionModel fuses parts -> pHat, integrates over the analysis window, and emits system_power_mw
// 	//   and system_energy_mj with source=redfish_corrected.
// 	// - Warmup policy: raw system is emitted by SystemRawFromRedfish until MetricFusionReady is true;
// 	//   after takeover, raw system and raw residual series are deleted.

// 	monoQuantumSec := c.Mono.Quantum().Seconds()
// 	if monoQuantumSec <= 0 {
// 		return 0, 0, 0
// 	}
// 	dtSec := float64(cache.QuantumTicks) * monoQuantumSec
// 	if dtSec <= 0 {
// 		return 0, 0, 0
// 	}

// 	// Cache horizon bounds (corrected ticks).
// 	endBin := cache.StartBin + fusion.BinIndex(cache.HorizonBins) - 1
// 	hStart := uint64(cache.StartBin) * cache.QuantumTicks
// 	hEnd := uint64(endBin+1) * cache.QuantumTicks

// 	// Baseline/fallback delay in ms (current behavior).
// 	baseDelayMs := config.RedfishDelayMs()
// 	if baseDelayMs < 0 {
// 		baseDelayMs = 0
// 	}

// 	// Slice 12C search defaults (can be promoted to config getters later).
// 	const (
// 		dMinMs      = 0
// 		dMaxMs      = 8000
// 		dStepMs     = 250
// 		rateLimitMs = 500 // per analysis trigger/cycle
// 		minObs      = 5
// 	)

// 	// Load previous chosen delay from state (per chassis) to rate-limit flapping.
// 	prevDelayMs := baseDelayMs
// 	stateKey := analysis.Key(MetricFusionRedfishDelayState, analysis.Labels{"chassis": chassis})
// 	if v, ok := c.State.Get(stateKey); ok {
// 		if i, ok2 := v.(int); ok2 {
// 			prevDelayMs = i
// 		} else if f, ok2 := v.(float64); ok2 {
// 			prevDelayMs = int(f)
// 		}
// 	}
// 	if prevDelayMs < dMinMs {
// 		prevDelayMs = dMinMs
// 	}
// 	if prevDelayMs > dMaxMs {
// 		prevDelayMs = dMaxMs
// 	}

// 	// Epsilon margin (mW): max(2W, 2% of median parts power over horizon).
// 	epsMW := 2000.0
// 	{
// 		parts := make([]float64, 0, cache.HorizonBins)
// 		for i := 0; i < cache.HorizonBins; i++ {
// 			pParts := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
// 			if pParts >= 0 {
// 				parts = append(parts, pParts)
// 			}
// 		}
// 		if len(parts) > 0 {
// 			sort.Float64s(parts)
// 			med := parts[len(parts)/2]
// 			e2 := 0.02 * med
// 			if e2 > epsMW {
// 				epsMW = e2
// 			}
// 			if epsMW < 2000.0 {
// 				epsMW = 2000.0
// 			}
// 		}
// 	}

// 	// Score deficit over observation points.
// 	scoreCandidate := func(obs []fusion.RedfishObs) (sumMW float64, maxMW float64, ok bool) {
// 		if len(obs) < minObs {
// 			return 0, 0, false
// 		}

// 		used := 0
// 		for i := range obs {
// 			o := obs[i]
// 			pPartsMW, ok2 := partsPowerAtObs(cache, dtSec, monoQuantumSec, o)
// 			if !ok2 {
// 				continue
// 			}
// 			used++

// 			def := pPartsMW - o.ValueMW - epsMW
// 			if def > 0 {
// 				sumMW += def
// 				if def > maxMW {
// 					maxMW = def
// 				}
// 			}
// 		}

// 		if used < minObs {
// 			return 0, 0, false
// 		}
// 		return sumMW, maxMW, true
// 	}

// 	// --- Delay search (pre-rate-limit) ---
// 	bestDelayMs := baseDelayMs
// 	bestScore := math.Inf(1)
// 	bestMaxSearch := 0.0
// 	found := false

// 	for d := dMinMs; d <= dMaxMs; d += dStepMs {
// 		rawStart := hStart + uint64(c.Mono.TicksForMsCeil(d))
// 		rawEnd := hEnd + uint64(c.Mono.TicksForMsCeil(d))

// 		obs, n := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)
// 		if n <= 0 {
// 			continue
// 		}

// 		sum, mx, ok := scoreCandidate(obs)
// 		if !ok {
// 			continue
// 		}

// 		if sum < bestScore || (sum == bestScore && d < bestDelayMs) {
// 			bestScore = sum
// 			bestDelayMs = d
// 			bestMaxSearch = mx
// 			found = true
// 		}
// 	}

// 	if !found || math.IsInf(bestScore, 1) {
// 		bestDelayMs = baseDelayMs
// 		bestScore = 0
// 		bestMaxSearch = 0
// 	}

// 	// Rate-limit vs previous chosen delay to avoid flapping.
// 	outDelayMs := bestDelayMs
// 	if outDelayMs > prevDelayMs+rateLimitMs {
// 		outDelayMs = prevDelayMs + rateLimitMs
// 	} else if outDelayMs < prevDelayMs-rateLimitMs {
// 		outDelayMs = prevDelayMs - rateLimitMs
// 	}
// 	if outDelayMs < dMinMs {
// 		outDelayMs = dMinMs
// 	}
// 	if outDelayMs > dMaxMs {
// 		outDelayMs = dMaxMs
// 	}

// 	// Persist chosen delay in state.
// 	c.State.Set(stateKey, outDelayMs)

// 	// Extract final obs with outDelayMs (post-rate-limit) and compute diagnostics for THIS delay.
// 	rawStart := hStart + uint64(c.Mono.TicksForMsCeil(outDelayMs))
// 	rawEnd := hEnd + uint64(c.Mono.TicksForMsCeil(outDelayMs))

// 	obsFinal, nFinal := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)

// 	cache.RedfishObs = cache.RedfishObs[:0]
// 	if nFinal > 0 {
// 		cache.RedfishObs = append(cache.RedfishObs, obsFinal...)
// 	}

// 	// Diagnostics: deficit computed using the FINAL chosen delay.
// 	deficitSumMW = 0
// 	deficitMaxMW = 0
// 	if len(cache.RedfishObs) > 0 {
// 		sum, mx, ok := scoreCandidate(cache.RedfishObs)
// 		if ok {
// 			deficitSumMW = sum
// 			deficitMaxMW = mx
// 		}
// 	}

// 	selectedDelayMs = outDelayMs

// 	klog.V(6).Infof(
// 		"[analysis] fusion redfish delay-search chassis=%q kernel=%q epsMW=%.1f best(preRL)=(delayMs=%d score=%.1f max=%.1f) final(postRL)=(delayMs=%d deficitSum=%.1f deficitMax=%.1f) obs=%d",
// 		chassis, string(kernel), epsMW,
// 		bestDelayMs, bestScore, bestMaxSearch,
// 		selectedDelayMs, deficitSumMW, deficitMaxMW, len(cache.RedfishObs),
// 	)

// 	return selectedDelayMs, deficitSumMW, deficitMaxMW
// }
//-------------------

// func (m *FusionSubstrate) refreshRedfishObs(
// 	c *analysis.Cycle,
// 	cache *fusion.Cache,
// 	chassis string,
// 	kernel fusion.RedfishKernel,
// 	kernelMs int,
// ) (selectedDelayMs int, deficitSumMW float64, deficitMaxMW float64) {
// 	if c == nil || cache == nil || c.Mono == nil || cache.QuantumTicks == 0 {
// 		return 0, 0, 0
// 	}

// 	monoQuantumSec := c.Mono.Quantum().Seconds()
// 	if monoQuantumSec <= 0 {
// 		return 0, 0, 0
// 	}
// 	dtSec := float64(cache.QuantumTicks) * monoQuantumSec
// 	if dtSec <= 0 {
// 		return 0, 0, 0
// 	}

// 	// Cache horizon bounds (corrected ticks).
// 	endBin := cache.StartBin + fusion.BinIndex(cache.HorizonBins) - 1
// 	hStart := uint64(cache.StartBin) * cache.QuantumTicks
// 	hEnd := uint64(endBin+1) * cache.QuantumTicks

// 	// Baseline/fallback delay in ms (current behavior).
// 	baseDelayMs := config.RedfishDelayMs()
// 	if baseDelayMs < 0 {
// 		baseDelayMs = 0
// 	}

// 	// Slice 12C search defaults (can be promoted to config getters later).
// 	const (
// 		dMinMs      = 0
// 		dMaxMs      = 8000
// 		dStepMs     = 250
// 		rateLimitMs = 500 // per analysis trigger/cycle

// 		minObs = 5

// 		// NEW: score only recent obs for delay selection (helps extreme steps).
// 		recentScoreSec = 10.0
// 	)

// 	// Load previous chosen delay from state (per chassis) to rate-limit flapping.
// 	prevDelayMs := baseDelayMs
// 	stateKey := analysis.Key(MetricFusionRedfishDelayState, analysis.Labels{"chassis": chassis})
// 	if v, ok := c.State.Get(stateKey); ok {
// 		if i, ok2 := v.(int); ok2 {
// 			prevDelayMs = i
// 		} else if f, ok2 := v.(float64); ok2 {
// 			prevDelayMs = int(f)
// 		}
// 	}
// 	if prevDelayMs < dMinMs {
// 		prevDelayMs = dMinMs
// 	}
// 	if prevDelayMs > dMaxMs {
// 		prevDelayMs = dMaxMs
// 	}

// 	// Epsilon margin (mW): max(2W, 2% of median parts power over horizon).
// 	epsMW := 2000.0
// 	{
// 		parts := make([]float64, 0, cache.HorizonBins)
// 		for i := 0; i < cache.HorizonBins; i++ {
// 			pParts := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
// 			if pParts >= 0 {
// 				parts = append(parts, pParts)
// 			}
// 		}
// 		if len(parts) > 0 {
// 			sort.Float64s(parts)
// 			med := parts[len(parts)/2]
// 			e2 := 0.02 * med
// 			if e2 > epsMW {
// 				epsMW = e2
// 			}
// 			if epsMW < 2000.0 {
// 				epsMW = 2000.0
// 			}
// 		}
// 	}

// 	// Score deficit over observation points.
// 	// Returns: (sum deficit mW, max deficit mW, ok).
// 	scoreObs := func(obs []fusion.RedfishObs) (float64, float64, bool) {
// 		if len(obs) < minObs {
// 			return 0, 0, false
// 		}
// 		used := 0
// 		var sum, mx float64
// 		for i := range obs {
// 			o := obs[i]
// 			pPartsMW, ok := partsPowerAtObs(cache, dtSec, monoQuantumSec, o)
// 			if !ok {
// 				continue
// 			}
// 			used++
// 			def := pPartsMW - o.ValueMW - epsMW
// 			if def > 0 {
// 				sum += def
// 				if def > mx {
// 					mx = def
// 				}
// 			}
// 		}
// 		if used < minObs {
// 			return 0, 0, false
// 		}
// 		return sum, mx, true
// 	}

// 	// NEW: Filter obs to the most recent time window in corrected ticks.
// 	recentSubset := func(obs []fusion.RedfishObs) []fusion.RedfishObs {
// 		if len(obs) == 0 {
// 			return obs
// 		}
// 		// recentScoreSec seconds in corrected ticks
// 		recentTicks := uint64(math.Round(recentScoreSec / monoQuantumSec))
// 		if recentTicks == 0 {
// 			return obs
// 		}
// 		t1 := obs[len(obs)-1].MonoCorr
// 		t0 := uint64(0)
// 		if t1 > recentTicks {
// 			t0 = t1 - recentTicks
// 		}

// 		// Keep only obs with MonoCorr >= t0
// 		j := 0
// 		for ; j < len(obs); j++ {
// 			if obs[j].MonoCorr >= t0 {
// 				break
// 			}
// 		}
// 		if j >= len(obs) {
// 			return obs[len(obs)-1:]
// 		}
// 		return obs[j:]
// 	}

// 	// NEW: Delay-search scoring uses recent obs if possible, else falls back to full set.
// 	scoreForDelaySearch := func(obs []fusion.RedfishObs) (float64, float64, bool) {
// 		r := recentSubset(obs)
// 		if sum, mx, ok := scoreObs(r); ok {
// 			return sum, mx, true
// 		}
// 		// Fallback: too sparse in recent window
// 		return scoreObs(obs)
// 	}

// 	// --- Delay search (pre-rate-limit) ---
// 	bestDelayMs := baseDelayMs
// 	bestScore := math.Inf(1)
// 	bestMaxSearch := 0.0
// 	found := false

// 	for d := dMinMs; d <= dMaxMs; d += dStepMs {
// 		rawStart := hStart + uint64(c.Mono.TicksForMsCeil(d))
// 		rawEnd := hEnd + uint64(c.Mono.TicksForMsCeil(d))

// 		obs, n := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)
// 		if n <= 0 {
// 			continue
// 		}

// 		sum, mx, ok := scoreForDelaySearch(obs)
// 		if !ok {
// 			continue
// 		}

// 		if sum < bestScore || (sum == bestScore && d < bestDelayMs) {
// 			bestScore = sum
// 			bestDelayMs = d
// 			bestMaxSearch = mx
// 			found = true
// 		}
// 	}

// 	if !found || math.IsInf(bestScore, 1) {
// 		bestDelayMs = baseDelayMs
// 		bestScore = 0
// 		bestMaxSearch = 0
// 	}

// 	// Rate-limit vs previous chosen delay to avoid flapping.
// 	outDelayMs := bestDelayMs
// 	if outDelayMs > prevDelayMs+rateLimitMs {
// 		outDelayMs = prevDelayMs + rateLimitMs
// 	} else if outDelayMs < prevDelayMs-rateLimitMs {
// 		outDelayMs = prevDelayMs - rateLimitMs
// 	}
// 	if outDelayMs < dMinMs {
// 		outDelayMs = dMinMs
// 	}
// 	if outDelayMs > dMaxMs {
// 		outDelayMs = dMaxMs
// 	}

// 	// Persist chosen delay in state.
// 	c.State.Set(stateKey, outDelayMs)

// 	// Extract final obs with outDelayMs.
// 	rawStart := hStart + uint64(c.Mono.TicksForMsCeil(outDelayMs))
// 	rawEnd := hEnd + uint64(c.Mono.TicksForMsCeil(outDelayMs))

// 	obsFinal, nFinal := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)

// 	cache.RedfishObs = cache.RedfishObs[:0]
// 	if nFinal > 0 {
// 		cache.RedfishObs = append(cache.RedfishObs, obsFinal...)
// 	}

// 	// Diagnostics remain as before: computed on FINAL delay, on the full obs set.
// 	deficitSumMW = 0
// 	deficitMaxMW = 0
// 	if len(cache.RedfishObs) > 0 {
// 		sum, mx, ok := scoreObs(cache.RedfishObs)
// 		if ok {
// 			deficitSumMW = sum
// 			deficitMaxMW = mx
// 		}
// 	}

// 	selectedDelayMs = outDelayMs

// 	klog.V(6).Infof(
// 		"[analysis] fusion redfish delay-search(recent=%.1fs) chassis=%q kernel=%q epsMW=%.1f best(preRL)=(delayMs=%d score=%.1f max=%.1f) final(postRL)=(delayMs=%d deficitSum=%.1f deficitMax=%.1f) obs=%d",
// 		recentScoreSec, chassis, string(kernel), epsMW,
// 		bestDelayMs, bestScore, bestMaxSearch,
// 		selectedDelayMs, deficitSumMW, deficitMaxMW, len(cache.RedfishObs),
// 	)

// 	return selectedDelayMs, deficitSumMW, deficitMaxMW
// }

func (m *FusionSubstrate) refreshRedfishObs(
	c *analysis.Cycle,
	cache *fusion.Cache,
	chassis string,
	kernel fusion.RedfishKernel,
	kernelMs int,
) (selDelayMs int, deficitSumMW float64, deficitMaxMW float64) {
	// Defaults (safe fallbacks)
	selDelayMs = config.RedfishDelayMs()
	deficitSumMW = 0
	deficitMaxMW = 0

	if c == nil || cache == nil || c.Mono == nil || cache.QuantumTicks == 0 || c.State == nil {
		return
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()
	if monoQuantumSec <= 0 {
		return
	}
	dtSec := float64(cache.QuantumTicks) * monoQuantumSec
	if dtSec <= 0 {
		return
	}

	// Horizon in corrected domain.
	endBin := cache.StartBin + fusion.BinIndex(cache.HorizonBins) - 1
	hStart := uint64(cache.StartBin) * cache.QuantumTicks
	hEnd := uint64(endBin+1) * cache.QuantumTicks // exclusive

	// Fixed configured delay (fallback).
	cfgDelayMs := config.RedfishDelayMs()
	if cfgDelayMs < 0 {
		cfgDelayMs = 0
	}

	// Candidate search defaults.
	const (
		dMinMs          = 0
		dMaxMs          = 8000
		dStepMs         = 250
		maxStepPerCycle = 500 // ms change limit per analysis cycle

		// Score only most recent part of horizon (better for sharp steps).
		recentScoreSec = 20.0
	)

	// Candidate list.
	cands := make([]int, 0, (dMaxMs-dMinMs)/dStepMs+1)
	for d := dMinMs; d <= dMaxMs; d += dStepMs {
		cands = append(cands, d)
	}
	if len(cands) == 0 {
		cands = append(cands, cfgDelayMs)
	}

	// Hold-last-good delay in state.
	prevDelayMs := -1
	prevKey := analysis.Key(MetricFusionRedfishDelaySelectedMsState, analysis.Labels{"chassis": chassis})
	if v, ok := c.State.Get(prevKey); ok {
		if i, ok2 := v.(int); ok2 {
			prevDelayMs = i
		}
	}

	// Compute epsilon margin for scoring (noise guard):
	// eps = max(2000, 0.02 * median(parts power over recent bins)).
	epsMW := 2000.0
	if cache.HorizonBins > 0 {
		recentBins := int(math.Round(recentScoreSec / dtSec))
		if recentBins < 1 {
			recentBins = 1
		}
		if recentBins > cache.HorizonBins {
			recentBins = cache.HorizonBins
		}
		startIdx := cache.HorizonBins - recentBins

		parts := make([]float64, 0, recentBins)
		for i := startIdx; i < cache.HorizonBins; i++ {
			p := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
			if !finiteLocal(p) || p < 0 {
				p = 0
			}
			parts = append(parts, p)
		}
		med := medianLocal(parts)
		if finiteLocal(med) && med > 0 {
			epsMW = math.Max(epsMW, 0.02*med)
		}
	}

	// Define "recent" cutoff in corrected-domain ticks.
	scoreCut := hStart
	if recentScoreSec > 0 {
		recentTicks := uint64(math.Round(recentScoreSec / monoQuantumSec))
		if hEnd > recentTicks {
			scoreCut = hEnd - recentTicks
		}
	}

	// Score candidate by extracting obs, then summing deficit over recent obs only.
	// Search objective uses eps (noise margin).
	scoreCandidate := func(delayMs int) (score float64, obsOut []fusion.RedfishObs, ok bool) {
		if delayMs < 0 {
			delayMs = 0
		}

		dTicks := c.Mono.TicksForMsCeil(delayMs)
		rawStart := hStart + dTicks
		rawEnd := hEnd + dTicks

		obs, n := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)
		if n <= 0 || len(obs) == 0 {
			// Trigger: insufficient Redfish samples.
			return 0, nil, false
		}

		var s float64
		used := 0

		for i := range obs {
			o := obs[i]

			// Only score the most recent part.
			if o.MonoCorr+1 <= scoreCut {
				continue
			}

			k := fusion.BinIndex(int64(o.MonoCorr / cache.QuantumTicks))
			idx, okIdx := cacheIdx(cache, k) // from fusion_model.go, same package
			if !okIdx {
				continue
			}

			partsP := (cache.EpkgMJ[idx] + cache.EdramMJ[idx] + cache.EgpuMJ[idx]) / dtSec
			if !finiteLocal(partsP) || partsP < 0 {
				partsP = 0
			}

			// Search objective: max(0, parts - sys - eps)
			d := partsP - o.ValueMW - epsMW
			if d > 0 && finiteLocal(d) {
				s += d
			}
			used++
		}

		// Trigger: too few usable obs in scoring region.
		if used < 3 {
			return 0, nil, false
		}

		return s, obs, true
	}

	// Run search.
	bestDelayMs := -1
	bestScore := math.Inf(1)
	var bestObs []fusion.RedfishObs

	for _, d := range cands {
		s, obs, ok := scoreCandidate(d)
		if !ok {
			continue
		}
		// Tie-breaker: prefer smaller delay.
		if s < bestScore || (s == bestScore && (bestDelayMs < 0 || d < bestDelayMs)) {
			bestScore = s
			bestDelayMs = d
			bestObs = obs
		}
	}

	searchOK := (bestDelayMs >= 0) && len(bestObs) > 0 && finiteLocal(bestScore)

	// Choose delay:
	// - if search succeeded: bestDelayMs
	// - else: hold last known-good
	// - else: configured fixed delay
	chosenMs := cfgDelayMs
	if searchOK {
		chosenMs = bestDelayMs
	} else if prevDelayMs >= 0 {
		chosenMs = prevDelayMs
	}

	// Rate-limit delay changes.
	if prevDelayMs >= 0 && maxStepPerCycle > 0 {
		lo := prevDelayMs - maxStepPerCycle
		hi := prevDelayMs + maxStepPerCycle
		if chosenMs < lo {
			chosenMs = lo
		}
		if chosenMs > hi {
			chosenMs = hi
		}
		if chosenMs < 0 {
			chosenMs = 0
		}
	}

	// Persist last known-good.
	c.State.Set(prevKey, chosenMs)
	selDelayMs = chosenMs

	// Populate cache.RedfishObs for chosen delay (reuse bestObs if matches).
	cache.RedfishObs = cache.RedfishObs[:0]
	var chosenObs []fusion.RedfishObs

	if searchOK && chosenMs == bestDelayMs {
		chosenObs = bestObs
	} else {
		dTicks := c.Mono.TicksForMsCeil(chosenMs)
		rawStart := hStart + dTicks
		rawEnd := hEnd + dTicks
		obs, n := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)
		if n > 0 && len(obs) > 0 {
			chosenObs = obs
		}
	}

	if len(chosenObs) > 0 {
		cache.RedfishObs = append(cache.RedfishObs, chosenObs...)
	}

	// Diagnostics return values: deficit (no eps) over the same recent region.
	// deficit = max(0, parts - sys)
	if len(chosenObs) > 0 {
		var sum float64
		var mx float64

		used := 0
		for i := range chosenObs {
			o := chosenObs[i]
			if o.MonoCorr+1 <= scoreCut {
				continue
			}

			k := fusion.BinIndex(int64(o.MonoCorr / cache.QuantumTicks))
			idx, okIdx := cacheIdx(cache, k)
			if !okIdx {
				continue
			}

			partsP := (cache.EpkgMJ[idx] + cache.EdramMJ[idx] + cache.EgpuMJ[idx]) / dtSec
			if !finiteLocal(partsP) || partsP < 0 {
				partsP = 0
			}

			d := partsP - o.ValueMW
			if d > 0 && finiteLocal(d) {
				sum += d
				if d > mx {
					mx = d
				}
			}
			used++
		}

		if used >= 1 {
			deficitSumMW = sum
			deficitMaxMW = mx
		}
	}

	klog.V(6).Infof("[analysis] fusion redfish obs chassis=%q n=%d delayMs=%d kernel=%q searchOK=%v",
		chassis, len(cache.RedfishObs), selDelayMs, string(kernel), searchOK)

	return
}

func partsPowerAtObs(cache *fusion.Cache, dtSec, monoQuantumSec float64, obs fusion.RedfishObs) (float64, bool) {
	if cache == nil || cache.QuantumTicks == 0 || cache.HorizonBins <= 0 {
		return 0, false
	}
	if dtSec <= 0 || monoQuantumSec <= 0 {
		return 0, false
	}

	switch obs.Kernel {
	case fusion.KernelInstant:
		k := fusion.BinIndex(int64(obs.MonoCorr / cache.QuantumTicks))
		i, ok := cacheIdx(cache, k)
		if !ok {
			return 0, false
		}
		pParts := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
		if pParts < 0 {
			pParts = 0
		}
		return pParts, true

	case fusion.KernelAvg1sTrailing:
		Tms := obs.KernelMs
		if Tms <= 0 {
			Tms = 1000
		}
		Tsec := float64(Tms) / 1000.0
		if Tsec <= 0 {
			return 0, false
		}

		// Trailing window in ticks, in corrected domain.
		Tticks := uint64(math.Round(Tsec / monoQuantumSec))
		if Tticks == 0 {
			// fall back to instant
			k := fusion.BinIndex(int64(obs.MonoCorr / cache.QuantumTicks))
			i, ok := cacheIdx(cache, k)
			if !ok {
				return 0, false
			}
			pParts := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
			if pParts < 0 {
				pParts = 0
			}
			return pParts, true
		}

		t1 := obs.MonoCorr
		t0 := uint64(0)
		if t1 > Tticks {
			t0 = t1 - Tticks
		}

		kStart := fusion.BinIndex(int64(t0 / cache.QuantumTicks))
		kEnd := fusion.BinIndex(int64((t1 - 1) / cache.QuantumTicks))

		var sumParts float64
		var sumSec float64

		for k := kStart; k <= kEnd; k++ {
			i, ok := cacheIdx(cache, k)
			if !ok {
				continue
			}
			b0 := uint64(k) * cache.QuantumTicks
			b1 := b0 + cache.QuantumTicks

			lo := maxU64(t0, b0)
			hi := minU64(t1, b1)
			if hi <= lo {
				continue
			}
			segSec := float64(hi-lo) * monoQuantumSec
			if segSec <= 0 {
				continue
			}
			sumSec += segSec

			pParts := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
			if pParts < 0 {
				pParts = 0
			}
			sumParts += pParts * segSec
		}

		if sumSec <= 0 {
			return 0, false
		}
		return sumParts / sumSec, true

	default:
		return 0, false
	}
}

func putCache(s *analysis.StateStore, key analysis.MetricKey, cache *fusion.Cache) {
	if s == nil || cache == nil {
		return
	}
	s.Set(key, cache)
}

// Safety helper for debugging.
func (m *FusionSubstrate) String() string {
	return fmt.Sprintf("%s", m.ID())
}

// -------------------- Slice 12C local helpers (keep file self-contained) --------------------

func finiteLocal(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

func medianLocal(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	tmp := make([]float64, 0, len(xs))
	for _, v := range xs {
		if finiteLocal(v) && v >= 0 {
			tmp = append(tmp, v)
		}
	}
	if len(tmp) == 0 {
		return 0
	}
	sort.Float64s(tmp)
	n := len(tmp)
	if n%2 == 1 {
		return tmp[n/2]
	}
	return 0.5 * (tmp[n/2-1] + tmp[n/2])
}
