package calibration

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"

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

// 	klog.V(2).Infof("calibration finished: status=%v notes=%v", res.Status, res.Notes)
// 	return res, nil
// }

func Run(ctx context.Context) (Results, error) {
	res := Results{
		Notes:  make(map[string]string),
		Status: make(map[string]string),
	}

	// Shared monotonic clock (same as production)
	mono := clock.NewMono(clock.DefaultSource, time.Duration(cfg.TimebaseQuantumMs())*time.Millisecond)
	nowMono := int64(mono.Now())
	lastMono := LastCalibMono()

	// --- 24h re-calibration policy -------------------------------------------
	const day = 24 * time.Hour
	var sinceLastStr string
	eligibleByTime := false
	if lastMono == 0 {
		eligibleByTime = true
		sinceLastStr = "never"
	} else {
		elapsed := time.Duration(nowMono - lastMono) // mono.Now() and lastMono are in ns
		if elapsed >= day {
			eligibleByTime = true
		}
		// human-ish note (hours with one decimal)
		hrs := float64(elapsed) / float64(time.Hour)
		sinceLastStr = fmt.Sprintf("%.1fh", hrs)
	}
	res.Notes["since_last_calibration"] = sinceLastStr

	// Quick idle gate helper (best-effort)
	idleBudget := time.Duration(cfg.CalibrationIdleBudgetSec()) * time.Second
	guardDur := idleBudget / 5
	if guardDur <= 0 {
		guardDur = 2 * time.Second
	}
	if guardDur > 5*time.Second {
		guardDur = 5 * time.Second
	}

	eligibleByIdle := false
	var idleReason string
	if ok, reason := QuietWindowGuard(ctx, mono, guardDur); ok {
		eligibleByIdle = true
		idleReason = "cpu_util_ok"
	} else {
		idleReason = reason
	}

	// Overall eligibility
	if !(eligibleByTime && eligibleByIdle) {
		why := "within_24h"
		if !eligibleByIdle {
			why = "not_idle:" + idleReason
		}
		res.Notes["calibration_skipped"] = why

		// Mark all configured calibrations as skipped with reason
		if cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
			res.Status["gpu.poll"] = "skipped:" + why
		}
		if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
			res.Status["gpu.idle"] = "skipped:" + why
		}
		if cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
			res.Status["redfish.poll"] = "skipped:" + why
		}
		if cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
			res.Status["redfish.idle"] = "skipped:" + why
		}
		if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
			res.Status["rapl.idle"] = "skipped:" + why
		}

		klog.V(2).Infof("phase=calibrate summary ok=false reason=%s since_last=%s status=%v notes=%v",
			why, sinceLastStr, res.Status, res.Notes)
		return res, nil
	}

	// --- Eligible: run calibrations (as enabled) ------------------------------

	// GPU: Poll, Idle
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

	if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
		pollMs := cfg.GpuPollMs()
		p5, ok := IdleBaselineGPU(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
		if ok {
			res.GpuIdleP5 = &p5
			res.Status["gpu.idle"] = "ok"
		} else {
			res.Status["gpu.idle"] = "skipped_or_failed"
		}
	}

	// Redfish: Poll, Idle
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

	if cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
		pollMs := cfg.RedfishPollMs()
		p5, ok := IdleBaselineRedfish(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
		if ok {
			res.RedfishIdleP5 = &p5
			res.Status["redfish.idle"] = "ok"
		} else {
			res.Status["redfish.idle"] = "skipped_or_failed"
		}
	}

	// RAPL: Idle (multi-socket/domain)
	if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
		pollMs := cfg.RaplPollMs()
		baselines, ok := IdleBaselineRAPL(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
		if ok {
			res.RAPLIdle = baselines
			res.Status["rapl.idle"] = "ok"
		} else {
			res.Status["rapl.idle"] = "skipped_or_failed"
		}
	}

	// Mark completion time only after running (even if some parts failed)
	SetLastCalibMono(int64(mono.Now()))

	// Summary
	klog.V(2).Infof("phase=calibrate summary ok=true since_last=%s idle_guard=%s status=%v notes=%v",
		sinceLastStr, idleReason, res.Status, res.Notes)

	return res, nil
}
