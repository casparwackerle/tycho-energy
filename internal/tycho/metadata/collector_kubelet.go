package metadata

import (
	"context"
	"time"
)

// kubeletCollector is responsible for collecting pod and container metadata
// from the kubelet (or Kubernetes API as a fallback).
type kubeletCollector struct {
	cfg   Config
	store *Store

	// TODO: add kubelet client / HTTP client fields as needed.
}

// newKubeletCollector constructs a kubeletCollector bound to the shared Store.
func newKubeletCollector(cfg Config, store *Store) *kubeletCollector {
	return &kubeletCollector{
		cfg:   cfg,
		store: store,
	}
}

// Collect refreshes pod and container metadata at the given time.
//
// Parameters:
//   - ctx: request context, for cancellation.
//   - ts:  aligned wall-clock time from the engine.
//   - mono: monotonic index derived from ts (0 if MonoSource not configured).
//
// In the concrete implementation, this will likely:
//   - list pods via kubelet,
//   - update PodMeta records in the store,
//   - update ContainerMeta records for each container status,
//   - set LastSeenMono/LastSeenWall appropriately.
func (kc *kubeletCollector) Collect(ctx context.Context, ts time.Time, mono uint64) {
	// TODO: implement kubelet pod listing and metadata extraction.
	// This is a placeholder for the actual logic.
	_ = ctx
	_ = ts
	_ = mono
}
