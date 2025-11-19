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

/* =========================
   Results & baselines
   ========================= */

type Results struct {
	// Mode used by orchestrator (optional; filled by orchestrator).

	// GPU
	GpuBestPollMS *int

	GpuIdlePerDevice map[string]float64

	// Redfish
	RedfishBestPollMS *int

	Redfish *PollProbeReport
	GPU     *PollProbeReport

	// Diagnostics
	Notes  map[string]string
	Status map[string]string
}

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
