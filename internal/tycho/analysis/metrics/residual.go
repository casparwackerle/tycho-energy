// file: internal/tycho/analysis/metrics/residual.go
package analysismetrics

import (
	"math"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

const (
	MetricResidualPowerMW  analysis.MetricID = "residual_power_mw"
	MetricResidualEnergyMJ analysis.MetricID = "residual_energy_mj"

	MetricResidualNegativePowerMW analysis.MetricID = "residual_negative_mw" // diag only
	MetricFusionTheta             analysis.MetricID = "fusion_theta"         // state key family

	// Slice 11 diagnostics
	MetricResidualTransient    analysis.MetricID = "residual_transient"      // diag: 0/1
	MetricResidualPartsSlopeMW analysis.MetricID = "residual_parts_slope_mw" // diag: Δ(P_rapl+P_gpu)

	// Slice 11 state-only keys (avoid collisions with exported metric IDs)
	MetricResidualLastPartsPowerMW    analysis.MetricID = "residual_last_parts_power_mw_state"
	MetricResidualDeficitMW           analysis.MetricID = "residual_deficit_mw"
	MetricResidualTransientHold       analysis.MetricID = "residual_transient_hold_state"
	MetricResidualTakeoverDone        analysis.MetricID = "residual_takeover_done" // state-only
	MetricResidualWindowUsable        analysis.MetricID = "residual_window_usable"
	MetricResidualIdleBaselineMW      analysis.MetricID = "residual_idle_baseline_mw"
	MetricResidualtransientNow        analysis.MetricID = "residual_transient_now"
	MetricResidualtransientHold       analysis.MetricID = "residual_transient_hold"
	MetricResidualIdleBaselineMWState analysis.MetricID = "residual_idle_baseline_mw_state"
	MetricResidualIdleCandMWState     analysis.MetricID = "residual_idle_cand_mw_state"
	MetricResidualIdleLowRunState     analysis.MetricID = "residual_idle_low_run_state"

	residualKindTotal   = "total"
	residualKindIdle    = "idle"
	residualKindDynamic = "dynamic"
)

type Residual struct{}

func NewResidual() *Residual { return &Residual{} }

func (m *Residual) ID() analysis.MetricID { return "residual" }

func (m *Residual) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil
}

