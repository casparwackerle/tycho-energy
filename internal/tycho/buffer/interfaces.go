package buffer

// RingBuffer is a generic time-ordered buffer contract.
// Concrete impl will handle event-time vs arrival-time later.
type RingBuffer[T any] interface {
	Push(v T)            // append (may evict oldest)
	Len() int            // current items
	Cap() int            // capacity
	Get(i int) (T, bool) // i from 0..Len()-1
	Watermark() float64  // OPTIONAL: e.g., Len/Cap
	Clear()
}
