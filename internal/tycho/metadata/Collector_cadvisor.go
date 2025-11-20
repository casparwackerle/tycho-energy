package metadata

import (
	"context"
	"time"
)

// cadvisorCollector is an optional collector that enriches container metadata
// with additional context (throttling, memory, IO, etc.) from cAdvisor or
// a similar component.
//
// For the initial proof-of-concept, this can remain unimplemented or partially
// implemented if time allows.
type cadvisorCollector struct {
	cfg   Config
	store *Store

	// TODO: add cAdvisor client / HTTP client fields as needed.
}

// newCADvisorCollector constructs a cadvisorCollector bound to the shared Store.
func newCADvisorCollector(cfg Config, store *Store) *cadvisorCollector {
	return &cadvisorCollector{
		cfg:   cfg,
		store: store,
	}
}

// Collect refreshes cAdvisor-based context for containers at the given time.
//
// Parameters:
//   - ctx: request context, for cancellation.
//   - ts:  aligned wall-clock time from the engine.
//   - mono: monotonic index derived from ts (0 if MonoSource not configured).
//
// In the concrete implementation, this will likely:
//   - query cAdvisor or kubelet summary API,
//   - update ContextContainerStats fields inside ContainerMeta entries,
//   - keep LastSeen* fields consistent.
func (cc *cadvisorCollector) Collect(ctx context.Context, ts time.Time, mono uint64) {
	// TODO: implement cAdvisor-based context collection.
	// This is a placeholder for the actual logic.
	_ = ctx
	_ = ts
	_ = mono
}
