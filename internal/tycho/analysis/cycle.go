package analysis

import (
	"context"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// ReadPolicy defines shared rules for reading rings.
type ReadPolicy struct {
	// SafetyOffset is already applied to Cycle.Window via SelectWindow.
	// Keep it here for introspection/logging and future extensions.
	SafetyOffsetTicks uint64
}

// Rings is the per-cycle access to raw sample rings.
type Rings struct {
	Rapl    *ring.Sync[ring.RaplTick]
	Bpf     *ring.Sync[ring.BpfTick]
	Redfish *ring.Sync[ring.RedfishSample]
	Gpu     *ring.Sync[ring.GpuTick]
}

// Cycle is a per-run context handed to metrics.
type Cycle struct {
	Ctx     context.Context
	Mono    *clock.Mono
	NowMono uint64

	// Window is the analysis window in corrected/event-time domain.
	// All metrics should emit points with Window = this Window.
	Window Window
	Policy ReadPolicy

	Rings Rings
	Sink  Sink
	State *StateStore

	// Store is a per-cycle point store populated by a collecting sink wrapper.
	// It is cycle-local (not a global memoization layer).
	Store *PointStore

	// per-cycle cache: delayTicks -> shifted window for raw sample selection
	winCache map[uint64]Window
}

// Rapl returns the RAPL ring handle (may be nil if not wired/enabled).
func (c *Cycle) Rapl() *ring.Sync[ring.RaplTick] { return c.Rings.Rapl }

// Bpf returns the eBPF ring handle (may be nil if not wired/enabled).
func (c *Cycle) Bpf() *ring.Sync[ring.BpfTick] { return c.Rings.Bpf }

// Redfish returns the redfish ring handle (may be nil if not wired/enabled).
func (c *Cycle) Redfish() *ring.Sync[ring.RedfishSample] { return c.Rings.Redfish }

// Gpu returns the GPU ring handle (may be nil if not wired/enabled).
func (c *Cycle) Gpu() *ring.Sync[ring.GpuTick] { return c.Rings.Gpu }

// EffectiveWindowTicks returns the raw-sample selection window for a metric
// with a constant delay in ticks.
//
// Semantics:
//
//	t_corrected = t_sample - delayTicks
//
// We want corrected time within Cycle.Window, so raw samples must fall into:
//
//	raw ∈ Cycle.Window shifted forward by delayTicks.
func (c *Cycle) EffectiveWindowTicks(delayTicks uint64) Window {
	if delayTicks == 0 {
		return c.Window
	}
	if c.winCache == nil {
		c.winCache = make(map[uint64]Window, 4)
	}
	if w, ok := c.winCache[delayTicks]; ok {
		return w
	}
	w := c.Window.ShiftForward(delayTicks)
	c.winCache[delayTicks] = w
	return w
}
