package gpuCollector

import (
	"context"
	"time"
)

// Caps reports backend feature availability discovered at init time.
type Caps struct {
	HasCumulativeEnergy bool // device exposes a cumulative energy counter
	HasPerProcess       bool // per-PID GPU metrics supported
	HasMIGHierarchy     bool // backend can enumerate MIG hierarchy/parents
	BackendName         string
}

// Static, backend-agnostic device metadata.
type DeviceMeta struct {
	Index    int
	UUID     string
	Name     string
	PCIBusID string
}

// Instantaneous per-device snapshot for a single tick.
// All values are RAW as returned by the backend (no analysis/ratios).
type DeviceSnap struct {
	Index int

	// Instantaneous telemetry
	PowerMilliW   int
	SMUtilPct     float64
	MemUtilPct    float64
	EncUtilPct    *float64
	DecUtilPct    *float64
	MemUsedBytes  uint64
	MemTotalBytes uint64
	SMClockMHz    uint32
	MemClockMHz   uint32
	TempC         int

	// Optional raw cumulative energy counter (mJ) if supported by the device.
	// If nil, the backend does not provide it (or not for this device).
	CumEnergyMilliJ *uint64

	// Backend/topology hints (best-effort, no ratios or analysis)
	BackendName string // e.g. "nvml" or "dcgm"
	IsMIG       bool
	ParentIndex *int    // physical GPU index if MIG
	ParentUUID  *string // physical GPU UUID if MIG
	// Optional MIG descriptors; present only if the backend can provide them.
	MIGProfileID *int    // backend-specific profile ID (e.g., NVML/DCGM)
	MIGSMCount   *uint32 // number of SMs in this MIG instance
}

// Optional per-process snapshot as provided by the backend.
// All fields are raw; no normalization (e.g., MIG SM ratios) happens here.
type ProcSnap struct {
	Pid               uint32
	TimeStampUS       uint64 // microsecond timestamp from backend if available
	ComputeUtil       uint32 // %
	MemUtil           uint32 // %
	EncUtil           uint32 // %
	DecUtil           uint32 // %
	GpuIndex          int
	GpuUUID           string
	GpuInstanceID     *uint32 // MIG GPU instance id, if provided by backend
	ComputeInstanceID *uint32 // MIG compute instance id, if provided by backend
}

// Optional MIG snapshot for hierarchy discovery (best-effort).
// This is useful if a backend exposes hierarchy separately from device snaps.
type MigSnap struct {
	DeviceIndex int
	IsMIG       bool
	ParentIndex *int
	ParentUUID  *string
	ProfileID   *int
	SMCount     *uint32
}

// GpuBackend is the minimal capability-driven interface a backend must implement.
// Implementations MUST return raw values only; NO analysis or normalization.
type GpuBackend interface {
	// Init prepares the backend (probe features, open handles, watchers, etc).
	Init(ctx context.Context) error

	// Shutdown releases resources/handles and stops any watchers/timers.
	Shutdown() error

	// Capabilities returns probed feature flags and a human-readable backend name.
	Capabilities() Caps

	// Enumerate returns static device metadata for stable indexing/identity.
	Enumerate(ctx context.Context) ([]DeviceMeta, error)

	// SnapshotDevices returns instantaneous raw per-device metrics for the current tick.
	// The collector will call this once per tick and push the results into the ring.
	SnapshotDevices(ctx context.Context) ([]DeviceSnap, error)

	// SnapshotPerProcess returns raw per-PID metrics (if supported) over the given window.
	// Backends should prefer native "since" APIs when available and otherwise
	// provide the best-effort raw data they can. Return ok=false if unsupported.
	SnapshotPerProcess(ctx context.Context, since time.Duration) (out map[uint32]ProcSnap, ok bool)

	// SnapshotMIG returns best-effort MIG hierarchy information if available.
	// Return ok=false if unsupported or not applicable.
	SnapshotMIG(ctx context.Context) (out []MigSnap, ok bool)
}
