package attribution

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
)

// ResolveProc resolves a PID into a stable process identity (PID, StartJiffies)
// and returns the associated cgroup ID when known.
//
// Semantics (Slice 13A):
// - ok=true iff StartJiffies is known (non-zero) and the proc exists in metadata.
// - ok=false means ProcID is not safe; caller should pass procID=nil and rely on cgroup fallback if available; only if workload cannot be resolved, bucket to system.
// - cgroupID may be 0 even when ok=true (mapping chain work is in later slices).
func ResolveProc(store *metadata.Store, pid uint64) (procID ProcID, cgroupID uint64, ok bool) {
	if store == nil || pid == 0 {
		return ProcID{}, 0, false
	}

	pm, found := store.LookupProc(pid)
	if !found || pm == nil {
		return ProcID{}, 0, false
	}

	// ok=true iff StartJiffies is known; caller may still resolve via cgroup fallback when ok=false.
	if pm.StartJiffies == 0 {
		return ProcID{}, 0, false
	}

	// Use the lookup PID to avoid any inconsistency if pm.PID ever differs.
	return ProcID{PID: pid, StartJiffies: pm.StartJiffies}, pm.CgroupID, true
}

// ResolveProcFromCycle resolves PID identity using the Cycle's metadata store.
// This keeps attribution call sites uniform and prevents accidental nil-store use.
func ResolveProcFromCycle(c *analysis.Cycle, pid uint64) (ProcID, uint64, bool) {
	if c == nil {
		return ProcID{}, 0, false
	}
	return ResolveProc(c.Meta, pid)
}

func ProcIDPtr(id ProcID, ok bool) *ProcID {
	if !ok || id.PID == 0 || id.StartJiffies == 0 {
		return nil
	}
	return &id
}

// IsAttributableCgroupID returns true iff the cgroup ID is safe to use for attribution.
//
// We treat cgroupID 0/1 as non-attributable:
// - 0 = missing / unknown
// - 1 = root/default/sentinel (observed poisoning source)
//
// This invariant must be applied consistently everywhere cgroup IDs are used for mapping.
func IsAttributableCgroupID(cgid uint64) bool {
	return cgid > 1
}
