package calibration

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"

	raplCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/rapl"
)

// MeasureDelayRAPL: placeholder
func MeasureDelayRAPL(ctx context.Context, _ *clock.Mono, budgetSec int) (delayMS int, ok bool) {
	_ = budgetSec
	return 0, false
}

func IdleBaselineRAPL(ctx context.Context, mono *clock.Mono, idleBudgetSec int, pollMs int) (p5 float64, ok bool) {
	bufMgr := ring.NewManager()
	raplSz := ring.SizeForWindow(idleBudgetSec, pollMs)
	raplBuf := ring.GetOrCreateSync[ring.RaplSample](bufMgr, "rapl-idle", raplSz)
	r := raplCollector.New(raplCollector.Config{Buf: raplBuf, Mono: mono})

	per := time.Duration(pollMs) * time.Millisecond
	values := make([]float64, 0, raplSz)

	var last struct {
		ok   bool
		t    time.Time
		pkgm float64 // mJ
	}
	readPkgMilliJ := func(s ring.RaplSample) float64 {
		var sum float64
		for _, d := range s.Sockets {
			sum += float64(d.Pkg) // mJ
		}
		return sum
	}

	BusyLoop(ctx, time.Duration(idleBudgetSec)*time.Second, per, func(ts time.Time) {
		r.Collect(ctx, ts)
		if s, ok := PeekOne(raplBuf); ok {
			curE := readPkgMilliJ(s)
			if last.ok {
				dE_mJ := curE - last.pkgm
				dt_s := ts.Sub(last.t).Seconds()
				if dt_s > 0 {
					powerW := (dE_mJ / 1000.0) / dt_s
					values = append(values, powerW)
				}
			}
			last.ok = true
			last.pkgm = curE
			last.t = ts
		}
	})

	if len(values) == 0 {
		return 0, false
	}
	return P5(values), true
}
