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

	Window Window
	Policy ReadPolicy

	Rings Rings
	Sink  Sink
	State *StateStore

	// per-cycle cache: delayTicks -> shifted window
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

// EffectiveWindowTicks returns Cycle.Window shifted back by delayTicks.
// Cached because a given metric source delay is fixed and reused across derived metrics.
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
	w := c.Window.ShiftBack(delayTicks)
	c.winCache[delayTicks] = w
	return w
}
