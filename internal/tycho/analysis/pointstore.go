package analysis

// PointStore is a minimal per-cycle accumulator of emitted points.
// It is not a global memoization system; it only exists for one analysis run.
type PointStore struct {
	byKey map[string]Point
	byID  map[MetricID][]Point

	// insertion order (debugging / deterministic “first encountered”)
	order []Point
}

func NewPointStore() *PointStore {
	return &PointStore{
		byKey: make(map[string]Point, 32),
		byID:  make(map[MetricID][]Point, 16),
		order: make([]Point, 0, 32),
	}
}

func (s *PointStore) Put(p Point) {
	if s == nil {
		return
	}
	k := p.Key.CanonicalString()
	s.byKey[k] = p
	s.byID[p.Key.ID] = append(s.byID[p.Key.ID], p)
	s.order = append(s.order, p)
}

func (s *PointStore) GetExact(key MetricKey) (Point, bool) {
	if s == nil {
		return Point{}, false
	}
	p, ok := s.byKey[key.CanonicalString()]
	return p, ok
}

func (s *PointStore) ListByID(id MetricID) []Point {
	if s == nil {
		return nil
	}
	ps := s.byID[id]
	// return a shallow copy to avoid accidental mutation
	out := make([]Point, len(ps))
	copy(out, ps)
	return out
}

func (s *PointStore) AllInOrder() []Point {
	if s == nil {
		return nil
	}
	out := make([]Point, len(s.order))
	copy(out, s.order)
	return out
}
