// file: internal/tycho/analysis/metrics/fusion_model.go
package analysismetrics

import (
	"math"
	"sort"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/fusion"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"k8s.io/klog/v2"
)

type FusionModel struct{}

func NewFusionModel() *FusionModel { return &FusionModel{} }

func (m *FusionModel) ID() analysis.MetricID { return "fusion_model" }

func (m *FusionModel) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.State != nil && c.Mono != nil
}

func (m *FusionModel) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.State == nil || c.Mono == nil {
		return nil
	}

	// Must match FusionSubstrate chassis policy.
	chassis := selectFusionChassis(c)

	// Load fusion cache (produced by FusionSubstrate).
	cacheKey := analysis.Key(MetricFusionCache, analysis.Labels{"chassis": chassis})
	cache, ok := getCache(c.State, cacheKey)
	if !ok || cache == nil || cache.QuantumTicks == 0 || cache.HorizonBins <= 0 {
		return nil
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()
	if monoQuantumSec <= 0 {
		return nil
	}
	dtSec := float64(cache.QuantumTicks) * monoQuantumSec
	if dtSec <= 0 {
		return nil
	}

	if len(cache.RedfishObs) == 0 {
		// Warmup policy: publish no corrected system series.
		// Canonical system metrics during warmup come from raw Redfish integration (SystemRawFromRedfish).
		return nil
	}

	// Fit theta; fallback to previous theta (or default).
	thetaKey := analysis.Key(MetricFusionTheta, analysis.Labels{"chassis": chassis})
	thetaPrev, hasPrev := getTheta(c.State, thetaKey)
	theta, okFit := fitTheta(cache, dtSec, monoQuantumSec, thetaPrev, hasPrev)
	if okFit {
		c.State.Set(thetaKey, theta)
	} else if hasPrev {
		theta = thetaPrev
	} else {
		theta = defaultTheta()
		c.State.Set(thetaKey, theta)
	}

	// Build per-bin fused power (mW) over full horizon.
	pHat := make([]float64, cache.HorizonBins)
	for i := 0; i < cache.HorizonBins; i++ {
		pPkg := cache.EpkgMJ[i] / dtSec
		pDram := cache.EdramMJ[i] / dtSec
		pGpu := cache.EgpuMJ[i] / dtSec
		instrRate := cache.CPUInstr[i] / dtSec

		v := theta.Alpha*pPkg + theta.Beta*pDram + theta.Gamma*pGpu + theta.Delta*instrRate + theta.Bias
		if v < 0 {
			v = 0
		}
		pHat[i] = v
	}

	// Emit power per overlapping bin + integrated energy over the analysis window.
	k0 := fusion.BinIndex(int64(c.Window.StartMono / cache.QuantumTicks))
	k1 := fusion.BinIndex(int64(c.Window.EndMono / cache.QuantumTicks))

	hStart := cache.StartBin
	hEnd := cache.StartBin + fusion.BinIndex(cache.HorizonBins) - 1
	if k0 < hStart {
		k0 = hStart
	}
	if k1 > hEnd {
		k1 = hEnd
	}
	if k1 < k0 {
		return nil
	}

	var eHatMJ float64

	for k := k0; k <= k1; k++ {
		i := int64(k - cache.StartBin)
		if i < 0 || i >= int64(len(pHat)) {
			continue
		}

		// Bin overlap with analysis window, in ticks.
		b0 := uint64(k) * cache.QuantumTicks
		b1 := b0 + cache.QuantumTicks

		lo := maxU64(c.Window.StartMono, b0)
		hi := minU64(c.Window.EndMono+1, b1) // +1 because Window.EndMono is inclusive-ish
		if hi <= lo {
			continue
		}
		overlapSec := float64(hi-lo) * monoQuantumSec

		// mW * s = mJ
		eHatMJ += pHat[i] * overlapSec

	}

	// Canonical corrected system series: power (window avg) + energy (counter).
	windowSec := float64(c.Window.EndMono-c.Window.StartMono+1) * monoQuantumSec
	if windowSec <= 0 {
		return nil
	}
	pMW := eHatMJ / windowSec
	labels := analysis.Labels{
		"chassis": chassis,
		"source":  redfishSourceCorrected,
		"kind":    systemKindTotal,
	}

	const MetricFusionTakeoverDone analysis.MetricID = "fusion_takeover_done" // state-only

	takeoverKey := analysis.Key(MetricFusionTakeoverDone, analysis.Labels{"chassis": chassis})
	if _, done := c.State.Get(takeoverKey); !done {

		// Delete raw system series
		rawSys := analysis.Labels{"chassis": chassis, "source": redfishSourceRaw, "kind": systemKindTotal}
		c.Sink.Delete(c.Ctx, analysis.Key(MetricSystemPowerMW, rawSys))
		c.Sink.Delete(c.Ctx, analysis.Key(MetricSystemEnergyMJ, rawSys))

		// Delete raw residual series (strings to avoid cross-file const deps)
		for _, k := range []string{"total", "idle", "dynamic"} {
			rawRes := analysis.Labels{"chassis": chassis, "source": redfishSourceRaw, "kind": k}
			c.Sink.Delete(c.Ctx, analysis.Key(MetricResidualPowerMW, rawRes))
			c.Sink.Delete(c.Ctx, analysis.Key(MetricResidualEnergyMJ, rawRes))
		}

		// Optional: delete raw negative diag
		rawNeg := analysis.Labels{"chassis": chassis, "source": redfishSourceRaw, "kind": "total"}
		c.Sink.Delete(c.Ctx, analysis.Key(MetricResidualNegativePowerMW, rawNeg))

		c.State.Set(takeoverKey, true)
	}

	// 1) Determine offset once (seed from last raw system counter).
	offKey := analysis.Key(MetricSystemEnergyOffsetMJ, analysis.Labels{
		"chassis": chassis,
		"kind":    systemKindTotal,
	})
	var offset float64
	if v, ok := c.State.Get(offKey); ok {
		if f, ok2 := v.(float64); ok2 {
			offset = f
		}
	} else {
		rawKey := analysis.Key(MetricSystemEnergyMJ, analysis.Labels{
			"chassis": chassis,
			"source":  redfishSourceRaw,
			"kind":    systemKindTotal,
		})
		if v2, ok2 := c.State.Get(rawKey); ok2 {
			if f2, ok3 := v2.(float64); ok3 {
				offset = f2
			}
		}
		c.State.Set(offKey, offset)
	}

	// 2) Accumulate corrected local counter (state-only, no offset).
	localKey := analysis.Key(MetricSystemEnergyLocalMJ, labels)
	var prevLocal float64
	if v, ok := c.State.Get(localKey); ok {
		if f, ok2 := v.(float64); ok2 {
			prevLocal = f
		}
	}
	newLocal := prevLocal + eHatMJ
	c.State.Set(localKey, newLocal)

	// 3) Exported cumulative counter includes offset.
	exportCum := offset + newLocal

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricSystemPowerMW, labels),
		Window: c.Window,
		Unit:   "mW",
		Value:  math.Floor(pMW),
	})
	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricSystemEnergyMJ, labels),
		Window: c.Window,
		Unit:   "mJ",
		Value:  math.Floor(exportCum),
	})

	return nil
}

