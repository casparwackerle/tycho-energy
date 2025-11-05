package calibration

// SocketID identifies a physical CPU socket (0..N-1).
type SocketID int

// Domain identifies a RAPL domain we track for idle baselines.
type Domain string

const (
	DomainPkg    Domain = "pkg"
	DomainCore   Domain = "core"
	DomainUncore Domain = "uncore"
	DomainDRAM   Domain = "dram"
)

// Optional: helper for iteration where needed (not used by store directly).
var AllDomains = []Domain{DomainPkg, DomainCore, DomainUncore, DomainDRAM}

/* =========================
   Orchestration primitives
   ========================= */

// SensorMask selects which sensors to calibrate in an orchestrated run.
type SensorMask uint8

const (
	MaskNone    SensorMask = 0
	MaskRAPL    SensorMask = 1 << 0
	MaskRedfish SensorMask = 1 << 1
	MaskGPU     SensorMask = 1 << 2
	MaskAll                = MaskRAPL | MaskRedfish | MaskGPU
)

// IdleMode indicates which policy the orchestrator used.
type IdleMode string

const (
	ModeGroundTruth         IdleMode = "ground_truth"         // full budgeted window; replaces baselines
	ModeOpportunisticRefine IdleMode = "opportunistic_refine" // current window; downward-only refine
)

/* =========================
   Results & baselines
   ========================= */

type Results struct {
	// Mode used by orchestrator (optional; filled by orchestrator).
	Mode IdleMode

	// GPU
	GpuBestPollMS *int
	GpuDelayMS    *int
	GpuIdleP5     *float64

	GpuIdlePerDevice map[string]float64

	// Redfish
	RedfishBestPollMS *int
	RedfishDelayMS    *int
	RedfishIdleP5     *float64

	// RAPL
	RaplDelayMS *int
	RaplIdleP5  *float64

	RAPLIdle IdleBaselines
	Redfish  *PollProbeReport
	GPU      *PollProbeReport

	// Diagnostics
	Notes  map[string]string
	Status map[string]string
}

type IdleBaseline struct {
	P5     float64 // robust idle estimate (preferred)
	Min    float64 // raw min (for reference)
	N      int     // samples considered after de-spiking
	FromTs uint64  // mono.Nano() or similar
}

type IdleBaselines map[SocketID]map[Domain]IdleBaseline

type PollProbePoint struct {
	Ms      int
	Ok      bool
	Samples int
	Errs    int
	EffRate float64 // effective arrival rate [0..1] relative to requested
	Notes   string  // short reason on fail/quirk
}

type PollProbeReport struct {
	Component   string // "redfish" | "gpu"
	CandidateMs int    // chosen
	MinGuardMs  int
	Path        []PollProbePoint // search path (ordered)
	WindowSec   int
	EarlyStop   bool
	Reason      string // why we stopped where we did
}
