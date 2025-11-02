package calibration

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
)

// func Run(ctx context.Context) (Results, error) {
// 	res := Results{
// 		Notes:  make(map[string]string),
// 		Status: make(map[string]string),
// 	}

// 	// Shared monotonic clock (same as production)
// 	mono := clock.NewMono(clock.DefaultSource, time.Duration(cfg.TimebaseQuantumMs())*time.Millisecond)

// 	// GPU: Poll, Idle
// 	if cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
// 		best, ok := PollProbeGPU(ctx, mono, cfg.CalibrationGpuPollBudgetSec(), cfg.CalibrationGpuPollMinMs())
// 		if ok {
// 			res.GpuBestPollMS = &best
// 			res.Status["gpu.poll"] = "ok"
// 		} else {
// 			res.Status["gpu.poll"] = "skipped_or_failed"
// 		}
// 	}

// 	if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
// 		// Use current (maybe updated) configured frequency
// 		pollMs := cfg.GpuPollMs()
// 		p5, ok := IdleBaselineGPU(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
// 		if ok {
// 			res.GpuIdleP5 = &p5
// 			res.Status["gpu.idle"] = "ok"
// 		} else {
// 			res.Status["gpu.idle"] = "skipped_or_failed"
// 		}
// 	}

// 	// Redfish: Poll, Idle
// 	if cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
// 		best, ok := PollProbeRedfish(ctx, mono, cfg.CalibrationRedfishPollBudgetSec(), cfg.CalibrationRedfishPollMinMs())
// 		if ok {
// 			res.RedfishBestPollMS = &best
// 			res.Status["redfish.poll"] = "ok"
// 		} else {
// 			res.Status["redfish.poll"] = "skipped_or_failed"
// 		}
// 	}

// 	if cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
// 		pollMs := cfg.RedfishPollMs()
// 		p5, ok := IdleBaselineRedfish(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
// 		if ok {
// 			res.RedfishIdleP5 = &p5
// 			res.Status["redfish.idle"] = "ok"
// 		} else {
// 			res.Status["redfish.idle"] = "skipped_or_failed"
// 		}
// 	}

// 	if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
// 		pollMs := cfg.RaplPollMs()
// 		p5, ok := IdleBaselineRAPL(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
// 		if ok {
// 			res.RaplIdleP5 = &p5
// 			res.Status["rapl.idle"] = "ok"
// 		} else {
// 			res.Status["rapl.idle"] = "skipped_or_failed"
// 		}
// 	}

//		klog.V(2).Infof("calibration finished: status=%v notes=%v", res.Status, res.Notes)
//		return res, nil
//	}
//
// RunPollCalibration calibrates highest viable poll frequencies for enabled sources.
// Call once during startup. No 24h policy, no idle guard.
func RunPollCalibration(ctx context.Context) (Results, error) {
	res := Results{
		Notes:  make(map[string]string),
		Status: make(map[string]string),
	}

	// Shared monotonic clock (same as production)
	mono := clock.NewMono(clock.DefaultSource, time.Duration(cfg.TimebaseQuantumMs())*time.Millisecond)

	// --- GPU: Poll ------------------------------------------------------------
	if cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
		best, ok := PollProbeGPU(ctx, mono, cfg.CalibrationGpuPollBudgetSec(), cfg.CalibrationGpuPollMinMs())
		if ok {
			res.GpuBestPollMS = &best
			res.Status["gpu.poll"] = "ok"
		} else {
			res.Status["gpu.poll"] = "skipped_or_failed"
		}
		if rep := LastGPUReport(); rep != nil {
			res.GPU = rep
		}
	}

	// --- Redfish: Poll --------------------------------------------------------
	if cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
		best, ok := PollProbeRedfish(ctx, mono, cfg.CalibrationRedfishPollBudgetSec(), cfg.CalibrationRedfishPollMinMs())
		if ok {
			res.RedfishBestPollMS = &best
			res.Status["redfish.poll"] = "ok"
		} else {
			res.Status["redfish.poll"] = "skipped_or_failed"
		}
		if rep := LastRedfishReport(); rep != nil {
			res.Redfish = rep
		}
	}

	klog.V(2).Infof("phase=calibrate.poll summary status=%v notes=%v", res.Status, res.Notes)
	return res, nil
}

