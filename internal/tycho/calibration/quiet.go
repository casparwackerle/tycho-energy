package calibration

import (
	"math"
	"runtime"
	"sort"

	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

var cpuCount int

func init() {
	if cpuCount <= 0 {
		cpuCount = runtime.NumCPU()
	}
}

// A compact aggregation of one "tick" (all samples that share the same Mono)
type bpfTick struct {
	monoNS uint64
	// Bin-based totals (preferred when present)
	hasBins   bool
	idleNS    uint64
	irqNS     uint64
	softirqNS uint64
	// Process-based fallback (sum over all PIDs for this tick)
	sumRunUs uint64 // microseconds
}

// Group a chronological snapshot into per-tick aggregates keyed by Mono.
func groupBpfTicks(snap []ring.BpfSample) []bpfTick {
	if len(snap) == 0 {
		return nil
	}
	ticks := make([]bpfTick, 0, 64)
	var cur bpfTick
	var curInit bool

	flush := func() {
		if curInit {
			ticks = append(ticks, cur)
		}
		cur = bpfTick{}
		curInit = false
	}

	for i := 0; i < len(snap); i++ {
		s := snap[i]
		if !curInit || s.SampleMeta.Mono != cur.monoNS {
			flush()
			cur.monoNS = s.SampleMeta.Mono
			curInit = true
		}
		// "bin" record: Pid==0 and any bin non-zero
		if s.Pid == 0 && (s.IdleNS != 0 || s.IRQNS != 0 || s.SoftirqNS != 0) {
			cur.hasBins = true
			cur.idleNS += s.IdleNS
			cur.irqNS += s.IRQNS
			cur.softirqNS += s.SoftirqNS
			continue
		}
		// process record: accumulate run time delta in microseconds
		cur.sumRunUs += s.ProcessRunUs
	}
	flush()

	sort.Slice(ticks, func(i, j int) bool { return ticks[i].monoNS < ticks[j].monoNS })
	return ticks
}

// Compute instantaneous utilization between two consecutive ticks.
func instantaneousUtilBpfTicks(a, b bpfTick) float64 {
	if b.monoNS <= a.monoNS || cpuCount <= 0 {
		return 1.0
	}
	dtNS := float64(b.monoNS - a.monoNS) // interval width in ns
	denom := dtNS * float64(cpuCount)    // total CPU-time capacity across all CPUs

	// Preferred: bins on both ends (idle fraction directly from b)
	if a.hasBins && b.hasBins {
		idleFrac := float64(b.idleNS) / denom
		u := 1.0 - idleFrac
		if u < 0 {
			u = 0
		}
		if u > 1 {
			u = 1
		}
		return u
	}

	// Fallback: sum of per-process run time (from b), convert us -> ns
	busyNS := float64(b.sumRunUs) * 1e3
	u := busyNS / denom
	if u < 0 {
		u = 0
	}
	if u > 1 {
		u = 1
	}
	return u
}

// Decide "continuous low CPU" from a recent BPF snapshot.
func isQuietFromBpfSnap(snap []ring.BpfSample) (bool, float64, float64) {
	ticks := groupBpfTicks(snap)
	if len(ticks) < 2 {
		return false, 0, 1
	}

	const (
		requiredMean = 0.05 // 5%
		lowCut       = 0.10 // 10%
		requiredFrac = 0.80 // 80%
	)

	lowCount := 0
	sum := 0.0
	n := 0

	for i := 1; i < len(ticks); i++ {
		u := instantaneousUtilBpfTicks(ticks[i-1], ticks[i])
		if math.IsNaN(u) || math.IsInf(u, 0) || u < 0 {
			continue
		}
		sum += u
		n++
		if u <= lowCut {
			lowCount++
		}
	}
	if n == 0 {
		return false, 0, 1
	}

	mean := sum / float64(n)
	frac := float64(lowCount) / float64(n)
	return mean <= requiredMean && frac >= requiredFrac, frac, mean
}

// For logging only (optional)
func meanCPU(snap []ring.BpfSample) float64 {
	ticks := groupBpfTicks(snap)
	if len(ticks) < 2 {
		return 1.0
	}
	sum := 0.0
	n := 0
	for i := 1; i < len(ticks); i++ {
		u := instantaneousUtilBpfTicks(ticks[i-1], ticks[i])
		if !math.IsNaN(u) && !math.IsInf(u, 0) && u >= 0 {
			sum += u
			n++
		}
	}
	if n == 0 {
		return 1.0
	}
	return sum / float64(n)
}
