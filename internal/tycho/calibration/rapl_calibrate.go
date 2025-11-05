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
	"sort"

	"k8s.io/klog/v2"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// Returns (baselines, true) on success, or (nil, false) if no usable series
// can be built. Context cancelation aborts early with (nil, false).
func IdleBaselineRAPLFromSnap(
	ctx context.Context,
	mono *clock.Mono,
	snap []ring.RaplSample,
) (IdleBaselines, bool) {
	if len(snap) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=empty_snapshot")
		return nil, false
	}

	// Ensure chronological order by monotonic timestamp (ns).
	s := make([]ring.RaplSample, len(snap))
	copy(s, snap)
	sort.Slice(s, func(i, j int) bool { return s[i].SampleMeta.Mono < s[j].SampleMeta.Mono })

	// Per-socket accumulators of instantaneous power samples (in Watts).
	type domVals map[Domain][]float64
	acc := make(map[SocketID]domVals, 4)

	// Keep last raw energy counters (mJ) to compute deltas per socket/domain.
	var last struct {
		ok      bool
		monoNS  uint64
		sockets map[int]ring.RaplDomainCounters
	}

	push := func(sock SocketID, dom Domain, w float64) {
		if _, ok := acc[sock]; !ok {
			acc[sock] = make(domVals, 4)
		}
		acc[sock][dom] = append(acc[sock][dom], w)
	}

	for i := 0; i < len(s); i++ {
		select {
		case <-ctx.Done():
			return nil, false
		default:
		}
		cur := s[i]

		if last.ok {
			// Δt from monotonic timestamps (ns → s)
			if cur.SampleMeta.Mono <= last.monoNS {
				// Non-increasing or zero Δt — skip this step
				goto after
			}
			dt := float64(cur.SampleMeta.Mono-last.monoNS) / 1e9
			if dt <= 0 || math.IsInf(dt, 0) || math.IsNaN(dt) {
				goto after
			}

			// For sockets present in both last and current, compute per-domain ΔE (mJ) -> W
			for socketID, curCtr := range cur.Sockets {
				prevCtr, ok := last.sockets[socketID]
				if !ok {
					continue
				}

				type pair struct {
					dom Domain
					dmJ float64
				}
				deltas := []pair{
					{DomainPkg, float64(curCtr.Pkg) - float64(prevCtr.Pkg)},
					{DomainCore, float64(curCtr.Core) - float64(prevCtr.Core)},
					{DomainUncore, float64(curCtr.Uncore) - float64(prevCtr.Uncore)},
					{DomainDRAM, float64(curCtr.DRAM) - float64(prevCtr.DRAM)},
				}

				for _, p := range deltas {
					// Guard against wraps/garbage: negative or absurd deltas are skipped.
					if p.dmJ <= 0 || p.dmJ > 1e12 { // 1e12 mJ == 1e9 J — impossible for one tick
						continue
					}
					watts := (p.dmJ / 1000.0) / dt // mJ→J, then /s
					if watts < 0 || math.IsInf(watts, 0) || math.IsNaN(watts) {
						continue
					}
					push(SocketID(socketID), p.dom, watts)
				}
			}
		}

	after:
		last.ok = true
		last.monoNS = cur.SampleMeta.Mono
		last.sockets = cur.Sockets // reference for next delta step
	}

	if len(acc) == 0 {
		klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=false reason=no_power_series")
		return nil, false
	}

	out := make(IdleBaselines, len(acc))
	domainsPresent := make(map[Domain]bool, 4)
	totalKept := 0

	for sock, domMap := range acc {
		out[sock] = make(map[Domain]IdleBaseline, len(domMap))
		for dom, vals := range domMap {
			// Removed: len(vals) < 3 guard.
			// Rely on DeSpikeP5 to return n>0 when it could compute robust stats.
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

	klog.V(2).Infof("phase=calibrate component=rapl kind=idle ok=true sockets=%d domains=%v kept=%d",
		len(out), keysOfDomains(domainsPresent), totalKept)

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
