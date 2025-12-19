package analysisops

import "math"

// IntegrateDeltaWindow integrates per-interval deltas over a window by overlap.
//
// Interpretation:
// - samples must be chronological (best-effort; non-forward intervals are skipped)
// - deltaOf(samples[i]) belongs to interval (mono[i-1], mono[i]]
//
// For each interval, we compute the overlap with [wStart,wEnd] and scale the delta linearly.
// This conserves totals and fixes partial-edge effects without needing end-exclusive windows.
func IntegrateDeltaWindow[T any](
	samples []T,
	wStart, wEnd uint64,
	monoOf func(T) uint64,
	deltaOf func(T) float64,
) (sum float64, usedIntervals int) {
	if len(samples) < 2 || monoOf == nil || deltaOf == nil {
		return 0, 0
	}
	if wEnd <= wStart {
		return 0, 0
	}

	for i := 1; i < len(samples); i++ {
		t0 := monoOf(samples[i-1])
		t1 := monoOf(samples[i])
		if t1 <= t0 {
			continue
		}

		// Overlap of [t0,t1] with [wStart,wEnd] in tick space.
		lo := maxU64(t0, wStart)
		hi := minU64(t1, wEnd)
		if hi <= lo {
			continue
		}

		delta := deltaOf(samples[i])
		if delta == 0 {
			continue
		}

		frac := float64(hi-lo) / float64(t1-t0)
		frac = math.Max(0, math.Min(1, frac))

		sum += delta * frac
		usedIntervals++
	}

	return sum, usedIntervals
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
