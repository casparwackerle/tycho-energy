package gpuCollector

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// Config holds dependencies/settings for the BPF collector (placeholder for now).
type Config struct {
	Buf  *ring.Sync[ring.GpuSample]
	Mono *clock.Mono
	// ...
}

// Collector performs BPF collection (placeholder).
type Collector struct {
	buf  *ring.Sync[ring.GpuSample]
	mono *clock.Mono
	// ... drivers/handles
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono}
}

func (c *Collector) Collect(ctx context.Context, ts time.Time) {

	// ... gather pkg/core/dram/uncore (and take *their* device timestamps if you want)
	smutilpct := float64(1)
	memutilpct := float64(2)

	sample := ring.GpuSample{
		SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)}, // <- monotonic tick
		SMUtilPct:  smutilpct,
		MemUtilPct: memutilpct,
	}
	c.buf.Push(sample) // O(1), thread-safe via Sync wrapper
}
