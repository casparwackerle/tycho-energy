//package calibration

// import (
// 	"context"
// 	"time"

// 	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
// 	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"

// 	raplCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/rapl"
// )

// func IdleBaselineRAPL(ctx context.Context, mono *clock.Mono, idleBudgetSec int, pollMs int) (p5 float64, ok bool) {
// 	bufMgr := ring.NewManager()
// 	raplSz := ring.SizeForWindow(idleBudgetSec, pollMs)
// 	raplBuf := ring.GetOrCreateSync[ring.RaplSample](bufMgr, "rapl-idle", raplSz)
// 	r := raplCollector.New(raplCollector.Config{Buf: raplBuf, Mono: mono})

// 	per := time.Duration(pollMs) * time.Millisecond
// 	values := make([]float64, 0, raplSz)

// 	var last struct {
// 		ok   bool
// 		t    time.Time
// 		pkgm float64 // mJ
// 	}
// 	readPkgMilliJ := func(s ring.RaplSample) float64 {
// 		var sum float64
// 		for _, d := range s.Sockets {
// 			sum += float64(d.Pkg) // mJ
// 		}
// 		return sum
// 	}

// 	BusyLoop(ctx, time.Duration(idleBudgetSec)*time.Second, per, func(ts time.Time) {
// 		r.Collect(ctx, ts)
// 		if s, ok := PeekOne(raplBuf); ok {
// 			curE := readPkgMilliJ(s)
// 			if last.ok {
// 				dE_mJ := curE - last.pkgm
// 				dt_s := ts.Sub(last.t).Seconds()
// 				if dt_s > 0 {
// 					powerW := (dE_mJ / 1000.0) / dt_s
// 					values = append(values, powerW)
// 				}
// 			}
// 			last.ok = true
// 			last.pkgm = curE
// 			last.t = ts
// 		}
// 	})

// 	if len(values) == 0 {
// 		return 0, false
// 	}
// 	return P5(values), true
// }

package calibration

import (
	"context"
	"math"
	"time"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"

	raplCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/rapl"
)

// IdleBaselineRAPL runs an idle-baseline calibration for RAPL and returns
// per-socket/per-domain robust P5 values (plus min and N inside IdleBaseline).
// It requires a short "quiet" window first; if the system isn't quiet within
// the given budget, it returns ok=false.
func IdleBaselineRAPL(ctx context.Context, mono *clock.Mono, idleBudgetSec int, pollMs int) (IdleBaselines, bool) {
	if idleBudgetSec <= 0 || pollMs <= 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=bad_args idleBudgetSec=%d pollMs=%d", idleBudgetSec, pollMs)
		return nil, false
	}

	// --- 1) Require a quiet window (best-effort, small slice of the budget) ---
	guardDur := time.Second * 2
	if gd := time.Duration(idleBudgetSec) * time.Second / 5; gd > 0 && gd < guardDur {
		guardDur = gd
	}
	if ok, reason := QuietWindowGuard(ctx, mono, guardDur); !ok {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=%s", reason)
		return nil, false
	}

	// --- 2) Prepare ring + collector and sampling ---
	bufMgr := ring.NewManager()
	raplSz := ring.SizeForWindow(idleBudgetSec, pollMs)
	raplBuf := ring.GetOrCreateSync[ring.RaplSample](bufMgr, "rapl-idle", raplSz)
	r := raplCollector.New(raplCollector.Config{Buf: raplBuf, Mono: mono})

	per := time.Duration(pollMs) * time.Millisecond

	// We accumulate per-socket/per-domain power samples (W).
	type domVals map[Domain][]float64    // domain -> []W
	acc := make(map[SocketID]domVals, 4) // socket -> domVals

	// Keep last raw energy counters to compute deltas per socket/domain.
	// Units: mJ counters from the sample.
	var last struct {
		ok      bool
		t       time.Time
		sockets map[int]ring.RaplDomainCounters
	}

	// Helper: push computed power into acc
	push := func(sock SocketID, dom Domain, w float64) {
		if _, ok := acc[sock]; !ok {
			acc[sock] = make(domVals, 4)
		}
		acc[sock][dom] = append(acc[sock][dom], w)
	}

	sampleDur := time.Duration(idleBudgetSec) * time.Second
	startTs := time.Now()

	BusyLoop(ctx, sampleDur, per, func(ts time.Time) {
		r.Collect(ctx, ts)

		s, ok := PeekOne(raplBuf)
		if !ok {
			return
		}

		// Skip the very first sample (need a delta)
		if last.ok {
			dt := ts.Sub(last.t).Seconds()
			if dt <= 0 {
				// Non-positive dt (clock oddity) — skip
				goto after
			}

			// For each socket present in both last and current, compute per-domain deltas
			for socketID, cur := range s.Sockets {
				prev, havePrev := last.sockets[socketID]
				if !havePrev {
					continue
				}

				// Domain helpers (mJ deltas)
				type pair struct {
					dom Domain
					d   float64
				}
				deltas := []pair{
					{DomainPkg, float64(cur.Pkg) - float64(prev.Pkg)},
					{DomainCore, float64(cur.Core) - float64(prev.Core)},
					{DomainUncore, float64(cur.Uncore) - float64(prev.Uncore)},
					{DomainDRAM, float64(cur.DRAM) - float64(prev.DRAM)},
				}

				for _, p := range deltas {
					// Wrap/garbage guard: negative or absurd deltas are skipped
					if p.d <= 0 || p.d > 1e12 { // 1e12 mJ ~ 1e9 J — clearly bogus for one tick
						continue
					}
					watts := (p.d / 1000.0) / dt // mJ -> J, then /s
					if watts < 0 || math.IsInf(watts, 0) || math.IsNaN(watts) {
						continue
					}
					push(SocketID(socketID), p.dom, watts)
				}
			}
		}

	after:
		last.ok = true
		last.t = ts
		// Deep-ish copy reference for next delta step
		last.sockets = s.Sockets
	})

	// --- 3) Build per-socket/per-domain robust baselines (p5 with de-spiking) ---
	if len(acc) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=no_samples")
		return nil, false
	}

	out := make(IdleBaselines, len(acc))
	domainsPresent := make(map[Domain]bool, 4)
	totalKept := 0

	for sock, domMap := range acc {
		out[sock] = make(map[Domain]IdleBaseline, len(domMap))
		for dom, vals := range domMap {
			if len(vals) < 3 {
				// too few points → skip that domain/socket silently
				continue
			}
			p5, minv, n := DeSpikeP5(vals)
			if n == 0 {
				continue
			}
			out[sock][dom] = IdleBaseline{
				P5:     p5,
				Min:    minv,
				N:      n,
				FromTs: mono.Now(),
			}
			domainsPresent[dom] = true
			totalKept += n
		}
	}

	if len(out) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=empty_after_trim")
		return nil, false
	}

	// --- 4) Structured logs + brief explain (caller can also stash into res.Notes) ---
	elapsed := time.Since(startTs)
	// one compact summary
	klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=true sockets=%d domains=%v kept=%d dur=%v",
		len(out), keysOfDomains(domainsPresent), totalKept, elapsed)

	// per-socket/per-domain lines
	for sock, domMap := range out {
		for dom, bl := range domMap {
			klog.V(2).Infof("phase=calibrate component=rapl kind=idle socket=%d domain=%s p5=%.6fW min=%.6fW n=%d ts=%d",
				int(sock), string(dom), bl.P5, bl.Min, bl.N, bl.FromTs)
		}
	}

	return out, true
}

func keysOfDomains(m map[Domain]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for d := range m {
		out = append(out, string(d))
	}
	return out
}
