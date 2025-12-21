package gpu

import "math"

// cgSolve solves A x = b for SPD A using Conjugate Gradient.
// applyA computes y = A(x).
func cgSolve(applyA func(dst, x []float64), b []float64, maxIter int, tolRel float64) (x []float64, it int, relRes float64) {
	n := len(b)
	x = make([]float64, n)
	if n == 0 {
		return x, 0, 0
	}
	if maxIter <= 0 {
		maxIter = 100
	}
	if tolRel <= 0 {
		tolRel = 1e-6
	}

	r := make([]float64, n) // r = b - A x; initially b
	copy(r, b)

	p := make([]float64, n)
	copy(p, r)

	rsold := dot(r, r)
	bnorm := math.Sqrt(dot(b, b))
	if bnorm == 0 {
		return x, 0, 0
	}

	Ap := make([]float64, n)

	for it = 0; it < maxIter; it++ {
		zero(Ap)
		applyA(Ap, p)

		den := dot(p, Ap)
		if den <= 0 {
			// Should not happen for SPD, but guard against numerical/pathological cases.
			break
		}

		alpha := rsold / den
		axpy(x, p, alpha)   // x += alpha p
		axpy(r, Ap, -alpha) // r -= alpha A p
		rsnew := dot(r, r)

		res := math.Sqrt(rsnew) / bnorm
		if res <= tolRel {
			relRes = res
			return x, it + 1, relRes
		}

		beta := rsnew / rsold
		scale(p, beta)
		axpy(p, r, 1.0) // p = r + beta p

		rsold = rsnew
	}

	relRes = math.Sqrt(dot(r, r)) / bnorm
	return x, it, relRes
}

func dot(a, b []float64) float64 {
	var s float64
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

func axpy(dst, x []float64, a float64) {
	for i := range dst {
		dst[i] += a * x[i]
	}
}

func scale(x []float64, a float64) {
	for i := range x {
		x[i] *= a
	}
}

func zero(x []float64) {
	for i := range x {
		x[i] = 0
	}
}
