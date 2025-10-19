package raplCollector

import (
	"context"
	"fmt"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/components"
	"k8s.io/klog/v2"
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

// func (c *Collector) Collect(ctx context.Context, ts time.Time) {

// 	// ... gather pkg/core/dram/uncore (and take *their* device timestamps if you want)
// 	pkg := uint64(1)
// 	core := uint64(2)
// 	dram := uint64(3)
// 	uncore := uint64(4)

// 	sample := ring.RaplSample{
// 		SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)}, // <- monotonic tick
// 		Package_uJ: pkg,
// 		Core_uJ:    core,
// 		DRAM_uJ:    dram,
// 		Uncore_uJ:  uncore,
// 	}
// 	c.buf.Push(sample) // O(1), thread-safe via Sync wrapper
// }

// Collect reads raw RAPL counters (mJ) per socket and appends one sample to the ring.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if !components.IsSystemCollectionSupported() {
		return
	}

	nodeE := components.GetAbsEnergyFromNodeComponents()
	if len(nodeE) == 0 {
		return
	}

	sockets := make(map[int]ring.RaplDomainCounters, len(nodeE))
	for socketID, e := range nodeE {
		sockets[socketID] = ring.RaplDomainCounters{
			Pkg:    e.Pkg,
			Core:   e.Core,
			Uncore: e.Uncore,
			DRAM:   e.DRAM,
		}
	}

	sample := ring.RaplSample{
		SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)},
		Source:     components.GetSourceName(),
		Sockets:    sockets,
	}

	// --- DEBUG LOG ----------------------------------------------------------
	// Prints one line per collection with per-socket raw counters.
	// Example:
	// I1019 12:34:56.789012 12345 rapl_collector.go:45] RAPL(sysfs): sockets=2 | [0] pkg=12345mJ core=6789mJ dram=100mJ uncore=500mJ | [1] pkg=...
	// ------------------------------------------------------------------------
	msg := ""
	for id, s := range sample.Sockets {
		msg +=
			"\n\tSocket " + fmt.Sprintf("%d", id) +
				fmt.Sprintf(": Pkg=%d Core=%d DRAM=%d Uncore=%d mJ", s.Pkg, s.Core, s.DRAM, s.Uncore)
	}
	klog.V(2).Infof("RAPL(%s): collected %d sockets%s",
		sample.Source, len(sample.Sockets), msg)
	// ------------------------------------------------------------------------

	c.buf.Push(sample)
}
