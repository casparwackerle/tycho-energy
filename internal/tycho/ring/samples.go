package ring

// SampleMeta is embedded in each metric-specific sample type.
// Mono is Tycho's monotonic event index/tick.

type SampleMeta struct {
	Mono uint64
}

type RaplDomainCounters struct {
	Pkg    uint64 // mJ raw counter (monotonic, wraps)
	Core   uint64 // mJ
	Uncore uint64 // mJ
	DRAM   uint64 // mJ
}

// Example metric-specific sample types. These are just examples to demonstrate
// typed rings; adjust fields as your engine requires.

type BpfSample struct {
	SampleMeta
	Pid      uint64
	CgroupID uint64

	// Software counters (deltas)
	ProcessRunUs uint64 // µs from kernel; convert later if needed
	PageCacheHit uint64
	IRQNetTX     uint64
	IRQNetRX     uint64
	IRQBlock     uint64

	// Hardware counters (deltas) – only populated if enabled
	CPUCycles uint64
	CPUInstr  uint64
	CacheMiss uint64
}

type RaplSample struct {
	SampleMeta SampleMeta
	Source     string                     // components.GetSourceName()
	Sockets    map[int]RaplDomainCounters // per-socket raw counters
}

type RedfishSample struct {
	SampleMeta
	PowerWatts float64
}

type GpuSample struct {
	SampleMeta
	SMUtilPct  float64
	MemUtilPct float64
}
