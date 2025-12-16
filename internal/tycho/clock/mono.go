// clock/mono.go
package clock

import (
	"sync/atomic"
	"time"
)

type Mono struct {
	src     Source
	base    time.Time
	quantum time.Duration // e.g., 1 * time.Millisecond
	last    atomic.Uint64 // to guarantee strictly non-decreasing ticks
}

// NewMono captures a monotonic base and sets the time quantum.
// If src is nil, DefaultSource is used.
func NewMono(src Source, quantum time.Duration) *Mono {
	if src == nil {
		src = DefaultSource
	}
	if quantum <= 0 {
		quantum = time.Millisecond // sane default; adjust to your config
	}
	return &Mono{src: src, base: src.Now(), quantum: quantum}
}

// From converts a timestamp (ideally produced by the same Source)
// into Tycho's tick index: floor((ts - base) / quantum), then enforces strictly
// increasing sequence via atomic bump (so collectors never go backward).
func (m *Mono) From(ts time.Time) uint64 {
	d := ts.Sub(m.base) // uses monotonic component if present
	if d < 0 {
		d = 0
	}
	tick := uint64(d / m.quantum)

	for {
		last := m.last.Load()
		if tick <= last {
			tick = last + 1
		}
		if m.last.CompareAndSwap(last, tick) {
			return tick
		}
	}
}

// Now is a convenience wrapper for From(src.Now()).
func (m *Mono) Now() uint64 {
	return m.From(m.src.Now())
}

// Quantum returns the tick duration used by this Mono clock.
func (m *Mono) Quantum() time.Duration { return m.quantum }
