package attribution

import (
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/utils"
)

// BuildCgroupIndexFromBpfWindow refreshes the store's cgroupID->containerID index
// using the current BPF window plus existing ProcMeta(pid->containerID) facts.
//
// Join logic:
//   - For each (pid,cgroupID) observed in the BPF window,
//   - Look up ProcMeta(pid). If it has a non-system ContainerID,
//   - Upsert mapping (cgroupID -> containerID).
//
// This is intentionally called once per analysis window (not per eBPF tick).
func BuildCgroupIndexFromBpfWindow(
	store *metadata.Store,
	nowWall time.Time,
	nowMono uint64,
	ticks []ring.BpfTick,
) {
	if store == nil || len(ticks) == 0 {
		return
	}

	// De-dup by PID for the window. Last cgroupID wins (usually stable anyway).
	pidToCgid := make(map[uint64]uint64, 1024)
	for i := range ticks {
		t := &ticks[i]
		for j := range t.Procs {
			p := &t.Procs[j]
			if p.PID == 0 || p.CgroupID == 0 {
				continue
			}
			pidToCgid[p.PID] = p.CgroupID
		}
	}

	// Join: PID -> ContainerID (from proc scan) and store cgroup mapping.
	for pid, cgid := range pidToCgid {
		pm, ok := store.LookupProc(pid)
		if !ok || pm == nil {
			continue
		}
		if pm.ContainerID == "" || pm.ContainerID == utils.SystemProcessName {
			continue
		}
		store.UpsertCgroupMapping(cgid, pm.ContainerID, nowMono, nowWall)
	}
}
