package raplCollector

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

	c.buf.Push(sample)
}

func PrintAvailableRaplDomains() {
	// Only relevant for sysfs-backed collector
	src := components.GetSourceName()
	if src == "" {
		src = "unknown"
	}

	// Try to locate powercap sysfs paths directly
	root := "/sys/class/powercap"
	matches, err := filepath.Glob(filepath.Join(root, "intel-rapl*"))
	if err != nil || len(matches) == 0 {
		klog.Warningf("RAPL(%s): no intel-rapl directories found under %s", src, root)
		return
	}

	klog.Infof("RAPL(%s): discovered domains:", src)
	for _, p := range matches {
		nameFile := filepath.Join(p, "name")
		data, err := os.ReadFile(nameFile)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(data))
		klog.Infof("  %s → name=%q", filepath.Base(p), name)
	}
}
