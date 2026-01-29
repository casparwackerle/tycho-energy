// file: internal/tycho/analysis/metrics/util.go

package analysismetrics

import (
	"fmt"
	"math"
	"runtime"
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/fusion"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/jaypipes/ghw"
)

// Diagnostics metric IDs remain separate (unchanged).
const (
	MetricIdleModelReady  analysis.MetricID = "idle_model_ready"
	MetricIdleModelBins   analysis.MetricID = "idle_model_bins"
	MetricIdleModelPoints analysis.MetricID = "idle_model_points"
	MetricIdleModelMode   analysis.MetricID = "idle_model_mode"
	MetricIdleModelBetaMW analysis.MetricID = "idle_model_beta_mw"
)

func getCPUCores() int {
	cores := runtime.NumCPU()
	if cpu, err := ghw.CPU(ghw.WithDisableWarnings()); err == nil && cpu != nil {
		if cpu.TotalThreads > 0 {
			cores = int(cpu.TotalThreads)
		}
	}
	if cores <= 0 {
		cores = 1
	}
	return cores
}

// getPointValue returns the point value for an exact MetricKey from a PointStore.
func getPointValue(store *analysis.PointStore, key analysis.MetricKey) float64 {
	if store == nil {
		return 0
	}
	p, ok := store.GetExact(key)
	if !ok {
		return 0
	}
	return p.Value
}

// getPointValueOk returns (value, ok) for an exact MetricKey from a PointStore.
// This is critical to distinguish "missing" from "present and 0".
func getPointValueOk(store *analysis.PointStore, key analysis.MetricKey) (float64, bool) {
	if store == nil {
		return 0, false
	}
	p, ok := store.GetExact(key)
	if !ok {
		return 0, false
	}
	return p.Value, true
}

// sumPoints sums Point.Value across a slice.
func sumPoints(ps []analysis.Point) float64 {
	var s float64
	for _, p := range ps {
		s += p.Value
	}
	return s
}

func sumByID(ps []analysis.Point) float64 {
	var s float64
	for _, p := range ps {
		// Assume unit correctness (mJ). This slice keeps it minimal.
		s += p.Value
	}
	return s
}

// selectRedfishEnergyMJ matches the chassis selection policy used elsewhere:
// prefer chassis="Self", else pick lexicographically smallest chassis label.
func selectRedfishEnergyMJ(ps []analysis.Point) (val float64, chassis string, ok bool) {
	if len(ps) == 0 {
		return 0, "", false
	}

	for _, p := range ps {
		ch := ""
		if p.Key.Labels != nil {
			ch = p.Key.Labels["chassis"]
		}
		if ch == "Self" {
			return p.Value, "Self", true
		}
	}

	type cand struct {
		ch string
		v  float64
	}
	cands := make([]cand, 0, len(ps))
	for _, p := range ps {
		ch := ""
		if p.Key.Labels != nil {
			ch = p.Key.Labels["chassis"]
		}
		cands = append(cands, cand{ch: ch, v: p.Value})
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].ch < cands[j].ch })
	return cands[0].v, cands[0].ch, true
}

