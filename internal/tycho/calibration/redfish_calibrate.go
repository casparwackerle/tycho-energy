package calibration

import (
	"context"
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	rfCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/redfish"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"k8s.io/klog/v2"
)

// Package-private stash for the latest probe report.
// Engine can read it after calling PollProbeRedfish and place into Results.Redfish.
var redfishReport *PollProbeReport

// LastRedfishReport returns the most recent PollProbeReport produced by PollProbeRedfish.
// Returns nil if none yet.
func LastRedfishReport() *PollProbeReport {
	return redfishReport
}

// PollProbeRedfish: dynamic search for a conservative, stable polling period.
// Strategy: exponential descent from a safe start → bracket (lastGood, firstBad) → bisection/refine → early stop.
// Success criteria tolerate Redfish irregularity: we judge mainly by effective arrival rate and drop ratio.
// Failure = high drop (>5%) OR effRate < 0.4 over the window (proxy for transport/timeouts/throttle).
func PollProbeRedfish(ctx context.Context, mono *clock.Mono, budgetSec int, minMs int) (bestMS int, ok bool) {
	// ---- Guards and setup ----------------------------------------------------
	if budgetSec <= 0 {
		klog.V(2).Infof("phase=calibrate component=redfish step=start ok=false reason=bad_budget budgetSec=%d", budgetSec)
		redfishReport = nil
		return 0, false
	}
	if minMs < 500 {
		minMs = 500 // hard safety floor, as requested
	}

	bufMgr := ring.NewManager()
	// Allocate a reasonably large buffer based on the MIN period to avoid churn.
	rfSz := ring.SizeForWindow(budgetSec, minMs)
	rfBuf := ring.GetOrCreateSync[ring.RedfishSample](bufMgr, "rf-cal", rfSz)
	rf := rfCollector.New(rfCollector.Config{Buf: rfBuf, Mono: mono})

	totalBudget := time.Duration(budgetSec) * time.Second
	deadline := time.Now().Add(totalBudget)

	// Default knobs (can be made configurable later via getters)
	startMs := 3000
	targetPrecMs := 50
	windowSec := 45 // per-probe measurement window
	coolOff := 15 * time.Second
	minSamples := 6 // need a handful of arrivals to judge stability

	if startMs < minMs {
		startMs = minMs
	}
	if time.Until(deadline) < (time.Duration(windowSec) * time.Second) {
		// If there's barely any time, try a single probe at startMs.
		startMs = maxInt(startMs, minMs)
	}

	path := make([]PollProbePoint, 0, 12)
	report := &PollProbeReport{
		Component:  "redfish",
		MinGuardMs: minMs,
		WindowSec:  windowSec,
		Path:       nil, // fill later
	}

	// ---- Phase 1: Exponential descent (find firstBad) ------------------------
	lastGood := startMs
	firstBad := 0
	p := startMs

	for {
		if time.Now().After(deadline) {
			klog.V(2).Infof("phase=calibrate component=redfish step=descend ok=false reason=budget_exhausted")
			break
		}
		pp := probeRedfishOnce(ctx, rf, rfBuf, p, windowSec)
		path = append(path, pp)

		klog.V(2).Infof("phase=calibrate component=redfish step=descend p=%dms samples=%d errs=%d effRate=%.2f ok=%v",
			pp.Ms, pp.Samples, pp.Errs, pp.EffRate, pp.Ok)

		if pp.Ok {
			lastGood = p
			// Try faster unless already at floor
			if p == minMs {
				break
			}
			next := int(math.Floor(float64(p) / 1.5))
			if next < minMs {
				next = minMs
			}
			// Early stop: if we are already close to floor and stable, accept.
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

	// If never failed and reached minMs (or early stopped), accept lastGood (conservative).
	if firstBad == 0 {
		report.CandidateMs = lastGood
		report.Path = path
		if report.Reason == "" {
			report.Reason = "no_failure_within_floor_or_early_stop"
		}
		redfishReport = report
		klog.V(2).Infof("phase=calibrate component=redfish step=summary ok=true candidate=%dms early=%v reason=%s points=%d",
			report.CandidateMs, report.EarlyStop, report.Reason, len(report.Path))
		return report.CandidateMs, true
	}

	// ---- Phase 2: Bisection between [lastGood, firstBad] ---------------------
	lo := lastGood
	hi := firstBad

	for {
		// Stop if precision reached or budget out
		if absInt(hi-lo) <= targetPrecMs || time.Now().Add(time.Duration(windowSec)*time.Second).After(deadline) {
			break
		}
		mid := (lo + hi) / 2
		// Snap to at least minMs
		if mid < minMs {
			mid = minMs
		}

		pp := probeRedfishOnce(ctx, rf, rfBuf, mid, windowSec)
		path = append(path, pp)

		klog.V(2).Infof("phase=calibrate component=redfish step=bisect p=%dms samples=%d errs=%d effRate=%.2f ok=%v",
			pp.Ms, pp.Samples, pp.Errs, pp.EffRate, pp.Ok)

		if pp.Ok {
			lo = mid // we can go faster (smaller ms)
			// Optional refine: check early stop around lo
			okEarly, reason := EarlyStopSatisfied(path, targetPrecMs, minSamples)
			if okEarly {
				report.EarlyStop = true
				report.Reason = reason
				break
			}
		} else {
			time.Sleep(coolOff)
			hi = mid // too aggressive
		}
	}

	// Conservative choice: take the larger ms that was OK (lo)
	report.CandidateMs = maxInt(lo, minMs)
	report.Path = path
	if report.Reason == "" {
		report.Reason = "bracketed_conservative_choice"
	}
	redfishReport = report

	klog.V(2).Infof("phase=calibrate component=redfish step=summary ok=true candidate=%dms early=%v reason=%s points=%d",
		report.CandidateMs, report.EarlyStop, report.Reason, len(report.Path))

	return report.CandidateMs, true
}

// probeRedfishOnce runs a single measurement window at the requested polling period.
// It judges success/failure using:
//   - err_rate > 5%  => fail
//   - effRate  < 0.4 => fail
//
// Jitter is tolerated; we only track whether new samples arrived and how often.
func probeRedfishOnce(ctx context.Context, rf *rfCollector.Collector, rfBuf interface {
	PeekNewestCopy(int) []ring.RedfishSample
}, ms int, windowSec int) PollProbePoint {
	per := time.Duration(ms) * time.Millisecond
	window := time.Duration(windowSec) * time.Second

	// We count "total polls" as BusyLoop ticks and "ok" when the buffer shows a NEW sample (mono changed).
	total := 0
	okCount := 0
	errs := 0
	times := make([]time.Time, 0, 64)

	var lastMono uint64
	var haveLast bool
	var lastArrival time.Time
	var longGapCount int

	windowCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	BusyLoop(windowCtx, window, per, func(ts time.Time) {
		total++
		rf.Collect(windowCtx, ts)
		if s, got := PeekOne(rfBuf); got {
			mono := s.SampleMeta.Mono
			if !haveLast || mono != lastMono {
				okCount++
				times = append(times, ts)
				// Track long gaps as "repeated timeouts/throttle" indicator
				if !lastArrival.IsZero() {
					gap := ts.Sub(lastArrival)
					if gap >= 3*per {
						longGapCount++
					}
				}
				lastArrival = ts
				lastMono = mono
				haveLast = true
				return
			}
		}
		// If we didn’t see a new sample this tick, treat it as a "drop".
		// (Collector may still be heartbeat-limited or BMC silent.)
		errs++
	})

	// Effective arrival rate (0..1) relative to requested period
	eff := EffectiveArrivalRate(ms, times)
	drop := DropRatio(total, okCount)

	pp := PollProbePoint{
		Ms:      ms,
		Ok:      true, // assume ok, then demote on failure rules
		Samples: total,
		Errs:    errs,
		EffRate: eff,
		Notes:   "",
	}

	// Failure rules (Redfish-friendly)
	if drop > 0.05 {
		pp.Ok = false
		pp.Notes = "high_drop"
	}
	if eff < 0.40 {
		pp.Ok = false
		if pp.Notes == "" {
			pp.Notes = "low_eff_rate"
		} else {
			pp.Notes += ",low_eff_rate"
		}
	}
	if longGapCount >= 2 {
		// Proxy for repeated timeouts/429 or throttling (cannot see HTTP codes directly here)
		pp.Ok = false
		if pp.Notes == "" {
			pp.Notes = "repeated_long_gaps"
		} else {
			pp.Notes += ",repeated_long_gaps"
		}
	}

	// Final structured log for this probe
	klog.V(2).Infof("phase=calibrate component=redfish step=probe p=%dms total=%d okTicks=%d drops=%d drop=%.2f effRate=%.2f longGaps=%d ok=%v notes=%s",
		ms, total, okCount, errs, drop, eff, longGapCount, pp.Ok, pp.Notes)

	return pp
}

// --- small ints utilities (keep local to this file if you prefer) ---

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Returns (p5, true) on success, or (0, false) if there are no usable samples
// or the context is canceled.
func IdleBaselineRedfishFromSnap(
	ctx context.Context,
	mono *clock.Mono, // signature parity
	snap []ring.RedfishSample,
) (float64, bool) {
	if len(snap) == 0 {
		return 0, false
	}

	// (Optional) Check effective time span
	// If SampleMeta.Mono exists and you want a minimal span, enforce it here.
	// Comment out if you don't want a span guard.
	firstMono := snap[0].SampleMeta.Mono
	lastMono := snap[0].SampleMeta.Mono

	values := make([]float64, 0, len(snap))
	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return 0, false
		default:
		}
		s := snap[i]
		if s.SampleMeta.Mono < firstMono {
			firstMono = s.SampleMeta.Mono
		}
		if s.SampleMeta.Mono > lastMono {
			lastMono = s.SampleMeta.Mono
		}
		// Accept zeros (idle), skip invalid
		if s.PowerWatts < 0 || math.IsNaN(s.PowerWatts) || math.IsInf(s.PowerWatts, 0) {
			continue
		}
		values = append(values, s.PowerWatts)
	}

	if len(values) == 0 {
		return 0, false
	}

	// Outlier-robust idle baseline
	p5, _, n := DeSpikeP5(values)
	if n == 0 {
		return 0, false
	}
	return p5, true
}
