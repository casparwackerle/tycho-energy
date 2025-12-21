package gpu

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
)

// Metric ID under which corrected GPU series are stored in analysis.StateStore.
const MetricGpuCorrectedSeries analysis.MetricID = "gpu_corrected_series"

// PutCorrectedSeries stores the reconstructed grid series for a single GPU UUID.
// Storage is per-UUID so it scales and avoids rewriting a large map each cycle.
func PutCorrectedSeries(state *analysis.StateStore, uuid string, ser Series) {
	if state == nil || uuid == "" {
		return
	}
	key := analysis.Key(MetricGpuCorrectedSeries, analysis.Labels{"gpu_uuid": uuid})
	state.Set(key, ser)
}

// GetCorrectedSeries loads the last stored corrected grid series for a GPU UUID.
func GetCorrectedSeries(state *analysis.StateStore, uuid string) (Series, bool) {
	if state == nil || uuid == "" {
		return Series{}, false
	}
	key := analysis.Key(MetricGpuCorrectedSeries, analysis.Labels{"gpu_uuid": uuid})
	v, ok := state.Get(key)
	if !ok {
		return Series{}, false
	}
	ser, ok := v.(Series)
	return ser, ok
}
