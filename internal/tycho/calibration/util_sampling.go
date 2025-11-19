package calibration

import (
	"context"
	"math"
	"sort"
	"time"
)

// BusyLoop runs fn at (approximately) the given period for the duration.
// If period<=0 it runs as fast as possible (not recommended).
func BusyLoop(ctx context.Context, duration time.Duration, period time.Duration, fn func(ts time.Time)) {
	start := time.Now()
	ticker := time.NewTicker(period)
	defer ticker.Stop()

	// First immediate call
	fn(time.Now())

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			fn(now)
			if time.Since(start) >= duration {
				return
			}
		}
	}
}

// PeekOne returns the newest element if available.
// It avoids repeated slice boilerplate when PeekNewestCopy(k) exists.
func PeekOne[T any](r interface{ PeekNewestCopy(int) []T }) (T, bool) {
	var zero T
	s := r.PeekNewestCopy(1)
	if len(s) == 0 {
		return zero, false
	}
	return s[0], true
}

// P5 returns the 5th percentile of xs (or min if not enough points).
func P5(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	idx := int(0.05 * float64(n-1))
	if idx < 0 {
		idx = 0
	}
	return cp[idx]
}

// Median returns the median of xs (0 if empty).
func Median(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return 0.5 * (cp[n/2-1] + cp[n/2])
}

// DeSpikeP5 computes a robust p5 after trimming outliers with MAD-based 3σ rule.
// Returns (p5, min, n_kept). If trimming discards everything, falls back to plain P5.
func DeSpikeP5(xs []float64) (float64, float64, int) {
	n := len(xs)
	if n == 0 {
		return 0, 0, 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)

	med := Median(cp)

	// Compute MAD
	mads := make([]float64, n)
	for i := range cp {
		mads[i] = math.Abs(cp[i] - med)
	}
	mad := Median(mads)
	if mad == 0 {
		// All (or most) values equal → no trimming needed
		return P5(cp), cp[0], n
	}

	// Convert MAD to robust sigma: sigma ≈ 1.4826 * MAD
	sigma := 1.4826 * mad
	lo := med - 3*sigma
	hi := med + 3*sigma

	kept := make([]float64, 0, n)
	minv := math.MaxFloat64
	for _, v := range cp {
		if v >= lo && v <= hi {
			kept = append(kept, v)
			if v < minv {
				minv = v
			}
		}
	}

	if len(kept) == 0 {
		return P5(cp), cp[0], n
	}
	return P5(kept), minv, len(kept)
}
