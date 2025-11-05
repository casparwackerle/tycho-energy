package calibration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

func Apply(res Results) {
	klog.V(5).Info("TYCHO-CAL: applying calibration results to configuration")

	// GPU PollMs → config
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

	// Redfish PollMs → config
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

	// RAPL idle baselines → store (per-socket/per-domain)
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

	// Optional: legacy single-value idles (if still produced)
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

// RunIdleCalibration runs idle calibration for all enabled sensors (RAPL, Redfish, GPU)
// using the stored dependencies (Init must have been called).
func RunIdleCalibration(ctx context.Context) (Results, error) {
	return runIdleWithMask(ctx, MaskAll)
}

func RunIdleRaplCalibration(ctx context.Context) (Results, error) {
	return runIdleWithMask(ctx, MaskRAPL)
}

func RunIdleRedfishCalibration(ctx context.Context) (Results, error) {
	return runIdleWithMask(ctx, MaskRedfish)
}

func RunIdleGpuCalibration(ctx context.Context) (Results, error) {
	return runIdleWithMask(ctx, MaskGPU)
}

// Shared policy/timing orchestrator. Decides mode, performs quiet guard, snapshots,
// and applies per-sensor baselines. Requires Init() to have set dependencies.
func runIdleWithMask(ctx context.Context, mask SensorMask) (Results, error) {
	res := Results{
		Notes:  make(map[string]string),
		Status: make(map[string]string),
	}

	deps := getDeps()
	if deps == nil || deps.Mono == nil {
		return res, fmt.Errorf("calibration deps not initialized")
	}

	nowMono := int64(deps.Mono.Now())
	lastMono := LastCalibMono()

	// 24h policy applies to "ground_truth" runs only (kept; no time windows elsewhere)
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

	// --- Quiet guard over entire BPF ring (no timing config) ------------------
	if deps.Bpf == nil {
		const why = "no_bpf_ring"
		res.Notes["idle_calibration_skipped"] = why
		if (mask&MaskGPU) != 0 && cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
			res.Status["gpu.idle"] = "skipped:" + why
		}
		if (mask&MaskRedfish) != 0 && cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
			res.Status["redfish.idle"] = "skipped:" + why
		}
		if (mask&MaskRAPL) != 0 && cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
			res.Status["rapl.idle"] = "skipped:" + why
		}
		klog.V(2).Infof("phase=calibrate.idle summary ok=false reason=%s since_last=%s status=%v notes=%v",
			why, sinceLastStr, res.Status, res.Notes)
		return res, nil
	}

	bpfGuardSnap := deps.Bpf.SnapshotChrono() // use full ring contents
	if ok, fracLow, mean := isQuietFromBpfSnap(bpfGuardSnap); !ok {
		why := fmt.Sprintf("not_idle:mean=%.3f fracLow=%.2f", mean, fracLow)
		res.Notes["idle_calibration_skipped"] = why
		if (mask&MaskGPU) != 0 && cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() {
			res.Status["gpu.idle"] = "skipped:" + why
		}
		if (mask&MaskRedfish) != 0 && cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() {
			res.Status["redfish.idle"] = "skipped:" + why
		}
		if (mask&MaskRAPL) != 0 && cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() {
			res.Status["rapl.idle"] = "skipped:" + why
		}
		klog.V(2).Infof("phase=calibrate.idle summary ok=false reason=%s since_last=%s status=%v notes=%v",
			why, sinceLastStr, res.Status, res.Notes)
		return res, nil
	}
	// optional: informational metric
	res.Notes["quiet_guard_mean_cpu"] = fmt.Sprintf("%.3f", meanCPU(bpfGuardSnap))

	// --- Mode: ground_truth if 24h elapsed; otherwise opportunistic_refine ----
	var mode IdleMode
	if eligibleByTime {
		mode = ModeGroundTruth
	} else {
		mode = ModeOpportunisticRefine
	}
	res.Mode = mode
	res.Notes["mode"] = string(mode)

	// --- Full-ring snapshots for sensors (no time slicing) --------------------
	var raplSnap []ring.RaplSample
	var rfSnap []ring.RedfishSample
	var gpuSnap []ring.GpuTick

	if (mask&MaskRAPL) != 0 && deps.Rapl != nil {
		raplSnap = snapshotRaplAll(deps.Rapl) // or deps.Rapl.SnapshotChrono()
	}
	if (mask&MaskRedfish) != 0 && deps.Rf != nil {
		rfSnap = snapshotRedfishAll(deps.Rf) // or deps.Rf.SnapshotChrono()
	}
	if (mask&MaskGPU) != 0 && deps.Gpu != nil {
		gpuSnap = snapshotGpuAll(deps.Gpu) // or deps.Gpu.SnapshotChrono()
	}

	// --- Compute & apply per-sensor baselines --------------------------------

	// GPU
	if (mask&MaskGPU) != 0 && cfg.CalibrationGpuIdleEnabled() && cfg.EnableGpu() && deps.Gpu != nil {
		perDev, ok := IdleBaselineGPUPerDeviceFromSnap(ctx, deps.Mono, gpuSnap)
		if ok && len(perDev) > 0 {
			res.GpuIdlePerDevice = perDev
			// Legacy scalar: sum per-device p5s
			var total float64
			for _, v := range perDev {
				total += v
			}
			res.GpuIdleP5 = &total
			res.Status["gpu.idle"] = "ok"
			res.Notes["gpu.idle.devices"] = fmt.Sprintf("%d", len(perDev))
		} else {
			res.Status["gpu.idle"] = "skipped_or_failed"
		}
	}

	// Redfish
	if (mask&MaskRedfish) != 0 && cfg.CalibrationRedfishIdleEnabled() && cfg.EnableRedfish() && deps.Rf != nil {
		if p5, ok := IdleBaselineRedfishFromSnap(ctx, deps.Mono, rfSnap); ok {
			res.RedfishIdleP5 = &p5
			res.Status["redfish.idle"] = "ok"
		} else {
			res.Status["redfish.idle"] = "skipped_or_failed"
		}
	}

	// RAPL
	if (mask&MaskRAPL) != 0 && cfg.CalibrationRaplIdleEnabled() && cfg.EnableRapl() && deps.Rapl != nil {
		if bl, ok := IdleBaselineRAPLFromSnap(ctx, deps.Mono, raplSnap); ok {
			res.RAPLIdle = bl
			if mode == ModeGroundTruth {
				SetIdleAll(bl) // replace full baselines
				SetLastCalibMono(int64(deps.Mono.Now()))
			} else {
				// Opportunistic: downward-only refinement via hysteresis/confirmations
				nowTs := uint64(deps.Mono.Now())
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
		res.Mode, sinceLastStr, res.Status, res.Notes)
	return res, nil
}
