// file: internal/tycho/analysis/gpu/reconstructor.go
package gpu

import (
	"math"
	"sort"
)

// Default (internal) weights. Not user-configurable for now.
//
// Weights are inverse expected error (1/sigma) in the residual's unit.
// - Instant: noisy -> larger sigma -> smaller weight
// - Avg(1s): smoother -> smaller sigma -> larger weight
// - Energy: very reliable when present -> very small sigma_E -> very large weight
//
// Semantics:
// - wInst: pulls the reconstructed signal toward instantaneous readings (fast but noisy)
// - wAvg: pulls toward NVML "1 second average" (energy accurate but smeared)
// - wEnergy: pulls toward cumulative energy deltas (if present and enabled)
// - lambdaD: smoothness prior (2nd difference); discourages unrealistic high-frequency oscillation
const (
	wInst   = 1.0   // moderate
	wAvg    = 6.0   // high: enforce 1s-average strongly
	wEnergy = 40.0  // very high: cumulative energy when present
	lambdaD = 5.0   // smoothness (2nd diff). Tune later if needed.
	eps     = 1e-12 // Numerical diagonal stabilization
)

// Internal solver defaults (not user-configurable for now).
//
// Why maxIter ~120?
// - With operator-form CG and our regularization, convergence is typically fast.
// - We want bounded runtime per GPU per cycle (analysis cadence).
// - 120 is a practical upper bound that has shown good residuals in your logs.
// We also cap by 2*N to avoid overshooting on tiny problems.
const (
	defaultMaxIter = 120
	defaultTol     = 1e-6 // relative residual target (dimensionless)
)