// -------------------- theta + fitting --------------------

type theta5 struct {
	Alpha float64
	Beta  float64
	Gamma float64
	Delta float64
	Bias  float64
}

type solverRow struct {
	x [5]float64
	y float64
	w float64
}

func defaultTheta() theta5 {
	return theta5{Alpha: 1, Beta: 1, Gamma: 1, Delta: 0, Bias: 0}
}

func getTheta(s *analysis.StateStore, key analysis.MetricKey) (theta5, bool) {
	if s == nil {
		return theta5{}, false
	}
	v, ok := s.Get(key)
	if !ok {
		return theta5{}, false
	}
	th, ok := v.(theta5)
	return th, ok
}
func fitTheta(cache *fusion.Cache, dtSec, monoQuantumSec float64, prev theta5, hasPrev bool) (theta5, bool) {
	// --- Defaults (can be promoted to config later) ---
	const (
		wRF          = 1.0
		lambdaScaled = 1e-6 // ridge in scaled space
		minObs       = 5    // N_min (addendum recommends ~5 for typical horizons)
		epsScale     = 1e-12

		// Low-excitation gating:
		// Use only physical meter columns (pkg/dram/gpu) so CPUInstr does not dominate.
		excTauMW = 1e-3

		// Optional one-pass trimming (IRLS-lite). Keep disabled by default.
		enableTrim = false
		trimFrac   = 0.05 // drop top 5% |residual|
	)

	if cache == nil || dtSec <= 0 || monoQuantumSec <= 0 {
		return theta5{}, false
	}

	// Build solver rows (unscaled) from Redfish observations.
	rows := make([]solverRow, 0, len(cache.RedfishObs))

	for i := range cache.RedfishObs {
		obs := cache.RedfishObs[i]
		x, ok := obsToRow(cache, dtSec, monoQuantumSec, obs)
		if !ok {
			continue
		}
		rows = append(rows, solverRow{x: x, y: obs.ValueMW, w: wRF})
	}

	// Freeze-on-low-information: not enough usable obs.
	if len(rows) < minObs {
		return theta5{}, false
	}

	// Fix A: compute per-column RMS scales from the actual X used in the solve.
	// Intercept column is forced to scale=1.
	scales := rmsScales(rows, epsScale)

	// Fix B: low-excitation gating on physical meters.
	exc := scales[0] + scales[1] + scales[2]
	if exc < excTauMW {
		// skip refit; caller will fall back to prev/default.
		return theta5{}, false
	}

	// Fit once (scaled ridge LS), then apply non-negativity + one constrained refit.
	th, ok := solveThetaScaled(rows, scales, lambdaScaled)
	if !ok {
		if hasPrev {
			return prev, false
		}
		return defaultTheta(), false
	}

	// Enforce non-negativity constraints.
	th, nClamped := clampThetaNonNeg(th)
	if nClamped > 0 {
		refit, ok2 := refitWithFixedNonNeg(cache, dtSec, monoQuantumSec, th)
		if ok2 {
			th = refit
		}
	}

	if !finite(th.Alpha) || !finite(th.Beta) || !finite(th.Gamma) || !finite(th.Delta) || !finite(th.Bias) {
		if hasPrev {
			return prev, false
		}
		return defaultTheta(), false
	}

	// Fix D (optional): one-pass trimming against Redfish outliers, then refit.
	if enableTrim && len(rows) >= minObs {
		kept := trimRowsByResidual(rows, th, trimFrac)
		if len(kept) >= minObs {
			th2, ok2 := solveThetaScaled(kept, scales, lambdaScaled)
			if ok2 {
				th2, nClamped2 := clampThetaNonNeg(th2)
				if nClamped2 > 0 {
					// constrained refit needs cache access; keep it consistent by running it on cache.
					refit, ok3 := refitWithFixedNonNeg(cache, dtSec, monoQuantumSec, th2)
					if ok3 {
						th2 = refit
					}
				}
				if finite(th2.Alpha) && finite(th2.Beta) && finite(th2.Gamma) && finite(th2.Delta) && finite(th2.Bias) {
					th = th2
				}
			}
		}
	}

	// Optional: log scales + excitation at a higher verbosity for debugging.
	klog.V(4).Infof(
		"[analysis] fusion_model fit scales(pkg=%.6g dram=%.6g gpu=%.6g instr=%.6g) exc=%.6g rows=%d",
		scales[0], scales[1], scales[2], scales[3], exc, len(rows),
	)

	return th, true
}

