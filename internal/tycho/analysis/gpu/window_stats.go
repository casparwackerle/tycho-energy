// file: internal/tycho/analysis/gpu/window_stats.go
package gpu

import (
	"math"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
)

// EnergyMJ integrates reconstructed power series over the corrected analysis window.
// Returns energy in mJ.
//
// Window convention:
// - We interpret Window as continuous seconds using tick * monoQuantum.
// - We integrate over [startSec, endSec], where endSec is computed from EndMono.
// This matches your “align with corrected analysis window end” rule.
func EnergyMJ(ser Series, w analysis.Window, monoQuantumSec float64) (float64, bool) {
	if ser.DtSec <= 0 || len(ser.PW) == 0 || monoQuantumSec <= 0 {
		return 0, false
	}
	if !w.IsValid() {
		return 0, false
	}

	startSec := float64(w.StartMono) * monoQuantumSec
	endSec := float64(w.EndMono) * monoQuantumSec
	if endSec <= startSec {
		return 0, false
	}

	// Clamp window to series extent.
	serStart := ser.T0Sec
	serEnd := ser.T0Sec + float64(len(ser.PW))*ser.DtSec
	a := math.Max(startSec, serStart)
	b := math.Min(endSec, serEnd)
	if b <= a {
		return 0, false
	}

	// Integrate bins with ZOH on the grid: p[i] holds on [t_i, t_{i+1})
	var eJ float64

	// Find first bin index that could overlap a.
	i0 := int(math.Floor((a - ser.T0Sec) / ser.DtSec))
	if i0 < 0 {
		i0 = 0
	}
	if i0 >= len(ser.PW) {
		return 0, false
	}

	t := a
	for i := i0; i < len(ser.PW); i++ {
		t0 := ser.T0Sec + float64(i)*ser.DtSec
		t1 := t0 + ser.DtSec
		if t1 <= t {
			continue
		}
		segA := math.Max(t, t0)
		segB := math.Min(b, t1)
		if segB > segA {
			p := ser.PW[i]
			if p < 0 {
				// Should already be clamped, but stay defensive.
				p = 0
			}
			eJ += p * (segB - segA)
		}
		if t1 >= b {
			break
		}
	}

	// J -> mJ
	return eJ * 1000.0, true
}

// WindowMeanPowerW computes mean reconstructed power (W) over the window.
// It uses EnergyMJ / windowSeconds.
func WindowMeanPowerW(energyMJ float64, w analysis.Window, monoQuantumSec float64) (float64, bool) {
	if monoQuantumSec <= 0 || !w.IsValid() {
		return 0, false
	}
	startSec := float64(w.StartMono) * monoQuantumSec
	endSec := float64(w.EndMono) * monoQuantumSec
	dt := endSec - startSec
	if dt <= 0 {
		return 0, false
	}
	return (energyMJ / 1000.0) / dt, true
}

// MeanObsPowerW returns the arithmetic mean of obs.Power for samples whose timestamp is in the
// corrected window [startSec, endSec]. This is a simple sanity comparator, not an integral.
func MeanObsPowerW(obs []PowerObs, w analysis.Window, monoQuantumSec float64) (mean float64, n int) {
	if len(obs) == 0 || monoQuantumSec <= 0 || !w.IsValid() {
		return 0, 0
	}
	startSec := float64(w.StartMono) * monoQuantumSec
	endSec := float64(w.EndMono) * monoQuantumSec
	if endSec <= startSec {
		return 0, 0
	}

	var sum float64
	for _, o := range obs {
		if o.TSec < startSec || o.TSec > endSec {
			continue
		}
		if o.Power < 0 {
			continue
		}
		sum += o.Power
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}
