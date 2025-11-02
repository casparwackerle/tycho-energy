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
// With the new model, each ring.BpfTick is already a full tick: bins + per-PID deltas.
// We still merge duplicates defensively if multiple entries share the same Mono.
func groupBpfTicks(snap []ring.BpfTick) []bpfTick {
	if len(snap) == 0 {
		return nil
	}

	ticks := make([]bpfTick, 0, len(snap))
	var cur bpfTick
	var curInit bool
	var curMono uint64

	flush := func() {
		if curInit {
			ticks = append(ticks, cur)
		}
		cur = bpfTick{}
		curInit = false
	}

	for i := 0; i < len(snap); i++ {
		s := snap[i]

		// Start a new aggregate if this is the first or Mono changed.
		if !curInit || s.SampleMeta.Mono != curMono {
			flush()
			curMono = s.SampleMeta.Mono
			cur.monoNS = curMono
			cur.idleNS = s.IdleNS
			cur.irqNS = s.IRQNS
			cur.softirqNS = s.SoftirqNS
			cur.hasBins = (s.IdleNS != 0) || (s.IRQNS != 0) || (s.SoftirqNS != 0)

			// Sum per-PID runtime (µs) for this tick.
			var sum uint64
			for j := range s.Procs {
				sum += s.Procs[j].ProcessRunUs
			}
			cur.sumRunUs = sum
			curInit = true
			continue
		}

		// Defensive merge for duplicate Mono entries (shouldn't normally happen).
		cur.idleNS += s.IdleNS
		cur.irqNS += s.IRQNS
		cur.softirqNS += s.SoftirqNS
		if (s.IdleNS != 0) || (s.IRQNS != 0) || (s.SoftirqNS != 0) {
			cur.hasBins = true
		}
		for j := range s.Procs {
			cur.sumRunUs += s.Procs[j].ProcessRunUs
		}
	}
	flush()

	// SnapshotChrono() should already be chronological, but keep sort for robustness.
	sort.Slice(ticks, func(i, j int) bool { return ticks[i].monoNS < ticks[j].monoNS })
	return ticks
}

// Compute instantaneous utilization between two consecutive aggregated BPF ticks.
func instantaneousUtilBpfTicks(a, b bpfTick) float64 {
	if b.monoNS <= a.monoNS || cpuCount <= 0 {
		return 1.0
	}

	dtNS := float64(b.monoNS - a.monoNS) // interval width in ns
	denom := dtNS * float64(cpuCount)    // total CPU-time capacity across all CPUs

	// Preferred path: bins available in both ticks — derive idle fraction directly.
	if a.hasBins && b.hasBins {
		idleFrac := float64(b.idleNS) / denom
		if idleFrac < 0 {
			idleFrac = 0
		}
		if idleFrac > 1 {
			idleFrac = 1
		}
		return 1.0 - idleFrac
	}

	// Fallback: no reliable bins, approximate from per-process runtime deltas (µs → ns).
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
// Returns: (isQuiet, fracBelowCut, meanUtil)
func isQuietFromBpfSnap(snap []ring.BpfTick) (bool, float64, float64) {
	ticks := groupBpfTicks(snap)
	if len(ticks) < 2 {
		return false, 0, 1
	}

	const (
		requiredMean = 0.05 // 5%
		lowCut       = 0.10 // 10%
		requiredFrac = 0.80 // 80%
	)

	var (
		weightedSumU float64 // Σ u_i * dt_i
		totalDt      float64 // Σ dt_i
		lowDt        float64 // Σ dt_i where u_i <= lowCut
	)

	for i := 1; i < len(ticks); i++ {
		a, b := ticks[i-1], ticks[i]
		if b.monoNS <= a.monoNS {
			continue
		}
		dt := float64(b.monoNS - a.monoNS) // ns
		if dt <= 0 {
			continue
		}
		u := instantaneousUtilBpfTicks(a, b)
		if math.IsNaN(u) || math.IsInf(u, 0) || u < 0 {
			continue
		}
		weightedSumU += u * dt
		totalDt += dt
		if u <= lowCut {
			lowDt += dt
		}
	}

	if totalDt == 0 {
		return false, 0, 1
	}

	mean := weightedSumU / totalDt
	frac := lowDt / totalDt
	return mean <= requiredMean && frac >= requiredFrac, frac, mean
}

// meanCPU computes the average instantaneous CPU utilization over a recent BPF snapshot.
// Used only for logging or summary statistics.
func meanCPU(snap []ring.BpfTick) float64 {
	ticks := groupBpfTicks(snap)
	if len(ticks) < 2 {
		return 1.0
	}

	var sumWeighted float64
	var totalDt float64

	for i := 1; i < len(ticks); i++ {
		a, b := ticks[i-1], ticks[i]
		if b.monoNS <= a.monoNS {
			continue
		}
		dt := float64(b.monoNS - a.monoNS)
		u := instantaneousUtilBpfTicks(a, b)
		if math.IsNaN(u) || math.IsInf(u, 0) || u < 0 {
			continue
		}
		sumWeighted += u * dt
		totalDt += dt
	}

	if totalDt == 0 {
		return 1.0
	}
	return sumWeighted / totalDt
}
