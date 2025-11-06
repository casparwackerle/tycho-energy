package ring

import "fmt"

// Ring is a fixed-size, overwrite-on-full circular buffer.
//
// Invariants:
//   - data has length N >= 1 (capacity)
//   - head is the index where the next Push will write
//   - full indicates whether the buffer has wrapped at least once
//
// Chronological order for reads is always oldest -> newest.
// Before first wrap: oldest at data[0], newest at data[head-1].
// After wrap: oldest at data[head], newest at data[(head-1+N)%N].
//
// No method allocates on the hot path (Push, views, Len, etc.).
// Copying methods are provided separately for safety.
//
// Not thread-safe by itself. See Sync[T].
//
// T is typically a metric sample (e.g., RaplTick) that embeds SampleMeta.
// But any type works.

type Ring[T any] struct {
	data []T
	head int
	full bool
}

// New creates a ring of the given capacity (>=1). Panics if capacity < 1.
func New[T any](capacity int) *Ring[T] {
	if capacity < 1 {
		panic(fmt.Sprintf("ring.New: capacity must be >= 1, got %d", capacity))
	}
	return &Ring[T]{
		data: make([]T, capacity),
		head: 0,
		full: false,
	}
}

// Cap returns the fixed capacity N.
func (r *Ring[T]) Cap() int { return len(r.data) }

// Len returns the number of valid items (<= N).
func (r *Ring[T]) Len() int {
	if r.full {
		return len(r.data)
	}
	return r.head
}

// IsFull reports whether the ring has wrapped at least once.
func (r *Ring[T]) IsFull() bool { return r.full }

// Clear logically empties the ring (does not zero the backing array).
func (r *Ring[T]) Clear() {
	r.head = 0
	r.full = false
}

// Reset zero-fills storage with the provided zero value, then Clear().
func (r *Ring[T]) Reset(zero T) {
	for i := range r.data {
		r.data[i] = zero
	}
	r.Clear()
}

// Push writes v at the current head, advances head modulo N, and sets full on wrap.
// No allocations. O(1).
func (r *Ring[T]) Push(v T) {
	r.data[r.head] = v
	r.head++
	if r.head >= len(r.data) {
		r.head = 0
		r.full = true
	}
}

// ViewPhysical returns the raw backing slice along with head/full.
// Callers can map indices themselves: chronological i maps to phys index via AtChrono.
func (r *Ring[T]) ViewPhysical() (data []T, head int, full bool) {
	return r.data, r.head, r.full
}

// ViewChrono exposes the buffer as up to two zero-copy slices in chronological order
// (oldest -> newest). When not full: seg1 = data[:head], seg2 = nil. When full:
// seg1 = data[head:], seg2 = data[:head]. Never allocates.
func (r *Ring[T]) ViewChrono() (seg1 []T, seg2 []T) {
	if !r.full {
		if r.head == 0 {
			return nil, nil
		}
		return r.data[:r.head], nil
	}
	// wrapped: oldest starts at head
	if r.head == 0 {
		// special case: full and head=0 means chronological is the entire data
		return r.data[:], nil
	}
	return r.data[r.head:], r.data[:r.head]
}

// AtChrono returns the i-th element in chronological order (i=0 is oldest).
// The second return value is false if i is out of range.
func (r *Ring[T]) AtChrono(i int) (T, bool) {
	var zero T
	n := r.Len()
	if i < 0 || i >= n {
		return zero, false
	}
	var idx int
	if r.full {
		idx = (r.head + i) % len(r.data)
	} else {
		idx = i
	}
	return r.data[idx], true
}

// Newest returns a zero-copy view of the last k elements (chronological).
// If k >= Len(), it is equivalent to ViewChrono(). Never allocates.
func (r *Ring[T]) Newest(k int) (seg1 []T, seg2 []T) {
	n := r.Len()
	if k >= n {
		return r.ViewChrono()
	}
	if k <= 0 {
		return nil, nil
	}
	// Strategy: slice the tail of the chronological view without copying.
	c1, c2 := r.ViewChrono()
	total := len(c1) + len(c2)
	start := total - k // first index (in chrono space) we want to include
	if start < len(c1) {
		// spans end of c1 and all of c2
		return c1[start:], c2
	}
	// entirely within c2
	off := start - len(c1)
	return c2[off:], nil
}

// SnapshotChrono returns a copied slice with contents in chronological order.
func (r *Ring[T]) SnapshotChrono() []T {
	n := r.Len()
	if n == 0 {
		return nil
	}
	out := make([]T, 0, n)
	c1, c2 := r.ViewChrono()
	out = append(out, c1...)
	out = append(out, c2...)
	return out
}

// PeekNewestCopy returns a copied slice with the last k elements in chronological order.
func (r *Ring[T]) PeekNewestCopy(k int) []T {
	n := r.Len()
	if n == 0 || k <= 0 {
		return nil
	}
	if k > n {
		k = n
	}
	out := make([]T, 0, k)
	c1, c2 := r.Newest(k)
	out = append(out, c1...)
	out = append(out, c2...)
	return out
}
