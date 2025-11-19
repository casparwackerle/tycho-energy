package calibration

// SetLastCalibMono sets the monotonic timestamp of the last full calibration run.
func SetLastCalibMono(ts int64) {
	_store.mu.Lock()
	defer _store.mu.Unlock()
	_store.lastCalibMono = ts
}

// SetCumEnergyDiag stores the per-device cumulative energy validation results
// in the global calibration store. Thread-safe and overwrites previous results.
func SetCumEnergyDiag(diags map[string]CumEnergyDiag) {
	_store.mu.Lock()
	defer _store.mu.Unlock()
	if _store.hasCumEnergy == nil {
		_store.hasCumEnergy = make(map[string]CumEnergyDiag)
	}
	for uuid, d := range diags {
		_store.hasCumEnergy[uuid] = d
	}
}

// GetCumEnergyDiag returns a copy of the currently stored cumulative energy diagnostics.
func GetCumEnergyDiag() map[string]CumEnergyDiag {
	_store.mu.RLock()
	defer _store.mu.RUnlock()
	out := make(map[string]CumEnergyDiag, len(_store.hasCumEnergy))
	for k, v := range _store.hasCumEnergy {
		out[k] = v
	}
	return out
}
