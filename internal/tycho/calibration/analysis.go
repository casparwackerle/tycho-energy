package calibration

import (
	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

func Apply(res Results) {
	// Notes:
	// - Only write values that were actually measured (non-nil).
	// - Clamp poll values to MinMs getters (hard lower bound).
	// - Delay/Idle are written as-is (placeholders for now, except Idle=P5).

	klog.V(5).Info("TYCHO-CAL: applying calibration results to configuration")

	if res.GpuBestPollMS != nil && cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
		best := *res.GpuBestPollMS
		min := cfg.CalibrationGpuPollMinMs()
		klog.V(5).Infof("TYCHO-CAL: GPU PollProbe result=%d ms (min allowed=%d)", best, min)
		if best < min {
			klog.V(5).Infof("TYCHO-CAL: GPU poll period clamped from %d → %d", best, min)
			best = min
		}
		cfg.SetGpuPollMs(best)
		klog.V(5).Infof("TYCHO-CAL: GPU poll period set to %d ms", best)

	} else {
		klog.V(5).Info("TYCHO-CAL: GPU PollProbe skipped or failed")
	}

	if res.RedfishBestPollMS != nil && cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
		best := *res.RedfishBestPollMS
		min := cfg.CalibrationRedfishPollMinMs()
		klog.V(5).Infof("TYCHO-CAL: Redfish PollProbe result=%d ms (min allowed=%d)", best, min)
		if best < min {
			klog.V(5).Infof("TYCHO-CAL: Redfish poll period clamped from %d → %d", best, min)
			best = min
		}
		cfg.SetRedfishPollMs(best)
		klog.V(5).Infof("TYCHO-CAL: Redfish poll period set to %d ms", best)
	} else {
		klog.V(5).Info("TYCHO-CAL: Redfish PollProbe skipped or failed")
	}

	// // DELAY --> not viable, as no active load can be applied from within tycho. Only other option is continuously cross-referencing, or external measurement.
	// if res.RaplDelayMS != nil && cfg.CalibrationRaplDelayEnabled() {
	// 	cfg.SetRaplDelayMs(*res.RaplDelayMS)
	// }
	// if res.RedfishDelayMS != nil && cfg.CalibrationRedfishDelayEnabled() {
	// 	cfg.SetRedfishDelayMs(*res.RedfishDelayMS)
	// }
	// if res.GpuDelayMS != nil && cfg.CalibrationGpuDelayEnabled() {
	// 	cfg.SetGpuDelayMs(*res.GpuDelayMS)
	// }

	// === Idle baselines (RAPL, Redfish, GPU) ===
	if res.RaplIdleP5 != nil && cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
		//cfg.TychoAnalysis.RaplIdleBaseline = *res.RaplIdleP5
		klog.V(5).Infof("TYCHO-CAL: RAPL idle baseline (p5) = %.3f W", *res.RaplIdleP5)
	} else {
		klog.V(5).Info("TYCHO-CAL: RAPL idle baseline skipped or unchanged")
	}
	if res.RedfishIdleP5 != nil && cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
		//cfg.TychoAnalysis.RedfishIdleBaseline = *res.RedfishIdleP5
		klog.V(5).Infof("TYCHO-CAL: Redfish idle baseline (p5) = %.3f W", *res.RedfishIdleP5)
	} else {
		klog.V(5).Info("TYCHO-CAL: Redfish idle baseline skipped or unchanged")
	}
	if res.GpuIdleP5 != nil && cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
		//cfg.TychoAnalysis.GpuIdleBaseline = *res.GpuIdleP5
		klog.V(5).Infof("TYCHO-CAL: GPU idle baseline (p5) = %.3f W", *res.GpuIdleP5)
	} else {
		klog.V(5).Info("TYCHO-CAL: GPU idle baseline skipped or unchanged")
	}

	klog.V(5).Info("TYCHO-CAL: configuration updated with calibrated values")
}
