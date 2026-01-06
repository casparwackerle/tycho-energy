// file: internal/tycho/analysis/metrics/fusion_substrate.go
package analysismetrics

import (
	"fmt"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/fusion"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

const (
	MetricFusionCache analysis.MetricID = "fusion_cache" // state key family
	MetricFusionReady analysis.MetricID = "fusion_ready" // state key family
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
	m.refreshRedfishObs(c, cache, chassis, kernel, kernelMs)

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

func (m *FusionSubstrate) refreshRedfishObs(
	c *analysis.Cycle,
	cache *fusion.Cache,
	chassis string,
	kernel fusion.RedfishKernel,
	kernelMs int,
) {
	if c == nil || cache == nil || c.Mono == nil || cache.QuantumTicks == 0 {
		return
	}

	// Build a raw window covering the cache horizon (in corrected ticks) and shift forward by Redfish delay.
	endBin := cache.StartBin + fusion.BinIndex(cache.HorizonBins) - 1
	hStart := uint64(cache.StartBin) * cache.QuantumTicks
	hEnd := uint64(endBin+1) * cache.QuantumTicks

	rfDelay := c.Mono.TicksForMsCeil(config.RedfishDelayMs())
	rawStart := hStart + rfDelay
	rawEnd := hEnd + rfDelay

	// rawStart/rawEnd are already in raw time (corrected + delay).
	// Pass delayTicks=0 so ExtractRedfishObs does not shift a second time.
	obs, n := fusion.ExtractRedfishObs(c, chassis, 0, rawStart, rawEnd, kernel, kernelMs)

	cache.RedfishObs = cache.RedfishObs[:0]
	if n > 0 {
		cache.RedfishObs = append(cache.RedfishObs, obs...)
	}

	// second deep-debug (kept minimal)
	klog.V(6).Infof("[analysis] fusion redfish obs chassis=%q n=%d delayTicks=%d kernel=%q",
		chassis, len(cache.RedfishObs), rfDelay, string(kernel))
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