// rmsScales computes per-column RMS of X over the provided rows.
// Intercept scale is forced to 1.
func rmsScales(rows []solverRow, eps float64) [5]float64 {
	var s2 [5]float64
	if len(rows) == 0 {
		return [5]float64{1, 1, 1, 1, 1}
	}
	for i := range rows {
		x := rows[i].x
		for j := 0; j < 5; j++ {
			s2[j] += x[j] * x[j]
		}
	}
	n := float64(len(rows))
	var scales [5]float64
	for j := 0; j < 5; j++ {
		scales[j] = math.Sqrt(s2[j]/n) + eps
	}
	// Intercept column: do not scale.
	scales[4] = 1.0
	return scales
}

// solveThetaScaled solves ridge LS in scaled space and returns theta in original units.
// This uses normal equations on the scaled X.
func solveThetaScaled(
	rows []solverRow,
	scales [5]float64,
	lambdaScaled float64,
) (theta5, bool) {
	if len(rows) == 0 {
		return theta5{}, false
	}

	// Normal equations: (X~ᵀ Wᵀ W X~ + λ I) θ~ = X~ᵀ Wᵀ W y
	var A [5][5]float64
	var b [5]float64

	addRow := func(xScaled [5]float64, y float64, w float64) {
		ww := w * w
		for i := 0; i < 5; i++ {
			b[i] += ww * xScaled[i] * y
			for j := 0; j < 5; j++ {
				A[i][j] += ww * xScaled[i] * xScaled[j]
			}
		}
	}

	for i := range rows {
		x := rows[i].x
		var xs [5]float64
		for j := 0; j < 5; j++ {
			s := scales[j]
			if s <= 0 {
				s = 1
			}
			xs[j] = x[j] / s
		}
		addRow(xs, rows[i].y, rows[i].w)
	}

	// Ridge stabilization in scaled space.
	for i := 0; i < 5; i++ {
		A[i][i] += lambdaScaled
	}

	vecScaled, ok := solve5x5(A, b)
	if !ok {
		return theta5{}, false
	}

	// Unscale coefficients: theta_j = theta~_j / s_j (intercept has s=1).
	var vec [5]float64
	for j := 0; j < 5; j++ {
		s := scales[j]
		if s <= 0 {
			s = 1
		}
		vec[j] = vecScaled[j] / s
	}

	out := theta5{Alpha: vec[0], Beta: vec[1], Gamma: vec[2], Delta: vec[3], Bias: vec[4]}
	if !finite(out.Alpha) || !finite(out.Beta) || !finite(out.Gamma) || !finite(out.Delta) || !finite(out.Bias) {
		return theta5{}, false
	}
	return out, true
}

