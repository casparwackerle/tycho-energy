package calibration

import (
	"context"
	"math"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"

	gpuCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/gpu"
)

// 	klog.V(2).Infof("gpu.PollProbe: medianΔ=%.1fms => best=%dms (min=%d)", medianDt*1000, best, minMs)
// 	return best, true
// }

// Package-private stash for the latest GPU probe report.
var gpuReport *PollProbeReport

// LastGPUReport returns the most recent PollProbeReport produced by PollProbeGPU.
func LastGPUReport() *PollProbeReport { return gpuReport }

// PollProbeGPU: dynamic search for a conservative, stable GPU polling period.
// Strategy: exponential descent from a safe start → bracket (lastGood, firstBad) → bisection/refine → early stop.
// GPU thresholds (stricter than Redfish):
//   - err_rate ≤ 1% (drop ≤ 0.01)
//   - effRate  ≥ 0.90
//
// Notes:
//   - If no GPU is present/initialized, return ok=false and log "skipped".
//   - Respect minMs guard (≥100ms). NVML commonly averages ~1s; oversampling is allowed but provides no extra information.
func PollProbeGPU(ctx context.Context, mono *clock.Mono, budgetSec int, minMs int) (bestMS int, ok bool) {
	if budgetSec <= 0 {
		klog.V(2).Infof("phase=calibrate component=gpu step=start ok=false reason=bad_budget budgetSec=%d", budgetSec)
		gpuReport = nil
		return 0, false
	}
	if minMs < 100 {
		minMs = 100 // guard floor
	}

	bufMgr := ring.NewManager()
	gpuSz := ring.SizeForWindow(budgetSec, minMs)
	gpuBuf := ring.GetOrCreateSync[ring.GpuTick](bufMgr, "gpu-cal", gpuSz)
	g := gpuCollector.New(gpuCollector.Config{Buf: gpuBuf, Mono: mono})

	// Init collector → if it fails, treat as "skipped_or_failed".
	if err := g.Init(ctx); err != nil {
		klog.V(2).Infof("phase=calibrate component=gpu step=start ok=false reason=init_failed err=%v", err)
		gpuReport = &PollProbeReport{
			Component:   "gpu",
			CandidateMs: 0,
			MinGuardMs:  minMs,
			WindowSec:   0,
			EarlyStop:   false,
			Reason:      "skipped_or_failed",
			Path:        nil,
		}
		return 0, false
	}

	totalBudget := time.Duration(budgetSec) * time.Second
	deadline := time.Now().Add(totalBudget)

	// Defaults (can later be wired to internal getters)
	startMs := 1000
	targetPrecMs := 20
	windowSec := 30
	coolOff := 5 * time.Second // GPUs tolerate aggressive probing; short cool-off is fine
	minSamples := 8

	if startMs < minMs {
		startMs = minMs
	}
	if time.Until(deadline) < time.Duration(windowSec)*time.Second {
		startMs = maxInt(startMs, minMs)
	}

	path := make([]PollProbePoint, 0, 12)
	report := &PollProbeReport{
		Component:  "gpu",
		MinGuardMs: minMs,
		WindowSec:  windowSec,
		Path:       nil,
	}

	// ---- Quick presence sanity check: try a brief single probe at startMs.
	pp0 := probeGpuOnce(ctx, g, gpuBuf, startMs, 5 /*sec*/)
	if pp0.Samples == 0 && pp0.Errs == 0 {
		// No movement at all → likely no devices or disabled collector
		klog.V(2).Infof("phase=calibrate component=gpu step=probe ok=false reason=no_samples_present")
		report.CandidateMs = 0
		report.Reason = "skipped_or_failed"
		gpuReport = report
		return 0, false
	}
	// If the quick probe had time, keep it as the first point; otherwise we run again in descend
	if pp0.Samples > 0 {
		path = append(path, pp0)
	}

	// ---- Phase 1: Exponential descent ---------------------------------------
	lastGood := startMs
	firstBad := 0
	p := startMs

	for {
		if time.Now().After(deadline) {
			klog.V(2).Infof("phase=calibrate component=gpu step=descend ok=false reason=budget_exhausted")
			break
		}
		// Avoid duplicating the quick probe if it was at p=startMs and long enough
		var pp PollProbePoint
		if len(path) > 0 && path[len(path)-1].Ms == p && path[len(path)-1].Samples >= 5 {
			pp = path[len(path)-1]
		} else {
			pp = probeGpuOnce(ctx, g, gpuBuf, p, windowSec)
			path = append(path, pp)
		}

		klog.V(2).Infof("phase=calibrate component=gpu step=descend p=%dms samples=%d errs=%d effRate=%.2f ok=%v",
			pp.Ms, pp.Samples, pp.Errs, pp.EffRate, pp.Ok)

		if pp.Ok {
			lastGood = p
			if p == minMs {
				break
			}
			next := int(math.Floor(float64(p) / 1.5))
			if next < minMs {
				next = minMs
			}
			okEarly, reason := EarlyStopSatisfied(path, targetPrecMs, minSamples)
			if okEarly {
				report.EarlyStop = true
				report.Reason = reason
				break
			}
			p = next
			continue
		}

		// Bad at p → we’ve crossed the safe boundary; cool off and bracket.
		time.Sleep(coolOff)
		firstBad = p
		break
	}

	// If never failed and reached floor or early-stopped, accept lastGood
	if firstBad == 0 {
		report.CandidateMs = lastGood
		report.Path = path
		if report.Reason == "" {
			report.Reason = "no_failure_within_floor_or_early_stop"
		}
		gpuReport = report

		klog.V(2).Infof("phase=calibrate component=gpu step=summary ok=true candidate=%dms early=%v reason=%s points=%d",
			report.CandidateMs, report.EarlyStop, report.Reason, len(report.Path))
		return report.CandidateMs, true
	}

	// ---- Phase 2: Bisection --------------------------------------------------
	lo := lastGood
	hi := firstBad

	for {
		if absInt(hi-lo) <= targetPrecMs || time.Now().Add(time.Duration(windowSec)*time.Second).After(deadline) {
			break
		}
		mid := (lo + hi) / 2
		if mid < minMs {
			mid = minMs
		}

		pp := probeGpuOnce(ctx, g, gpuBuf, mid, windowSec)
		path = append(path, pp)

		klog.V(2).Infof("phase=calibrate component=gpu step=bisect p=%dms samples=%d errs=%d effRate=%.2f ok=%v",
			pp.Ms, pp.Samples, pp.Errs, pp.EffRate, pp.Ok)

		if pp.Ok {
			lo = mid
			okEarly, reason := EarlyStopSatisfied(path, targetPrecMs, minSamples)
			if okEarly {
				report.EarlyStop = true
				report.Reason = reason
				break
			}
		} else {
			time.Sleep(coolOff)
			hi = mid
		}
	}

	report.CandidateMs = maxInt(lo, minMs)
	report.Path = path
	if report.Reason == "" {
		report.Reason = "bracketed_conservative_choice"
	}
	gpuReport = report

	klog.V(2).Infof("phase=calibrate component=gpu step=summary ok=true candidate=%dms early=%v reason=%s points=%d",
		report.CandidateMs, report.EarlyStop, report.Reason, len(report.Path))

	return report.CandidateMs, true
}

