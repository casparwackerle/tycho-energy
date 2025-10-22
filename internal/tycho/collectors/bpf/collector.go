package bpfCollector

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/bpf"
	"k8s.io/klog/v2"
)

// include the already-initialized eBPF exporter.
type Config struct {
	Buf  *ring.Sync[ring.BpfSample]
	Mono *clock.Mono
	Exp  bpf.Exporter // injected from main; lifecycle managed in main
}

type Collector struct {
	buf  *ring.Sync[ring.BpfSample]
	mono *clock.Mono
	exp  bpf.Exporter
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono, exp: cfg.Exp}
}

// func (c *Collector) Collect(ctx context.Context, ts time.Time) {

// 	// ... gather pkg/core/dram/uncore (and take *their* device timestamps if you want)
// 	cycles := uint64(1)
// 	instructions := uint64(2)
// 	cachemiss := uint64(3)
// 	pagecachehit := uint64(4)

// 	sample := ring.BpfSample{
// 		SampleMeta:   ring.SampleMeta{Mono: c.mono.From(ts)}, // <- monotonic tick
// 		CPUCycles:    cycles,
// 		CPUInstr:     instructions,
// 		CacheMiss:    cachemiss,
// 		PageCacheHit: pagecachehit,
// 	}
// 	c.buf.Push(sample) // O(1), thread-safe via Sync wrapper
// }

// Collect drains per-process deltas from the eBPF map and appends samples to the ring.
// CollectProcesses() returns deltas since the previous call (batch lookup+delete).
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if c.exp == nil {
		klog.V(2).Info("bpf: exporter not available; skip tick")
		return
	}

	// Derive monotonic timestamp for this tick once (shared by all samples)
	mono := c.mono.From(ts)

	// 1) drain/reset per-CPU bin counters (idle/irq/softirq) for this tick
	if binsTotal, _, err := c.exp.CollectCPUBins(); err != nil {
		klog.Warningf("bpf: CollectCPUBins failed: %v", err)
	} else {
		// Emit a single per-tick bin sample (Pid/CgroupID left zero)
		c.buf.Push(ring.BpfSample{
			SampleMeta: ring.SampleMeta{Mono: mono},
			IdleNS:     binsTotal.IdleNS,
			IRQNS:      binsTotal.IRQNS,
			SoftirqNS:  binsTotal.SoftirqNS,
			// All process-specific fields remain zero in this record.
		})
	}

	// 2) Existing: per-process deltas
	rows, err := c.exp.CollectProcesses()
	if err != nil {
		klog.Warningf("bpf: CollectProcesses failed: %v", err)
		return
	}

	sup := c.exp.SupportedMetrics()

	for i := range rows {
		ct := &rows[i]

		s := ring.BpfSample{
			SampleMeta:   ring.SampleMeta{Mono: mono},
			Pid:          ct.Pid,
			CgroupID:     ct.CgroupId,
			ProcessRunUs: ct.ProcessRunTime, // µs from kernel
			PageCacheHit: ct.PageCacheHit,
			IRQNetTX:     uint64(ct.VecNr[bpf.IRQNetTX]),
			IRQNetRX:     uint64(ct.VecNr[bpf.IRQNetRX]),
			IRQBlock:     uint64(ct.VecNr[bpf.IRQBlock]),
			// IdleNS/IRQNS/SoftirqNS intentionally left zero in per-process rows.
		}

		if sup.HardwareCounters.Has("cpu_cycles") {
			s.CPUCycles = ct.CpuCycles
		}
		if sup.HardwareCounters.Has("cpu_instructions") {
			s.CPUInstr = ct.CpuInstr
		}
		if sup.HardwareCounters.Has("cache_miss") {
			s.CacheMiss = ct.CacheMiss
		}

		c.buf.Push(s)
	}
}