func (m *Residual) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil {
		return nil
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()
	if monoQuantumSec <= 0 {
		return nil
	}
	winSec := float64(c.Window.EndMono-c.Window.StartMono+1) * monoQuantumSec
	if winSec <= 0 {
		return nil
	}

	// Prefer corrected system power, else raw (warmup).
	sysP, chassis, source, ok := getSystemPower(c)
	if !ok {
		return nil
	}

	// System window energy from average power.
	sysEWinMJ := sysP * winSec

	// Accounted window energy from average power (aligned semantics).
	raplPW := raplTotalPowerMW(c) // pkg + dram
	gpuPW := gpuTotalPowerMW(c)   // sum over gpu_uuid
	partsPW := raplPW + gpuPW
	accEWinMJ := partsPW * winSec

	// Residual window energy and power (may be negative).
	resWinMJ := sysEWinMJ - accEWinMJ
	resMW := resWinMJ / winSec

	// Clamp at 0 for counters and split (no negative residual energy).
	resClamped := resWinMJ < 0
	resWinMJClamp := math.Max(0, resWinMJ)
	resMWClamp := resWinMJClamp / winSec

	// ---------------------------
	// Slice 11A: transient detection (conservative heuristics)
	// ---------------------------

	// Conservative defaults; promote to config later.
	const (
		thetaRiseMW    = 20000.0 // 20 W step in parts power
		thetaDeficitMW = 10000.0 // 10 W system lag behind parts indicates misalignment
		epsNearMW      = 500.0   // residual near-zero
		minLearnMW     = 5000.0  // do not learn below 5 W residual
		transientHoldN = 3       // keep gating for 3 windows after transient clears
	)

	// Per-chassis slope state
	lastPartsKey := analysis.Key(analysis.MetricID(MetricResidualLastPartsPowerMW), analysis.Labels{"chassis": chassis})
	var lastParts float64
	if v, ok := c.State.Get(lastPartsKey); ok {
		if f, ok2 := v.(float64); ok2 {
			lastParts = f
		}
	}
	partsSlope := partsPW - lastParts
	c.State.Set(lastPartsKey, partsPW)

	// Sustained lag indicator: parts ahead of system.
	deficitMW := partsPW - sysP // >0 => system behind parts

	nearZero := resMWClamp <= epsNearMW

	// Primary transient classification:
	//  - strong lag behind parts (covers variable / long Redfish delay)
	//  - or sharp parts step coupled with clamp/near-zero (edge trigger)
	transientNow := (deficitMW > thetaDeficitMW) || ((partsSlope > thetaRiseMW) && (resClamped || nearZero))

	// Hold / cooldown to prevent flapping and early ungating
	holdKey := analysis.Key(analysis.MetricID(MetricResidualTransientHold), analysis.Labels{"chassis": chassis})
	hold := 0
	if v, ok := c.State.Get(holdKey); ok {
		if i, ok2 := v.(int); ok2 {
			hold = i
		}
	}
	if transientNow {
		hold = transientHoldN
	} else if hold > 0 {
		hold--
	}
	c.State.Set(holdKey, hold)

	transient := transientNow || (hold > 0)

	// Window usability explicitly for residual idle/dynamic split:
	// usable means "safe to learn and interpret split as physically meaningful".
	windowUsable := (!transient) && (!resClamped)

	labelsBase := analysis.Labels{
		"chassis": chassis,
		"source":  source, // raw during warmup, corrected afterwards
	}

	// ---------------------------
	// Raw -> corrected takeover: delete raw residual series once corrected is active
	// ---------------------------
	if source == redfishSourceCorrected {
		takeoverKey := analysis.Key(MetricResidualTakeoverDone, analysis.Labels{"chassis": chassis})
		if _, done := c.State.Get(takeoverKey); !done {

			// Delete raw residual power series
			for _, kind := range []string{residualKindTotal, residualKindIdle, residualKindDynamic} {
				c.Sink.Delete(c.Ctx, analysis.Key(MetricResidualPowerMW, analysis.Labels{
					"chassis": chassis,
					"source":  redfishSourceRaw,
					"kind":    kind,
				}))
			}

			// Delete raw residual energy series
			for _, kind := range []string{residualKindTotal, residualKindIdle, residualKindDynamic} {
				c.Sink.Delete(c.Ctx, analysis.Key(MetricResidualEnergyMJ, analysis.Labels{
					"chassis": chassis,
					"source":  redfishSourceRaw,
					"kind":    kind,
				}))
			}

			// Delete raw negative residual diag series (if present)
			c.Sink.Delete(c.Ctx, analysis.Key(MetricResidualNegativePowerMW, analysis.Labels{
				"chassis": chassis,
				"source":  redfishSourceRaw,
				"kind":    residualKindTotal,
			}))

			// Delete raw window usability series
			c.Sink.Delete(c.Ctx, analysis.Key(analysis.MetricID(MetricResidualWindowUsable), analysis.Labels{
				"chassis": chassis,
				"source":  redfishSourceRaw,
			}))

			// Delete legacy baseline series if it ever existed with a source label.
			// (Your PrometheusSink keeps the first-seen label schema, so old series can stick around.)
			c.Sink.Delete(c.Ctx, analysis.Key(analysis.MetricID(MetricResidualIdleBaselineMW), analysis.Labels{
				"chassis": chassis,
				"source":  redfishSourceRaw,
			}))
			c.Sink.Delete(c.Ctx, analysis.Key(analysis.MetricID(MetricResidualIdleBaselineMW), analysis.Labels{
				"chassis": chassis,
				"source":  redfishSourceCorrected,
			}))

			c.State.Set(takeoverKey, true)

			if config.GetIdleDiagnosticsEnabled() {
				c.Sink.Emit(c.Ctx, analysis.Point{
					Key:    analysis.Key(analysis.MetricID(MetricResidualtransientNow), analysis.Labels{"chassis": chassis}),
					Window: c.Window,
					Unit:   "bool",
					Value:  boolToFloat(transientNow),
				})
				c.Sink.Emit(c.Ctx, analysis.Point{
					Key:    analysis.Key(analysis.MetricID(MetricResidualtransientHold), analysis.Labels{"chassis": chassis}),
					Window: c.Window,
					Unit:   "count",
					Value:  math.Floor(float64(hold)),
				})
			}
		}
	}

	// ---------------------------
	// Valuable diagnostics (always-on)
	// ---------------------------

	// 1) transient flag (0/1)
	if config.GetIdleDiagnosticsEnabled() {
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(analysis.MetricID(MetricResidualTransient), analysis.Labels{"chassis": chassis}),
			Window: c.Window,
			Unit:   "bool",
			Value:  boolToFloat(transient),
		})
	}

	// 2) window usability flag (0/1) specifically for residual idle/dynamic interpretation
	{
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(analysis.MetricID(MetricResidualWindowUsable), analysis.Labels{"chassis": chassis}),
			Window: c.Window,
			Unit:   "bool",
			Value:  boolToFloat(windowUsable),
		})
	}
	if config.GetIdleDiagnosticsEnabled() {
		// 3) parts slope + deficit are typically valuable for end users tuning thresholds and understanding artifacts
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(analysis.MetricID(MetricResidualPartsSlopeMW), analysis.Labels{"chassis": chassis}),
			Window: c.Window,
			Unit:   "mW",
			Value:  partsSlope,
		})
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(analysis.MetricID(MetricResidualDeficitMW), analysis.Labels{"chassis": chassis}),
			Window: c.Window,
			Unit:   "mW",
			Value:  deficitMW,
		})
	}

	// ---------------------------
	// Slice 11B + Slice 12C: gated residual idle learning (robust against transient dips)
	// ---------------------------

	// Learn only when window is usable AND residual is meaningful.
	// Also: only learn on corrected source, so warmup never pollutes the baseline.
	allowLearn := (source == redfishSourceCorrected) && windowUsable && (resMWClamp >= minLearnMW)

	// Conservative "candidate then commit" parameters.
	// - dropMarginMW: ignore tiny dips (noise)
	// - lowPersistN: require N consecutive learnable windows before committing a lower baseline
	// - commitEta: once confirmed, move baseline down slowly (EMA step)
	const (
		dropMarginMW    = 3000.0 // 3 W
		lowPersistN     = 4      // number of consecutive windows required
		commitEta       = 0.10   // 10% move toward candidate when committing
		maxPermDropFrac = 0.05   // allow at most 5% permanent drop per commit
	)

	// State keys (per chassis)
	lastIdleKey := analysis.Key(analysis.MetricID(MetricResidualIdleBaselineMWState), analysis.Labels{"chassis": chassis})
	candKey := analysis.Key(analysis.MetricID(MetricResidualIdleCandMWState), analysis.Labels{"chassis": chassis})
	runKey := analysis.Key(analysis.MetricID(MetricResidualIdleLowRunState), analysis.Labels{"chassis": chassis})

	getFloat := func(k analysis.MetricKey) (float64, bool) {
		if v, ok := c.State.Get(k); ok {
			if f, ok2 := v.(float64); ok2 && f >= 0 && !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f, true
			}
		}
		return 0, false
	}
	setFloat := func(k analysis.MetricKey, v float64) {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		c.State.Set(k, v)
	}
	getInt := func(k analysis.MetricKey) int {
		if v, ok := c.State.Get(k); ok {
			if i, ok2 := v.(int); ok2 {
				return i
			}
			if f, ok2 := v.(float64); ok2 {
				return int(f)
			}
		}
		return 0
	}
	setInt := func(k analysis.MetricKey, v int) {
		c.State.Set(k, v)
	}

	// Load committed baseline (what we use for idle/dynamic split).
	baselineMW, hasBase := getFloat(lastIdleKey)

	// Load candidate + run length.
	candMW := baselineMW
	if v, ok := getFloat(candKey); ok {
		candMW = v
	}
	lowRun := getInt(runKey)

	// If we have no baseline yet, seed it on the first learnable window.
	// This avoids exporting a long 0 baseline at startup.
	if !hasBase && allowLearn {
		baselineMW = resMWClamp
		if baselineMW < 0 || math.IsNaN(baselineMW) || math.IsInf(baselineMW, 0) {
			baselineMW = 0
		}
		setFloat(lastIdleKey, baselineMW)
		// Reset candidate/run
		setFloat(candKey, baselineMW)
		setInt(runKey, 0)
		candMW = baselineMW
		lowRun = 0
	}

	// Candidate-then-commit: only allow downward baseline movement if low persists.
	if allowLearn && hasBase {
		x := resMWClamp // the current residual (clamped) observation in mW

		// If x is a meaningful new low (beyond margin), track it as a candidate.
		if x < baselineMW-dropMarginMW {
			if candMW > x {
				candMW = x
			}
			lowRun++
		} else {
			// Not a sustained low: reset candidate tracking.
			candMW = baselineMW
			lowRun = 0
		}

		// Commit only if we saw a sustained low for N consecutive learnable windows.
		if lowRun >= lowPersistN {
			// Allow small, legitimate idle drops to be committed quickly,
			// but cap any permanent drop to at most maxPermDropFrac per commit.
			floor := baselineMW * (1.0 - maxPermDropFrac)

			var newBase float64
			if candMW >= floor {
				// Small drop (<= 5%): accept fully (stops "hugging" quickly).
				newBase = candMW
			} else {
				// Large drop (> 5%): cap the permanent change (accuracy-first).
				// Optionally blend slightly toward cand for stability, but never below the 5% floor.
				blend := (1.0-commitEta)*baselineMW + commitEta*candMW
				if blend < floor {
					newBase = floor
				} else {
					newBase = blend
				}
			}

			if newBase < 0 || math.IsNaN(newBase) || math.IsInf(newBase, 0) {
				newBase = 0
			}
			baselineMW = newBase
			setFloat(lastIdleKey, baselineMW)

			// Reset candidate/run so we require a new sustained low for further drops.
			candMW = baselineMW
			lowRun = 0
		}

		// Persist candidate/run even if we did not commit.
		setFloat(candKey, candMW)
		setInt(runKey, lowRun)
	}

	// If we are not learning, keep the last committed baseline (or 0 if none exists).
	if !hasBase && !allowLearn {
		baselineMW = 0
	}

	// Export baseline as a separate model-state metric (no conservation claim).
	baseLabels := analysis.Labels{"chassis": chassis}

	c.Sink.Emit(c.Ctx, analysis.Point{
		Key:    analysis.Key(analysis.MetricID(MetricResidualIdleBaselineMW), baseLabels),
		Window: c.Window,
		Unit:   "mW",
		Value:  baselineMW,
	})

	// Canonical idle/dynamic must remain conservative and conserve exactly:
	// total_clamped = idle + dynamic, with idle <= total_clamped.
	idleMW := baselineMW
	if math.IsNaN(idleMW) || math.IsInf(idleMW, 0) || idleMW < 0 {
		idleMW = 0
	}
	if idleMW > resMWClamp {
		idleMW = resMWClamp
	}
	dynMW := math.Max(0, resMWClamp-idleMW)

	idleWinMJ := idleMW * winSec
	dynWinMJ := dynMW * winSec

	emitPower := func(kind string, v float64) {
		labels := copyLabels(labelsBase)
		labels["kind"] = kind
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricResidualPowerMW, labels),
			Window: c.Window,
			Unit:   "mW",
			Value:  math.Floor(v),
		})
	}

	// total (diagnostic signed) + idle/dynamic (conservative)
	emitPower(residualKindTotal, resMW)   // may be negative (diagnostic)
	emitPower(residualKindIdle, idleMW)   // conservative
	emitPower(residualKindDynamic, dynMW) // conservative

	// ---------------------------
	// Energy counters (unchanged policy): monotone via clamp, with warmup offset handling
	// ---------------------------

	emitEnergyCounter := func(kind string, winAddMJ float64) {
		labels := copyLabels(labelsBase)
		labels["kind"] = kind

		if source == redfishSourceRaw {
			// Warmup: just accumulate raw as before.
			key := analysis.Key(MetricResidualEnergyMJ, labels)
			var prev float64
			if v, ok := c.State.Get(key); ok {
				if f, ok2 := v.(float64); ok2 {
					prev = f
				}
			}
			next := prev + winAddMJ
			c.State.Set(key, next)
			c.Sink.Emit(c.Ctx, analysis.Point{Key: key, Window: c.Window, Unit: "mJ", Value: next})
			return
		}

		// Clamp before accumulating to keep monotonic counters.
		if winAddMJ < 0 || math.IsNaN(winAddMJ) || math.IsInf(winAddMJ, 0) {
			winAddMJ = 0
		}

		// Offset key is per (chassis, kind). Seed once when corrected starts.
		offKey := analysis.Key(MetricResidualEnergyOffsetMJ, analysis.Labels{
			"chassis": chassis,
			"kind":    kind,
		})

		var offset float64
		if v, ok := c.State.Get(offKey); ok {
			if f, ok2 := v.(float64); ok2 {
				offset = f
			}
		} else {
			// Seed offset from the last raw residual counter of the same kind.
			rawKey := analysis.Key(MetricResidualEnergyMJ, analysis.Labels{
				"chassis": chassis,
				"source":  redfishSourceRaw,
				"kind":    kind,
			})
			if v2, ok2 := c.State.Get(rawKey); ok2 {
				if f2, ok3 := v2.(float64); ok3 {
					offset = f2
				}
			}
			c.State.Set(offKey, offset)
		}

		// Local corrected accumulator is stored separately (state-only).
		localKey := analysis.Key(MetricResidualEnergyLocalMJ, labels)
		var prevLocal float64
		if v, ok := c.State.Get(localKey); ok {
			if f, ok2 := v.(float64); ok2 {
				prevLocal = f
			}
		}
		newLocal := prevLocal + winAddMJ
		c.State.Set(localKey, newLocal)

		exportCum := offset + newLocal

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricResidualEnergyMJ, labels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  exportCum,
		})
	}

	emitEnergyCounter(residualKindTotal, resWinMJClamp)
	emitEnergyCounter(residualKindIdle, idleWinMJ)
	emitEnergyCounter(residualKindDynamic, dynWinMJ)

	// Keep the existing "negative residual" diagnostic if you still want it:
	// (This one is arguably valuable; if you want it hidden behind IdleDiagnostics, tell me.)
	if resMW < 0 {
		labels := copyLabels(labelsBase)
		labels["kind"] = residualKindTotal
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricResidualNegativePowerMW, labels),
			Window: c.Window,
			Unit:   "mW",
			Value:  resMW,
		})
	}

	// ---------------------------
	// Extra troubleshooting logs/metrics only when enabled
	// ---------------------------
	if config.GetIdleDiagnosticsEnabled() {
		klog.V(2).Infof(
			"[analysis] residual chassis=%q source=%q win=%s sysP=%.3f raplP=%.3f gpuP=%.3f partsP=%.3f resMW=%.3f resClamp=%.3f clamp=%v slope=%.3f deficit=%.3f transient=%v usable=%v learn=%v baseline=%.3f idle=%.3f dyn=%.3f",
			chassis, source, c.Window.String(),
			sysP, raplPW, gpuPW, partsPW, resMW, resMWClamp, resClamped,
			partsSlope, deficitMW, transient, windowUsable, allowLearn,
			baselineMW, idleMW, dynMW,
		)
	}

	return nil
}

