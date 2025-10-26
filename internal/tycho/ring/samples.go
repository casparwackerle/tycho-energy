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
	ProcessRunUs uint64 // micro-s from kernel; convert later if needed
	PageCacheHit uint64
	IRQNetTX     uint64
	IRQNetRX     uint64
	IRQBlock     uint64

	// Hardware counters (deltas) – only populated if enabled
	CPUCycles uint64
	CPUInstr  uint64
	CacheMiss uint64

	// NEW: per-tick CPU bin totals (in nanoseconds)
	IdleNS    uint64
	IRQNS     uint64
	SoftirqNS uint64
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
	// Common sample metadata (Tycho monotonic timestamp)
	SampleMeta

	// --- Identity / topology (backend-agnostic) ---
	DeviceIndex int    // 0..N-1 in current host view
	UUID        string // NVML device UUID (stable identifier)
	PCIBusID    string // e.g., "0000:01:00.0"
	Name        string // Human-readable GPU name (for logs)

	// --- Instantaneous telemetry (per tick) ---
	PowerMilliW   int      // Instant power from NVML (mW)
	SMUtilPct     float64  // GPU core/SM utilization (%); was already present
	MemUtilPct    float64  // Memory controller utilization (%); was already present
	EncUtilPct    *float64 // Optional: NVENC utilization (%) if available
	DecUtilPct    *float64 // Optional: NVDEC utilization (%) if available
	MemUsedBytes  uint64   // FB memory used (bytes)
	MemTotalBytes uint64   // FB memory total (bytes)
	SMClockMHz    uint32   // SM/graphics clock
	MemClockMHz   uint32   // Memory clock
	TempC         int      // GPU temperature (°C)

	// --- Energy accounting (per tick) ---
	// If the device exposes cumulative energy (mJ) we compute a delta and store it as microJ.
	// Otherwise we integrate instantaneous power across dt (trapezoid) and store that as microJ.
	EnergyMicroJ        uint64 // Energy delta for this tick (microJ)
	HasCumulativeEnergy bool   // True when cumulative energy is used underneath

	// --- Backend / topology hints (optional) ---
	Backend       string  // "nvml" | "dcgm" (for debugging/telemetry)
	IsMIG         bool    // true if this sample refers to a MIG slice (future use)
	MIGParentID   *int    // physical GPU index for MIG parent (nil when not MIG)
	MIGParentUUID *string // physical GPU UUID for MIG parent (nil when not MIG)
}
