//go:build !darwin
// +build !darwin

/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package bpf

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/jaypipes/ghw"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type exporter struct {
	bpfObjects tychoObjects

	schedSwitchLink  link.Link
	irqEntryLink     link.Link
	irqExitLink      link.Link
	softirqEntryLink link.Link
	softirqExitLink  link.Link
	irqLink          link.Link
	pageWriteLink    link.Link
	pageReadLink     link.Link

	perfEvents *hardwarePerfEvents

	enabledHardwareCounters sets.Set[string]
	enabledSoftwareCounters sets.Set[string]
}

// CPUBinCounters mirrors the BPF struct cpu_bin_counters { u64 idle_ns; u64 irq_ns; u64 softirq_ns; }.
type CPUBinCounters struct {
	IdleNS    uint64
	IRQNS     uint64
	SoftirqNS uint64
}

func NewExporter() (Exporter, error) {
	e := &exporter{
		enabledHardwareCounters: sets.New[string](config.BPFHwCounters()...),
		enabledSoftwareCounters: sets.New[string](config.BPFSwCounters()...),
	}
	err := e.attach()
	if err != nil {
		e.Detach()
	}
	return e, err
}

func (e *exporter) SupportedMetrics() SupportedMetrics {
	return SupportedMetrics{
		HardwareCounters: e.enabledHardwareCounters.Clone(),
		SoftwareCounters: e.enabledSoftwareCounters.Clone(),
	}
}

func (e *exporter) attach() error {
	// Remove resource limits for kernels <5.11.
	if err := rlimit.RemoveMemlock(); err != nil {
		return fmt.Errorf("error removing memlock: %v", err)
	}

	// Load eBPF Specs
	specs, err := loadTycho()
	if err != nil {
		return fmt.Errorf("error loading eBPF specs: %v", err)
	}

	// Adjust map sizes to the number of available CPUs
	numCPU := getCPUCores()
	klog.Infof("Number of CPUs: %d", numCPU)
	for _, m := range specs.Maps {
		// Only resize maps that have a MaxEntries of NUM_CPUS constant
		if m.MaxEntries == 128 {
			m.MaxEntries = uint32(numCPU)
		}
	}

	// Set program global variables
	err = specs.RewriteConstants(map[string]interface{}{
		"SAMPLE_RATE": int32(config.GetBPFSampleRate()),
	})
	if err != nil {
		return fmt.Errorf("error rewriting program constants: %v", err)
	}

	// Load the eBPF program(s)
	if err := specs.LoadAndAssign(&e.bpfObjects, nil); err != nil {
		return fmt.Errorf("error loading eBPF objects: %v", err)
	}

	// Attach the eBPF program(s)
	e.schedSwitchLink, err = link.AttachTracing(link.TracingOptions{
		Program:    e.bpfObjects.KeplerSchedSwitchTrace,
		AttachType: ebpf.AttachTraceRawTp,
	})
	if err != nil {
		return fmt.Errorf("error attaching sched_switch tracepoint: %v", err)
	}

	if config.ExposeIRQCounterMetrics() {
		// SoftIRQ entry/exit (vector counting + time attribution)
		e.softirqEntryLink, err = link.AttachTracing(link.TracingOptions{
			Program:    e.bpfObjects.KeplerSoftirqEntry,
			AttachType: ebpf.AttachTraceRawTp,
		})
		if err != nil {
			return fmt.Errorf("could not attach tp_btf/softirq_entry: %w", err)
		}

		e.softirqExitLink, err = link.AttachTracing(link.TracingOptions{
			Program:    e.bpfObjects.KeplerSoftirqExit,
			AttachType: ebpf.AttachTraceRawTp,
		})
		if err != nil {
			return fmt.Errorf("could not attach tp_btf/softirq_exit: %w", err)
		}

		// Hard IRQ entry/exit (time attribution)
		e.irqEntryLink, err = link.AttachTracing(link.TracingOptions{
			Program:    e.bpfObjects.KeplerIrqEntry,
			AttachType: ebpf.AttachTraceRawTp,
		})
		if err != nil {
			return fmt.Errorf("could not attach tp_btf/irq_handler_entry: %w", err)
		}

		e.irqExitLink, err = link.AttachTracing(link.TracingOptions{
			Program:    e.bpfObjects.KeplerIrqExit,
			AttachType: ebpf.AttachTraceRawTp,
		})
		if err != nil {
			return fmt.Errorf("could not attach tp_btf/irq_handler_exit: %w", err)
		}
	}

	group := "writeback"
	name := "writeback_dirty_page"
	if _, err := os.Stat(config.SysDir() + "/kernel/debug/tracing/events/writeback/writeback_dirty_folio"); err == nil {
		name = "writeback_dirty_folio"
	}
	e.pageWriteLink, err = link.Tracepoint(group, name, e.bpfObjects.KeplerWritePageTrace, nil)
	if err != nil {
		klog.Warningf("failed to attach tp/%s/%s: %v. Kepler will not collect page cache write events. This will affect the DRAM power model estimation on VMs.", group, name, err)
	} else {
		e.enabledSoftwareCounters[config.PageCacheHit] = struct{}{}
	}

	e.pageReadLink, err = link.AttachTracing(link.TracingOptions{
		Program:    e.bpfObjects.KeplerReadPageTrace,
		AttachType: ebpf.AttachTraceFEntry,
	})
	if err != nil {
		klog.Warningf("failed to attach fentry/mark_page_accessed: %v. Kepler will not collect page cache read events. This will affect the DRAM power model estimation on VMs.", err)
	}

	// Return early if hardware counters are not enabled
	if !config.ExposeHardwareCounterMetrics() {
		klog.Infof("Hardware counter metrics are disabled")
		return nil
	}

	e.perfEvents, err = createHardwarePerfEvents(
		e.bpfObjects.CpuInstructionsEventReader,
		e.bpfObjects.CpuCyclesEventReader,
		e.bpfObjects.CacheMissEventReader,
		numCPU,
	)
	if err != nil {
		return err
	}

	return nil
}