func boolTo01(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Same cache helpers as fusion_substrate.go
func getCache(s *analysis.StateStore, key analysis.MetricKey) (*fusion.Cache, bool) {
	if s == nil {
		return nil, false
	}
	v, ok := s.Get(key)
	if !ok {
		return nil, false
	}
	c, ok := v.(*fusion.Cache)
	if ok && c != nil {
		return c, true
	}
	cv, ok2 := v.(fusion.Cache)
	if ok2 {
		tmp := cv
		return &tmp, true
	}
	return nil, false
}
func tickToAbsSec(m *clock.Mono, tick uint64) float64 {
	if m == nil {
		return 0
	}
	q := m.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	return float64(time.Duration(tick)*q) / float64(time.Second)
}

func ticksToDurSec(m *clock.Mono, ticks uint64) float64 {
	if m == nil {
		return 0
	}
	q := m.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	return float64(time.Duration(ticks)*q) / float64(time.Second)
}

func normalizeRate(rate, amax float64) float64 {
	if amax <= 0 {
		return 0
	}
	u := rate / amax
	if u < 0 {
		return 0
	}
	if u > 1 {
		return 1
	}
	return u
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
func windowDurSec(c *analysis.Cycle) float64 {
	if c == nil || c.Mono == nil {
		return 0
	}
	if c.Window.EndMono <= c.Window.StartMono {
		return 0
	}
	q := c.Mono.Quantum()
	if q <= 0 {
		q = time.Millisecond
	}
	dtTicks := c.Window.EndMono - c.Window.StartMono
	return float64(time.Duration(dtTicks)*q) / float64(time.Second)
}

func modeToFloat(mode string) float64 {
	// Prometheus prefers labels over enums, but keep a numeric for quick dashboards.
	if mode == "model" {
		return 1
	}
	return 0
}

func stateKeyEnergyCumF64(component, domain, kind string) string {
	return fmt.Sprintf("cum_energy_mj_f64:%s:%s:%s", component, domain, kind)
}

func stateKeyCumEnergyU64(component, domain, kind string) string {
	return fmt.Sprintf("cum_energy_u64_mj:%s:%s:%s", component, domain, kind)
}

func stateGetU64(st any, key string) (uint64, bool) {
	// Preferred: StateStore-backed
	if ss, ok := st.(*analysis.StateStore); ok && ss != nil {
		v, ok := ss.Get(analysis.Key("__state_u64", analysis.Labels{"k": key}))
		if !ok {
			return 0, false
		}
		u, ok := v.(uint64)
		return u, ok
	}

	// Fallback: map-backed
	m, ok := st.(map[string]any)
	if !ok || m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	u, ok := v.(uint64)
	return u, ok
}

func stateSetU64(st any, key string, v uint64) {
	// Preferred: StateStore-backed
	if ss, ok := st.(*analysis.StateStore); ok && ss != nil {
		ss.Set(analysis.Key("__state_u64", analysis.Labels{"k": key}), v)
		return
	}

	// Fallback: map-backed
	m, ok := st.(map[string]any)
	if !ok || m == nil {
		return
	}
	m[key] = v
}

func stateAddU64(st any, key string, delta uint64) uint64 {
	prev, _ := stateGetU64(st, key)
	next := prev + delta
	stateSetU64(st, key, next)
	return next
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return 0
	}
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func stateKeyCumEnergyF64(component, id, kind string) string {
	// id is "gpu_uuid" for GPUs; "domain" for RAPL; keep it generic for shared utils.
	return fmt.Sprintf("cum_energy_f64_mj:%s:%s:%s", component, id, kind)
}

func stateKeyLastSeen(component, id, name string) string {
	return fmt.Sprintf("last_seen:%s:%s:%s", component, id, name)
}

// deltaFromCumulative returns (curr - prev) and stores curr as the new prev, clamped at >=0.
func deltaFromCumulative(state any, lastKey string, curr float64) float64 {
	prev, _ := stateGetF64(state, lastKey)
	stateSetF64(state, lastKey, curr)
	d := curr - prev
	if d < 0 {
		d = 0
	}
	return d
}

func stateGetF64(st any, key string) (float64, bool) {
	// Preferred: StateStore-backed
	if ss, ok := st.(*analysis.StateStore); ok && ss != nil {
		v, ok := ss.Get(analysis.Key("__state_f64", analysis.Labels{"k": key}))
		if !ok {
			return 0, false
		}
		f, ok := v.(float64)
		return f, ok
	}

	// Fallback: map-backed
	m, ok := st.(map[string]any)
	if !ok || m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func stateSetF64(st any, key string, v float64) {
	// Preferred: StateStore-backed
	if ss, ok := st.(*analysis.StateStore); ok && ss != nil {
		ss.Set(analysis.Key("__state_f64", analysis.Labels{"k": key}), v)
		return
	}

	// Fallback: map-backed
	m, ok := st.(map[string]any)
	if !ok || m == nil {
		return
	}
	m[key] = v
}

func stateAddF64(st any, key string, delta float64) float64 {
	prev, _ := stateGetF64(st, key)
	next := prev + delta
	stateSetF64(st, key, next)
	return next
}

func sumEnergyBy(component string, kind string, ps []analysis.Point) float64 {
	sum := 0.0
	for _, p := range ps {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["component"] != component {
			continue
		}
		if kind != "" && p.Key.Labels["kind"] != kind {
			continue
		}
		sum += p.Value
	}
	return sum
}

// stateKeyOffsetU64 stores the fixed startup offset for native counters (uint64).
func stateKeyOffsetU64(component, id, name string) string {
	// id is domain for RAPL, gpu_uuid for GPUs, etc.
	return fmt.Sprintf("offset_u64:%s:%s:%s", component, id, name)
}