// RunIdleCalibration analyzes existing synchronized rings to compute idle baselines.
// It enforces the 24h policy for full ("ground_truth") runs and a buffer-based
// quiet guard using recent BPF samples. No collectors are started here; the
// Engine must already be running.
//
// NOTE: This version uses monotonic-ns windows (uint64) to avoid time.Time conversions.
func RunIdleCalibration(
	ctx context.Context,
	mono *clock.Mono,
	bpfBuf *ring.Sync[ring.BpfTick],
	raplBuf *ring.Sync[ring.RaplSample],
	rfBuf *ring.Sync[ring.RedfishSample],
	gpuBuf *ring.Sync[ring.GpuTick],
) (Results, error) {
	res := Results{
		Notes:  make(map[string]string),
		Status: make(map[string]string),
	}

	nowMono := int64(mono.Now())
	lastMono := LastCalibMono()

	// --- 24h policy applies to "ground_truth" runs only -----------------------
	const day = 24 * time.Hour
	var sinceLastStr string
	eligibleByTime := false
	if lastMono == 0 {
		eligibleByTime = true
		sinceLastStr = "never"
	} else {
		elapsed := time.Duration(nowMono - lastMono)
		if elapsed >= day {
			eligibleByTime = true
		}
		sinceLastStr = fmt.Sprintf("%.1fh", float64(elapsed)/float64(time.Hour))
	}
	res.Notes["idle_since_last_calibration"] = sinceLastStr

	// --- Buffer-based quiet guard over recent BPF samples ---------------------
	const guardDur = 10 * time.Second // evaluate "continuous low" over last 10s

	// First ensure the guard window actually exists in the BPF ring.
	endMono := mono.Now()
	startGuard := endMono - uint64(guardDur.Nanoseconds())

	if !hasWindowBpfMono(bpfBuf, startGuard, endMono) {
		const why = "insufficient_guard_window"
		res.Notes["idle_calibration_skipped"] = why
		if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
			res.Status["gpu.idle"] = "skipped:" + why
		}
		if cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
			res.Status["redfish.idle"] = "skipped:" + why
		}
		if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
			res.Status["rapl.idle"] = "skipped:" + why
		}
		klog.V(2).Infof("phase=calibrate.idle summary ok=false reason=%s since_last=%s status=%v notes=%v",
			why, sinceLastStr, res.Status, res.Notes)
		return res, nil
	}

	// With coverage guaranteed, now check quietness.
	bpfGuardSnap := snapshotBpfMono(bpfBuf, startGuard, endMono)
	if ok, fracLow, mean := isQuietFromBpfSnap(bpfGuardSnap); !ok {
		why := fmt.Sprintf("not_idle:mean=%.3f fracLow=%.2f", mean, fracLow)
		res.Notes["idle_calibration_skipped"] = why
		if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
			res.Status["gpu.idle"] = "skipped:" + why
		}
		if cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
			res.Status["redfish.idle"] = "skipped:" + why
		}
		if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
			res.Status["rapl.idle"] = "skipped:" + why
		}
		klog.V(2).Infof("phase=calibrate.idle summary ok=false reason=%s since_last=%s status=%v notes=%v",
			why, sinceLastStr, res.Status, res.Notes)
		return res, nil
	}
	// optional: informational log metric
	res.Notes["quiet_guard_mean_cpu"] = fmt.Sprintf("%.3f", meanCPU(bpfGuardSnap))

	// --- Choose mode: ground_truth (full budget window) or opportunistic_refine
	budgetNs := uint64(time.Duration(cfg.CalibrationIdleBudgetSec()) * time.Second) // ns
	startMono := endMono - uint64(budgetNs)

	haveBudget :=
		(!cfg.EnableRapl() || !cfg.CalibrationRaplIdleEnabled() || hasWindowRaplMono(raplBuf, startMono, endMono)) &&
			(!cfg.EnableRedfish() || !cfg.CalibrationRedfishIdleEnabled() || hasWindowRedfishMono(rfBuf, startMono, endMono)) &&
			(!cfg.EnableGpu() || !cfg.CalibrationGpuIdleEnabled() || hasWindowGpuMono(gpuBuf, startMono, endMono))

	var mode string
	var raplSnap []ring.RaplSample
	var rfSnap []ring.RedfishSample
	var gpuSnap []ring.GpuTick

	if haveBudget && eligibleByTime {
		mode = "ground_truth"
		raplSnap = snapshotRaplMono(raplBuf, startMono, endMono)
		rfSnap = snapshotRedfishMono(rfBuf, startMono, endMono)
		gpuSnap = snapshotGpuMono(gpuBuf, startMono, endMono)
	} else {
		mode = "opportunistic_refine"
		raplSnap = snapshotRaplAll(raplBuf)
		rfSnap = snapshotRedfishAll(rfBuf)
		gpuSnap = snapshotGpuAll(gpuBuf)
	}

	res.Notes["mode"] = mode

	// --- Compute baselines from snapshots -------------------------------------
	if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
		perDev, ok := IdleBaselineGPUPerDeviceFromSnap(ctx, mono, gpuSnap)
		if ok && len(perDev) > 0 {
			// Store per-device idle baselines
			res.GpuIdlePerDevice = perDev

			// Maintain legacy scalar: sum of per-device p5s as a host-level idle proxy
			var total float64
			for _, v := range perDev {
				total += v
			}
			res.GpuIdleP5 = &total

			// Status/notes
			if res.Status == nil {
				res.Status = make(map[string]string)
			}
			res.Status["gpu.idle"] = "ok"

			if res.Notes == nil {
				res.Notes = make(map[string]string)
			}
			res.Notes["gpu.idle.devices"] = fmt.Sprintf("%d", len(perDev))
		} else {
			if res.Status == nil {
				res.Status = make(map[string]string)
			}
			res.Status["gpu.idle"] = "skipped_or_failed"
		}
	}

	if cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
		if p5, ok := IdleBaselineRedfishFromSnap(ctx, mono, rfSnap); ok {
			res.RedfishIdleP5 = &p5
			res.Status["redfish.idle"] = "ok"
		} else {
			res.Status["redfish.idle"] = "skipped_or_failed"
		}
	}

	if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
		if bl, ok := IdleBaselineRAPLFromSnap(ctx, mono, raplSnap); ok {
			res.RAPLIdle = bl
			if mode == "ground_truth" {
				SetIdleAll(bl)                      // replace full baselines
				SetLastCalibMono(int64(mono.Now())) // mark last FULL idle-calibration time
			} else {
				// Opportunistic: downward-only refinement via hysteresis/confirmations.
				nowTs := uint64(mono.Now())
				for sock, domMap := range bl {
					for dom, base := range domMap {
						_, _ = MaybeUpdateIdle(sock, dom, base.P5, nowTs)
					}
				}
			}
			res.Status["rapl.idle"] = "ok"
		} else {
			res.Status["rapl.idle"] = "skipped_or_failed"
		}
	}

	klog.V(2).Infof("phase=calibrate.idle summary ok=true mode=%s since_last=%s status=%v notes=%v",
		mode, sinceLastStr, res.Status, res.Notes)
	return res, nil
}

