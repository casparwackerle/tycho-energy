// package clock

// import "time"

// // Source abstracts the time base; real impl will use monotonic time.
// type Source interface {
// 	Now() time.Time
// 	Since(t time.Time) time.Duration
// }

// // AlignedTicker will later provide drift-bounded ticks aligned to a base epoch.
// // For now we only define the API to let other packages compile.
// type AlignedTicker interface {
// 	C() <-chan time.Time
// 	Stop()
// }

// // NewAlignedTicker is a placeholder constructor we will implement next step.
// func NewAlignedTicker(src Source, period time.Duration, base time.Time) (AlignedTicker, error) {
// 	// TODO: implement real aligned ticker
// 	t := time.NewTicker(period)
// 	return &stdTicker{t: t}, nil
// }

// type stdTicker struct{ t *time.Ticker }

// func (s *stdTicker) C() <-chan time.Time { return s.t.C }
// func (s *stdTicker) Stop()               { s.t.Stop() }

package clock

import (
	"errors"
	"sync"
	"time"
)

// Source abstracts the time base; using time.Now() preserves Go's monotonic clock.
type Source interface {
	Now() time.Time
	Since(t time.Time) time.Duration
}

type realSource struct{}

func (realSource) Now() time.Time                  { return time.Now() }
func (realSource) Since(t time.Time) time.Duration { return time.Since(t) }

// DefaultSource is used if nil is passed to NewAlignedTicker.
var DefaultSource Source = realSource{}

// AlignedTicker provides drift-bounded ticks aligned to a base epoch.
type AlignedTicker interface {
	C() <-chan time.Time
	Stop()
}

type alignedTicker struct {
	c    chan time.Time //channel
	stop chan struct{}
	wg   sync.WaitGroup
	once sync.Once
}

func (t *alignedTicker) C() <-chan time.Time { return t.c }

func (t *alignedTicker) Stop() {
	t.once.Do(func() { close(t.stop) })
	t.wg.Wait()
}

// NewAlignedTicker emits ticks aligned to base + n*period.
// If base is zero, it aligns to "now" (i.e., schedules the next tick to be now or the next multiple).
func NewAlignedTicker(src Source, period time.Duration, base time.Time) (AlignedTicker, error) {
	if period <= 0 {
		return nil, errors.New("period must be > 0")
	}
	if src == nil {
		src = DefaultSource
	}

	t := &alignedTicker{
		c:    make(chan time.Time, 1), // small buffer to avoid blocking if a reader is momentarily late
		stop: make(chan struct{}),
	}
	t.wg.Add(1)

	go func() {
		defer t.wg.Done()

		now := src.Now()
		if base.IsZero() {
			base = now
		}

		// compute delay until the next aligned tick: base + k*period >= now
		var delay time.Duration
		if now.Before(base) {
			delay = base.Sub(now)
		} else {
			elapsed := now.Sub(base)
			rem := elapsed % period
			if rem == 0 {
				delay = 0
			} else {
				delay = period - rem
			}
		}

		// initial alignment wait
		timer := time.NewTimer(delay)
		select {
		case <-t.stop:
			timer.Stop()
			return
		case ts := <-timer.C:
			// non-blocking send so a missing early reader doesn't stall alignment
			select {
			case t.c <- ts:
			default:
			}
		}

		// steady-state periodic ticks (aligned by starting at the aligned moment)
		ticker := time.NewTicker(period)
		defer ticker.Stop()

		for {
			select {
			case <-t.stop:
				return
			case ts := <-ticker.C:
				select {
				case t.c <- ts:
				default:
				}
			}
		}
	}()

	return t, nil
}
