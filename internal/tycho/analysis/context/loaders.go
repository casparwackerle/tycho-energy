package analysisctx

import "github.com/casparwackerle/tycho-energy/internal/tycho/ring"

// FilterWindowChrono copies only those samples whose mono is within [start,end].
// The mono selector is provided by caller to keep this generic.
// This does NOT lock the ring for the whole read; it uses a single ViewChrono() call.
// Note: ViewChrono itself takes the ring's RLock briefly (inside ring.Sync).
func FilterWindowChrono[T any](
	r *ring.Sync[T],
	start uint64,
	end uint64,
	monoOf func(T) uint64,
) []T {
	if r == nil || monoOf == nil {
		return nil
	}

	seg1, seg2 := r.ViewChrono()
	// We copy only the matching subset, preserving chronological order.
	out := make([]T, 0, 16)

	appendIf := func(seg []T) {
		for _, s := range seg {
			m := monoOf(s)
			if m < start {
				continue
			}
			if m > end {
				// samples are usually chronological, but not guaranteed; we still treat as best-effort.
				continue
			}
			out = append(out, s)
		}
	}

	appendIf(seg1)
	appendIf(seg2)
	return out
}