// trimRowsByResidual drops the largest |residual| fraction and returns the kept subset.
// Residual is computed in original (unscaled) space using the provided theta.
func trimRowsByResidual(
	rows []solverRow,
	th theta5,
	frac float64,
) []solverRow {
	n := len(rows)
	if n == 0 || frac <= 0 {
		return rows
	}
	if frac >= 0.5 {
		frac = 0.5
	}
	absr := make([]float64, 0, n)
	for i := range rows {
		x := rows[i].x
		yhat := th.Alpha*x[0] + th.Beta*x[1] + th.Gamma*x[2] + th.Delta*x[3] + th.Bias*x[4]
		r := yhat - rows[i].y
		if r < 0 {
			r = -r
		}
		absr = append(absr, r)
	}
	sort.Float64s(absr)
	// Keep (1-frac) fraction => threshold at quantile q = 1-frac.
	qIdx := int(math.Floor(float64(n-1) * (1.0 - frac)))
	if qIdx < 0 {
		qIdx = 0
	}
	if qIdx >= n {
		qIdx = n - 1
	}
	tau := absr[qIdx]

	kept := make([]solverRow, 0, n)

	dropped := 0
	for i := range rows {
		x := rows[i].x
		yhat := th.Alpha*x[0] + th.Beta*x[1] + th.Gamma*x[2] + th.Delta*x[3] + th.Bias*x[4]
		r := yhat - rows[i].y
		if r < 0 {
			r = -r
		}
		if r <= tau {
			kept = append(kept, rows[i])
		} else {
			dropped++
		}
	}

	klog.V(3).Infof("[analysis] fusion_model trim dropped=%d kept=%d tau=%.6g", dropped, len(kept), tau)
	return kept
}

