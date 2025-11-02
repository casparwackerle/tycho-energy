package ring

import (
	"time"
)

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

type BpfTick struct {
	SampleMeta // Mono
	IdleNS     uint64
	IRQNS      uint64
	SoftirqNS  uint64

	// Finalized, PID-sorted, read-only payload:
	Procs []BpfProcDelta // sorted by PID (primary), with cgroup fields included
}

type BpfProcDelta struct {
	PID         uint64
	StartTimeNs uint64 // optional but recommended for cross-tick identity
	CgroupID    uint64 // attribute, not key
	// deltas for the tick:
	ProcessRunUs uint64
	PageCacheHit uint64
	IRQNetTX     uint64
	IRQNetRX     uint64
	IRQBlock     uint64
	CPUCycles    uint64
	CPUInstr     uint64
	CacheMiss    uint64
}

type RaplSample struct {
	SampleMeta SampleMeta
	Source     string                     // components.GetSourceName()
	Sockets    map[int]RaplDomainCounters // per-socket raw counters
}

type RedfishSample struct {
	SampleMeta
	ChassisID     string    // e.g., "1" from /redfish/v1/Chassis/<id>
	PowerWatts    float64   // instantaneous power reported by BMC
	Seq           uint64    // increments whenever the BMC exposes a new state
	SourceTime    time.Time // BMC-provided HTTP Date if present; zero if unknown
	CollectorTime time.Time // local wall-clock when the sample was received/pushed
	FreshnessMs   float64   // max(0, CollectorTime - SourceTime) in ms; 0 if SourceTime is zero
}

type GpuTick struct {
	SampleMeta
	Devices   []GpuSample
	Processes []GpuProcSample
}

type GpuProcSample struct {
	Pid               uint32
	TimeStampUS       uint64 // as delivered by backend
	ComputeUtil       uint32 // %
	MemUtil           uint32 // %
	EncUtil           uint32 // %
	DecUtil           uint32 // %
	GpuIndex          int
	GpuUUID           string
	GpuInstanceID     *uint32 // MIG ids if present (backend-specific)
	ComputeInstanceID *uint32
}

type GpuSample struct {
	// --- Identity / topology (backend-agnostic) ---
	DeviceIndex int    // 0..N-1 in current host view
	UUID        string // NVML device UUID (stable identifier)
	PCIBusID    string // e.g., "0000:01:00.0"
	Name        string // Human-readable GPU name (for logs)

	// --- Instantaneous telemetry (per tick) ---
	PowerMilliW     int      // Instant power from NVML (mW)
	CumEnergyMilliJ *uint64  // cumulative energy (if supported)
	SMUtilPct       float64  // GPU core/SM utilization (%); was already present
	MemUtilPct      float64  // Memory controller utilization (%); was already present
	EncUtilPct      *float64 // Optional: NVENC utilization (%) if available
	DecUtilPct      *float64 // Optional: NVDEC utilization (%) if available
	MemUsedBytes    uint64   // FB memory used (bytes)
	MemTotalBytes   uint64   // FB memory total (bytes)
	SMClockMHz      uint32   // SM/graphics clock
	MemClockMHz     uint32   // Memory clock
	TempC           int      // GPU temperature (°C)

	// --- Energy accounting (per tick) ---
	// If the device exposes cumulative energy (mJ) we compute a delta and store it as microJ.
	// Otherwise we integrate instantaneous power across dt (trapezoid) and store that as microJ.
	HasCumulativeEnergy bool // True when cumulative energy is used underneath

	// --- Backend / topology hints (optional) ---
	Backend       string  // "nvml" | "dcgm" (for debugging/telemetry)
	IsMIG         bool    // true if this sample refers to a MIG slice (future use)
	MIGParentID   *int    // physical GPU index for MIG parent (nil when not MIG)
	MIGParentUUID *string // physical GPU UUID for MIG parent (nil when not MIG)
}
