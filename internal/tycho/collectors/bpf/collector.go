package bpfCollector

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// Config holds dependencies/settings for the BPF collector (placeholder for now).
type Config struct {
	Buf  *ring.Sync[ring.BpfSample]
	Mono *clock.Mono
	// ...
}

// Collector performs BPF collection (placeholder).
type Collector struct {
	buf  *ring.Sync[ring.BpfSample]
	mono *clock.Mono
	name string

	// ... drivers/handles
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono}
}

func (c *Collector) Collect(ctx context.Context, ts time.Time) {

	// ... gather pkg/core/dram/uncore (and take *their* device timestamps if you want)
	cycles := uint64(1)
	instructions := uint64(2)
	cachemiss := uint64(3)
	pagecachehit := uint64(4)

	sample := ring.BpfSample{
		SampleMeta:   ring.SampleMeta{Mono: c.mono.From(ts)}, // <- monotonic tick
		CPUCycles:    cycles,
		CPUInstr:     instructions,
		CacheMiss:    cachemiss,
		PageCacheHit: pagecachehit,
	}
	c.buf.Push(sample) // O(1), thread-safe via Sync wrapper
}
