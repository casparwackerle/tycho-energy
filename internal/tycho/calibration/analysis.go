package calibration

import (
	"encoding/json"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

// func Apply(res Results) {
// 	// Notes:
// 	// - Only write values that were actually measured (non-nil).
// 	// - Clamp poll values to MinMs getters (hard lower bound).
// 	// - Delay/Idle are written as-is (placeholders for now, except Idle=P5).

// 	klog.V(5).Info("TYCHO-CAL: applying calibration results to configuration")

// 	if res.GpuBestPollMS != nil && cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
// 		best := *res.GpuBestPollMS
// 		min := cfg.CalibrationGpuPollMinMs()
// 		klog.V(5).Infof("TYCHO-CAL: GPU PollProbe result=%d ms (min allowed=%d)", best, min)
// 		if best < min {
// 			klog.V(5).Infof("TYCHO-CAL: GPU poll period clamped from %d → %d", best, min)
// 			best = min
// 		}
// 		cfg.SetGpuPollMs(best)
// 		klog.V(5).Infof("TYCHO-CAL: GPU poll period set to %d ms", best)

// 	} else {
// 		klog.V(5).Info("TYCHO-CAL: GPU PollProbe skipped or failed")
// 	}

// 	if res.RedfishBestPollMS != nil && cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
// 		best := *res.RedfishBestPollMS
// 		min := cfg.CalibrationRedfishPollMinMs()
// 		klog.V(5).Infof("TYCHO-CAL: Redfish PollProbe result=%d ms (min allowed=%d)", best, min)
// 		if best < min {
// 			klog.V(5).Infof("TYCHO-CAL: Redfish poll period clamped from %d → %d", best, min)
// 			best = min
// 		}
// 		cfg.SetRedfishPollMs(best)
// 		klog.V(5).Infof("TYCHO-CAL: Redfish poll period set to %d ms", best)
// 	} else {
// 		klog.V(5).Info("TYCHO-CAL: Redfish PollProbe skipped or failed")
// 	}

// 	// === Idle baselines (RAPL, Redfish, GPU) ===
// 	if res.RaplIdleP5 != nil && cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
// 		//cfg.TychoAnalysis.RaplIdleBaseline = *res.RaplIdleP5
// 		klog.V(5).Infof("TYCHO-CAL: RAPL idle baseline (p5) = %.3f W", *res.RaplIdleP5)
// 	} else {
// 		klog.V(5).Info("TYCHO-CAL: RAPL idle baseline skipped or unchanged")
// 	}
// 	if res.RedfishIdleP5 != nil && cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
// 		//cfg.TychoAnalysis.RedfishIdleBaseline = *res.RedfishIdleP5
// 		klog.V(5).Infof("TYCHO-CAL: Redfish idle baseline (p5) = %.3f W", *res.RedfishIdleP5)
// 	} else {
// 		klog.V(5).Info("TYCHO-CAL: Redfish idle baseline skipped or unchanged")
// 	}
// 	if res.GpuIdleP5 != nil && cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
// 		//cfg.TychoAnalysis.GpuIdleBaseline = *res.GpuIdleP5
// 		klog.V(5).Infof("TYCHO-CAL: GPU idle baseline (p5) = %.3f W", *res.GpuIdleP5)
// 	} else {
// 		klog.V(5).Info("TYCHO-CAL: GPU idle baseline skipped or unchanged")
// 	}
// 	klog.V(5).Infof("TYCHO-CAL: final pollMs: rapl=%d, redfish=%d, gpu=%d", cfg.RaplPollMs(), cfg.RedfishPollMs(), cfg.GpuPollMs())
// 	klog.V(5).Info("TYCHO-CAL: configuration updated with calibrated values")
// }

func Apply(res Results) {
	klog.V(5).Info("TYCHO-CAL: applying calibration results to configuration")

	// -----------------------
	// GPU PollMs → config
	// -----------------------
	if res.GpuBestPollMS != nil && cfg.CalibrationGpuPollEnabled() && cfg.EnableGpu() {
		best := *res.GpuBestPollMS
		min := cfg.CalibrationGpuPollMinMs()
		if best < min {
			klog.V(2).Infof("TYCHO-CAL: GPU poll period clamped from %d → %d (min allowed)", best, min)
			best = min
		}
		cfg.SetGpuPollMs(best)
		klog.V(2).Infof("TYCHO-CAL: GPU poll period set to %d ms", best)
	} else {
		klog.V(2).Info("TYCHO-CAL: GPU PollProbe skipped or failed")
	}

	// -----------------------
	// Redfish PollMs → config
	// -----------------------
	if res.RedfishBestPollMS != nil && cfg.CalibrationRedfishPollEnabled() && cfg.EnableRedfish() {
		best := *res.RedfishBestPollMS
		min := cfg.CalibrationRedfishPollMinMs()
		if best < min {
			klog.V(2).Infof("TYCHO-CAL: Redfish poll period clamped from %d → %d (min allowed)", best, min)
			best = min
		}
		cfg.SetRedfishPollMs(best)
		klog.V(2).Infof("TYCHO-CAL: Redfish poll period set to %d ms", best)
	} else {
		klog.V(2).Info("TYCHO-CAL: Redfish PollProbe skipped or failed")
	}

	// -----------------------
	// RAPL idle baselines → store (per-socket/per-domain)
	// -----------------------
	if res.RAPLIdle != nil && cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
		SetIdleAll(res.RAPLIdle)

		// Structured logs per socket/domain
		totalSockets := 0
		for sock, domMap := range res.RAPLIdle {
			totalSockets++
			for dom, bl := range domMap {
				klog.V(2).Infof("TYCHO-CAL: RAPL idle baseline socket=%d domain=%s p5=%.6fW min=%.6fW n=%d ts=%d",
					int(sock), string(dom), bl.P5, bl.Min, bl.N, bl.FromTs)
			}
		}
		klog.V(2).Infof("TYCHO-CAL: RAPL idle baselines applied (sockets=%d)", totalSockets)
	} else {
		klog.V(2).Info("TYCHO-CAL: RAPL idle baseline skipped or unchanged")
	}

	// -----------------------
	// Optional: legacy single-value idles (if still produced)
	// -----------------------
	if res.RedfishIdleP5 != nil && cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
		klog.V(2).Infof("TYCHO-CAL: Redfish idle baseline (p5) = %.3f W", *res.RedfishIdleP5)
	} else {
		klog.V(5).Info("TYCHO-CAL: Redfish idle baseline skipped or unchanged")
	}
	if res.GpuIdleP5 != nil && cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
		klog.V(2).Infof("TYCHO-CAL: GPU idle baseline (p5) = %.3f W", *res.GpuIdleP5)
	} else {
		klog.V(5).Info("TYCHO-CAL: GPU idle baseline skipped or unchanged")
	}

	// -----------------------
	// Explain JSON blobs
	// -----------------------
	// Print res.Notes["rapl.idle"] / ["redfish.search"] / ["gpu.search"] if present.
	if s, ok := res.Notes["rapl.idle"]; ok && s != "" {
		klog.V(2).Infof("TYCHO-CAL: explain.rapl.idle=%s", s)
	}
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

	// Final poll summary
	klog.V(2).Infof("TYCHO-CAL: final pollMs: rapl=%d, redfish=%d, gpu=%d",
		cfg.RaplPollMs(), cfg.RedfishPollMs(), cfg.GpuPollMs())
	klog.V(2).Info("TYCHO-CAL: configuration updated with calibrated values")
}