// clampThetaNonNeg enforces simple physical constraints.
// - Alpha/Beta/Gamma are constrained to be >= 0.
// - Bias is constrained to be >= 0 (system baseline power cannot be negative).
// - Delta is left unconstrained (it can be tiny/noisy and sign can flip if it is not useful).
//
// Returns (clampedTheta, numClamped).
func clampThetaNonNeg(th theta5) (theta5, int) {
	clamped := 0

	if th.Alpha < 0 {
		th.Alpha = 0
		clamped++
	}
	if th.Beta < 0 {
		th.Beta = 0
		clamped++
	}
	if th.Gamma < 0 {
		th.Gamma = 0
		clamped++
	}
	if th.Bias < 0 {
		th.Bias = 0
		clamped++
	}

	// Delta: keep as-is.
	return th, clamped
}

func obsToRow(cache *fusion.Cache, dtSec, monoQuantumSec float64, obs fusion.RedfishObs) ([5]float64, bool) {
	var x [5]float64
	if cache == nil || cache.QuantumTicks == 0 || cache.HorizonBins <= 0 {
		return x, false
	}

	switch obs.Kernel {
	case fusion.KernelInstant:
		k := fusion.BinIndex(int64(obs.MonoCorr / cache.QuantumTicks))
		i, ok := cacheIdx(cache, k)
		if !ok {
			return x, false
		}
		x[0] = cache.EpkgMJ[i] / dtSec
		x[1] = cache.EdramMJ[i] / dtSec
		x[2] = cache.EgpuMJ[i] / dtSec
		x[3] = cache.CPUInstr[i] / dtSec
		x[4] = 1.0
		return x, true

	case fusion.KernelAvg1sTrailing:
		Tms := obs.KernelMs
		if Tms <= 0 {
			Tms = 1000
		}
		Tsec := float64(Tms) / 1000.0
		if Tsec <= 0 {
			return x, false
		}

		// Trailing window in ticks, in corrected domain.
		Tticks := uint64(math.Round(Tsec / monoQuantumSec))
		if Tticks == 0 {
			// fall back to instant
			k := fusion.BinIndex(int64(obs.MonoCorr / cache.QuantumTicks))
			i, ok := cacheIdx(cache, k)
			if !ok {
				return x, false
			}
			x[0] = cache.EpkgMJ[i] / dtSec
			x[1] = cache.EdramMJ[i] / dtSec
			x[2] = cache.EgpuMJ[i] / dtSec
			x[3] = cache.CPUInstr[i] / dtSec
			x[4] = 1.0
			return x, true
		}

		t1 := obs.MonoCorr
		t0 := uint64(0)
		if t1 > Tticks {
			t0 = t1 - Tticks
		}

		kStart := fusion.BinIndex(int64(t0 / cache.QuantumTicks))
		kEnd := fusion.BinIndex(int64((t1 - 1) / cache.QuantumTicks))

		var sumPkg, sumDram, sumGpu, sumInstr float64
		var sumSec float64

		for k := kStart; k <= kEnd; k++ {
			i, ok := cacheIdx(cache, k)
			if !ok {
				continue
			}
			b0 := uint64(k) * cache.QuantumTicks
			b1 := b0 + cache.QuantumTicks

			lo := maxU64(t0, b0)
			hi := minU64(t1, b1)
			if hi <= lo {
				continue
			}
			segSec := float64(hi-lo) * monoQuantumSec
			if segSec <= 0 {
				continue
			}
			sumSec += segSec

			pPkg := cache.EpkgMJ[i] / dtSec
			pDram := cache.EdramMJ[i] / dtSec
			pGpu := cache.EgpuMJ[i] / dtSec
			instrRate := cache.CPUInstr[i] / dtSec

			sumPkg += pPkg * segSec
			sumDram += pDram * segSec
			sumGpu += pGpu * segSec
			sumInstr += instrRate * segSec
		}

		if sumSec <= 0 {
			return x, false
		}

		x[0] = sumPkg / sumSec
		x[1] = sumDram / sumSec
		x[2] = sumGpu / sumSec
		x[3] = sumInstr / sumSec
		x[4] = 1.0
		return x, true

	default:
		return x, false
	}
}

