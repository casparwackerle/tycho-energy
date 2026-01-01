// file: internal/tycho/analysis/idle/store.go
package idle

import "github.com/casparwackerle/tycho-energy/internal/tycho/analysis"

const (
	MetricIdleScalarState analysis.MetricID = "idle_scalar_state"
	MetricIdleVec2State   analysis.MetricID = "idle_vec2_state"
	MetricP95State        analysis.MetricID = "idle_p95_state"
)

func GetOrInitScalar(state *analysis.StateStore, name string, cfg Config) *ScalarModel {
	if state == nil || name == "" {
		return nil
	}
	key := analysis.Key(MetricIdleScalarState, analysis.Labels{"name": name})
	v, ok := state.Get(key)
	if ok {
		if m, ok2 := v.(*ScalarModel); ok2 && m != nil {
			return m
		}
	}
	m := NewScalarModel(cfg)
	state.Set(key, m)
	return m
}

func GetOrInitVec2(state *analysis.StateStore, name string, cfg Config) *Vec2Model {
	if state == nil || name == "" {
		return nil
	}
	key := analysis.Key(MetricIdleVec2State, analysis.Labels{"name": name})
	v, ok := state.Get(key)
	if ok {
		if m, ok2 := v.(*Vec2Model); ok2 && m != nil {
			return m
		}
	}
	m := NewVec2Model(cfg)
	state.Set(key, m)
	return m
}

func GetOrInitP95(state *analysis.StateStore, name string, window int, decayPerSec float64) *P95Decay {
	if state == nil || name == "" {
		return nil
	}
	key := analysis.Key(MetricP95State, analysis.Labels{"name": name})
	v, ok := state.Get(key)
	if ok {
		if t, ok2 := v.(*P95Decay); ok2 && t != nil {
			return t
		}
	}
	t := NewP95Decay(window, decayPerSec)
	state.Set(key, t)
	return t
}
