package metadata

import (
	"sync"
	"time"

	"github.com/casparwackerle/tycho-energy/pkg/utils"
)

// Store holds the in-memory metadata caches for processes, containers, and pods.
//
// It is designed for node-local, short-horizon usage (≈ 60–90 seconds of history).
// Collectors only ever upsert or read; the GC is the only component that deletes.
type Store struct {
	mu sync.RWMutex

	// Process-level metadata keyed by PID.
	// StartJiffies inside ProcMeta can be used to guard against PID reuse if needed.
	procs map[uint64]*ProcMeta

	// ContainerID → ContainerMeta
	containers map[string]*ContainerMeta

	// PodUID → PodMeta
	pods map[string]*PodMeta

	// How long entries are kept before GC considers them stale.
	maxAge time.Duration

	// cgroupID -> containerID index (best-effort), populated by analysis from BPF windows.
	cgroups map[uint64]*CgroupIndexEntry
}

// NewStore creates a new Store with the given maximum age for entries.
func NewStore(maxAge time.Duration) *Store {
	if maxAge <= 0 {
		// Reasonable default; caller can override.
		maxAge = 90 * time.Second
	}
	return &Store{
		procs:      make(map[uint64]*ProcMeta),
		containers: make(map[string]*ContainerMeta),
		pods:       make(map[string]*PodMeta),
		cgroups:    make(map[uint64]*CgroupIndexEntry),
		maxAge:     maxAge,
	}
}

// UpsertProc inserts or updates process metadata.
//
// Slice 13A contract:
//   - StartJiffies is immutable for a given process incarnation.
//   - Once StartJiffies is known for a PID, it must never regress to 0.
//   - If StartJiffies changes to a different non-zero value, treat this as PID reuse
//     and replace the stored record for that PID.
func (s *Store) UpsertProc(meta *ProcMeta) {
	if meta == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.procs[meta.PID]
	if old == nil {
		// First observation of this PID.
		s.procs[meta.PID] = meta
		return
	}

	// PID reuse detection: both start times known and different -> new incarnation.
	if old.StartJiffies != 0 && meta.StartJiffies != 0 && old.StartJiffies != meta.StartJiffies {
		s.procs[meta.PID] = meta
		return
	}

	// Do not allow StartJiffies to regress to 0 once known.
	if old.StartJiffies != 0 && meta.StartJiffies == 0 {
		meta.StartJiffies = old.StartJiffies
	}

	// Merge into existing record to avoid losing previously-known fields.
	old.LastSeenMono = meta.LastSeenMono
	old.LastSeenWall = meta.LastSeenWall

	// Identity: write once (after regression guard above).
	if old.StartJiffies == 0 && meta.StartJiffies != 0 {
		old.StartJiffies = meta.StartJiffies
	}

	// Optional fields: update opportunistically.
	if meta.CgroupID != 0 {
		old.CgroupID = meta.CgroupID
	}
	if meta.ContainerID != "" {
		old.ContainerID = meta.ContainerID
	}
	if meta.Command != "" {
		old.Command = meta.Command
	}
}

// UpsertContainer inserts or updates container metadata.
func (s *Store) UpsertContainer(meta *ContainerMeta) {
	if meta == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.containers[meta.ContainerID] = meta
}

// UpsertPod inserts or updates pod metadata.
func (s *Store) UpsertPod(meta *PodMeta) {
	if meta == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pods[meta.PodUID] = meta
}

// LookupProc returns the process metadata for a PID, if present.
func (s *Store) LookupProc(pid uint64) (*ProcMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	meta, ok := s.procs[pid]
	return meta, ok
}

// LookupContainer returns container metadata for a given ContainerID, if present.
func (s *Store) LookupContainer(id string) (*ContainerMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	meta, ok := s.containers[id]
	return meta, ok
}

// LookupPod returns pod metadata for a given PodUID, if present.
func (s *Store) LookupPod(uid string) (*PodMeta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	meta, ok := s.pods[uid]
	return meta, ok
}