func cacheIdx(cache *fusion.Cache, k fusion.BinIndex) (int, bool) {
	if cache == nil || cache.HorizonBins <= 0 {
		return 0, false
	}
	if k < cache.StartBin {
		return 0, false
	}
	i := int64(k - cache.StartBin)
	if i < 0 || i >= int64(cache.HorizonBins) {
		return 0, false
	}
	return int(i), true
}

func solve5x5(A [5][5]float64, b [5]float64) ([5]float64, bool) {
	var M [5][6]float64
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			M[i][j] = A[i][j]
		}
		M[i][5] = b[i]
	}

	for col := 0; col < 5; col++ {
		piv := col
		maxAbs := math.Abs(M[col][col])
		for r := col + 1; r < 5; r++ {
			if v := math.Abs(M[r][col]); v > maxAbs {
				maxAbs = v
				piv = r
			}
		}
		if maxAbs < 1e-12 {
			return [5]float64{}, false
		}
		if piv != col {
			M[col], M[piv] = M[piv], M[col]
		}

		p := M[col][col]
		for j := col; j < 6; j++ {
			M[col][j] /= p
		}

		for r := 0; r < 5; r++ {
			if r == col {
				continue
			}
			f := M[r][col]
			if f == 0 {
				continue
			}
			for j := col; j < 6; j++ {
				M[r][j] -= f * M[col][j]
			}
		}
	}

	var x [5]float64
	for i := 0; i < 5; i++ {
		x[i] = M[i][5]
	}
	return x, true
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

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

// Match fusion_substrate.go behavior.
func selectFusionChassis(c *analysis.Cycle) string {
	if c == nil || c.Redfish() == nil {
		return "Self"
	}
	seg1, seg2 := c.Redfish().ViewChrono()

	seen := map[string]bool{}
	for _, seg := range [][]ring.RedfishSample{seg1, seg2} {
		for i := range seg {
			ch := seg[i].ChassisID
			if ch == "" {
				continue
			}
			seen[ch] = true
		}
	}
	if seen["Self"] {
		return "Self"
	}
	best := ""
	for ch := range seen {
		if best == "" || ch < best {
			best = ch
		}
	}
	if best == "" {
		best = "Self"
	}
	return best
}

