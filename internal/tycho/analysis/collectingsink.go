package analysis

import "context"

// CollectingSink tees all emitted points into a per-cycle PointStore,
// and forwards them to the downstream Sink.
type CollectingSink struct {
	downstream Sink
	store      *PointStore
}

func NewCollectingSink(downstream Sink, store *PointStore) *CollectingSink {
	return &CollectingSink{
		downstream: downstream,
		store:      store,
	}
}

func (s *CollectingSink) Emit(ctx context.Context, p Point) {
	if s.store != nil {
		s.store.Put(p)
	}
	if s.downstream != nil {
		s.downstream.Emit(ctx, p)
	}
}

// Delete forwards deletions to the downstream sink.
// The PointStore is per-cycle and does not need deletion semantics.
func (s *CollectingSink) Delete(ctx context.Context, key MetricKey) {
	if s.store != nil {
		s.store.Delete(key)
	}
	if s.downstream != nil {
		s.downstream.Delete(ctx, key)
	}
}