// // quietGuardFromBpf: best-effort CPU quiet check using recent BPF samples.
// func quietGuardFromBpf(
// 	ctx context.Context,
// 	mono *clock.Mono,
// 	bpfBuf *ring.Sync[ring.BpfSample],
// 	guardDur time.Duration,
// ) (bool, string) {
// 	t1 := mono.Now()
// 	t0 := t1.Add(-guardDur)
// 	snap := snapshotBpf(bpfBuf, t0, t1)
// 	if len(snap) == 0 {
// 		return false, "no_bpf_data"
// 	}
// 	// Example heuristic: avg runnable or CPU% must be below threshold.
// 	// Replace with your established QuietWindowGuard logic once adapted to buffers.
// 	const cpuThresh = 0.05 // 5% avg CPU as a placeholder
// 	avg := estimateCpuFromBpf(snap)
// 	if avg <= cpuThresh {
// 		return true, "cpu_ok"
// 	}
// 	return false, fmt.Sprintf("cpu_high:%.3f", avg)
// }

// // waitForCoverage blocks until each enabled metric ring contains enough NEW samples
// // since tStart to cover idleBudget. It returns if the context is canceled or timeout occurs.
// func waitForCoverage(
// 	ctx context.Context,
// 	mono *clock.Mono,
// 	tStart time.Time,
// 	idleBudget time.Duration,
// 	bpfMs, raplMs, rfMs, gpuMs int,
// 	bpfBuf *ring.Sync[ring.BpfSample],
// 	raplBuf *ring.Sync[ring.RaplSample],
// 	rfBuf *ring.Sync[ring.RedfishSample],
// 	gpuBuf *ring.Sync[ring.GpuSample],
// ) error {
// 	targetStart := tStart
// 	targetEnd := tStart.Add(idleBudget)

// 	tick := time.NewTicker(time.Duration(cfg.TimebaseQuantumMs()) * time.Millisecond)
// 	defer tick.Stop()

// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return ctx.Err()
// 		case <-tick.C:
// 			okAll := true

// 			if cfg.EnableBpf() {
// 				if !hasWindowBpf(bpfBuf, targetStart, targetEnd) {
// 					okAll = false
// 				}
// 			}
// 			if cfg.EnableRapl() && cfg.CalibrationRaplIdleEnabled() {
// 				if !hasWindowRapl(raplBuf, targetStart, targetEnd) {
// 					okAll = false
// 				}
// 			}
// 			if cfg.EnableRedfish() && cfg.CalibrationRedfishIdleEnabled() {
// 				if !hasWindowRedfish(rfBuf, targetStart, targetEnd) {
// 					okAll = false
// 				}
// 			}
// 			if cfg.EnableGpu() && cfg.CalibrationGpuIdleEnabled() {
// 				if !hasWindowGpu(gpuBuf, targetStart, targetEnd) {
// 					okAll = false
// 				}
// 			}

// 			if okAll {
// 				return nil
// 			}
// 		}
// 	}
// }
