package redfishCollector

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"k8s.io/klog/v2"
)

// Config holds dependencies/settings for the BPF collector (placeholder for now).
type Config struct {
	Buf  *ring.Sync[ring.RedfishSample]
	Mono *clock.Mono
	// ...
}

// Collector performs BPF collection (placeholder).
type Collector struct {
	buf  *ring.Sync[ring.RedfishSample]
	mono *clock.Mono
	// ... drivers/handles
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono}
}

func (c *Collector) Collect(ctx context.Context, ts time.Time) {

	klog.Infof("Tycho collect: tick=%d @ %s",
		c.mono.From(ts),
		ts.Format(time.RFC3339Nano))

	// ... gather pkg/core/dram/uncore (and take *their* device timestamps if you want)
	powerwatts := float64(1)

	sample := ring.RedfishSample{
		SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)}, // <- monotonic tick
		PowerWatts: powerwatts,
	}
	c.buf.Push(sample) // O(1), thread-safe via Sync wrapper
}
