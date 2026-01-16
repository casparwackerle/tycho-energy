// file: internal/tycho/analysis/fusion/extract.go
package fusion

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	gpufuse "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/gpu"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"k8s.io/klog/v2"
)

// DistributeDeltaToBins distributes a delta value (in "units") over overlap between
// interval (t0,t1] and bins of size quantumTicks.
// DistributeDeltaToBinsClamped distributes a delta value over overlap between interval (t0,t1]
// and fusion bins, but only writes into bins k in [kMin,kMax].
// This makes incremental updates safe even when a predecessor sample extends before the update region.
func DistributeDeltaToBinsClamped(
	cache *Cache,
	t0, t1 uint64,
	delta float64,
	kMin, kMax BinIndex,
	adder func(k BinIndex, v float64),
) int {
	if cache == nil || cache.QuantumTicks == 0 || adder == nil {
		return 0
	}
	if t1 <= t0 || delta == 0 {
		return 0
	}
	if kMax < kMin {
		return 0
	}

	q := cache.QuantumTicks

	// Determine bin range touched by interval.
	kStart := BinIndex(int64(t0 / q))
	kEnd := BinIndex(int64((t1 - 1) / q)) // inclusive bin covering last tick before t1

	// Clamp to requested range.
	if kStart < kMin {
		kStart = kMin
	}
	if kEnd > kMax {
		kEnd = kMax
	}
	if kEnd < kStart {
		return 0
	}

	used := 0
	intervalTicks := float64(t1 - t0)

	for k := kStart; k <= kEnd; k++ {
		b0 := uint64(k) * q
		b1 := b0 + q

		lo := maxU64(t0, b0)
		hi := minU64(t1, b1)
		if hi <= lo {
			continue
		}
		frac := float64(hi-lo) / intervalTicks
		if frac <= 0 {
			continue
		}
		if frac > 1 {
			frac = 1
		}
		adder(k, delta*frac)
		used++
	}

	return used
}
func ExtractRAPLIntoCache(
	c *analysis.Cycle,
	cache *Cache,
	delayTicks uint64,
	rawStart, rawEnd uint64,
	kStart, kEnd BinIndex,
) (samples int, usedIntervals int) {
	if c == nil || cache == nil || c.Rapl() == nil {
		return 0, 0
	}
	if kEnd < kStart {
		return 0, 0
	}
	r := c.Rapl()

	// Include predecessor tick for interval deltas.
	ticks := analysisctx.FilterWindowWithPrevChrono[ring.RaplTick](
		r, rawStart, rawEnd,
		func(t ring.RaplTick) uint64 { return t.SampleMeta.Mono },
	)
	if len(ticks) < 2 {
		return len(ticks), 0
	}
	samples = len(ticks)

	for i := 1; i < len(ticks); i++ {
		prev := ticks[i-1]
		cur := ticks[i]
		t0 := prev.SampleMeta.Mono
		t1 := cur.SampleMeta.Mono
		if t1 <= t0 {
			continue
		}

		// Sum socket deltas (monotonic normalized semantics).
		var dPkg, dDram uint64
		for sid, ctrPrev := range prev.Sockets {
			ctrCur, ok := cur.Sockets[sid]
			if !ok {
				continue
			}
			dPkg += analysisops.DeltaU64(ctrPrev.Pkg, ctrCur.Pkg)
			dDram += analysisops.DeltaU64(ctrPrev.DRAM, ctrCur.DRAM)
		}

		// Distribute into bins (mJ) but clamp to update range.
		if dPkg != 0 {
			usedIntervals += DistributeDeltaToBinsClamped(cache, t0, t1, float64(dPkg), kStart, kEnd, cache.AddEpkg)
		}
		if dDram != 0 {
			usedIntervals += DistributeDeltaToBinsClamped(cache, t0, t1, float64(dDram), kStart, kEnd, cache.AddEdram)
		}
	}

	_ = delayTicks // reserved for future quality accounting
	return samples, usedIntervals
}
func ExtractBpfCPUInstrIntoCache(
	c *analysis.Cycle,
	cache *Cache,
	rawStart, rawEnd uint64,
	kStart, kEnd BinIndex,
) (samples int, usedIntervals int) {
	if c == nil || cache == nil || c.Bpf() == nil {
		return 0, 0
	}
	if kEnd < kStart {
		return 0, 0
	}
	r := c.Bpf()

	ticks := analysisctx.FilterWindowWithPrevChrono[ring.BpfTick](
		r, rawStart, rawEnd,
		func(t ring.BpfTick) uint64 { return t.Mono },
	)
	if len(ticks) < 2 {
		return len(ticks), 0
	}
	samples = len(ticks)

	// delta for samples[i] belongs to (t_{i-1}, t_i]
	for i := 1; i < len(ticks); i++ {
		prev := ticks[i-1]
		cur := ticks[i]
		t0 := prev.Mono
		t1 := cur.Mono
		if t1 <= t0 {
			continue
		}
		var dInstr uint64
		for j := range cur.Procs {
			dInstr += cur.Procs[j].CPUInstr
		}
		if dInstr == 0 {
			continue
		}
		usedIntervals += DistributeDeltaToBinsClamped(cache, t0, t1, float64(dInstr), kStart, kEnd, cache.AddCPUInstr)
	}

	return samples, usedIntervals
}