func getSystemPower(c *analysis.Cycle) (pMW float64, chassis, source string, ok bool) {
	if c == nil || c.Store == nil {
		return 0, "", "", false
	}

	pts := c.Store.ListByID(MetricSystemPowerMW)
	if len(pts) == 0 {
		return 0, "", "", false
	}

	// Prefer corrected.
	for _, p := range pts {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["source"] == redfishSourceCorrected && p.Key.Labels["kind"] == systemKindTotal {
			ch := p.Key.Labels["chassis"]
			if ch == "" {
				ch = "Self"
			}
			return p.Value, ch, redfishSourceCorrected, true
		}
	}
	// Else raw.
	for _, p := range pts {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["source"] == redfishSourceRaw && p.Key.Labels["kind"] == systemKindTotal {
			ch := p.Key.Labels["chassis"]
			if ch == "" {
				ch = "Self"
			}
			return p.Value, ch, redfishSourceRaw, true
		}
	}

	return 0, "", "", false
}

func raplTotalPowerMW(c *analysis.Cycle) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	sum := 0.0
	for _, domain := range []string{"pkg", "dram"} {
		key := analysis.Key(
			analysis.MetricID("rapl_power_mw"),
			analysis.Labels{
				"domain": domain,
				"kind":   "total",
				"source": "rapl",
			},
		)
		p, ok := c.Store.GetExact(key)
		if !ok {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		sum += v
	}
	return sum
}

func gpuTotalPowerMW(c *analysis.Cycle) float64 {
	if c == nil || c.Store == nil {
		return 0
	}
	ps := c.Store.ListByID(analysis.MetricID(MetricGpuPowerMW))
	sum := 0.0
	for _, p := range ps {
		if p.Key.Labels == nil {
			continue
		}
		if p.Key.Labels["source"] != "nvml_corrected" {
			continue
		}
		if k := p.Key.Labels["kind"]; k != "" && k != "total" {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		sum += v
	}
	return sum
}

func copyLabels(in analysis.Labels) analysis.Labels {
	out := analysis.Labels{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
