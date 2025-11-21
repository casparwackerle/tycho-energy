package metadata

import (
	"sync"
	"time"
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
		maxAge:     maxAge,
	}
}

// UpsertProc inserts or updates process metadata.
func (s *Store) UpsertProc(meta *ProcMeta) {
	if meta == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.procs[meta.PID] = meta
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

	return droppedProcs, droppedContainers, droppedPods
}
