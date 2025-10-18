package ring

import "math"

// SizeForWindow computes the ring capacity from a time window (seconds) and a
// polling interval (milliseconds). size = ceil((W*1000)/P), clamped to >= 1.
func SizeForWindow(windowSec int, pollMs int) int {
	if pollMs <= 0 {
		return 1
	}
	s := math.Ceil(float64(windowSec*1000) / float64(pollMs))
	if s < 1 {
		s = 1
	}
	return int(s)
}
