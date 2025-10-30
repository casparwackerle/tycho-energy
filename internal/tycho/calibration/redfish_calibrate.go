package calibration

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	rfCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/redfish"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// PollProbeRedfish: probe down to minMs, infer publish cadence from value changes; keep 0.8×medianΔ as best.
func PollProbeRedfish(ctx context.Context, mono *clock.Mono, budgetSec int, minMs int) (bestMS int, ok bool) {
	bufMgr := ring.NewManager()
	rfSz := ring.SizeForWindow(budgetSec, minMs)
	rfBuf := ring.GetOrCreateSync[ring.RedfishSample](bufMgr, "rf-cal", rfSz)
	rf := rfCollector.New(rfCollector.Config{Buf: rfBuf, Mono: mono})

	steps := []int{2000, 1000, 750, 500}
	if minMs < steps[len(steps)-1] {
		steps = append(steps, minMs)
	}

	var changeDts []float64
	deadline := time.Duration(budgetSec) * time.Second
	stepBudget := deadline / time.Duration(len(steps))

	lastVal := func(s ring.RedfishSample) int { return int(s.PowerWatts) } // adjust if different

	var prev int
	var havePrev bool
	var lastChange time.Time

	for _, ms := range steps {
		per := time.Duration(ms) * time.Millisecond
		windowCtx, cancel := context.WithTimeout(ctx, stepBudget)

		havePrev = false
		lastChange = time.Time{}

		BusyLoop(windowCtx, stepBudget, per, func(ts time.Time) {
			rf.Collect(windowCtx, ts)
			if s, ok := PeekOne(rfBuf); ok {
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
		return 0, false
	}

	medianDt := Median(changeDts)
	best := int(0.8 * medianDt * 1000.0)
	if best < minMs {
		best = minMs
	}
	return best, true
}

func MeasureDelayRedfish(ctx context.Context, _ *clock.Mono, budgetSec int) (delayMS int, ok bool) {
	_ = budgetSec
	return 0, false
}

func IdleBaselineRedfish(ctx context.Context, mono *clock.Mono, idleBudgetSec int, pollMs int) (p5 float64, ok bool) {
	bufMgr := ring.NewManager()
	rfSz := ring.SizeForWindow(idleBudgetSec, pollMs)
	rfBuf := ring.GetOrCreateSync[ring.RedfishSample](bufMgr, "rf-idle", rfSz)
	rf := rfCollector.New(rfCollector.Config{Buf: rfBuf, Mono: mono})

	values := make([]float64, 0, rfSz)
	per := time.Duration(pollMs) * time.Millisecond

	BusyLoop(ctx, time.Duration(idleBudgetSec)*time.Second, per, func(ts time.Time) {
		rf.Collect(ctx, ts)
		if s, ok := PeekOne(rfBuf); ok {
			values = append(values, float64(s.PowerWatts))
		}
	})

	if len(values) == 0 {
		return 0, false
	}
	return P5(values), true
}
