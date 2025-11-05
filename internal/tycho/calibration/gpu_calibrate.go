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

// IdleBaselineGPUPerDeviceFromSnap returns a per-device idle baseline (5th percentile, in Watts)
// for devices that have enough "quiet" samples within the snapshot. Devices that do not meet
// the quietness/min-sample criteria are omitted from the result map.
// Returns (map, true) if at least one device produced a baseline; otherwise (nil, false).
func IdleBaselineGPUPerDeviceFromSnap(
	ctx context.Context,
	mono *clock.Mono, // kept for signature symmetry; not used
	snap []ring.GpuTick,
) (map[string]float64, bool) {
	if len(snap) == 0 {
		return nil, false
	}

	// Tunables
	const (
		smQuietMaxPct   = 3.0 // SM/core util threshold for "quiet"
		memQuietMaxPct  = 5.0 // Memory controller util threshold
		encQuietMaxPct  = 1.0 // NVENC util threshold
		decQuietMaxPct  = 1.0 // NVDEC util threshold
		procQuietMaxPct = 3.0 // Sum of per-process compute util on that GPU
	)

	perDevQuietWatts := make(map[string][]float64)

	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		tick := snap[i]

		// Per-tick process util grouped by GPU index
		procComputeByGPU := make(map[int]float64)
		for _, p := range tick.Processes {
			u := float64(p.ComputeUtil)
			if u < 0 {
				u = 0
			} else if u > 100 {
				u = 100
			}
			procComputeByGPU[p.GpuIndex] += u
		}

		// Check each device
		for _, dev := range tick.Devices {
			if dev.PowerMilliW < 0 {
				continue
			}
			if !isGpuDeviceQuiet(dev, procComputeByGPU[dev.DeviceIndex],
				smQuietMaxPct, memQuietMaxPct, encQuietMaxPct, decQuietMaxPct, procQuietMaxPct) {
				continue
			}
			perDevQuietWatts[dev.UUID] = append(perDevQuietWatts[dev.UUID], float64(dev.PowerMilliW)/1000.0)
		}
	}

	if len(perDevQuietWatts) == 0 {
		return nil, false
	}

	out := make(map[string]float64, len(perDevQuietWatts))
	for uuid, watts := range perDevQuietWatts {
		if len(watts) == 0 {
			continue
		}
		// Compute P5 directly, no enforced sample count
		out[uuid] = P5(watts)
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// Quietness check for a single device sample within a tick.
// procComputeTotal is the sum of per-process GPU compute util for this device's GPU index in this tick.
func isGpuDeviceQuiet(
	dev ring.GpuSample,
	procComputeTotal float64,
	smMax, memMax, encMax, decMax, procMax float64,
) bool {
	// Clamp/normalize optional enc/dec pointers.
	enc := 0.0
	if dev.EncUtilPct != nil {
		enc = *dev.EncUtilPct
		if enc < 0 {
			enc = 0
		} else if enc > 100 {
			enc = 100
		}
	}
	dec := 0.0
	if dev.DecUtilPct != nil {
		dec = *dev.DecUtilPct
		if dec < 0 {
			dec = 0
		} else if dec > 100 {
			dec = 100
		}
	}

	// Clamp primary utils as well (defensive).
	sm := dev.SMUtilPct
	if sm < 0 {
		sm = 0
	} else if sm > 100 {
		sm = 100
	}
	mem := dev.MemUtilPct
	if mem < 0 {
		mem = 0
	} else if mem > 100 {
		mem = 100
	}

	// Processes sum clamp.
	if procComputeTotal < 0 {
		procComputeTotal = 0
	} else if procComputeTotal > 100 {
		procComputeTotal = 100
	}

	return sm <= smMax &&
		mem <= memMax &&
		enc <= encMax &&
		dec <= decMax &&
		procComputeTotal <= procMax
}

// CumEnergyValidationPerDeviceFromSnap validates NVML cumulative energy per device
// using a snapshot of GPU ticks. For each device that has enough data, it checks:
//   - cumulative counter is monotonic (non-decreasing)
//   - slope matches integrated InstantPowerMilliW within a tolerance
//
// Returns (map[uuid]CumEnergyDiag, true) if at least one device produced a verdict;
// otherwise (nil, false).
func CumEnergyValidationPerDeviceFromSnap(
	ctx context.Context,
	mono *clock.Mono,
	snap []ring.GpuTick,
) (map[string]CumEnergyDiag, bool) {
	if len(snap) < 2 {
		return nil, false
	}

	// ---- Tunables (consider moving to config) ----
	const (
		minEnergyJ  = 0.25 // require at least 0.25 J over window to be meaningful
		maxRelError = 0.15 // 15% relative error tolerance
	)

	// Per-device time series accumulators
	type dp struct {
		tMono uint64 // ns (from tick.SampleMeta.Mono)
		pMW   uint64 // instant power (mW)
		eMJ   uint64 // cumulative energy (mJ); only valid if okE
		okE   bool   // whether cumulative reading was present
	}

	series := make(map[string][]dp) // uuid -> []dp

	// Build per-device series from the snapshot
	for i := 0; i < len(snap); i++ {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		tick := snap[i]
		// NOTE: This assumes your tick carries per-device slices (like your idle function).
		// If your current tick is flattened, adapt this loop accordingly.
		for _, dev := range tick.Devices {
			d := dp{
				tMono: tick.SampleMeta.Mono,
				pMW:   dev.InstantPowerMilliW, // integrate the *instant* signal
			}
			if dev.CumEnergyMilliJ != nil {
				d.eMJ = *dev.CumEnergyMilliJ
				d.okE = true
			}

			series[dev.UUID] = append(series[dev.UUID], d)
		}
	}

	out := make(map[string]CumEnergyDiag, len(series))
	haveAny := false

	for uuid, s := range series {
		// Count available cumulative reads
		cumReads := 0
		for _, x := range s {
			if x.okE {
				cumReads++
			}
		}
		if cumReads < 2 {
			continue
		}

		// Compute window span
		t0 := s[0].tMono
		t1 := s[len(s)-1].tMono
		winSec := float64(t1-t0) / 1e9

		// Monotonicity check (non-decreasing on the subsequence where okE==true)
		monoViol := 0
		var prev uint64
		prevSet := false
		for _, x := range s {
			if !x.okE {
				continue
			}
			if prevSet && x.eMJ < prev {
				monoViol++
			}
			prev = x.eMJ
			prevSet = true
		}
		if monoViol > 0 {
			out[uuid] = CumEnergyDiag{
				Valid:               false,
				Samples:             len(s),
				CumReads:            cumReads,
				MonotonicViolations: monoViol,
				WindowSeconds:       winSec,
			}
			haveAny = true
			continue
		}

		// Integrate instant power with trapezoidal rule
		var eintJ float64
		for i := 1; i < len(s); i++ {
			dt := float64(s[i].tMono-s[i-1].tMono) / 1e9 // seconds
			p0 := float64(s[i-1].pMW) / 1000.0           // W
			p1 := float64(s[i].pMW) / 1000.0             // W
			eintJ += 0.5 * (p0 + p1) * dt
		}

		// Delta of cumulative over first/last valid cum sample
		// (scan from both ends to find valid endpoints)
		var e0, e1 uint64
		found0 := false
		for i := 0; i < len(s); i++ {
			if s[i].okE {
				e0 = s[i].eMJ
				found0 = true
				break
			}
		}
		if !found0 {
			continue
		}
		for i := len(s) - 1; i >= 0; i-- {
			if s[i].okE {
				e1 = s[i].eMJ
				break
			}
		}
		ecumJ := float64(e1-e0) / 1000.0 // mJ → J

		// Require a meaningful amount of energy change
		if eintJ < minEnergyJ {
			out[uuid] = CumEnergyDiag{
				Valid:            false,
				Samples:          len(s),
				CumReads:         cumReads,
				IntegratedJ:      eintJ,
				CumulativeDeltaJ: ecumJ,
				RelativeError:    1.0,
				WindowSeconds:    winSec,
			}
			haveAny = true
			continue
		}

		rel := math.Abs(ecumJ-eintJ) / math.Max(eintJ, 1e-6)
		valid := rel <= maxRelError

		out[uuid] = CumEnergyDiag{
			Valid:               valid,
			Samples:             len(s),
			CumReads:            cumReads,
			MonotonicViolations: monoViol,
			IntegratedJ:         eintJ,
			CumulativeDeltaJ:    ecumJ,
			RelativeError:       rel,
			WindowSeconds:       winSec,
		}
		haveAny = true
	}

	if !haveAny {
		return nil, false
	}
	return out, true
}
