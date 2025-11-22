package metadata

import "time"

// ProcMeta holds process-level metadata for attribution.
type ProcMeta struct {
	PID          uint64 // OS PID
	StartJiffies uint64 // kernel start time, for disambiguating PID reuse
	CgroupID     uint64 // optional; 0 if unknown
	ContainerID  string // normalized container ID, or system sentinel for non-K8s
	Command      string // best-effort command name, for debugging/inspection

	// Last seen timestamps:
	// - Mono: Tycho's shared monotonic timebase (same domain as power/utilization).
	// - Wall: wall-clock time, used for GC based on durations.
	LastSeenMono uint64
	LastSeenWall time.Time
}

// ContainerState represents the lifecycle state of a container as seen via kubelet.
type ContainerState string

const (
	ContainerStateUnknown    ContainerState = "Unknown"
	ContainerStateRunning    ContainerState = "Running"
	ContainerStateTerminated ContainerState = "Terminated"
	ContainerStateWaiting    ContainerState = "Waiting"
)

// PodPhase mirrors Kubernetes pod phase semantics at a coarse level.
type PodPhase string

const (
	PodPhaseUnknown   PodPhase = "Unknown"
	PodPhasePending   PodPhase = "Pending"
	PodPhaseRunning   PodPhase = "Running"
	PodPhaseSucceeded PodPhase = "Succeeded"
	PodPhaseFailed    PodPhase = "Failed"
)

// ContainerMeta holds container-level metadata as derived from kubelet and cgroup information.
type ContainerMeta struct {
	ContainerID   string
	ContainerName string

	PodUID    string
	PodName   string
	Namespace string

	// Coarse pod phase this container belongs to, as seen during the last kubelet scan.
	Phase PodPhase

	// Lifecycle state of this container (Running / Waiting / Terminated / Unknown).
	State ContainerState

	// ExitCode is set when the container is in a terminated state and kubelet
	// reported a numeric exit code. Nil means "no code reported / not terminated".
	ExitCode *int

	LastSeenMono uint64
	LastSeenWall time.Time

	RequestsCPUMillis int64
	RequestsMemBytes  int64
	LimitsCPUMillis   int64
	LimitsMemBytes    int64

	// Optional context fields; fill or ignore as needed.
	QoSClass    string
	Labels      map[string]string
	Annotations map[string]string
}

// PodMeta holds pod-level metadata as obtained from kubelet/Kubernetes.
type PodMeta struct {
	PodUID    string
	PodName   string
	Namespace string

	Phase    PodPhase
	NodeName string
	QoSClass string

	OwnerKind string
	OwnerName string

	LastSeenMono uint64
	LastSeenWall time.Time

	RequestsCPUMillis int64
	RequestsMemBytes  int64
	LimitsCPUMillis   int64
	LimitsMemBytes    int64

	// Optional: selected labels.
	Labels map[string]string
}

// LookupResult encapsulates metadata lookups for a process.
// It avoids exposing internal store maps directly to the analysis layer.
type LookupResult struct {
	Process   *ProcMeta
	Container *ContainerMeta
	Pod       *PodMeta
}