func (e *exporter) Detach() {
	// Links
	if e.schedSwitchLink != nil {
		e.schedSwitchLink.Close()
		e.schedSwitchLink = nil
	}

	if e.irqLink != nil {
		e.irqLink.Close()
		e.irqLink = nil
	}

	if e.irqEntryLink != nil {
		e.irqEntryLink.Close()
		e.irqEntryLink = nil
	}

	if e.irqExitLink != nil {
		e.irqExitLink.Close()
		e.irqExitLink = nil
	}

	if e.softirqEntryLink != nil {
		e.softirqEntryLink.Close()
		e.softirqEntryLink = nil
	}

	if e.softirqExitLink != nil {
		e.softirqExitLink.Close()
		e.softirqExitLink = nil
	}

	if e.pageWriteLink != nil {
		e.pageWriteLink.Close()
		e.pageWriteLink = nil
	}

	if e.pageReadLink != nil {
		e.pageReadLink.Close()
		e.pageReadLink = nil
	}

	// Perf events
	if e.perfEvents != nil {
		e.perfEvents.close()
		e.perfEvents = nil
	}

	// Objects
	e.bpfObjects.Close()
}

func (e *exporter) CollectProcesses() ([]ProcessMetrics, error) {
	// Snapshot the current content of the map (batched), then reset only the counter fields.
	// This avoids the sched_switch "missing entry" delta-drop when userspace deletes keys.

	maxEntries := e.bpfObjects.Processes.MaxEntries()
	if maxEntries == 0 {
		return nil, nil
	}

	keys := make([]uint32, maxEntries)
	vals := make([]ProcessMetrics, maxEntries)

	var cursor ebpf.MapBatchCursor
	total := 0

	for {
		n, err := e.bpfObjects.Processes.BatchLookup(
			&cursor,
			keys,
			vals,
			&ebpf.BatchOptions{},
		)
		total += n

		// BatchLookup returns ErrKeyNotExist when the cursor reaches the end.
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to batch lookup processes: %v", err)
		}

		// Continue until ErrKeyNotExist.
		if n == 0 {
			// Defensive: avoid tight loop on unexpected "0, nil".
			break
		}
	}

	if total == 0 {
		return nil, nil
	}

	// Reset counters for the keys we actually read.
	//
	// IMPORTANT: keep identity fields, only reset "delta" fields:
	// - ProcessRunTime, CpuCycles, CpuInstr, CacheMiss, PageCacheHit, VecNr
	//
	// We do this per-key Update for correctness and simplicity.
	// If you later want to optimize, we can switch to BatchUpdate (if supported in your ebpf lib version).
	for i := 0; i < total; i++ {
		k := keys[i]
		v := vals[i]

		// Keep identity. Zero everything else.
		reset := v
		reset.ProcessRunTime = 0
		reset.CpuCycles = 0
		reset.CpuInstr = 0
		reset.CacheMiss = 0
		reset.PageCacheHit = 0
		reset.VecNr = [10]uint16{}

		// // Write back the reset value. Use UpdateAny to handle concurrent existence reliably.
		if err := e.bpfObjects.Processes.Update(k, reset, ebpf.UpdateAny); err != nil {
			// This can happen if the key was evicted/replaced between lookup and update (LRU map),
			// or if the entry got deleted for other reasons.
			// We treat it as non-fatal, because the next tick will re-sync.
			//klog.V(4).Infof("[bpf] reset processes[%d] failed: %v", k, err)
		}
	}

	// Return a compact slice of the snapshot we took.
	// NOTE: this includes entries that may be "all zeros"; your caller can filter if desired.
	out := make([]ProcessMetrics, total)
	copy(out, vals[:total])
	return out, nil
}

