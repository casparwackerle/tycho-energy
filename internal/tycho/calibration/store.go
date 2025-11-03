package calibration

import (
	"sync"
)

// idleStore holds the current best idle baselines and simple calibration bookkeeping.
// Memory-only; thread-safe; no external persistence.
type idleStore struct {
	mu sync.RWMutex

	idle         IdleBaselines
	hasCumEnergy map[string]CumEnergyDiag

	lastCalibMono        int64
	hysteresisPct        float64
	confirmationsNeeded  int
	pendingConfirmations map[idKey]int
}

// CumEnergyDiag reports per-device validation results for cumulative energy.
type CumEnergyDiag struct {
	Valid               bool    // final verdict
	Samples             int     // number of ticks considered
	CumReads            int     // ticks where cumulative mJ was available
	MonotonicViolations int     // count of strictly decreasing cum samples
	IntegratedJ         float64 // energy from integrating InstantPowerMilliW (J)
	CumulativeDeltaJ    float64 // delta of cumulative counter over window (J)
	RelativeError       float64 // |Ecum - Eintegrated| / max(Eintegrated, eps)
	WindowSeconds       float64 // total time span of the considered ticks
}

type idKey struct {
	socket SocketID
	domain Domain
}

var (
	_store = &idleStore{
		idle:                 make(IdleBaselines),
		hasCumEnergy:         make(map[string]CumEnergyDiag),
		hysteresisPct:        0.01,
		confirmationsNeeded:  3,
		pendingConfirmations: make(map[idKey]int),
	}
)

// Optionally allow tweaking defaults from elsewhere in the package (not exported).
func configureIdleHysteresis(pct float64, confirmations int) {
	_store.mu.Lock()
	defer _store.mu.Unlock()
	if pct < 0 {
		pct = 0
	}
	if confirmations < 1 {
		confirmations = 1
	}
	_store.hysteresisPct = pct
	_store.confirmationsNeeded = confirmations
	_store.pendingConfirmations = make(map[idKey]int) // reset counters
}

// GetIdle returns a shallow copy of the current IdleBaselines.
// Callers should treat the returned structure as read-only.
func GetIdle() IdleBaselines {
	_store.mu.RLock()
	defer _store.mu.RUnlock()

	out := make(IdleBaselines, len(_store.idle))
	for s, domMap := range _store.idle {
		cp := make(map[Domain]IdleBaseline, len(domMap))
		for d, bl := range domMap {
			cp[d] = bl
		}
		out[s] = cp
	}
	return out
}

// SetIdleAll replaces all known idle baselines (e.g., after a successful calibration run).
// Resets pending confirmations (fresh ground truth).
func SetIdleAll(b IdleBaselines) {
	_store.mu.Lock()
	defer _store.mu.Unlock()

	// Deep copy to internal store
	newStore := make(IdleBaselines, len(b))
	for s, domMap := range b {
		cp := make(map[Domain]IdleBaseline, len(domMap))
		for d, bl := range domMap {
			cp[d] = bl
		}
		newStore[s] = cp
	}
	_store.idle = newStore
	_store.pendingConfirmations = make(map[idKey]int)
}

// MaybeUpdateIdle attempts to lower the P5 baseline for a given socket/domain.
// It only accepts an update if the candidate value is lower than the current P5
// by at least hysteresisPct AND this has been observed confirmationsNeeded times.
// Returns (updated, newBaseline).
func MaybeUpdateIdle(socket SocketID, dom Domain, candidateP5 float64, ts uint64) (bool, IdleBaseline) {
	_store.mu.Lock()
	defer _store.mu.Unlock()

	dmap, ok := _store.idle[socket]
	if !ok || dmap == nil {
		// No existing baseline → we refuse to initialize via runtime refine.
		// A full calibration (SetIdleAll) should initialize first.
		return false, IdleBaseline{}
	}
	cur, ok := dmap[dom]
	if !ok {
		return false, IdleBaseline{}
	}

	// Require strictly lower by hysteresisPct.
	threshold := cur.P5 * (1.0 - _store.hysteresisPct)
	if candidateP5 >= threshold {
		// Not low enough → reset confirmations for this key (avoid stale positives).
		delete(_store.pendingConfirmations, idKey{socket, dom})
		return false, cur
	}

	// Count confirmations for this key.
	k := idKey{socket, dom}
	_store.pendingConfirmations[k] = _store.pendingConfirmations[k] + 1
	if _store.pendingConfirmations[k] < _store.confirmationsNeeded {
		return false, cur
	}

	// Accept: update baseline, reset counter.
	newBL := IdleBaseline{
		P5:     candidateP5,
		Min:    minf(cur.Min, candidateP5), // keep min as the best known minimum
		N:      cur.N,                      // N remains from last robust window (or update if you’ve recomputed it)
		FromTs: ts,
	}
	dmap[dom] = newBL
	_store.pendingConfirmations[k] = 0
	return true, newBL
}

func minf(a, b float64) float64 {
	if a == 0 {
		return b
	}
	if b < a {
		return b
	}
	return a
}

// LastCalibMono returns the monotonic timestamp of the last full calibration run.
func LastCalibMono() int64 {
	_store.mu.RLock()
	defer _store.mu.RUnlock()
	return _store.lastCalibMono
}

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