// Reconstruct builds a corrected uniform-grid power series for one GPU UUID.
//
// Inputs:
// - endSec: corrected alignment time (seconds), typically corrected analysis window end
// - historySec: how far back to cover (seconds)
// - solveSec: window length (seconds) on which constraints are applied near the end (<= historySec)
// - dtSec: grid spacing in seconds (e.g., 0.05)
// - inst, avg: power observations in W
// - cum: cumulative energy observations in mJ (optional)
// - useEnergy: whether to include cum constraints (requires enough points)
//
// Output:
// - Series covering [endSec-historySec, endSec] on dtSec grid
// - ok=false if there is insufficient data to do anything sensible
func Reconstruct(
	uuid string,
	endSec float64,
	historySec float64,
	solveSec float64,
	dtSec float64,
	inst []PowerObs,
	avg []PowerObs,
	cum []CumEnergyObs,
	useEnergy bool,
) (Series, bool) {
	if uuid == "" || dtSec <= 0 || historySec <= 0 || endSec <= 0 {
		return Series{}, false
	}
	if solveSec <= 0 {
		solveSec = math.Min(10.0, historySec)
	}
	if solveSec > historySec {
		solveSec = historySec
	}

	// Grid definition: [t0, endSec] with N bins, power held per bin.
	t0 := endSec - historySec
	if t0 < 0 {
		t0 = 0
	}
	N := int(math.Ceil((endSec - t0) / dtSec))
	if N <= 0 {
		return Series{}, false
	}

	// Sort observations by time (defensive; ring ingest is chronological but keep robust).
	sort.Slice(inst, func(i, j int) bool { return inst[i].TSec < inst[j].TSec })
	sort.Slice(avg, func(i, j int) bool { return avg[i].TSec < avg[j].TSec })
	sort.Slice(cum, func(i, j int) bool { return cum[i].TSec < cum[j].TSec })

	// Solve region is the last solveSec ending at endSec.
	solveStart := endSec - solveSec
	if solveStart < t0 {
		solveStart = t0
	}

	// Filter observations to those relevant for constraints near the end.
	// For avg constraints (1s boxcar ending at t), we need bins up to t and back 1s.
	instF := filterPowerObs(inst, solveStart-0.25, endSec+0.25) // a bit of slack
	avgF := filterPowerObs(avg, solveStart-1.25, endSec+0.25)

	var cumF []CumEnergyObs
	if useEnergy {
		cumF = filterCumObs(cum, solveStart-0.25, endSec+0.25)
		if len(cumF) < 2 {
			useEnergy = false
		}
	}

	// If we have no avg and no inst, we cannot reconstruct.
	if len(instF) == 0 && len(avgF) == 0 && !useEnergy {
		return Series{}, false
	}

	// Map obs -> grid indices (end-aligned).
	// idx = floor((t - t0)/dtSec), clamped.
	instIdx := make([]int, 0, len(instF))
	instVal := make([]float64, 0, len(instF))
	for _, o := range instF {
		i := idxOf(o.TSec, t0, dtSec, N)
		if i < 0 {
			continue
		}
		instIdx = append(instIdx, i)
		instVal = append(instVal, o.Power)
	}

	avgIdx := make([]int, 0, len(avgF))
	avgVal := make([]float64, 0, len(avgF))
	for _, o := range avgF {
		i := idxOf(o.TSec, t0, dtSec, N)
		if i < 0 {
			continue
		}
		avgIdx = append(avgIdx, i)
		avgVal = append(avgVal, o.Power)
	}

	// Energy constraints: consecutive deltas of cumulative energy, mapped to bin ranges.
	type eRow struct {
		i0 int
		i1 int
		dE float64 // J
	}
	var eRows []eRow
	if useEnergy {
		for k := 1; k < len(cumF); k++ {
			a := cumF[k-1]
			b := cumF[k]
			if b.TSec <= a.TSec {
				continue
			}
			// Must be strictly positive; collector may emit 0 for unsupported/invalid.
			if b.EnergyMJ == 0 || a.EnergyMJ == 0 {
				continue
			}
			if b.EnergyMJ <= a.EnergyMJ {
				continue
			}
			i0 := idxOf(a.TSec, t0, dtSec, N)
			i1 := idxOf(b.TSec, t0, dtSec, N)
			if i0 < 0 || i1 < 0 || i1 <= i0 {
				continue
			}
			dEJ := float64(b.EnergyMJ-a.EnergyMJ) / 1000.0
			eRows = append(eRows, eRow{i0: i0, i1: i1, dE: dEJ})
		}
		if len(eRows) == 0 {
			useEnergy = false
		}
	}

	// Averaging operator length: L bins ~ 1 second.
	L := int(math.Round(1.0 / dtSec))
	if L < 1 {
		L = 1
	}

	// Build b = sum w^2 M^T y and operator applyA = sum w^2 M^T M + λ D^T D + ridge I
	bvec := make([]float64, N)

	// Inst: M = select at idx
	for j, i := range instIdx {
		if i < 0 || i >= N {
			continue
		}
		bvec[i] += (wInst * wInst) * instVal[j]
	}

	// Avg: M = boxcar average over last L bins ending at idx (inclusive)
	for j, i := range avgIdx {
		lo := i - (L - 1)
		if lo < 0 {
			lo = 0
		}
		den := float64(i - lo + 1) // near start, fewer bins
		if den <= 0 {
			continue
		}
		cf := (wAvg * wAvg) * (1.0 / den)
		// b += w^2 * M^T y: each contributing bin gets (1/den)*y
		for k := lo; k <= i; k++ {
			bvec[k] += cf * avgVal[j]
		}
	}

	// Energy: M = integrate bins in [i0, i1) with coeff dtSec
	if useEnergy {
		for _, row := range eRows {
			if row.i0 < 0 || row.i1 > N || row.i1 <= row.i0 {
				continue
			}
			// b += w^2 * C^T dE, coeff per bin is dtSec
			cf := (wEnergy * wEnergy) * dtSec
			for k := row.i0; k < row.i1; k++ {
				bvec[k] += cf * row.dE
			}
		}
	}

	applyA := func(dst, x []float64, useEnergyLocal bool) {

		// Numerical diagonal stabilization
		for i := 0; i < N; i++ {
			dst[i] += eps * x[i]
		}

		// Inst: w^2 S^T S is diagonal at inst indices (accumulate duplicates)
		if len(instIdx) > 0 {
			for _, i := range instIdx {
				if i < 0 || i >= N {
					continue
				}
				dst[i] += (wInst * wInst) * x[i]
			}
		}

		// Avg: dst += w^2 A^T (A x)
		if len(avgIdx) > 0 {
			tmp := make([]float64, len(avgIdx))
			// tmp[j] = average over bins for obs j
			for j, i := range avgIdx {
				lo := i - (L - 1)
				if lo < 0 {
					lo = 0
				}
				var s float64
				for k := lo; k <= i; k++ {
					s += x[k]
				}
				den := float64(i - lo + 1)
				if den > 0 {
					tmp[j] = s / den
				}
			}
			// dst += A^T tmp
			for j, i := range avgIdx {
				lo := i - (L - 1)
				if lo < 0 {
					lo = 0
				}
				den := float64(i - lo + 1)
				if den <= 0 {
					continue
				}
				cf := (wAvg * wAvg) * (1.0 / den)
				for k := lo; k <= i; k++ {
					dst[k] += cf * tmp[j]
				}
			}
		}

		// Energy: dst += w^2 C^T (C x)
		if useEnergyLocal && len(eRows) > 0 {
			tmp := make([]float64, len(eRows))
			for j, row := range eRows {
				var s float64
				for k := row.i0; k < row.i1; k++ {
					s += x[k] * dtSec
				}
				tmp[j] = s
			}
			for j, row := range eRows {
				cf := (wEnergy * wEnergy) * dtSec
				for k := row.i0; k < row.i1; k++ {
					dst[k] += cf * tmp[j]
				}
			}
		}

		// Smoothness: λ * D^T D x using 2nd difference.
		if lambdaD > 0 && N >= 3 {
			applySecondDiffDtD(dst, x, lambdaD)
		}
	}

	// Choose a bounded maxIter.
	maxIter := defaultMaxIter
	if maxIter > 2*N {
		maxIter = 2 * N
	}
	if maxIter < 20 {
		maxIter = 20
	}

	// Solve with CG (operator form).
	// NOTE: Iter counts "iterations performed". Hitting maxIter is acceptable if relRes is already small.
	x, iters, relRes := cgSolve(
		func(dst, v []float64) { applyA(dst, v, useEnergy) },
		bvec,
		maxIter,
		defaultTol,
	)

	// Clamp non-negativity (post-solve projection).
	minRaw := minVal(x)
	clamped := 0
	for i := range x {
		if x[i] < 0 {
			x[i] = 0
			clamped++
		}
	}
	minFinal := minVal(x)

	// If negativity is substantial, do a conservative second pass:
	// - disable energy constraints (they can amplify inconsistencies if the device is unstable)
	// - keep the same weights but the projection outcome typically improves by removing one constraint family
	//
	// We only do this when clamping touches a noticeable fraction of bins.
	if clamped > 0 && float64(clamped)/float64(len(x)) >= 0.15 {
		x2, it2, rr2 := cgSolve(
			func(dst, v []float64) { applyA(dst, v, false) },
			bvec,
			maxIter,
			defaultTol,
		)

		minRaw2 := minVal(x2)
		clamped2 := 0
		for i := range x2 {
			if x2[i] < 0 {
				x2[i] = 0
				clamped2++
			}
		}
		minFinal2 := minVal(x2)

		// Prefer the second pass if it reduces clamping significantly (or improves residual materially).
		if clamped2 < clamped || rr2 < relRes*0.8 {
			x = x2
			iters = it2
			relRes = rr2
			minRaw = minRaw2
			clamped = clamped2
			minFinal = minFinal2
			useEnergy = false // record what was actually used in solve info
		}
	}

	ser := Series{
		UUID:  uuid,
		T0Sec: t0,
		DtSec: dtSec,
		PW:    x,
		Last: SolveInfo{
			RowsInst:   len(instIdx),
			RowsAvg:    len(avgIdx),
			RowsEnergy: len(eRows),
			Iter:       iters,
			RelRes:     relRes,
			Clamped:    clamped,
			MinRaw:     minRaw,
			MinFinal:   minFinal,
		},
	}
	return ser, true
}