///////////////////////////////////////////////////////////////////////////
// utility functions

// CollectCPUBins reads & resets the per-CPU bin counters (idle/irq/softirq) for the current window.
func (e *exporter) CollectCPUBins() (total CPUBinCounters, perCPU []CPUBinCounters, err error) {
	key := uint32(0)

	numCPU := getCPUCores()
	perCPU = make([]CPUBinCounters, numCPU)

	// Lookup the per-CPU values for key=0 from the PERCPU_ARRAY map.
	// For per-CPU maps, cilium/ebpf expects a slice of length == #CPUs.
	if err = e.bpfObjects.CpuBins.Lookup(key, &perCPU); err != nil {
		return CPUBinCounters{}, nil, fmt.Errorf("lookup cpu_bins failed: %w", err)
	}

	// Sum totals across CPUs.
	for i := 0; i < numCPU; i++ {
		total.IdleNS += perCPU[i].IdleNS
		total.IRQNS += perCPU[i].IRQNS
		total.SoftirqNS += perCPU[i].SoftirqNS
	}

	// Zero the per-CPU bin so the next collection window starts fresh.
	zero := make([]CPUBinCounters, numCPU) // zero-initialized
	if err = e.bpfObjects.CpuBins.Update(key, zero, ebpf.UpdateAny); err != nil {
		return CPUBinCounters{}, nil, fmt.Errorf("reset cpu_bins failed: %w", err)
	}

	return total, perCPU, nil
}

func unixOpenPerfEvent(typ, conf, cpuCores int) ([]int, error) {
	sysAttr := &unix.PerfEventAttr{
		Type:   uint32(typ),
		Size:   uint32(unsafe.Sizeof(unix.PerfEventAttr{})),
		Config: uint64(conf),
	}
	fds := []int{}
	for i := 0; i < cpuCores; i++ {
		cloexecFlags := unix.PERF_FLAG_FD_CLOEXEC
		fd, err := unix.PerfEventOpen(sysAttr, -1, i, -1, cloexecFlags)
		if fd < 0 {
			return nil, fmt.Errorf("failed to open bpf perf event on cpu %d: %w", i, err)
		}

		if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_RESET, 0); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("perf reset failed on cpu %d: %w", i, err)
		}
		if err := unix.IoctlSetInt(fd, unix.PERF_EVENT_IOC_ENABLE, 0); err != nil {
			unix.Close(fd)
			return nil, fmt.Errorf("perf enable failed on cpu %d: %w", i, err)
		}

		fds = append(fds, fd)
	}
	return fds, nil
}