// refitWithFixedNonNeg performs one constrained refit where any of Alpha/Beta/Gamma/Bias
// that are already at 0 are treated as fixed at 0, and the remaining variables are solved
// with the same normal-equation approach.
//
// This is a light-weight "one step" improvement over naive post-clamp.
func refitWithFixedNonNeg(cache *fusion.Cache, dtSec, monoQuantumSec float64, th theta5) (theta5, bool) {
	// Decide which variables are free.
	// Order: Alpha, Beta, Gamma, Delta, Bias
	free := [5]bool{
		th.Alpha > 0,
		th.Beta > 0,
		th.Gamma > 0,
		true,        // Delta always free
		th.Bias > 0, // If clamped to 0, fix at 0
	}

	// Map free vars to compact indices.
	var idx [5]int
	m := 0
	for i := 0; i < 5; i++ {
		if free[i] {
			idx[i] = m
			m++
		} else {
			idx[i] = -1
		}
	}
	if m == 0 {
		// Nothing to solve.
		return th, true
	}

	// Build compact normal equations of size m.
	// (We keep code simple; m <= 5).
	const lambda = 1e-6
	A := make([][]float64, m)
	for i := range A {
		A[i] = make([]float64, m)
	}
	b := make([]float64, m)

	addRow := func(x [5]float64, y float64, w float64) {
		ww := w * w

		// Subtract fixed-part contribution from y.
		yAdj := y
		if !free[0] {
			yAdj -= th.Alpha * x[0]
		}
		if !free[1] {
			yAdj -= th.Beta * x[1]
		}
		if !free[2] {
			yAdj -= th.Gamma * x[2]
		}
		if !free[4] {
			yAdj -= th.Bias * x[4]
		}
		// Delta (3) is always free here.

		for i := 0; i < 5; i++ {
			if !free[i] {
				continue
			}
			ii := idx[i]
			b[ii] += ww * x[i] * yAdj
			for j := 0; j < 5; j++ {
				if !free[j] {
					continue
				}
				jj := idx[j]
				A[ii][jj] += ww * x[i] * x[j]
			}
		}
	}

	// Add rows for obs.
	const wRF = 1.0
	used := 0
	for i := range cache.RedfishObs {
		obs := cache.RedfishObs[i]
		x, ok := obsToRow(cache, dtSec, monoQuantumSec, obs)
		if !ok {
			continue
		}
		addRow(x, obs.ValueMW, wRF)
		used++
	}
	if used < 3 {
		return th, false
	}

	// Ridge stabilization.
	for i := 0; i < m; i++ {
		A[i][i] += lambda
	}

	sol, ok := solveDense(A, b)
	if !ok {
		return th, false
	}

	// Write back.
	if free[0] {
		th.Alpha = sol[idx[0]]
	}
	if free[1] {
		th.Beta = sol[idx[1]]
	}
	if free[2] {
		th.Gamma = sol[idx[2]]
	}
	// Delta always free.
	th.Delta = sol[idx[3]]
	if free[4] {
		th.Bias = sol[idx[4]]
	}

	// Re-clamp (numerical jitter).
	th, _ = clampThetaNonNeg(th)
	return th, true
}

// solveDense solves A x = b for small dense A using Gauss-Jordan.
// A is modified.
func solveDense(A [][]float64, b []float64) ([]float64, bool) {
	n := len(b)
	if n == 0 || len(A) != n {
		return nil, false
	}
	// Augment.
	M := make([][]float64, n)
	for i := 0; i < n; i++ {
		if len(A[i]) != n {
			return nil, false
		}
		M[i] = make([]float64, n+1)
		copy(M[i], A[i])
		M[i][n] = b[i]
	}

	for col := 0; col < n; col++ {
		// pivot
		piv := col
		maxAbs := math.Abs(M[col][col])
		for r := col + 1; r < n; r++ {
			if v := math.Abs(M[r][col]); v > maxAbs {
				maxAbs = v
				piv = r
			}
		}
		if maxAbs < 1e-12 {
			return nil, false
		}
		if piv != col {
			M[col], M[piv] = M[piv], M[col]
		}
		// normalize
		p := M[col][col]
		for j := col; j < n+1; j++ {
			M[col][j] /= p
		}
		// eliminate
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			f := M[r][col]
			if f == 0 {
				continue
			}
			for j := col; j < n+1; j++ {
				M[r][j] -= f * M[col][j]
			}
		}
	}

	x := make([]float64, n)
	for i := 0; i < n; i++ {
		x[i] = M[i][n]
		if math.IsNaN(x[i]) || math.IsInf(x[i], 0) {
			return nil, false
		}
	}
	return x, true
}

