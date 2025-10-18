package ring

// SampleMeta is embedded in each metric-specific sample type.
// Mono is Tycho's monotonic event index/tick.

type SampleMeta struct {
	Mono uint64
}

// Example metric-specific sample types. These are just examples to demonstrate
// typed rings; adjust fields as your engine requires.

type BpfSample struct {
	SampleMeta
	CPUCycles    uint64
	CPUInstr     uint64
	CacheMiss    uint64
	PageCacheHit uint64
}

type RaplSample struct {
	SampleMeta
	Package_uJ uint64
	Core_uJ    uint64
	DRAM_uJ    uint64
	Uncore_uJ  uint64
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