// LookupAllForProcess is a convenience helper that resolves a process to its
// container and pod metadata when available.
func (s *Store) LookupAllForProcess(pid uint64) LookupResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var res LookupResult

	if p, ok := s.procs[pid]; ok {
		res.Process = p
		if p.ContainerID != "" {
			if c, ok := s.containers[p.ContainerID]; ok {
				res.Container = c
				if c.PodUID != "" {
					if pod, ok := s.pods[c.PodUID]; ok {
						res.Pod = pod
					}
				}
			}
		}
	}

	return res
}

// RangeProcs iterates over all process entries.
// The callback must not retain references to the returned pointer without its own
// synchronization. Returning false stops iteration early.
func (s *Store) RangeProcs(fn func(pid uint64, pm *ProcMeta) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for pid, pm := range s.procs {
		if !fn(pid, pm) {
			return
		}
	}
}

// RangeContainers iterates over all container entries.
func (s *Store) RangeContainers(fn func(id string, cm *ContainerMeta) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for id, cm := range s.containers {
		if !fn(id, cm) {
			return
		}
	}
}

// RangePods iterates over all pod entries.
func (s *Store) RangePods(fn func(uid string, pm *PodMeta) bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for uid, pm := range s.pods {
		if !fn(uid, pm) {
			return
		}
	}
}

// GC removes stale entries from the store based on their LastSeenWall timestamps.
//
// now is the current wall-clock time; entries older than maxAge are removed.
// It returns the number of entries removed for each category, which can be
// logged at a high verbosity level if desired.
func (s *Store) GC(now time.Time) (droppedProcs, droppedContainers, droppedPods int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := now.Add(-s.maxAge)

	for pid, meta := range s.procs {
		if meta.LastSeenWall.Before(cutoff) {
			delete(s.procs, pid)
			droppedProcs++
		}
	}

	for id, meta := range s.containers {
		if meta.LastSeenWall.Before(cutoff) {
			delete(s.containers, id)
			droppedContainers++
		}
	}

	for uid, meta := range s.pods {
		if meta.LastSeenWall.Before(cutoff) {
			delete(s.pods, uid)
			droppedPods++
		}
	}

	for cgid, meta := range s.cgroups {
		if meta.LastSeenWall.Before(cutoff) {
			delete(s.cgroups, cgid)
			// optional counter if you want, but not needed for slice
		}
	}

	return droppedProcs, droppedContainers, droppedPods
}

// UpsertCgroupMapping inserts or refreshes a cgroupID -> containerID mapping.
// This is best-effort and may be overwritten; it is GC'd via LastSeenWall.
func (s *Store) UpsertCgroupMapping(cgroupID uint64, containerID string, nowMono uint64, nowWall time.Time) {
	// Reject non-attributable IDs (0 = missing, 1 = root/default/sentinel).
	if s == nil || cgroupID <= 1 {
		return
	}
	if containerID == "" || containerID == utils.SystemProcessName {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	old := s.cgroups[cgroupID]
	if old == nil {
		s.cgroups[cgroupID] = &CgroupIndexEntry{
			CgroupID:     cgroupID,
			ContainerID:  containerID,
			LastSeenMono: nowMono,
			LastSeenWall: nowWall,
		}
		return
	}

	// Update container ID opportunistically (best-effort).
	// Never overwrite with empty/system values (already filtered above).
	old.ContainerID = containerID

	// Refresh timestamps.
	old.LastSeenMono = nowMono
	old.LastSeenWall = nowWall
}

// LookupContainerIDByCgroupID returns the container ID associated with a cgroup ID, if present.
func (s *Store) LookupContainerIDByCgroupID(cgroupID uint64) (string, bool) {
	if s == nil || cgroupID <= 1 {
		return "", false
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ent, ok := s.cgroups[cgroupID]
	if !ok || ent == nil || ent.ContainerID == "" || ent.ContainerID == utils.SystemProcessName {
		return "", false
	}
	return ent.ContainerID, true
}
