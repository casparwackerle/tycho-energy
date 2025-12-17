package analysisops

// DeltaU64 returns end-start for monotonic counters.
// If end < start, it returns 0 (caller may choose wrap-aware delta instead).
func DeltaU64(start, end uint64) uint64 {
	if end >= start {
		return end - start
	}
	return 0
}

// DeltaWrapU64 computes a wrap-aware delta given a modulus.
// modulus must be the counter's wrap value (the value at which it rolls over),
// in the same unit as start/end.
//
// If modulus == 0, it falls back to non-wrap delta semantics.
func DeltaWrapU64(start, end, modulus uint64) uint64 {
	if end >= start {
		return end - start
	}
	if modulus == 0 {
		return 0
	}
	// wrapped once: (modulus - start) + end
	return (modulus - start) + end
}
