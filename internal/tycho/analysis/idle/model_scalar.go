// file: internal/tycho/analysis/idle/model_scalar.go
package idle

import (
	"math"
	"sort"
	"time"
)

// scalarPt is one representative (u,p) point used for fitting the idle line.
type scalarPt struct {
	u float64
	p float64
}

type ScalarModel struct {
	Cfg Config

	// Bins: each bin stores up to KPerBin observed powers (mW) and matching utilizations.
	// We store only power values for quantile; utilization is approximated by bin center.
	bins [][]float64

	// Tracking.
	totalPoints int
	newSinceFit int
	lastFit     time.Time

	// Fit parameters: P = alpha*u + beta
	alpha float64
	beta  float64
	ready bool

	// Guardrail: minimum observed power.
	minP float64

	// For stability gating.
	prevU   float64
	prevSet bool
}

type ScalarFitQuality struct {
	Ready         bool
	BinsPopulated int
	TotalPoints   int
	NewSinceFit   int
	LastFitAgeSec float64
	Alpha         float64
	Beta          float64
	MinP          float64
	Mode          string // "baseline" | "model"
}

func NewScalarModel(cfg Config) *ScalarModel {
	nb := int(math.Ceil(cfg.UMax / cfg.BinWidth))
	if nb <= 0 {
		nb = 10
	}
	bins := make([][]float64, nb)
	for i := range bins {
		bins[i] = make([]float64, 0, cfg.KPerBin)
	}
	m := &ScalarModel{
		Cfg:  cfg,
		bins: bins,
		minP: math.Inf(1),
	}
	return m
}

func (m *ScalarModel) Observe(u float64, pMW float64, now time.Time) (accepted bool) {
	if m == nil {
		return false
	}
	u = clamp01(u)
	if pMW < 0 || math.IsNaN(pMW) || math.IsInf(pMW, 0) {
		return false
	}

	// Only consider low-util region at all (baseline + points).
	if u > m.Cfg.UMax {
		// Still update prev for stability tracking continuity.
		m.prevU = u
		m.prevSet = true
		return false
	}

	// Baseline tracker: update from any admissible low-util sample.
	// This restores "instant baseline" behavior (min observed power),
	// while still allowing strict gating for model fitting.
	if pMW < m.minP {
		m.minP = pMW
	}

	// Stability gating applies only to inserting points into bins.
	if m.prevSet {
		if math.Abs(u-m.prevU) > m.Cfg.EpsUScalar {
			// Update prevU and reject insertion (but baseline already updated).
			m.prevU = u
			return false
		}
	}
	m.prevU = u
	m.prevSet = true

	b := m.binIndex(u)
	if b < 0 || b >= len(m.bins) {
		return false
	}

	if len(m.bins[b]) < m.Cfg.KPerBin {
		m.bins[b] = append(m.bins[b], pMW)
	} else {
		j := m.totalPoints % m.Cfg.KPerBin
		m.bins[b][j] = pMW
	}
	m.totalPoints++
	m.newSinceFit++

	m.maybeRefit(now)
	return true
}

func (m *ScalarModel) Estimate() (betaMW float64, q ScalarFitQuality) {
	if m == nil {
		return 0, ScalarFitQuality{Ready: false, Mode: "baseline"}
	}

	// Baseline fallback if no min yet.
	if !isFinite(m.minP) {
		return 0, ScalarFitQuality{Ready: false, Mode: "baseline"}
	}

	beta := m.beta
	mode := "model"
	if !m.ready {
		beta = m.minP
		mode = "baseline"
	} else {
		// Guardrail clamp.
		if beta < m.minP+m.Cfg.DeltaMin {
			beta = m.minP + m.Cfg.DeltaMin
		}
	}

	q = ScalarFitQuality{
		Ready:         m.ready,
		BinsPopulated: m.binsPopulated(),
		TotalPoints:   m.totalPoints,
		NewSinceFit:   m.newSinceFit,
		LastFitAgeSec: ageSec(m.lastFit),
		Alpha:         m.alpha,
		Beta:          beta,
		MinP:          m.minP,
		Mode:          mode,
	}
	return beta, q
}

// func (m *ScalarModel) maybeRefit(now time.Time) {
// 	if m == nil {
// 		return
// 	}

// 	// Time gate.
// 	if !m.lastFit.IsZero() && now.Sub(m.lastFit) < m.Cfg.RefitEvery {
// 		return
// 	}

// 	// Coverage gates.
// 	bPop := m.binsPopulated()
// 	if bPop < m.Cfg.MinBinsPopulated {
// 		return
// 	}
// 	if m.totalPoints < m.Cfg.MinTotalPoints {
// 		return
// 	}
// 	if m.newSinceFit < m.Cfg.MinNewPoints && !m.lastFit.IsZero() {
// 		return
// 	}

