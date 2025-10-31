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
	Buf  *ring.Sync[ring.BpfTick]
	Mono *clock.Mono
	Exp  bpf.Exporter // injected from main; lifecycle managed in main
}

type Collector struct {
	buf  *ring.Sync[ring.BpfTick]
	mono *clock.Mono
	exp  bpf.Exporter
}

func New(cfg Config) *Collector {
	return &Collector{buf: cfg.Buf, mono: cfg.Mono, exp: cfg.Exp}
}

// Collect drains per-process deltas from the eBPF map and pushes exactly ONE
// tick into the ring, containing:
//   - this tick's CPU bin totals (idle/irq/softirq), and
//   - a PID-sorted slice of per-process deltas for the tick.
//
// NOTE: CollectProcesses() returns deltas since the previous call (batch lookup+delete).
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

	// Monotonic timestamp for this tick
	mono := c.mono.From(ts)

	// --- (1) CPU bins for this tick -------------------------------------------------
	var idleNS, irqNS, softirqNS uint64
	{
		start := time.Now()
		if binsTotal, _, err := c.exp.CollectCPUBins(); err != nil {
			klog.Warningf("bpf: CollectCPUBins failed: %v", err)
		} else {
			idleNS = binsTotal.IdleNS
			irqNS = binsTotal.IRQNS
			softirqNS = binsTotal.SoftirqNS
			klog.V(5).Infof("bpf: collected cpu_bins in %v (idle=%d ns, irq=%d ns, softirq=%d ns)",
				time.Since(start), idleNS, irqNS, softirqNS)
		}
	}

	// --- (2) Per-process deltas for this tick ---------------------------------------
	start := time.Now()
	rows, err := c.exp.CollectProcesses()
	if err != nil {
		klog.Warningf("bpf: CollectProcesses failed: %v", err)
		return
	}

	sup := c.exp.SupportedMetrics()

	// Aggregate per PID in case exporter delivers multiple records per PID.
	type agg struct {
		pid       uint64
		cgroupID  uint64
		runUs     uint64
		pageCache uint64
		irqTx     uint64
		irqRx     uint64
		irqBlock  uint64
		cpuCycles uint64
		cpuInstr  uint64
		cacheMiss uint64
	}
	tmp := make(map[uint64]*agg, len(rows))

	for i := range rows {
		ct := &rows[i]
		a := tmp[ct.Pid]
		if a == nil {
			a = &agg{pid: ct.Pid, cgroupID: ct.CgroupId}
			tmp[ct.Pid] = a
		}
		a.runUs += ct.ProcessRunTime
		a.pageCache += ct.PageCacheHit
		a.irqTx += uint64(ct.VecNr[bpf.IRQNetTX])
		a.irqRx += uint64(ct.VecNr[bpf.IRQNetRX])
		a.irqBlock += uint64(ct.VecNr[bpf.IRQBlock])

		if sup.HardwareCounters.Has("cpu_cycles") {
			a.cpuCycles += ct.CpuCycles
		}
		if sup.HardwareCounters.Has("cpu_instructions") {
			a.cpuInstr += ct.CpuInstr
		}
		if sup.HardwareCounters.Has("cache_miss") {
			a.cacheMiss += ct.CacheMiss
		}
	}

	// Materialize into a PID-sorted slice for compact, read-optimized storage.
	procs := make([]ring.BpfProcDelta, 0, len(tmp))
	for _, a := range tmp {
		procs = append(procs, ring.BpfProcDelta{
			PID:          a.pid,
			CgroupID:     a.cgroupID, // attribute (primary key remains PID within the tick)
			ProcessRunUs: a.runUs,
			PageCacheHit: a.pageCache,
			IRQNetTX:     a.irqTx,
			IRQNetRX:     a.irqRx,
			IRQBlock:     a.irqBlock,
			CPUCycles:    a.cpuCycles,
			CPUInstr:     a.cpuInstr,
			CacheMiss:    a.cacheMiss,
		})
	}

	// --- (3) Build ONE tick and push it ---------------------------------------------
	tick := ring.BpfTick{
		SampleMeta: ring.SampleMeta{Mono: mono},
		IdleNS:     idleNS,
		IRQNS:      irqNS,
		SoftirqNS:  softirqNS,
		Procs:      procs, // finalized, PID-sorted; treat as immutable after push
	}
	c.buf.Push(tick)

	klog.V(5).Infof("bpf: pushed 1 tick with %d procs in %v", len(procs), time.Since(start))

	// If you keep a previous mono, update it now:
	// c.lastMono = mono
}
