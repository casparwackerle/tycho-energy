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
		klog.V(5).Infof("[analysis] fusion diag window=%s chassis=%q epkg_mj=%.3f edram_mj=%.3f egpu_mj=%.3f instr=%.0f redfish_obs=%d",
			c.Window.String(), chassis, epkg, edram, egpu, instr, len(cache.RedfishObs))
	}

	// Minimal deep-debug, limited count.
	klog.V(6).Infof("[analysis] fusion update chassis=%q kStart=%s kNow=%s qTicks=%d hBins=%d rapl(samples=%d intervals=%d) bpf(samples=%d intervals=%d) gpu(uuids=%d bins=%d)",
		chassis, kStart.String(), kNow.String(), qTicks, hBins,
		raplSamples, raplIntervals, bpfSamples, bpfIntervals, gpuUUIDs, gpuBins,
	)

	return nil
}

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
		dMinMs  = 0
		dMaxMs  = 12000
		dStepMs = 250

		// Slew rate: stable in steady-state, fast only on detected steps.
		maxStepPerCycleNormal = 500  // ms change limit per analysis cycle (steady-state)
		maxStepPerCycleStep   = 3000 // ms change limit per analysis cycle (step-only fast path)

		// Score only most recent part of horizon (better for sharp steps).
		recentScoreSec = 20.0

		// Step detector threshold on parts power change (mW).
		stepTauMW = 20000.0 // 20 W
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

	// --- Slice 15: step proxy computed from already-available parts power series ---
	stepDetected := false
	stepMaxDeltaMW := 0.0

	if cache.HorizonBins > 1 {
		recentBins := int(math.Round(recentScoreSec / dtSec))
		if recentBins < 2 {
			recentBins = 2
		}
		if recentBins > cache.HorizonBins {
			recentBins = cache.HorizonBins
		}
		startIdx := cache.HorizonBins - recentBins

		prevP := -1.0
		for i := startIdx; i < cache.HorizonBins; i++ {
			p := (cache.EpkgMJ[i] + cache.EdramMJ[i] + cache.EgpuMJ[i]) / dtSec
			if !finiteLocal(p) || p < 0 {
				p = 0
			}
			if prevP >= 0 {
				d := math.Abs(p - prevP)
				if finiteLocal(d) && d > stepMaxDeltaMW {
					stepMaxDeltaMW = d
				}
			}
			prevP = p
		}

		if stepMaxDeltaMW > stepTauMW {
			stepDetected = true
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

		obs, n := fusion.ExtractRedfishObs(c, chassis, dTicks, rawStart, rawEnd, kernel, kernelMs)
		if n <= 0 || len(obs) == 0 {
			return 0, nil, false
		}

		var s float64
		used := 0

		for i := range obs {
			o := obs[i]

			// HARD GUARD: never allow MonoCorr==0 into scoring path.
			if o.MonoCorr == 0 {
				continue
			}

			// Only score the most recent part.
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
	chosenMs := cfgDelayMs
	if searchOK {
		chosenMs = bestDelayMs
	} else if prevDelayMs >= 0 {
		chosenMs = prevDelayMs
	}

	// Rate-limit delay changes (adaptive slew).
	maxStepPerCycle := maxStepPerCycleNormal
	if stepDetected {
		maxStepPerCycle = maxStepPerCycleStep
	}

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
		obs, n := fusion.ExtractRedfishObs(c, chassis, dTicks, rawStart, rawEnd, kernel, kernelMs)
		if n > 0 && len(obs) > 0 {
			chosenObs = obs
		}
	}

	if len(chosenObs) > 0 {
		// HARD GUARD: drop MonoCorr==0 even if upstream accidentally emits it.
		for i := range chosenObs {
			if chosenObs[i].MonoCorr == 0 {
				continue
			}
			cache.RedfishObs = append(cache.RedfishObs, chosenObs[i])
		}
	}

	// Diagnostics return values: deficit (no eps) over the same recent region.
	// deficit = max(0, parts - sys)
	if len(cache.RedfishObs) > 0 {
		var sum float64
		var mx float64

		used := 0
		for i := range cache.RedfishObs {
			o := cache.RedfishObs[i]
			if o.MonoCorr == 0 {
				continue
			}
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

	// NEW: actionable log when something looks wrong (search failed or no usable obs).
	if klog.V(2).Enabled() && (!searchOK || len(cache.RedfishObs) == 0) {
		// Quick breakdown for debugging: how many raw obs would have been dropped due to MonoCorr==0?
		dTicks := c.Mono.TicksForMsCeil(selDelayMs)
		rawStart := hStart + dTicks
		rawEnd := hEnd + dTicks
		obs, _ := fusion.ExtractRedfishObs(c, chassis, dTicks, rawStart, rawEnd, kernel, kernelMs)

		var nZero int
		var nRecent int
		var nIdxOK int

		for i := range obs {
			o := obs[i]
			if o.MonoCorr == 0 {
				nZero++
				continue
			}
			if o.MonoCorr+1 <= scoreCut {
				continue
			}
			nRecent++
			k := fusion.BinIndex(int64(o.MonoCorr / cache.QuantumTicks))
			if _, okIdx := cacheIdx(cache, k); okIdx {
				nIdxOK++
			}
		}

		klog.V(5).Infof(
			"[analysis] fusion redfish diag chassis=%q searchOK=%v bestDelayMs=%d chosenMs=%d rawObs=%d zeroMonoCorr=%d recent=%d recentIdxOK=%d kept=%d epsMW=%.3f stepDetected=%v stepMaxDeltaMW=%.3f scoreCut=%d h=[%d,%d)",
			chassis, searchOK, bestDelayMs, selDelayMs, len(obs), nZero, nRecent, nIdxOK, len(cache.RedfishObs),
			epsMW, stepDetected, stepMaxDeltaMW, scoreCut, hStart, hEnd,
		)
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
