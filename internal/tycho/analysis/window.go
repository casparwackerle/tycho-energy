package analysis

import (
	"fmt"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
)

// Window is a monotonic tick interval [StartMono, EndMono] (inclusive bounds for filtering).
type Window struct {
	StartMono uint64
	EndMono   uint64
}

func (w Window) String() string { return fmt.Sprintf("[start=%d end=%d]", w.StartMono, w.EndMono) }
func (w Window) IsValid() bool  { return w.EndMono >= w.StartMono }
func (w Window) Contains(mono uint64) bool {
	return mono >= w.StartMono && mono <= w.EndMono
}

// SelectWindow computes window bounds using mono.Now() and a safety offset.
// EndMono = mono.Now() - safetyOffsetTicks
// StartMono = EndMono - windowTicks
func SelectWindow(mono *clock.Mono, windowDur time.Duration, safetyOffset time.Duration) Window {
	now := mono.Now()
	q := mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}

	windowTicks := uint64(windowDur / q)
	safetyTicks := uint64(safetyOffset / q)

	end := now
	if safetyTicks < end {
		end = end - safetyTicks
	} else {
		end = 0
	}

	start := uint64(0)
	if windowTicks < end {
		start = end - windowTicks
	} else {
		start = 0
	}

	return Window{StartMono: start, EndMono: end}
}
