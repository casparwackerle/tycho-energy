// file: internal/tycho/analysis/gpu/windowed_getter.go
package gpu

import (
	"math"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
)

// GetCorrectedWindow returns a view (copy) of the corrected power series restricted to
// the requested corrected-time window [aSec, bSec].
//
// It returns:
// - pw: per-bin power values (W), held on uniform dt grid
// - t0Sec: start time of pw[0] in corrected seconds
// - dtSec: grid step in seconds
// - ok: whether a series existed and window overlap was non-empty
//
// Notes:
// - This function copies the window slice to avoid accidental mutation of stored state.
// - If the requested window partially exceeds the stored series support, it is clipped.
func GetCorrectedWindow(state *analysis.StateStore, uuid string, aSec, bSec float64) (pw []float64, t0Sec, dtSec float64, ok bool) {
	ser, ok := GetCorrectedSeries(state, uuid)
	if !ok {
		return nil, 0, 0, false
	}
	return WindowFromSeries(ser, aSec, bSec)
}

// WindowFromSeries is the pure helper that slices a Series to [aSec, bSec] (clipped).
// This is useful if a caller already has the Series (for example, inside the GPU metric itself).
func WindowFromSeries(ser Series, aSec, bSec float64) (pw []float64, t0Sec, dtSec float64, ok bool) {
	if ser.DtSec <= 0 || len(ser.PW) == 0 || bSec <= aSec {
		return nil, 0, 0, false
	}

	t0 := ser.T0Sec
	dt := ser.DtSec
	n := len(ser.PW)

	minT := t0
	maxT := t0 + float64(n)*dt

	// Clip to available support.
	if aSec < minT {
		aSec = minT
	}
	if bSec > maxT {
		bSec = maxT
	}
	if bSec <= aSec {
		return nil, 0, 0, false
	}

	// Convert to bin indices.
	i0 := int(math.Floor((aSec - t0) / dt))
	i1 := int(math.Ceil((bSec - t0) / dt)) // exclusive
	if i0 < 0 {
		i0 = 0
	}
	if i1 > n {
		i1 = n
	}
	if i1 <= i0 {
		return nil, 0, 0, false
	}

	out := make([]float64, i1-i0)
	copy(out, ser.PW[i0:i1])

	return out, t0 + float64(i0)*dt, dt, true
}
