package attribution

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/pkg/utils"
)

// ResolveWorkload resolves the canonical workload aggregation key for a process
// activity record, following Slice 13B / Decision B.
//
// Inputs:
//   - procID: stable process identity if known (nil if identity-unstable)
//   - bpfCgroupID: cgroup hint from BPF delta (may be 0)
//
// Returns:
//   - wk, ok=true iff (namespace,pod,container) resolved.
//   - ok=false means caller must use SystemWorkloadKey().
func ResolveWorkload(
	store *metadata.Store,
	procID *ProcID, // nil if identity-unstable
	bpfCgroupID uint64,
) (wk WorkloadKey, ok bool) {
	if store == nil {
		return WorkloadKey{}, false
	}

	// Step 2: resolve ContainerID (priority: ProcMeta.ContainerID, else cgroup fallback).
	containerID := ""

	// 2.1 ProcID path (preferred): PID + StartJiffies guard against PID reuse.
	if procID != nil && procID.PID != 0 && procID.StartJiffies != 0 {
		if pm, found := store.LookupProc(procID.PID); found && pm != nil {
			// Guard: only trust ProcMeta if StartJiffies matches.
			if pm.StartJiffies == procID.StartJiffies {
				if pm.ContainerID != "" && pm.ContainerID != utils.SystemProcessName {
					containerID = pm.ContainerID
				}
			}
		}
	}

	// 2.2 cgroup-only fallback (Decision B): if still unknown and BPF cgroup exists.
	if containerID == "" && bpfCgroupID != 0 {
		if cid, ok := store.LookupContainerIDByCgroupID(bpfCgroupID); ok {
			if cid != "" && cid != utils.SystemProcessName {
				containerID = cid
			}
		}
	}

	// If we still have no container identity, we must fall back to system bucket.
	if containerID == "" || containerID == utils.SystemProcessName {
		return WorkloadKey{}, false
	}

	// Step 3: resolve Pod identity via ContainerMeta.
	cm, found := store.LookupContainer(containerID)
	if !found || cm == nil {
		return WorkloadKey{}, false
	}

	// Extract canonical workload labels.
	if cm.Namespace == "" || cm.PodName == "" || cm.ContainerName == "" {
		return WorkloadKey{}, false
	}

	return WorkloadKey{
		Namespace: cm.Namespace,
		Pod:       cm.PodName,
		Container: cm.ContainerName,
	}, true
}