// 	// Build representative points per bin via quantile.
// 	pts := make([]scalarPt, 0, bPop)
// 	for i := range m.bins {
// 		if len(m.bins[i]) == 0 {
// 			continue
// 		}
// 		pq := quantile(m.bins[i], m.Cfg.Quantile)
// 		uc := m.binCenter(i)
// 		pts = append(pts, scalarPt{u: uc, p: pq})
// 	}
// 	if len(pts) < 2 {
// 		return
// 	}

// 	alpha, beta, ok := fitLine(pts)
// 	if !ok {
// 		return
// 	}

// 	m.alpha = alpha
// 	m.beta = beta
// 	m.ready = true
// 	m.lastFit = now
// 	m.newSinceFit = 0
// }

func (m *ScalarModel) maybeRefit(now time.Time) {
	if m == nil {
		return
	}

	// Time gate.
	if !m.lastFit.IsZero() && now.Sub(m.lastFit) < m.Cfg.RefitEvery {
		return
	}

	// Coverage gates.
	bPop := m.binsPopulated()
	if bPop < m.Cfg.MinBinsPopulated {
		return
	}
	if m.totalPoints < m.Cfg.MinTotalPoints {
		return
	}
	if m.newSinceFit < m.Cfg.MinNewPoints && !m.lastFit.IsZero() {
		return
	}

	// Build representative points per bin as a LOWER ENVELOPE.
	// Use the minimum observed power in each bin (or switch to a tiny quantile
	// like 0.02 if you want slight robustness against rare glitches).
	pts := make([]scalarPt, 0, bPop)
	for i := range m.bins {
		if len(m.bins[i]) == 0 {
			continue
		}
		pRep := minFloat64(m.bins[i])
		uc := m.binCenter(i)
		pts = append(pts, scalarPt{u: uc, p: pRep})
	}
	if len(pts) < 2 {
		return
	}

	alpha, beta, ok := fitLine(pts)
	if !ok {
		return
	}

	// Enforce lower-envelope constraint for the fitted line:
	// for all (u_i, p_i): alpha*u_i + beta <= p_i
	//
	// With alpha fixed, the maximal beta that satisfies all constraints is:
	// beta_max = min_i (p_i - alpha*u_i)
	//
	// If the fitted beta is above that, shift it down.
	betaMax := math.Inf(1)
	for _, pt := range pts {
		b := pt.p - alpha*pt.u
		if b < betaMax {
			betaMax = b
		}
	}
	if isFinite(betaMax) && beta > betaMax {
		beta = betaMax
	}

	m.alpha = alpha
	m.beta = beta
	m.ready = true
	m.lastFit = now
	m.newSinceFit = 0
}

func (m *ScalarModel) binsPopulated() int {
	n := 0
	for i := range m.bins {
		if len(m.bins[i]) > 0 {
			n++
		}
	}
	return n
}

func (m *ScalarModel) binIndex(u float64) int {
	if m.Cfg.BinWidth <= 0 {
		return 0
	}
	i := int(math.Floor(u / m.Cfg.BinWidth))
	if i < 0 {
		i = 0
	}
	if i >= len(m.bins) {
		i = len(m.bins) - 1
	}
	return i
}

func (m *ScalarModel) binCenter(i int) float64 {
	return (float64(i) + 0.5) * m.Cfg.BinWidth
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	if q <= 0 {
		q = 0
	}
	if q >= 1 {
		q = 1
	}
	tmp := make([]float64, len(xs))
	copy(tmp, xs)
	sort.Float64s(tmp)
	idx := int(math.Floor(q * float64(len(tmp)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(tmp) {
		idx = len(tmp) - 1
	}
	return tmp[idx]
}

// fitLine fits p ≈ alpha*u + beta using ordinary least squares (OLS).
// Returns ok=false if there are fewer than 2 points or if u has zero variance.
func fitLine(pts []scalarPt) (alpha, beta float64, ok bool) {
	if len(pts) < 2 {
		return 0, 0, false
	}

	// OLS for y = a x + b:
	// a = cov(x,y)/var(x)
	// b = mean(y) - a*mean(x)
	n := float64(len(pts))

	var sumU, sumP, sumUU, sumUP float64
	for _, x := range pts {
		u := x.u
		p := x.p
		sumU += u
		sumP += p
		sumUU += u * u
		sumUP += u * p
	}

	meanU := sumU / n
	meanP := sumP / n

	cov := (sumUP / n) - (meanU * meanP)
	varU := (sumUU / n) - (meanU * meanU)

	// If u has no variance, slope is undefined.
	if varU <= 1e-12 {
		return 0, 0, false
	}

	alpha = cov / varU
	beta = meanP - alpha*meanU
	return alpha, beta, true
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func isFinite(x float64) bool {
	return !math.IsNaN(x) && !math.IsInf(x, 0)
}

func ageSec(t time.Time) float64 {
	if t.IsZero() {
		return -1
	}
	return time.Since(t).Seconds()
}

func minFloat64(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for i := 1; i < len(xs); i++ {
		if xs[i] < m {
			m = xs[i]
		}
	}
	return m
}