func filterPowerObs(in []PowerObs, a, b float64) []PowerObs {
	if len(in) == 0 {
		return nil
	}
	out := make([]PowerObs, 0, len(in))
	for _, o := range in {
		if o.TSec < a || o.TSec > b {
			continue
		}
		// ignore obviously invalid power (negative)
		if o.Power < 0 {
			continue
		}
		out = append(out, o)
	}
	return out
}

func filterCumObs(in []CumEnergyObs, a, b float64) []CumEnergyObs {
	if len(in) == 0 {
		return nil
	}
	out := make([]CumEnergyObs, 0, len(in))
	for _, o := range in {
		if o.TSec < a || o.TSec > b {
			continue
		}
		if o.EnergyMJ == 0 {
			continue
		}
		out = append(out, o)
	}
	return out
}

func idxOf(t, t0, dt float64, n int) int {
	if t < t0 {
		return -1
	}
	i := int(math.Floor((t - t0) / dt))
	if i < 0 {
		return -1
	}
	if i >= n {
		return n - 1
	}
	return i
}

func applySecondDiffDtD(dst, x []float64, lam float64) {
	// D x at i (for i>=2): d2[i] = x[i] - 2x[i-1] + x[i-2]
	// Add lam * D^T D x to dst without forming matrices.
	n := len(x)
	d2 := make([]float64, n)
	for i := 2; i < n; i++ {
		d2[i] = x[i] - 2*x[i-1] + x[i-2]
	}
	// D^T d2 contributes:
	// at i:   + d2[i]
	// at i-1: -2 d2[i]
	// at i-2: + d2[i]
	for i := 2; i < n; i++ {
		v := lam * d2[i]
		dst[i] += v
		dst[i-1] += -2 * v
		dst[i-2] += v
	}
}

func minVal(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	m := x[0]
	for i := 1; i < len(x); i++ {
		if x[i] < m {
			m = x[i]
		}
	}
	return m
}
