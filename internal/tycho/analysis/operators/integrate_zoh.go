package analysisops

import "time"

// IntegrateHeldValueZOH integrates a piecewise-constant (zero-order hold) value
// over the window [wStart, wEnd] (mono ticks), returning area in "value * seconds".
//
// Interpretation:
// - samples are chronological.
// - sample i holds value v_i on [t_i, t_{i+1}) (ZOH).
// - if there is no sample after the last one used, the last value is held until wEnd.
// - if no sample exists at/before wStart, integration cannot infer pre-window value (returns 0).
func IntegrateHeldValueZOH[T any](
	samples []T,
	wStart, wEnd uint64,
	monoOf func(T) uint64,
	valueOf func(T) float64,
	quantum time.Duration,
) (area float64, intervals int) {
	if len(samples) == 0 || monoOf == nil || valueOf == nil {
		return 0, 0
	}
	if wEnd <= wStart {
		return 0, 0
	}
	if quantum <= 0 {
		quantum = time.Millisecond
	}

	// We need a value defined at window start. We assume caller prepended a "prev" sample
	// (the last sample with mono < wStart) if it exists. Without that, we cannot integrate correctly.
	// We still handle the case where the first sample occurs before/at wStart.
	// Find first index i such that t_i <= wStart < t_{i+1} (or last).
	i := 0
	for i+1 < len(samples) && monoOf(samples[i+1]) <= wStart {
		i++
	}

	tCur := monoOf(samples[i])
	if tCur > wStart {
		// No sample at/before window start -> undefined ZOH prior. Return 0 conservatively.
		return 0, 0
	}
	vCur := valueOf(samples[i])

	// Integrate from max(tCur, wStart) to min(nextT, wEnd), advancing across sample boundaries.
	t := wStart

	for {
		if t >= wEnd {
			break
		}

		// Next boundary is next sample time, if any, else window end.
		nextT := wEnd
		if i+1 < len(samples) {
			nextT = monoOf(samples[i+1])
			if nextT > wEnd {
				nextT = wEnd
			}
		}

		if nextT > t {
			dtTicks := nextT - t
			dtSec := float64(time.Duration(dtTicks)*quantum) / float64(time.Second)
			area += vCur * dtSec
			intervals++
		}

		// Advance to next sample if it is before window end.
		if i+1 >= len(samples) {
			break
		}
		i++
		t = monoOf(samples[i])
		if t >= wEnd {
			break
		}
		vCur = valueOf(samples[i])
	}

	return area, intervals
}
