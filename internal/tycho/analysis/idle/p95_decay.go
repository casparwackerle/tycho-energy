// file: internal/tycho/analysis/idle/p95_decay.go
package idle

import (
	"math"
	"sort"
	"time"
)

type P95Decay struct {
	// Rolling window buffer.
	buf []float64
	i   int
	n   int

	// Current estimate.
	est float64

	// Decay: when new p95 < est, we decrease est by at most (decayPerSec * dtSec) fraction.
	// Example: decayPerSec=0.02 means can drop ~2% per second.
	decayPerSec float64

	lastT time.Time
}

func NewP95Decay(window int, decayPerSec float64) *P95Decay {
	if window <= 0 {
		window = 120
	}
	if decayPerSec <= 0 {
		decayPerSec = 0.02
	}
	return &P95Decay{
		buf:         make([]float64, window),
		n:           0,
		est:         0,
		decayPerSec: decayPerSec,
	}
}

func (t *P95Decay) Observe(x float64, now time.Time) {
	if t == nil {
		return
	}
	if x < 0 || math.IsNaN(x) || math.IsInf(x, 0) {
		return
	}

	// Update ring buffer.
	t.buf[t.i] = x
	t.i = (t.i + 1) % len(t.buf)
	if t.n < len(t.buf) {
		t.n++
	}

	// Compute raw p95 from current buffer.
	raw := t.computeP95()

	// Initialize.
	if t.est == 0 {
		t.est = raw
		t.lastT = now
		return
	}

	// Time delta.
	dt := 0.0
	if !t.lastT.IsZero() && !now.IsZero() {
		dt = now.Sub(t.lastT).Seconds()
		if dt < 0 {
			dt = 0
		}
	}
	t.lastT = now

	if raw >= t.est {
		// Allow increases immediately.
		t.est = raw
		return
	}

	// Decay downward gradually: est *= (1 - r), with r = decayPerSec * dt capped.
	r := t.decayPerSec * dt
	if r > 0.25 {
		r = 0.25
	}
	if r < 0 {
		r = 0
	}
	minAllowed := t.est * (1 - r)
	if raw < minAllowed {
		t.est = minAllowed
	} else {
		t.est = raw
	}
}

func (t *P95Decay) Value() float64 {
	if t == nil {
		return 0
	}
	return t.est
}

func (t *P95Decay) Ready() bool {
	return t != nil && t.n >= maxInt(20, len(t.buf)/4)
}

func (t *P95Decay) computeP95() float64 {
	if t.n == 0 {
		return 0
	}
	tmp := make([]float64, t.n)
	// Copy most recent n entries (order irrelevant for quantile).
	copy(tmp, t.buf[:t.n])
	sort.Float64s(tmp)
	// p95 index.
	idx := int(math.Ceil(0.95*float64(len(tmp)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(tmp) {
		idx = len(tmp) - 1
	}
	return tmp[idx]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
