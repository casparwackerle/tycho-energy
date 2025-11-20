package metadata

import (
	"context"
	"time"
)

// processCollector is responsible for discovering and updating process-level
// metadata (PID, start time, cgroup/container mapping, etc.).
//
// It does not interact with power/utilization collectors; it only maintains facts.
type processCollector struct {
	cfg   Config
	store *Store

	// TODO: add dependencies here later (e.g., logger, cgroup helpers, /proc scanner).
}

// newProcessCollector constructs a processCollector bound to the shared Store.
func newProcessCollector(cfg Config, store *Store) *processCollector {
	return &processCollector{
		cfg:   cfg,
		store: store,
	}
}

// Collect refreshes process metadata at the given time.
//
// Parameters:
//   - ctx: request context, for cancellation.
//   - ts:  aligned wall-clock time from the engine.
//   - mono: monotonic index derived from ts (0 if MonoSource not configured).
//
// In the concrete implementation, this will likely:
//   - enumerate /proc PIDs or read from an existing BPF-derived structure,
//   - resolve start time and cgroup/container mapping,
//   - upsert ProcMeta entries in the store with LastSeenMono/LastSeenWall set.
func (pc *processCollector) Collect(ctx context.Context, ts time.Time, mono uint64) {
	// TODO: implement process enumeration and metadata resolution.
	// This is a placeholder for the actual logic.
	_ = ctx
	_ = ts
	_ = mono
}
