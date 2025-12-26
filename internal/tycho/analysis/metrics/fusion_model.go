// file: internal/tycho/analysis/metrics/fusion_model.go
package analysismetrics

import (
	"math"

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

		// Note: bin label is high cardinality for Prometheus.
		// c.Sink.Emit(c.Ctx, analysis.Point{
		// 	Key:    analysis.Key(MetricRedfishPowerHatMW, analysis.Labels{"chassis": chassis, "bin": k.String()}),
		// 	Window: c.Window,
		// 	Unit:   "mW",
		// 	Value:  pHat[i],
		// })
	}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(MetricRedfishEnergyHatMJ, analysis.Labels{"chassis": chassis}),
		Window: c.Window,
		Unit:   "mJ",
		Value:  eHatMJ,
	})

	klog.V(2).Infof(
		"[analysis] fusion_model chassis=%q theta(alpha=%.4f beta=%.4f gamma=%.4f delta=%.6g bias=%.3f) redfish_obs=%d",
		chassis, theta.Alpha, theta.Beta, theta.Gamma, theta.Delta, theta.Bias, len(cache.RedfishObs),
	)

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
	const (
		wRF    = 1.0
		lambda = 1e-6
		minObs = 3
	)

	if cache == nil || dtSec <= 0 || monoQuantumSec <= 0 {
		return theta5{}, false
	}
	if len(cache.RedfishObs) < minObs {
		return theta5{}, false
	}

	// Normal equations: (XᵀWᵀWX + λI)θ = XᵀWᵀWy
	var A [5][5]float64
	var b [5]float64

	addRow := func(x [5]float64, y float64, w float64) {
		ww := w * w
		for i := 0; i < 5; i++ {
			b[i] += ww * x[i] * y
			for j := 0; j < 5; j++ {
				A[i][j] += ww * x[i] * x[j]
			}
		}
	}

	for i := range cache.RedfishObs {
		obs := cache.RedfishObs[i]
		x, ok := obsToRow(cache, dtSec, monoQuantumSec, obs)
		if !ok {
			continue
		}
		addRow(x, obs.ValueMW, wRF)
	}

	for i := 0; i < 5; i++ {
		A[i][i] += lambda
	}

	vec, ok := solve5x5(A, b)
	if !ok {
		if hasPrev {
			return prev, false
		}
		return defaultTheta(), false
	}

	out := theta5{Alpha: vec[0], Beta: vec[1], Gamma: vec[2], Delta: vec[3], Bias: vec[4]}
	if !finite(out.Alpha) || !finite(out.Beta) || !finite(out.Gamma) || !finite(out.Delta) || !finite(out.Bias) {
		if hasPrev {
			return prev, false
		}
		return defaultTheta(), false
	}
	// Enforce non-negativity constraints.
	out, nClamped := clampThetaNonNeg(out)

	if nClamped > 0 {
		refit, ok2 := refitWithFixedNonNeg(cache, dtSec, monoQuantumSec, out)
		if ok2 {
			out = refit
		}
	}

	return out, true
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

// Same cache helpers as fusion_substrate.go
func getCache(s *analysis.StateStore, key analysis.MetricKey) (*fusion.Cache, bool) {
	if s == nil {
		return nil, false
	}
	v, ok := s.Get(key)
	if !ok {
		return nil, false
	}
	c, ok := v.(*fusion.Cache)
	if ok && c != nil {
		return c, true
	}
	cv, ok2 := v.(fusion.Cache)
	if ok2 {
		tmp := cv
		return &tmp, true
	}
	return nil, false
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
