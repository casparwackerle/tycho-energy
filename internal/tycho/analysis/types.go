package analysis

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MetricID identifies a metric family (stable string).
type MetricID string

// Labels are key/value dimensions. Keep as map for flexibility.
type Labels map[string]string

// MetricKey uniquely identifies a time-series within a metric family.
type MetricKey struct {
	ID     MetricID
	Labels Labels
}

// CanonicalString returns a stable string representation suitable for map keys/logs.
// This is intentionally deterministic: labels are sorted by key.
func (k MetricKey) CanonicalString() string {
	if len(k.Labels) == 0 {
		return string(k.ID)
	}
	keys := make([]string, 0, len(k.Labels))
	for kk := range k.Labels {
		keys = append(keys, kk)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString(string(k.ID))
	b.WriteString("{")
	for i, kk := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(kk)
		b.WriteString("=")
		b.WriteString(k.Labels[kk])
	}
	b.WriteString("}")
	return b.String()
}

func Key(id MetricID, labels Labels) MetricKey {
	// Defensive: allow nil labels.
	if labels == nil {
		labels = Labels{}
	}
	return MetricKey{ID: id, Labels: labels}
}

// Quality is intentionally minimal in Slice 0.
// Extend later (staleness, jitter, confidence intervals, etc.).
type Quality struct {
	SamplesUsed int
	Notes       string
}

// Point is the generic output unit emitted by metrics.
type Point struct {
	Key     MetricKey
	Window  Window
	Unit    string
	Value   float64
	Quality *Quality // optional
}

// Sink consumes generic points. Slice 0 uses a log sink.
type Sink interface {
	Emit(ctx context.Context, p Point)
}

// Metric is the plugin interface.
// Engine does not know anything about concrete metrics.
type Metric interface {
	ID() MetricID
	IsEnabled(c *Cycle) bool
	Run(c *Cycle) error
}

// Planner builds an execution plan each cycle.
// Implemented by analysis/registry.Registry.
type Planner interface {
	BuildPlan(c *Cycle) Plan
}

// Plan is the per-cycle runnable plan.
// Implemented by analysis/registry.Plan.
type Plan interface {
	Run(c *Cycle) error
}

// StateStore persists arbitrary per-metric state across cycles.
// Key is MetricKey (MetricID + labels).
type StateStore struct {
	mu sync.Mutex
	m  map[string]any
}

func NewStateStore() *StateStore {
	return &StateStore{m: make(map[string]any)}
}

func (s *StateStore) Get(key MetricKey) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key.CanonicalString()]
	return v, ok
}

func (s *StateStore) Set(key MetricKey, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key.CanonicalString()] = v
}

func (s *StateStore) Delete(key MetricKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, key.CanonicalString())
}

func (k MetricKey) String() string { return k.CanonicalString() }

func (l Labels) Clone() Labels {
	if l == nil {
		return Labels{}
	}
	out := make(Labels, len(l))
	for k, v := range l {
		out[k] = v
	}
	return out
}

func (p Point) String() string {
	return fmt.Sprintf("%s %v %s=%v",
		p.Key.CanonicalString(),
		p.Window,
		p.Unit,
		p.Value,
	)
}