// probeGpuOnce runs one measurement window at 'ms' and evaluates strict GPU criteria:
//   - drop ≤ 0.01 (i.e., err_rate ≤ 1%)
//   - effRate ≥ 0.90
//
// Because the GPU collector pushes one sample PER DEVICE per tick, we only check for
// "at least one new sample" per tick to count an OK arrival.
func probeGpuOnce(ctx context.Context, g *gpuCollector.Collector, gpuBuf interface {
	PeekNewestCopy(int) []ring.GpuTick
}, ms int, windowSec int) PollProbePoint {
	per := time.Duration(ms) * time.Millisecond
	window := time.Duration(windowSec) * time.Second

	totalTicks := 0
	okArrivals := 0
	drops := 0
	times := make([]time.Time, 0, 64)

	var lastMono uint64
	var haveLast bool
	var longGapCount int
	var lastArrival time.Time

	windowCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	BusyLoop(windowCtx, window, per, func(ts time.Time) {
		totalTicks++
		g.Collect(windowCtx, ts)
		if s, got := PeekOne(gpuBuf); got {
			mono := s.SampleMeta.Mono
			if !haveLast || mono != lastMono {
				okArrivals++
				times = append(times, ts)
				if !lastArrival.IsZero() {
					if gap := ts.Sub(lastArrival); gap >= 3*per {
						longGapCount++
					}
				}
				lastArrival = ts
				lastMono = mono
				haveLast = true
				return
			}
		}
		drops++
	})

	eff := EffectiveArrivalRate(ms, times)
	drop := DropRatio(totalTicks, okArrivals)

	pp := PollProbePoint{
		Ms:      ms,
		Ok:      true,
		Samples: totalTicks,
		Errs:    drops,
		EffRate: eff,
	}

	// Strict GPU failure rules
	if drop > 0.01 {
		pp.Ok = false
		pp.Notes = "high_drop"
	}
	if eff < 0.90 {
		pp.Ok = false
		if pp.Notes == "" {
			pp.Notes = "low_eff_rate"
		} else {
			pp.Notes += ",low_eff_rate"
		}
	}
	// Long gaps usually shouldn’t occur at sane ms values, but keep as a soft hint
	if longGapCount >= 2 {
		if pp.Notes == "" {
			pp.Notes = "repeated_long_gaps"
		} else {
			pp.Notes += ",repeated_long_gaps"
		}
	}

	klog.V(2).Infof("phase=calibrate component=gpu step=probe p=%dms total=%d okTicks=%d drops=%d drop=%.3f effRate=%.2f longGaps=%d ok=%v notes=%s",
		ms, totalTicks, okArrivals, drops, drop, eff, longGapCount, pp.Ok, pp.Notes)

	return pp
}

// Returns (p5, true) on success, or (0, false) if there are no usable samples
// or the context is canceled. Computes the 5th percentile of the *total GPU power*
// per tick (sum across all devices in each tick).
func IdleBaselineGPUFromSnap(
	ctx context.Context,
	mono *clock.Mono, // kept for signature consistency with other analyzers; not used here
	snap []ring.GpuTick,
) (float64, bool) {
	if len(snap) == 0 {
		return 0, false
	}

	values := make([]float64, 0, len(snap)) // totals per tick, in Watts
	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return 0, false
		default:
		}

		tick := snap[i]
		var totalMilliW int
		var hasValid bool

		for _, dev := range tick.Devices {
			// Accept zeros (true idle), skip invalid negatives.
			if dev.PowerMilliW < 0 {
				continue
			}
			totalMilliW += dev.PowerMilliW
			hasValid = true
		}

		if hasValid {
			values = append(values, float64(totalMilliW)/1000.0) // mW -> W
		}
	}

	if len(values) == 0 {
		return 0, false
	}
	return P5(values), true
}
