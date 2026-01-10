package analysis

import (
	"context"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

type ReadPolicy struct {
	SafetyOffsetTicks uint64
}

type Rings struct {
	Rapl    *ring.Sync[ring.RaplTick]
	Bpf     *ring.Sync[ring.BpfTick]
	Redfish *ring.Sync[ring.RedfishSample]
	Gpu     *ring.Sync[ring.GpuTick]
}

type Cycle struct {
	Ctx     context.Context
	Mono    *clock.Mono
	NowMono uint64

	Window Window
	Policy ReadPolicy

	Rings Rings
	Sink  Sink
	State *StateStore

	Store *PointStore

	// Meta provides read-only access to node-local metadata caches (proc/container/pod).
	Meta *metadata.Store

	winCache map[uint64]Window
}

func (c *Cycle) Rapl() *ring.Sync[ring.RaplTick]         { return c.Rings.Rapl }
func (c *Cycle) Bpf() *ring.Sync[ring.BpfTick]           { return c.Rings.Bpf }
func (c *Cycle) Redfish() *ring.Sync[ring.RedfishSample] { return c.Rings.Redfish }
func (c *Cycle) Gpu() *ring.Sync[ring.GpuTick]           { return c.Rings.Gpu }

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
