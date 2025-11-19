package calibration

import (
	"encoding/json"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

// Apply takes the aggregated calibration Results and updates the runtime
// configuration (poll periods) and emits structured logs. Idle calibration
// is no longer handled here; only GPU and Redfish polling are applied.
func Apply(res Results) {
	klog.V(5).Info("TYCHO-CAL: applying calibration results to configuration")

	// --- GPU PollMs → config -------------------------------------------------
	if res.GpuBestPollMS != nil && cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
		best := *res.GpuBestPollMS
		min := cfg.CalibrationGpuPollMinMs()
		if best < min {
			klog.V(2).Infof(
				"TYCHO-CAL: GPU poll period clamped from %d → %d (min allowed)",
				best, min,
			)
			best = min
		}
		cfg.SetGpuPollMs(best)
		klog.V(2).Infof("TYCHO-CAL: GPU poll period set to %d ms", best)
	} else {
		klog.V(2).Info("TYCHO-CAL: GPU PollProbe skipped or failed")
	}

	// --- Redfish PollMs → config --------------------------------------------
	if res.RedfishBestPollMS != nil && cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
		best := *res.RedfishBestPollMS
		min := cfg.CalibrationRedfishPollMinMs()
		if best < min {
			klog.V(2).Infof(
				"TYCHO-CAL: Redfish poll period clamped from %d → %d (min allowed)",
				best, min,
			)
			best = min
		}
		cfg.SetRedfishPollMs(best)
		klog.V(2).Infof("TYCHO-CAL: Redfish poll period set to %d ms", best)
	} else {
		klog.V(2).Info("TYCHO-CAL: Redfish PollProbe skipped or failed")
	}

	// --- Human-readable explanations ----------------------------------------
	// Print res.Notes["redfish.search"] / ["gpu.search"] if present,
	// otherwise fall back to the structured PollProbeReport.
	if s, ok := res.Notes["redfish.search"]; ok && s != "" {
		klog.V(2).Infof("TYCHO-CAL: explain.redfish.search=%s", s)
	} else if res.Redfish != nil {
		if b, err := json.Marshal(res.Redfish); err == nil {
			klog.V(2).Infof("TYCHO-CAL: explain.redfish.search=%s", string(b))
		}
	}

	if s, ok := res.Notes["gpu.search"]; ok && s != "" {
		klog.V(2).Infof("TYCHO-CAL: explain.gpu.search=%s", s)
	} else if res.GPU != nil {
		if b, err := json.Marshal(res.GPU); err == nil {
			klog.V(2).Infof("TYCHO-CAL: explain.gpu.search=%s", string(b))
		}
	}

	// --- Final poll summary --------------------------------------------------
	klog.V(2).Infof(
		"TYCHO-CAL: final pollMs: rapl=%d, redfish=%d, gpu=%d",
		cfg.RaplPollMs(), cfg.RedfishPollMs(), cfg.GpuPollMs(),
	)
	klog.V(2).Info("TYCHO-CAL: configuration updated with calibrated values")
}
