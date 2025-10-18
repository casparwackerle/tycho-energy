package ring

import "sync"

// Sync wraps a Ring[T] with an RWMutex to provide thread-safe access.
// Zero-copy views are still zero-copy; safety beyond the call boundary must
// be coordinated by callers if the producer keeps pushing.

type Sync[T any] struct {
	mu sync.RWMutex
	r  *Ring[T]
}

func NewSync[T any](capacity int) *Sync[T] { return &Sync[T]{r: New[T](capacity)} }

func (s *Sync[T]) Cap() int     { s.mu.RLock(); defer s.mu.RUnlock(); return s.r.Cap() }
func (s *Sync[T]) Len() int     { s.mu.RLock(); defer s.mu.RUnlock(); return s.r.Len() }
func (s *Sync[T]) IsFull() bool { s.mu.RLock(); defer s.mu.RUnlock(); return s.r.IsFull() }
func (s *Sync[T]) Clear()       { s.mu.Lock(); defer s.mu.Unlock(); s.r.Clear() }
func (s *Sync[T]) Reset(zero T) { s.mu.Lock(); defer s.mu.Unlock(); s.r.Reset(zero) }
func (s *Sync[T]) Push(v T)     { s.mu.Lock(); defer s.mu.Unlock(); s.r.Push(v) }
func (s *Sync[T]) ViewPhysical() ([]T, int, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.ViewPhysical()
}
func (s *Sync[T]) ViewChrono() ([]T, []T) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.ViewChrono()
}
func (s *Sync[T]) AtChrono(i int) (T, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.AtChrono(i)
}
func (s *Sync[T]) Newest(k int) ([]T, []T) { s.mu.RLock(); defer s.mu.RUnlock(); return s.r.Newest(k) }
func (s *Sync[T]) SnapshotChrono() []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.SnapshotChrono()
}
func (s *Sync[T]) PeekNewestCopy(k int) []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.r.PeekNewestCopy(k)
}
