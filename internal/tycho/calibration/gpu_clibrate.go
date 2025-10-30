package calibration

import (
	"context"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"

	gpuCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/gpu"
)

// PollProbeGPU: increase sampling aggressiveness down to minMs, infer NVML publish cadence from value changes.
// Best period = clamp(round(0.8 * medianDelta), minMs, ...).
func PollProbeGPU(ctx context.Context, mono *clock.Mono, budgetSec int, minMs int) (bestMS int, ok bool) {
	bufMgr := ring.NewManager()
	// windowSec=float64, periodMs=int
	gpuSz := ring.SizeForWindow(budgetSec, minMs)
	gpuBuf := ring.GetOrCreateSync[ring.GpuSample](bufMgr, "gpu-cal", gpuSz)
	g := gpuCollector.New(gpuCollector.Config{Buf: gpuBuf, Mono: mono})

	if err := g.Init(ctx); err != nil {
		klog.Errorf("gpu.PollProbe: init failed: %v", err)
		return 0, false
	}

	steps := []int{200, 100, 50}
	if minMs < steps[len(steps)-1] {
		steps = append(steps, minMs)
	}

	var changeDts []float64
	deadline := time.Duration(budgetSec) * time.Second
	stepBudget := deadline / time.Duration(len(steps))

	// Use mW integers (as reported by NVML via collector)
	lastVal := func(s ring.GpuSample) int { return s.PowerMilliW }

	var prev int
	var havePrev bool
	var lastChange time.Time

	for _, ms := range steps {
		per := time.Duration(ms) * time.Millisecond
		windowCtx, cancel := context.WithTimeout(ctx, stepBudget)

		havePrev = false
		lastChange = time.Time{}

		BusyLoop(windowCtx, stepBudget, per, func(ts time.Time) {
			g.Collect(windowCtx, ts)
			if s, ok := PeekOne(gpuBuf); ok {
				v := lastVal(s)
				if !havePrev {
					prev = v
					havePrev = true
					lastChange = ts
					return
				}
				if v != prev {
					dt := ts.Sub(lastChange).Seconds()
					changeDts = append(changeDts, dt)
					prev = v
					lastChange = ts
				}
			}
		})

		cancel()
	}

	if len(changeDts) == 0 {
		klog.V(2).Infof("gpu.PollProbe: no value changes observed within budget")
		return 0, false
	}

	medianDt := Median(changeDts)
	best := int(0.8 * medianDt * 1000.0)
	if best < minMs {
		best = minMs
	}

	klog.V(2).Infof("gpu.PollProbe: medianΔ=%.1fms => best=%dms (min=%d)", medianDt*1000, best, minMs)
	return best, true
}

func MeasureDelayGPU(ctx context.Context, _ *clock.Mono, budgetSec int) (delayMS int, ok bool) {
	_ = budgetSec
	return 0, false
}

func IdleBaselineGPU(ctx context.Context, mono *clock.Mono, idleBudgetSec int, pollMs int) (p5 float64, ok bool) {
	bufMgr := ring.NewManager()
	gpuSz := ring.SizeForWindow(idleBudgetSec, pollMs)
	gpuBuf := ring.GetOrCreateSync[ring.GpuSample](bufMgr, "gpu-idle", gpuSz)
	g := gpuCollector.New(gpuCollector.Config{Buf: gpuBuf, Mono: mono})
	if err := g.Init(ctx); err != nil {
		return 0, false
	}

	values := make([]float64, 0, gpuSz)
	per := time.Duration(pollMs) * time.Millisecond

	BusyLoop(ctx, time.Duration(idleBudgetSec)*time.Second, per, func(ts time.Time) {
		g.Collect(ctx, ts)
		if s, ok := PeekOne(gpuBuf); ok {
			// convert mW -> W if you want float watt stats
			values = append(values, float64(s.PowerMilliW)/1000.0)
		}
	})

	if len(values) == 0 {
		return 0, false
	}
	return P5(values), true
}
