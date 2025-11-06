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
	Buf  *ring.Sync[ring.RaplTick]
	Mono *clock.Mono
	// ...
}

// Collector performs BPF collection (placeholder).
type Collector struct {
	buf  *ring.Sync[ring.RaplTick]
	mono *clock.Mono
	// ... drivers/handles
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono}
}

// Collect reads raw RAPL counters (mJ) per socket and appends one sample to the ring.
// Collect reads raw RAPL counters (mJ) for all sockets and pushes exactly one tick.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if !components.IsSystemCollectionSupported() {
		return
	}

	start := time.Now()

	// Single snapshot of absolute counters per package/socket.
	nodeE := components.GetAbsEnergyFromNodeComponents()
	if len(nodeE) == 0 {
		return
	}

	// Build per-tick payload: one element holds all sockets/domains at this timestamp.
	sockets := make(map[int]ring.RaplDomainCounters, len(nodeE))
	for socketID, e := range nodeE {
		sockets[socketID] = ring.RaplDomainCounters{
			Pkg:    e.Pkg,    // package (PKG) energy
			Core:   e.Core,   // PP0 (cores) — per package aggregate
			Uncore: e.Uncore, // uncore/PP1 if available on this platform
			DRAM:   e.DRAM,   // DRAM domain (platform-dependent)
		}
	}

	tick := ring.RaplTick{
		SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)},
		Source:     components.GetSourceName(),
		Sockets:    sockets,
	}

	// Push exactly one immutable tick for this collection run.
	c.buf.Push(tick)

	klog.V(5).Infof("rapl: pushed 1 tick (sockets=%d) in %v", len(sockets), time.Since(start))
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
