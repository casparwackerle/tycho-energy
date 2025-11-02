// package calibration

// import (
// 	"context"
// 	"sort"
// 	"time"
// )

// // BusyLoop runs fn at (approximately) the given period for the duration.
// // If period<=0 it runs as fast as possible (not recommended).
// func BusyLoop(ctx context.Context, duration time.Duration, period time.Duration, fn func(ts time.Time)) {
// 	start := time.Now()
// 	ticker := time.NewTicker(period)
// 	defer ticker.Stop()

// 	// First immediate call
// 	fn(time.Now())

// 	for {
// 		select {
// 		case <-ctx.Done():
// 			return
// 		case <-ticker.C:
// 			now := time.Now()
// 			fn(now)
// 			if time.Since(start) >= duration {
// 				return
// 			}
// 		}
// 	}
// }

// // P5 returns the 5th percentile of xs (or min if not enough points).
// func P5(xs []float64) float64 {
// 	n := len(xs)
// 	if n == 0 {
// 		return 0
// 	}
// 	cp := append([]float64(nil), xs...)
// 	sort.Float64s(cp)
// 	idx := int(0.05 * float64(n-1))
// 	if idx < 0 {
// 		idx = 0
// 	}
// 	return cp[idx]
// }

// // Median returns the median of xs (0 if empty).
// func Median(xs []float64) float64 {
// 	n := len(xs)
// 	if n == 0 {
// 		return 0
// 	}
// 	cp := append([]float64(nil), xs...)
// 	sort.Float64s(cp)
// 	if n%2 == 1 {
// 		return cp[n/2]
// 	}
// 	return 0.5 * (cp[n/2-1] + cp[n/2])
// }

// // PeekOne returns the newest element if available.
// // It avoids repeated slice boilerplate when PeekNewestCopy(k) exists.
// func PeekOne[T any](r interface{ PeekNewestCopy(int) []T }) (T, bool) {
// 	var zero T
// 	s := r.PeekNewestCopy(1)
// 	if len(s) == 0 {
// 		return zero, false
// 	}
// 	return s[0], true
// }

/////////////////////////////////////////////////////////////////////////////////////////////////////

package calibration

import (
	"bufio"
	"context"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
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

func alignToQuantum(ms, quantumMs int) int {
	if quantumMs <= 1 {
		return ms
	}
	// Round up to the nearest multiple of quantum to avoid undersampling due to rounding.
	rem := ms % quantumMs
	if rem == 0 {
		return ms
	}
	return ms + (quantumMs - rem)
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

// -------------------------------
// New helpers for Prompt 2
// -------------------------------

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

// QuietWindowGuard performs a best-effort "quiet" check over 'dur' using Linux /proc/stat.
// It measures CPU utilization between two samples. If util < 2%, returns ok=true.
// I/O/temperature are omitted to keep this helper small; you can extend later.
func QuietWindowGuard(ctx context.Context, _ interface{}, dur time.Duration) (bool, string) {
	// Read t0
	idle0, total0, ok0 := readProcStatCPU()
	if !ok0 {
		// If we can't read CPU stats, err on the permissive side but explain.
		select {
		case <-time.After(dur):
		case <-ctx.Done():
		}
		return true, "cpu_unknown_permissive"
	}

	// Sleep for duration or until ctx canceled
	t := time.NewTimer(dur)
	select {
	case <-t.C:
	case <-ctx.Done():
		t.Stop()
		return false, "context_canceled"
	}

	// Read t1
	idle1, total1, ok1 := readProcStatCPU()
	if !ok1 || total1 <= total0 || idle1 < idle0 {
		return true, "cpu_unknown_permissive"
	}

	idleDelta := float64(idle1 - idle0)
	totalDelta := float64(total1 - total0)
	util := 1.0 - (idleDelta / totalDelta) // fraction

	if util < 0.02 {
		return true, "cpu_util_ok"
	}
	return false, "cpu_util_high"
}

// EffectiveArrivalRate computes the achieved sample rate relative to the requested period.
// Returns a value clamped to [0,1]. If fewer than 2 timestamps, returns 0.
func EffectiveArrivalRate(requestedMs int, times []time.Time) float64 {
	if requestedMs <= 0 || len(times) < 2 {
		return 0
	}
	first := times[0]
	last := times[len(times)-1]
	window := last.Sub(first)
	if window <= 0 {
		return 0
	}

	expected := float64(window.Milliseconds()) / float64(requestedMs)
	if expected <= 0 {
		return 0
	}
	// Observed intervals = len(times)-1
	observed := float64(len(times) - 1)
	r := observed / expected
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

// DropRatio returns (total-ok)/total in [0,1]. If total<=0, returns 0.
func DropRatio(total, ok int) float64 {
	if total <= 0 {
		return 0
	}
	if ok < 0 {
		ok = 0
	}
	if ok > total {
		ok = total
	}
	return float64(total-ok) / float64(total)
}

// EarlyStopSatisfied decides if search can stop early given the recent path.
// Rule of thumb (tunable):
// - Need at least 2 recent OK points with Samples>=minSamples.
// - The ms gap between them <= targetPrecMs.
// - Both have low drop ratio and similar effRate (delta <= 0.10).
func EarlyStopSatisfied(path []PollProbePoint, targetPrecMs, minSamples int) (bool, string) {
	n := len(path)
	if n < 2 {
		return false, "insufficient_points"
	}
	a := path[n-1]
	b := path[n-2]

	// Both must be OK with enough samples
	if !(a.Ok && b.Ok) || a.Samples < minSamples || b.Samples < minSamples {
		return false, "not_enough_ok_samples"
	}
	// Precision condition
	if absInt(a.Ms-b.Ms) > targetPrecMs {
		return false, "precision_not_reached"
	}
	// Drop & effRate conditions
	da := DropRatio(a.Samples, a.Samples-a.Errs)
	db := DropRatio(b.Samples, b.Samples-b.Errs)
	if da > 0.05 || db > 0.05 { // allow up to 5% drops
		return false, "high_drop_rate"
	}
	if math.Abs(a.EffRate-b.EffRate) > 0.10 {
		return false, "eff_rate_unstable"
	}
	return true, "stable_enough"
}

// -------------------------------
// Internal helpers
// -------------------------------

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// readProcStatCPU reads aggregate CPU counters from /proc/stat and returns (idle,total).
// total = user+nice+system+idle+iowait+irq+softirq+steal (guest excluded for simplicity).
func readProcStatCPU() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	line := sc.Text()
	if !strings.HasPrefix(line, "cpu ") {
		return 0, 0, false
	}
	fields := strings.Fields(line)
	// fields: cpu user nice system idle iowait irq softirq steal guest guest_nice
	// We use up to 'steal' (index 8)
	if len(fields) < 8 {
		return 0, 0, false
	}
	var nums [8]uint64
	for i := 0; i < 8; i++ {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		nums[i] = v
	}
	idle = nums[3] + nums[4] // idle + iowait
	total = 0
	for i := 0; i < 8; i++ {
		total += nums[i]
	}
	return idle, total, true
}