func unixClosePerfEvents(fds []int) {
	for _, fd := range fds {
		_ = unix.SetNonblock(fd, true)
		unix.Close(fd)
	}
}

func getCPUCores() int {
	cores := runtime.NumCPU()
	if cpu, err := ghw.CPU(ghw.WithDisableWarnings()); err == nil && cpu != nil {
		if cpu.TotalThreads > 0 {
			cores = int(cpu.TotalThreads)
		}
	}
	return cores
}

type hardwarePerfEvents struct {
	cpuCyclesPerfEvents       []int
	cpuInstructionsPerfEvents []int
	cacheMissPerfEvents       []int
}

func (h *hardwarePerfEvents) close() {
	if h == nil {
		return
	}
	unixClosePerfEvents(h.cpuCyclesPerfEvents)
	unixClosePerfEvents(h.cpuInstructionsPerfEvents)
	unixClosePerfEvents(h.cacheMissPerfEvents)
}

// CreateHardwarePerfEvents creates perf events for CPU cycles, CPU instructions, and cache misses
// and updates the corresponding eBPF maps.
func createHardwarePerfEvents(cpuInstructionsMap, cpuCyclesMap, cacheMissMap *ebpf.Map, numCPU int) (*hardwarePerfEvents, error) {
	var err error
	events := &hardwarePerfEvents{
		cpuCyclesPerfEvents:       []int{},
		cpuInstructionsPerfEvents: []int{},
		cacheMissPerfEvents:       []int{},
	}
	defer func() {
		if err != nil {
			unixClosePerfEvents(events.cpuCyclesPerfEvents)
			unixClosePerfEvents(events.cpuInstructionsPerfEvents)
			unixClosePerfEvents(events.cacheMissPerfEvents)
		}
	}()

	// Create perf events and update each eBPF map
	events.cpuCyclesPerfEvents, err = unixOpenPerfEvent(unix.PERF_TYPE_HARDWARE, unix.PERF_COUNT_HW_CPU_CYCLES, numCPU)
	if err != nil {
		klog.Warning("Failed to open perf event for CPU cycles: ", err)
		return nil, err
	}

	events.cpuInstructionsPerfEvents, err = unixOpenPerfEvent(unix.PERF_TYPE_HARDWARE, unix.PERF_COUNT_HW_INSTRUCTIONS, numCPU)
	if err != nil {
		klog.Warning("Failed to open perf event for CPU instructions: ", err)
		return nil, err
	}

	events.cacheMissPerfEvents, err = unixOpenPerfEvent(unix.PERF_TYPE_HW_CACHE, unix.PERF_COUNT_HW_CACHE_MISSES, numCPU)
	if err != nil {
		klog.Warning("Failed to open perf event for cache misses: ", err)
		return nil, err
	}

	for i, fd := range events.cpuCyclesPerfEvents {
		if err = cpuCyclesMap.Update(uint32(i), uint32(fd), ebpf.UpdateAny); err != nil {
			klog.Warningf("Failed to update cpu_cycles_event_reader map: %v", err)
			return nil, err
		}
	}
	for i, fd := range events.cpuInstructionsPerfEvents {
		if err = cpuInstructionsMap.Update(uint32(i), uint32(fd), ebpf.UpdateAny); err != nil {
			klog.Warningf("Failed to update cpu_instructions_event_reader map: %v", err)
			return nil, err
		}
	}
	for i, fd := range events.cacheMissPerfEvents {
		if err = cacheMissMap.Update(uint32(i), uint32(fd), ebpf.UpdateAny); err != nil {
			klog.Warningf("Failed to update cache_miss_event_reader map: %v", err)
			return nil, err
		}
	}
	return events, nil
}

