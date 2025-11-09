package calibration

import (
	"context"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
)

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
		// if rep := LastGPUReport(); rep != nil {
		// 	res.GPU = rep
		// }
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
