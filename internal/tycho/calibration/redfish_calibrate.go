package calibration

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	rfCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/redfish"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
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

// PollProbeRedfish performs a simple hyperpoll-based calibration for Redfish.
//
// Strategy:
//   - Poll Redfish at a fixed period `minMs` for `budgetSec` seconds.
//   - Every time the newest Redfish sample's Mono changes, treat this as a NEW
//     published BMC reading and record the arrival timestamp.
//   - After the window, compute the median inter-arrival time between NEW samples.
//   - Derive a final polling period:
//   - If continuous heartbeat is enabled:
//     poll = max(minMs, median_interval / 3)
//     (poll faster than BMC publish, auto-heartbeat handles freshness).
//   - Otherwise:
//     poll = max(minMs, median_interval)
//   - Populate a PollProbeReport for logging and return the chosen period.
func PollProbeRedfish(ctx context.Context, mono *clock.Mono, budgetSec int, minMs int) (bestMS int, ok bool) {
	// ---- Guards and setup ----------------------------------------------------
	if budgetSec <= 0 {
		klog.V(2).Infof("phase=calibrate component=redfish step=start ok=false reason=bad_budget budgetSec=%d", budgetSec)
		redfishReport = nil
		return 0, false
	}
	if minMs < 1 {
		klog.V(2).Infof("phase=calibrate component=redfish step=start ok=false reason=bad_minMs minMs=%d", minMs)
		redfishReport = nil
		return 0, false
	}

	pollPer := time.Duration(minMs) * time.Millisecond
	window := time.Duration(budgetSec) * time.Second
	autoHeartbeat := config.CalibrationRedfishContinuousHeartbeatEnabled()

	klog.V(2).Infof("phase=calibrate component=redfish step=start budgetSec=%d minMs=%d autoHeartbeat=%v",
		budgetSec, minMs, autoHeartbeat)

	bufMgr := ring.NewManager()
	rfSz := ring.SizeForWindow(budgetSec, minMs)
	rfBuf := ring.GetOrCreateSync[ring.RedfishSample](bufMgr, "rf-cal", rfSz)
	rf := rfCollector.New(rfCollector.Config{Buf: rfBuf, Mono: mono})

	if rf == nil {
		klog.V(2).Infof("phase=calibrate component=redfish step=start ok=false reason=no_redfish_client")
		redfishReport = nil
		return 0, false
	}

	// Report skeleton (Path will be filled with a single "hyperpoll" point).
	report := &PollProbeReport{
		Component:  "redfish",
		MinGuardMs: minMs,
		WindowSec:  budgetSec,
	}
	// ---- Hyperpoll loop: poll at minMs and record NEW samples ---------------
	windowCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()

	var (
		totalTicks   int    // total polls
		newSamples   int    // number of NEW BMC samples
		lastMono     uint64 // last observed Mono
		haveLastMono bool
		lastArrival  time.Time
		gaps         []time.Duration
	)

	BusyLoop(windowCtx, window, pollPer, func(ts time.Time) {
		totalTicks++
		rf.Collect(windowCtx, ts)

		// Look at the newest Redfish sample (any chassis).
		s, got := PeekOne(rfBuf)
		if !got {
			if klog.V(5).Enabled() {
				klog.V(5).Infof(
					"cal.redfish: tick=%d new=false (no sample in buffer)",
					totalTicks,
				)
			}
			return
		}

		monoVal := s.SampleMeta.Mono
		chassis := s.ChassisID
		seq := s.Seq
		watts := s.PowerWatts
		srcTime := s.SourceTime
		var freshness time.Duration
		if !srcTime.IsZero() {
			freshness = ts.Sub(srcTime)
		}

		isNew := false
		var gap time.Duration
		if !haveLastMono || monoVal != lastMono {
			// NEW BMC sample observed
			isNew = true
			if !lastArrival.IsZero() {
				gap = ts.Sub(lastArrival)
				gaps = append(gaps, gap)
			}
			lastArrival = ts
			lastMono = monoVal
			haveLastMono = true
			newSamples++
		}

		if klog.V(5).Enabled() {
			if isNew {
				klog.V(5).Infof(
					"cal.redfish: tick=%d new=true chassis=%s seq=%d watts=%.3f mono=%d freshness=%v gap=%v",
					totalTicks, chassis, seq, watts, monoVal, freshness, gap,
				)
			} else {
				// klog.V(5).Infof(
				// 	"cal.redfish: tick=%d new=false chassis=%s seq=%d watts=%.3f mono=%d freshness=%v",
				// 	totalTicks, chassis, seq, watts, monoVal, freshness,
				// )
			}
		}
	})

	if err := windowCtx.Err(); err != nil && err != context.DeadlineExceeded {
		klog.V(2).Infof("phase=calibrate component=redfish step=hyperpoll ok=false reason=context_err err=%v",
			err)
		redfishReport = nil
		return 0, false
	}

	// ---- Derive publish interval from inter-arrival gaps --------------------
	if len(gaps) < 1 || newSamples < 2 {
		klog.V(2).Infof("phase=calibrate component=redfish step=hyperpoll ok=false reason=insufficient_samples totalTicks=%d newSamples=%d",
			totalTicks, newSamples)
		redfishReport = nil
		return 0, false
	}

	medianGap := medianDuration(gaps)
	if medianGap <= 0 {
		klog.V(2).Infof("phase=calibrate component=redfish step=hyperpoll ok=false reason=nonpositive_median totalTicks=%d newSamples=%d",
			totalTicks, newSamples)
		redfishReport = nil
		return 0, false
	}

	observedMs := int(medianGap.Milliseconds())
	if observedMs < minMs {
		observedMs = minMs
	}

	var chosenMs int
	if autoHeartbeat {
		// Poll faster than publish interval; let heartbeat handle freshness.
		chosenMs = observedMs / 2
		if chosenMs < minMs {
			chosenMs = minMs
		}
	} else {
		// Poll roughly at the publish interval (but never below guard).
		chosenMs = observedMs
	}

	// Simple "effRate" notion for the report: fraction of ticks that saw a NEW sample.
	effRate := float64(newSamples) / float64(totalTicks)
	if totalTicks == 0 {
		effRate = 0
	}

	pp := PollProbePoint{
		Ms:      minMs,
		Ok:      true,
		Samples: totalTicks,
		Errs:    totalTicks - newSamples, // "non-new" ticks as a rough drop proxy
		EffRate: effRate,
		Notes:   "hyperpoll",
	}

	report.CandidateMs = chosenMs
	report.Path = []PollProbePoint{pp}
	report.EarlyStop = false
	report.Reason = "hyperpoll_median_publish_interval"

	redfishReport = report

	klog.V(2).Infof(
		"phase=calibrate component=redfish step=summary ok=true medianGap=%v observedMs=%d chosenMs=%d autoHeartbeat=%v totalTicks=%d newSamples=%d effRate=%.2f",
		medianGap, observedMs, chosenMs, autoHeartbeat, totalTicks, newSamples, effRate,
	)

	return chosenMs, true
}

// medianDuration computes the median of a slice of time.Duration values.
func medianDuration(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	tmp := make([]time.Duration, len(xs))
	copy(tmp, xs)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	n := len(tmp)
	if n%2 == 1 {
		return tmp[n/2]
	}
	// even -> average middle two
	return (tmp[n/2-1] + tmp[n/2]) / 2
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