func ExtractGPUIntoCache(
	c *analysis.Cycle,
	cache *Cache,
	rawStart, rawEnd uint64,
	kStart, kEnd BinIndex,
) (uuids int, binsTouched int) {
	if c == nil || cache == nil || c.Gpu() == nil || c.Mono == nil {
		return 0, 0
	}
	if cache.QuantumTicks == 0 || cache.HorizonBins <= 0 {
		return 0, 0
	}

	// Clamp requested bin range to cache horizon.
	hStart := cache.StartBin
	hEnd := cache.StartBin + BinIndex(cache.HorizonBins) - 1
	if kStart < hStart {
		kStart = hStart
	}
	if kEnd > hEnd {
		kEnd = hEnd
	}
	if kEnd < kStart {
		return 0, 0
	}

	// Collect GPU UUIDs observed in raw window.
	seg1, seg2 := c.Gpu().ViewChrono()
	seen := map[string]bool{}
	ingest := func(seg []ring.GpuTick) {
		for _, t := range seg {
			if t.SampleMeta.Mono < rawStart || t.SampleMeta.Mono > rawEnd {
				continue
			}
			for i := range t.Devices {
				u := t.Devices[i].UUID
				if u == "" {
					continue
				}
				seen[u] = true
			}
		}
	}
	ingest(seg1)
	ingest(seg2)

	if len(seen) == 0 {
		return 0, 0
	}

	monoQ := c.Mono.Quantum()
	if monoQ <= 0 {
		monoQ = time.Millisecond
	}
	monoQuantumSec := monoQ.Seconds()

	for uuid := range seen {
		ser, ok := gpufuse.GetCorrectedSeries(c.State, uuid)
		if !ok || ser.DtSec <= 0 || len(ser.PW) == 0 {
			continue
		}
		uuids++

		for k := kStart; k <= kEnd; k++ {
			b0 := uint64(k) * cache.QuantumTicks
			b1 := b0 + cache.QuantumTicks

			// Convert bin window to seconds.
			aSec := float64(b0) * monoQuantumSec
			bSec := float64(b1) * monoQuantumSec
			if bSec <= aSec {
				continue
			}

			// Compute energy over [aSec,bSec] by overlapping ZOH bins of the GPU series.
			eJ := integrateSeriesZOH(ser, aSec, bSec)
			if eJ <= 0 {
				continue
			}
			cache.AddEgpu(k, eJ*1000.0) // J -> mJ
			binsTouched++
		}
	}

	return uuids, binsTouched
}

// integrateSeriesZOH integrates Series power (W) over [aSec,bSec] assuming ZOH per bin.
func integrateSeriesZOH(ser gpufuse.Series, aSec, bSec float64) float64 {
	if ser.DtSec <= 0 || len(ser.PW) == 0 || bSec <= aSec {
		return 0
	}

	serStart := ser.T0Sec
	serEnd := ser.T0Sec + float64(len(ser.PW))*ser.DtSec
	a := math.Max(aSec, serStart)
	b := math.Min(bSec, serEnd)
	if b <= a {
		return 0
	}

	// Find first bin index that could overlap a.
	i0 := int(math.Floor((a - ser.T0Sec) / ser.DtSec))
	if i0 < 0 {
		i0 = 0
	}
	if i0 >= len(ser.PW) {
		return 0
	}

	var eJ float64
	for i := i0; i < len(ser.PW); i++ {
		t0 := ser.T0Sec + float64(i)*ser.DtSec
		t1 := t0 + ser.DtSec
		if t1 <= a {
			continue
		}
		segA := math.Max(a, t0)
		segB := math.Min(b, t1)
		if segB > segA {
			p := ser.PW[i]
			if p < 0 {
				p = 0
			}
			eJ += p * (segB - segA)
		}
		if t1 >= b {
			break
		}
	}
	return eJ
}

func ExtractRedfishObs(
	c *analysis.Cycle,
	chassis string,
	delayTicks uint64,
	rawStart, rawEnd uint64,
	kernel RedfishKernel,
	kernelMs int,
) ([]RedfishObs, int) {
	if c == nil || c.Redfish() == nil {
		return nil, 0
	}

	r := c.Redfish()
	samples := analysisctx.FilterWindowChrono[ring.RedfishSample](
		r, rawStart, rawEnd,
		func(s ring.RedfishSample) uint64 { return s.Mono },
	)
	if len(samples) == 0 {
		// No data in the window at all.
		return nil, 0
	}

	out := make([]RedfishObs, 0, len(samples))

	// Diagnostics counters (for the "why is out empty?" case).
	total := len(samples)
	var matchedChassis int
	var droppedChassis int
	var droppedEarly int // tRaw <= delayTicks
	var emitted int

	for i := range samples {
		s := samples[i]

		if chassis != "" && s.ChassisID != chassis {
			droppedChassis++
			continue
		}
		matchedChassis++

		tRaw := s.Mono

		// CRITICAL: never emit MonoCorr==0.
		// If we clamp to 0, obsToRow(avg1s_trailing) can underflow (t1-1) and/or scan absurd ranges.
		if tRaw <= delayTicks {
			droppedEarly++
			continue
		}
		tCorr := tRaw - delayTicks
		if tCorr == 0 {
			// Should be impossible due to guard above, but keep belt-and-suspenders.
			droppedEarly++
			continue
		}

		out = append(out, RedfishObs{
			ChassisID: s.ChassisID,
			MonoCorr:  tCorr,
			ValueMW:   s.PowerWatts * 1000.0,
			Kernel:    kernel,
			KernelMs:  kernelMs,
		})
		emitted++
	}

	// Log only in the pathological case (what you are debugging).
	// This makes the log actionable without spamming normal operation.
	if emitted == 0 {
		klog.V(2).Infof(
			"[analysis] redfish obs empty: chassis=%q total=%d matchedChassis=%d droppedChassis=%d droppedEarly(tRaw<=delayTicks)=%d delayTicks=%d raw=[%d,%d]",
			chassis, total, matchedChassis, droppedChassis, droppedEarly, delayTicks, rawStart, rawEnd,
		)
	}

	return out, len(out)
}

func minU64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
func maxU64(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}
