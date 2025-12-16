package analysis

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// ReadPolicy defines shared, engine-owned rules for reading rings.
// Slice 0: best-effort read with a safety offset baked into Window selection.
// No global locks, no full-ring copies.
type ReadPolicy struct {
	// SafetyOffset is subtracted from "now" to avoid reading newest samples
	// that might still be in-flight or not fully pushed by producers.
	SafetyOffset time.Duration
}

// Rings is the per-cycle access to raw sample rings.
// Slice 0: only RAPL is required, but struct is extensible.
type Rings struct {
	Rapl *ring.Sync[ring.RaplTick]
}

// Cycle is a per-run context handed to metrics.
type Cycle struct {
	Ctx     context.Context
	Mono    *clock.Mono
	NowMono uint64

	Window Window
	Policy ReadPolicy

	Rings Rings
	Sink  Sink
	State *StateStore
}

// Rapl returns the RAPL ring handle (may be nil if not wired/enabled).
func (c *Cycle) Rapl() *ring.Sync[ring.RaplTick] { return c.Rings.Rapl }