// avgRedfishPowerMWInRawWindow computes the time-weighted average Redfish power (mW)
// over the raw-time interval [rawStart, rawEnd] (inclusive-ish).
// It uses ZOH over sample intervals. Requires at least one usable sample.
func avgRedfishPowerMWInRawWindow(
	c *analysis.Cycle,
	chassis string,
	rawStart, rawEnd uint64,
	monoQuantumSec float64,
) (float64, bool) {
	if c == nil || c.Redfish() == nil || monoQuantumSec <= 0 {
		return 0, false
	}
	if rawEnd <= rawStart {
		return 0, false
	}

	seg1, seg2 := c.Redfish().ViewChrono()

	// Collect samples for the chassis; also keep one predecessor (best-effort) to integrate cleanly.
	// We keep it simple: scan and select all samples in [rawStart, rawEnd] plus last < rawStart.
	var prev *ring.RedfishSample
	samples := make([]ring.RedfishSample, 0, 16)

	ingest := func(seg []ring.RedfishSample) {
		for i := range seg {
			s := seg[i]
			if chassis != "" && s.ChassisID != chassis {
				continue
			}
			t := s.Mono
			if t < rawStart {
				// candidate predecessor
				if prev == nil || t > prev.Mono {
					tmp := s
					prev = &tmp
				}
				continue
			}
			if t > rawEnd {
				continue
			}
			samples = append(samples, s)
		}
	}
	ingest(seg1)
	ingest(seg2)

	if len(samples) == 0 && prev == nil {
		return 0, false
	}

	// Ensure chronological order if the two segments overlapped oddly.
	if len(samples) > 1 {
		sort.Slice(samples, func(i, j int) bool { return samples[i].Mono < samples[j].Mono })
	}

	// If we only have one sample in-window, use it as constant over the whole window.
	if len(samples) == 1 && prev == nil {
		pMW := samples[0].PowerWatts * 1000.0
		return pMW, true
	}

	// Build a working sequence with predecessor if available.
	seq := make([]ring.RedfishSample, 0, len(samples)+1)
	if prev != nil {
		seq = append(seq, *prev)
	}
	seq = append(seq, samples...)

	// Time-weighted integration of power over [rawStart, rawEnd+1) in ticks.
	// Use ZOH over intervals (t_{i-1}, t_i] with power = sample_i.
	winLo := rawStart
	winHi := rawEnd + 1

	var eMJ float64
	var usedTicks uint64

	for i := 1; i < len(seq); i++ {
		t0 := seq[i-1].Mono
		t1 := seq[i].Mono
		if t1 <= t0 {
			continue
		}

		lo := maxU64(winLo, t0)
		hi := minU64(winHi, t1)
		if hi <= lo {
			continue
		}

		pMW := seq[i].PowerWatts * 1000.0
		if pMW < 0 {
			pMW = 0
		}

		dtTicks := hi - lo
		usedTicks += dtTicks
		eMJ += pMW * (float64(dtTicks) * monoQuantumSec)
	}

	// If we couldn’t integrate (e.g. only predecessor exists), fall back to nearest available sample.
	if usedTicks == 0 {
		// Prefer earliest in-window sample, else predecessor.
		var pMW float64
		if len(samples) > 0 {
			pMW = samples[0].PowerWatts * 1000.0
		} else if prev != nil {
			pMW = prev.PowerWatts * 1000.0
		} else {
			return 0, false
		}
		if pMW < 0 {
			pMW = 0
		}
		return pMW, true
	}

	// Average power = energy / window duration (seconds).
	winTicks := float64(winHi - winLo)
	winSec := winTicks * monoQuantumSec
	if winSec <= 0 {
		return 0, false
	}
	pAvgMW := eMJ / winSec
	return pAvgMW, true
}
