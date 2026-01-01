// file: internal/tycho/analysis/idle/model_vec2.go
package idle

import (
	"math"
	"sort"
	"time"
)

type Vec2Model struct {
	Cfg Config

	// Same binning strategy but based on scalar u = max(u1,u2) for bin assignment.
	bins [][]vec2Pt

	totalPoints int
	newSinceFit int
	lastFit     time.Time

	// Fit: P = a1*u1 + a2*u2 + beta
	a1    float64
	a2    float64
	beta  float64
	ready bool

	minP float64

	prevU1, prevU2 float64
	prevSet        bool
}

type vec2Pt struct {
	u1 float64
	u2 float64
	p  float64
}

type Vec2FitQuality struct {
	Ready         bool
	BinsPopulated int
	TotalPoints   int
	NewSinceFit   int
	LastFitAgeSec float64
	A1            float64
	A2            float64
	Beta          float64
	MinP          float64
	Mode          string
}

func NewVec2Model(cfg Config) *Vec2Model {
	nb := int(math.Ceil(cfg.UMax / cfg.BinWidth))
	if nb <= 0 {
		nb = 10
	}
	bins := make([][]vec2Pt, nb)
	for i := range bins {
		bins[i] = make([]vec2Pt, 0, cfg.KPerBin)
	}
	return &Vec2Model{
		Cfg:  cfg,
		bins: bins,
		minP: math.Inf(1),
	}
}

func (m *Vec2Model) Observe(u1, u2 float64, pMW float64, now time.Time) bool {
	if m == nil {
		return false
	}
	u1 = clamp01(u1)
	u2 = clamp01(u2)
	if pMW < 0 || math.IsNaN(pMW) || math.IsInf(pMW, 0) {
		return false
	}

	if pMW < m.minP {
		m.minP = pMW
	}

	// Stability gate per dimension.
	if m.prevSet {
		if math.Abs(u1-m.prevU1) > m.Cfg.EpsUVec || math.Abs(u2-m.prevU2) > m.Cfg.EpsUVec {
			m.prevU1, m.prevU2 = u1, u2
			return false
		}
	}
	m.prevU1, m.prevU2 = u1, u2
	m.prevSet = true

	// Bin assignment uses u = max(u1,u2) to stay in low-util region.
	u := u1
	if u2 > u {
		u = u2
	}
	if u > m.Cfg.UMax {
		return false
	}

	b := m.binIndex(u)
	if b < 0 || b >= len(m.bins) {
		return false
	}

	if len(m.bins[b]) < m.Cfg.KPerBin {
		m.bins[b] = append(m.bins[b], vec2Pt{u1: u1, u2: u2, p: pMW})
	} else {
		j := m.totalPoints % m.Cfg.KPerBin
		m.bins[b][j] = vec2Pt{u1: u1, u2: u2, p: pMW}
	}

	m.totalPoints++
	m.newSinceFit++

	m.maybeRefit(now)
	return true
}

func (m *Vec2Model) Estimate() (betaMW float64, q Vec2FitQuality) {
	if m == nil {
		return 0, Vec2FitQuality{Ready: false, Mode: "baseline"}
	}
	if !isFinite(m.minP) {
		return 0, Vec2FitQuality{Ready: false, Mode: "baseline"}
	}

	beta := m.beta
	mode := "model"
	if !m.ready {
		beta = m.minP
		mode = "baseline"
	} else {
		if beta < m.minP+m.Cfg.DeltaMin {
			beta = m.minP + m.Cfg.DeltaMin
		}
	}

	q = Vec2FitQuality{
		Ready:         m.ready,
		BinsPopulated: m.binsPopulated(),
		TotalPoints:   m.totalPoints,
		NewSinceFit:   m.newSinceFit,
		LastFitAgeSec: ageSec(m.lastFit),
		A1:            m.a1,
		A2:            m.a2,
		Beta:          beta,
		MinP:          m.minP,
		Mode:          mode,
	}
	return beta, q
}

func (m *Vec2Model) maybeRefit(now time.Time) {
	if m == nil {
		return
	}
	if !m.lastFit.IsZero() && now.Sub(m.lastFit) < m.Cfg.RefitEvery {
		return
	}
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

	// Representative points per bin: take q10 on power, but keep corresponding (u1,u2) by selecting
	// the point at that quantile after sorting by power.
	rep := make([]vec2Pt, 0, bPop)
	for i := range m.bins {
		if len(m.bins[i]) == 0 {
			continue
		}
		pt := quantilePt(m.bins[i], m.Cfg.Quantile)
		rep = append(rep, pt)
	}
	if len(rep) < 3 {
		return
	}

	a1, a2, beta, ok := fitPlane(rep)
	if !ok {
		return
	}
	m.a1, m.a2, m.beta = a1, a2, beta
	m.ready = true
	m.lastFit = now
	m.newSinceFit = 0
}

func (m *Vec2Model) binsPopulated() int {
	n := 0
	for i := range m.bins {
		if len(m.bins[i]) > 0 {
			n++
		}
	}
	return n
}

func (m *Vec2Model) binIndex(u float64) int {
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

func quantilePt(xs []vec2Pt, q float64) vec2Pt {
	tmp := make([]vec2Pt, len(xs))
	copy(tmp, xs)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].p < tmp[j].p })
	if q <= 0 {
		return tmp[0]
	}
	if q >= 1 {
		return tmp[len(tmp)-1]
	}
	idx := int(math.Floor(q * float64(len(tmp)-1)))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(tmp) {
		idx = len(tmp) - 1
	}
	return tmp[idx]
}

func fitPlane(pts []vec2Pt) (a1, a2, beta float64, ok bool) {
	// Solve normal equations for [a1,a2,beta] in least squares.
	// X = [u1 u2 1], y = p
	n := float64(len(pts))
	if n < 3 {
		return 0, 0, 0, false
	}

	// Compute sums for normal equations.
	var s11, s12, s13 float64
	var s22, s23 float64
	var s33 float64
	var t1, t2, t3 float64

	s33 = n

	for _, p := range pts {
		u1 := p.u1
		u2 := p.u2
		y := p.p
		s11 += u1 * u1
		s12 += u1 * u2
		s13 += u1
		s22 += u2 * u2
		s23 += u2

		t1 += u1 * y
		t2 += u2 * y
		t3 += y
	}

	// Symmetric 3x3:
	// [s11 s12 s13][a1]   [t1]
	// [s12 s22 s23][a2] = [t2]
	// [s13 s23 s33][b ]   [t3]
	//
	// Solve by Cramer's rule / explicit inverse for 3x3 (small).
	det := det3(s11, s12, s13, s12, s22, s23, s13, s23, s33)
	if math.Abs(det) < 1e-12 {
		return 0, 0, 0, false
	}

	da1 := det3(t1, s12, s13, t2, s22, s23, t3, s23, s33)
	da2 := det3(s11, t1, s13, s12, t2, s23, s13, t3, s33)
	db := det3(s11, s12, t1, s12, s22, t2, s13, s23, t3)

	a1 = da1 / det
	a2 = da2 / det
	beta = db / det

	if math.IsNaN(a1) || math.IsNaN(a2) || math.IsNaN(beta) {
		return 0, 0, 0, false
	}
	if math.IsInf(a1, 0) || math.IsInf(a2, 0) || math.IsInf(beta, 0) {
		return 0, 0, 0, false
	}
	return a1, a2, beta, true
}

func det3(a, b, c, d, e, f, g, h, i float64) float64 {
	return a*(e*i-f*h) - b*(d*i-f*g) + c*(d*h-e*g)
}
