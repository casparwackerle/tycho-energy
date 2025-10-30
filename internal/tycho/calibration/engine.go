package calibration

import (
	"context"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
)

func Run(ctx context.Context) (Results, error) {
	res := Results{
		Notes:  make(map[string]string),
		Status: make(map[string]string),
	}

	// Shared monotonic clock (same as production)
	mono := clock.NewMono(clock.DefaultSource, time.Duration(cfg.TimebaseQuantumMs())*time.Millisecond)

	// GPU: Poll, Idle
	if cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
		best, ok := PollProbeGPU(ctx, mono, cfg.CalibrationGpuPollBudgetSec(), cfg.CalibrationGpuPollMinMs())
		if ok {
			res.GpuBestPollMS = &best
			res.Status["gpu.poll"] = "ok"
		} else {
			res.Status["gpu.poll"] = "skipped_or_failed"
		}
	}
	// if cfg.CalibrationGpuDelayEnabled() {
	// 	delay, ok := MeasureDelayGPU(ctx, mono, cfg.CalibrationGpuDelayBudgetSec())
	// 	if ok {
	// 		res.GpuDelayMS = &delay
	// 		res.Status["gpu.delay"] = "ok"
	// 	} else {
	// 		res.Status["gpu.delay"] = "skipped_or_failed"
	// 	}
	// }
	if cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
		// Use current (maybe updated) configured frequency
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
	}
	// if cfg.CalibrationRedfishDelayEnabled() {
	// 	delay, ok := MeasureDelayRedfish(ctx, mono, cfg.CalibrationRedfishDelayBudgetSec())
	// 	if ok {
	// 		res.RedfishDelayMS = &delay
	// 		res.Status["redfish.delay"] = "ok"
	// 	} else {
	// 		res.Status["redfish.delay"] = "skipped_or_failed"
	// 	}
	// }
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

	// RAPL: Idle (no poll probe)
	// if cfg.CalibrationRaplDelayEnabled() {
	// 	delay, ok := MeasureDelayRAPL(ctx, mono, cfg.CalibrationRaplDelayBudgetSec())
	// 	if ok {
	// 		res.RaplDelayMS = &delay
	// 		res.Status["rapl.delay"] = "ok"
	// 	} else {
	// 		res.Status["rapl.delay"] = "skipped_or_failed"
	// 	}
	// }
	if cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
		pollMs := cfg.RaplPollMs()
		p5, ok := IdleBaselineRAPL(ctx, mono, cfg.CalibrationIdleBudgetSec(), pollMs)
		if ok {
			res.RaplIdleP5 = &p5
			res.Status["rapl.idle"] = "ok"
		} else {
			res.Status["rapl.idle"] = "skipped_or_failed"
		}
	}

	klog.V(2).Infof("calibration finished: status=%v notes=%v", res.Status, res.Notes)
	return res, nil
}
