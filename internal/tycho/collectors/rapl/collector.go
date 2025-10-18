package raplCollector

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// Config holds dependencies/settings for the BPF collector (placeholder for now).
type Config struct {
	Buf  *ring.Sync[ring.RaplSample]
	Mono *clock.Mono
	// ...
}

// Collector performs BPF collection (placeholder).
type Collector struct {
	buf  *ring.Sync[ring.RaplSample]
	mono *clock.Mono
	// ... drivers/handles
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono}
}

func (c *Collector) Collect(ctx context.Context, ts time.Time) {

	// ... gather pkg/core/dram/uncore (and take *their* device timestamps if you want)
	pkg := uint64(1)
	core := uint64(2)
	dram := uint64(3)
	uncore := uint64(4)

	sample := ring.RaplSample{
		SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)}, // <- monotonic tick
		Package_uJ: pkg,
		Core_uJ:    core,
		DRAM_uJ:    dram,
		Uncore_uJ:  uncore,
	}
	c.buf.Push(sample) // O(1), thread-safe via Sync wrapper
}
