package attribution

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/pkg/utils"
)

// ResolveWorkload resolves the canonical workload aggregation key for a process activity record.
func ResolveWorkload(
	store *metadata.Store,
	procID *ProcID, // nil if identity-unstable
	bpfCgroupID uint64,
) (wk WorkloadKey, ok bool) {
	if store == nil {
		return WorkloadKey{}, false
	}

	containerID := ""

	// ---------------------------------------------------------------------
	// 1) ProcID path (preferred): PID + StartJiffies guard against PID reuse
	// ---------------------------------------------------------------------
	if procID != nil && procID.PID != 0 && procID.StartJiffies != 0 {
		if pm, found := store.LookupProc(procID.PID); found && pm != nil {
			// Only trust ProcMeta if StartJiffies matches (PID reuse hardening)
			if pm.StartJiffies == procID.StartJiffies {
				if pm.ContainerID != "" && pm.ContainerID != utils.SystemProcessName {
					containerID = pm.ContainerID
				}
			}
		}
	}

	// ---------------------------------------------------------------------
	// 2) Cgroup fallback (Decision B), guarded against root/sentinel IDs
	// ---------------------------------------------------------------------
	if containerID == "" && IsAttributableCgroupID(bpfCgroupID) {
		if cid, ok := store.LookupContainerIDByCgroupID(bpfCgroupID); ok {
			if cid != "" && cid != utils.SystemProcessName {
				containerID = cid
			}
		}
	}

	// ---------------------------------------------------------------------
	// 3) Resolve container -> workload labels
	// ---------------------------------------------------------------------
	if containerID == "" || containerID == utils.SystemProcessName {
		return WorkloadKey{}, false
	}

	cm, found := store.LookupContainer(containerID)
	if !found || cm == nil {
		return WorkloadKey{}, false
	}

	if cm.Namespace == "" || cm.PodName == "" || cm.ContainerName == "" {
		return WorkloadKey{}, false
	}

	return WorkloadKey{
		Namespace: cm.Namespace,
		Pod:       cm.PodName,
		Container: cm.ContainerName,
	}, true
}