// // DebugPerfReadErrors logs cumulative perf_event_read errors from BPF.
// // Call this occasionally (e.g., once per second), not per tick.
// func (e *exporter) DebugPerfReadErrors() {
// 	if e == nil {
// 		return
// 	}
// 	// Defensive: maps can be nil if object load/assign didn't include them.
// 	if e.bpfObjects.PerfReadErrCycles == nil || e.bpfObjects.PerfReadErrInstr == nil || e.bpfObjects.PerfReadErrMiss == nil {
// 		klog.Warningf("[bpf][pmu] error maps missing: cycles=%v instr=%v miss=%v",
// 			e.bpfObjects.PerfReadErrCycles != nil,
// 			e.bpfObjects.PerfReadErrInstr != nil,
// 			e.bpfObjects.PerfReadErrMiss != nil,
// 		)
// 		return
// 	}

// 	sumPerCPUArray0 := func(m *ebpf.Map) uint64 {
// 		if m == nil {
// 			return 0
// 		}

// 		numCPU := getCPUCores()
// 		if numCPU <= 0 {
// 			numCPU = runtime.NumCPU()
// 		}
// 		if numCPU <= 0 {
// 			numCPU = 1
// 		}

// 		key := uint32(0)
// 		vals := make([]uint64, numCPU) // per-cpu values

// 		if err := m.Lookup(key, &vals); err != nil {
// 			klog.Warningf("[bpf][pmu] lookup percpu err map failed: %v", err)
// 			return 0
// 		}

// 		var total uint64
// 		for i := 0; i < len(vals); i++ {
// 			total += vals[i]
// 		}
// 		return total
// 	}

// 	errCycles := sumPerCPUArray0(e.bpfObjects.PerfReadErrCycles)
// 	errInstr := sumPerCPUArray0(e.bpfObjects.PerfReadErrInstr)
// 	errMiss := sumPerCPUArray0(e.bpfObjects.PerfReadErrMiss)

// 	klog.Infof("[bpf][pmu] perf_event_read errors: cycles=%d instr=%d miss=%d", errCycles, errInstr, errMiss)

// }

// // DebugDumpPMURaw dumps a few entries from the raw per-CPU ARRAY maps that store
// // the last read absolute PMU counter values (not deltas). Call e.g. once/sec.
// func (e *exporter) DebugDumpPMURaw() {
// 	if e == nil {
// 		return
// 	}
// 	if e.bpfObjects.CpuInstructions == nil || e.bpfObjects.CpuCycles == nil {
// 		klog.Warningf("[bpf][pmu] raw maps missing: instr=%v cycles=%v",
// 			e.bpfObjects.CpuInstructions != nil,
// 			e.bpfObjects.CpuCycles != nil,
// 		)
// 		return
// 	}

// 	numCPU := getCPUCores()
// 	if numCPU <= 0 {
// 		numCPU = runtime.NumCPU()
// 	}
// 	if numCPU <= 0 {
// 		numCPU = 1
// 	}

// 	// Print only first few CPUs to keep logs readable.
// 	limit := 8
// 	if numCPU < limit {
// 		limit = numCPU
// 	}

// 	readU64 := func(m *ebpf.Map, cpu int) uint64 {
// 		var v uint64
// 		k := uint32(cpu)
// 		if err := m.Lookup(k, &v); err != nil {
// 			klog.Warningf("[bpf][pmu] lookup raw map failed cpu=%d: %v", cpu, err)
// 			return 0
// 		}
// 		return v
// 	}

// 	msg := "[bpf][pmu] raw last counters:"
// 	for cpu := 0; cpu < limit; cpu++ {
// 		instr := readU64(e.bpfObjects.CpuInstructions, cpu)
// 		cyc := readU64(e.bpfObjects.CpuCycles, cpu)
// 		msg += fmt.Sprintf(" cpu%d(instr=%d cycles=%d)", cpu, instr, cyc)
// 	}
// 	klog.Info(msg)
// }
