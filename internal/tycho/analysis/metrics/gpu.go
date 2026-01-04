// file: internal/tycho/analysis/metrics/gpu.go
package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	gpufuse "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/gpu"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/idle"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	MetricGpuEnergyMJ    analysis.MetricID = "gpu_energy_mj"
	MetricGpuEnergyState analysis.MetricID = "gpu_energy_state"
)

type GpuEnergyState struct {
	TotalMJ   float64
	IdleMJ    float64
	DynamicMJ float64

	NativeLastMJ  float64
	NativeHasLast bool
}

type GpuWindowEnergy struct {
	delayTicks uint64
}

func NewGpuWindowEnergy(mono *clock.Mono) *GpuWindowEnergy {
	var dt uint64
	if mono != nil {
		dt = mono.TicksForMsCeil(config.GpuDelayMs())
	}
	return &GpuWindowEnergy{delayTicks: dt}
}

func (m *GpuWindowEnergy) ID() analysis.MetricID { return "gpu_window_energy" }

func (m *GpuWindowEnergy) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Gpu() != nil && c.Mono != nil
}

func (m *GpuWindowEnergy) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Gpu() == nil || c.Mono == nil || c.State == nil {
		return nil
	}

	// Corrected analysis-window end (aligned to Cycle.Window).
	endTick := c.Window.EndMono
	endSec := tickToAbsSec(c.Mono, endTick)

	// Window duration (seconds) derived from ticks.
	winTicks := uint64(0)
	if c.Window.EndMono > c.Window.StartMono {
		winTicks = c.Window.EndMono - c.Window.StartMono
	}
	winSec := ticksToDurSec(c.Mono, winTicks)
	if winSec <= 0 {
		return nil
	}

	// Config knobs.
	dtMs := config.GpuQuantumMs()
	if dtMs <= 0 {
		dtMs = 50
	}
	dtSec := float64(dtMs) / 1000.0

	histSec := config.GpuHistoryWindowSec()
	if histSec <= 0 {
		if bw := config.BufferWindowSec(); bw > 0 {
			histSec = bw
		} else {
			histSec = 90
		}
	}

	solveSec := config.GpuSolveWindowSec()
	if solveSec <= 0 {
		solveSec = int(math.Ceil(winSec))
	}
	if solveSec > histSec {
		solveSec = histSec
	}

	useEnergy := config.EnableGpuEnergyConstraints()

	// Raw selection window for building/maintaining corrected history.
	histTicks := c.Mono.TicksForDurationCeil(time.Duration(histSec) * time.Second)
	rawEnd := endTick + m.delayTicks
	rawStart := uint64(0)
	if rawEnd > histTicks {
		rawStart = rawEnd - histTicks
	}
	rawW := analysis.Window{StartMono: rawStart, EndMono: rawEnd}

	seg1, seg2 := c.Gpu().ViewChrono()

	type acc struct {
		rawTicks int
		inst     []gpufuse.PowerObs
		avg      []gpufuse.PowerObs
		cum      []gpufuse.CumEnergyObs
	}
	byUUID := map[string]*acc{}

	ingestTick := func(t ring.GpuTick) {
		tRaw := t.SampleMeta.Mono
		if tRaw < rawW.StartMono || tRaw > rawW.EndMono {
			return
		}
		for i := range t.Devices {
			dev := t.Devices[i]
			uuid := dev.UUID
			if uuid == "" {
				continue
			}
			a := byUUID[uuid]
			if a == nil {
				a = &acc{
					inst: make([]gpufuse.PowerObs, 0, 128),
					avg:  make([]gpufuse.PowerObs, 0, 128),
					cum:  make([]gpufuse.CumEnergyObs, 0, 32),
				}
				byUUID[uuid] = a
			}
			a.rawTicks++

			// Corrected tick (event time): raw - delayTicks (clamp at 0).
			tCorr := tRaw
			if tCorr > m.delayTicks {
				tCorr = tCorr - m.delayTicks
			} else {
				tCorr = 0
			}
			tSec := tickToAbsSec(c.Mono, tCorr)

			// Instant power (W).
			if dev.InstantPowerMilliW > 0 {
				a.inst = append(a.inst, gpufuse.PowerObs{
					TSec:  tSec,
					Power: float64(dev.InstantPowerMilliW) / 1000.0,
				})
			}

			// 1-second average power (W).
			if dev.PowerMilliW > 0 {
				a.avg = append(a.avg, gpufuse.PowerObs{
					TSec:  tSec,
					Power: float64(dev.PowerMilliW) / 1000.0,
				})
			}

			// Native cumulative energy (mJ), optional.
			if dev.CumEnergyMilliJ != nil && *dev.CumEnergyMilliJ > 0 {
				a.cum = append(a.cum, gpufuse.CumEnergyObs{
					TSec:     tSec,
					EnergyMJ: *dev.CumEnergyMilliJ,
				})
			}
		}
	}

	for _, t := range seg1 {
		ingestTick(t)
	}
	for _, t := range seg2 {
		ingestTick(t)
	}

	// Desired history grid (corrected time).
	desiredT0 := endSec - float64(histSec)
	if desiredT0 < 0 {
		desiredT0 = 0
	}
	desiredN := int(math.Floor((endSec-desiredT0)/dtSec)) + 1
	if desiredN <= 0 {
		return nil
	}

	// Tail solve region [endSec-solveSec, endSec].
	tailT0 := endSec - float64(solveSec)
	if tailT0 < desiredT0 {
		tailT0 = desiredT0
	}
	tailDur := endSec - tailT0
	if tailDur <= 0 {
		return nil
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()

	// Helpers for per-GPU state (native tracking lives inside this same state).
	getEnergyState := func(uuid string) (GpuEnergyState, bool) {
		if uuid == "" {
			return GpuEnergyState{}, false
		}
		key := analysis.Key(MetricGpuEnergyState, analysis.Labels{"gpu_uuid": uuid})
		v, ok := c.State.Get(key)
		if !ok {
			return GpuEnergyState{}, false
		}
		st, ok := v.(GpuEnergyState)
		return st, ok
	}
	putEnergyState := func(uuid string, st GpuEnergyState) {
		if uuid == "" {
			return
		}
		key := analysis.Key(MetricGpuEnergyState, analysis.Labels{"gpu_uuid": uuid})
		c.State.Set(key, st)
	}

	latestNativeMJAtOrBefore := func(a *acc, tSec float64) (float64, bool) {
		if a == nil || len(a.cum) == 0 {
			return 0, false
		}
		// a.cum is appended in chrono order, but be defensive: pick the latest <= tSec.
		found := false
		var bestT float64
		var bestMJ float64
		for i := range a.cum {
			o := a.cum[i]
			if o.TSec > tSec {
				continue
			}
			if !found || o.TSec >= bestT {
				found = true
				bestT = o.TSec
				bestMJ = float64(o.EnergyMJ)
			}
		}
		if !found {
			return 0, false
		}
		return bestMJ, true
	}

	// Reconstruct per GPU UUID, persist corrected series, emit:
	// - gpu_energy_window_mj{kind="total"} (aux)
	// - gpu_power_mw{kind="total"} (contract)
	for uuid, a := range byUUID {
		if uuid == "" || a == nil {
			continue
		}

		// Build/refresh the full history grid (tail only).
		histPW := make([]float64, desiredN)

		prev, hasPrev := gpufuse.GetCorrectedSeries(c.State, uuid)
		if hasPrev && prev.DtSec > 0 && math.Abs(prev.DtSec-dtSec) < 1e-9 {
			// Snap desiredT0 to previous grid to avoid drift.
			k := math.Round((desiredT0 - prev.T0Sec) / dtSec)
			desiredT0 = prev.T0Sec + k*dtSec
			if desiredT0 < 0 {
				desiredT0 = 0
			}
		}

		tailSer, ok := gpufuse.Reconstruct(
			uuid,
			endSec,
			tailDur,
			tailDur,
			dtSec,
			a.inst,
			a.avg,
			a.cum,
			useEnergy,
		)

		if ok && tailSer.DtSec > 0 && len(tailSer.PW) > 0 {
			overwriteTail(histPW, desiredT0, dtSec, tailSer)
			merged := gpufuse.Series{
				UUID:  uuid,
				T0Sec: desiredT0,
				DtSec: dtSec,
				PW:    histPW,
				Last:  tailSer.Last,
			}
			gpufuse.PutCorrectedSeries(c.State, uuid, merged)
		} else if hasPrev {
			// Keep previous corrected series if we cannot solve now.
			gpufuse.PutCorrectedSeries(c.State, uuid, prev)
		} else {
			// No corrected history at all; we can still emit power/energy only if you later add a raw-only fallback.
			continue
		}

		// Decide energyWindowMJ:
		// Prefer native cumulative energy when available and when we have a previous native baseline.
		energyWindowMJ := 0.0
		usedNative := false

		nativeNowMJ, okNative := latestNativeMJAtOrBefore(a, endSec)

		st, stOk := getEnergyState(uuid)
		if !stOk {
			st = GpuEnergyState{}
		}

		// These fields must exist in your GpuEnergyState for native support:
		//   NativeLastMJ  float64
		//   NativeHasLast bool
		if okNative {
			if st.NativeHasLast {
				nativeWindowMJ := nativeNowMJ - st.NativeLastMJ
				if !math.IsNaN(nativeWindowMJ) && !math.IsInf(nativeWindowMJ, 0) && nativeWindowMJ >= 0 {
					energyWindowMJ = nativeWindowMJ
					usedNative = true
				}
			}
			// Always refresh the baseline when we see native.
			st.NativeLastMJ = nativeNowMJ
			st.NativeHasLast = true
			putEnergyState(uuid, st)
		}

		if !usedNative {
			// Fall back to integration over corrected series.
			ser, _ := gpufuse.GetCorrectedSeries(c.State, uuid)
			energyIntegratedMJ, okE := gpufuse.EnergyMJ(ser, c.Window, monoQuantumSec)
			if !okE {
				continue
			}
			if math.IsNaN(energyIntegratedMJ) || math.IsInf(energyIntegratedMJ, 0) || energyIntegratedMJ < 0 {
				energyIntegratedMJ = 0
			}
			energyWindowMJ = energyIntegratedMJ
		}

		// Power gauge derived from this window increment.
		powerTotalMW := 0.0
		if winSec > 0 {
			powerTotalMW = energyWindowMJ / winSec
		}

		labelsTotal := analysis.Labels{"gpu_uuid": uuid, "kind": "total"}

		// Aux: per-window energy increment (not part of contract).
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(analysis.MetricID("gpu_energy_window_mj"), labelsTotal),
			Window: c.Window,
			Unit:   "mJ",
			Value:  energyWindowMJ,
			Quality: &analysis.Quality{
				SamplesUsed: a.rawTicks,
				DelayTicks:  m.delayTicks,
			},
		})

		// Contract: total power gauge.
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(analysis.MetricID("gpu_power_mw"), labelsTotal),
			Window: c.Window,
			Unit:   "mW",
			Value:  powerTotalMW,
			Quality: &analysis.Quality{
				SamplesUsed: a.rawTicks,
				DelayTicks:  m.delayTicks,
			},
		})
	}

	return nil
}

// copyOverlapInto copies the overlapping part of prev into the new history grid histPW.
// histPW represents [newT0, newEnd] on step dt.
func copyOverlapInto(histPW []float64, newT0, dt, newEnd float64, prev gpufuse.Series) {
	if len(histPW) == 0 || dt <= 0 || prev.DtSec <= 0 || len(prev.PW) == 0 {
		return
	}

	prevT0 := prev.T0Sec
	prevDt := prev.DtSec
	prevN := len(prev.PW)
	prevEnd := prevT0 + float64(prevN)*prevDt

	// Overlap interval.
	a := math.Max(newT0, prevT0)
	b := math.Min(newEnd, prevEnd)
	if b <= a {
		return
	}

	// Map overlap onto new grid and prev grid. We assume dt matches and grids are aligned enough.
	// We copy bin-by-bin using time -> index conversions.
	newI0 := int(math.Floor((a - newT0) / dt))
	newI1 := int(math.Ceil((b - newT0) / dt))
	if newI0 < 0 {
		newI0 = 0
	}
	if newI1 > len(histPW) {
		newI1 = len(histPW)
	}
	for i := newI0; i < newI1; i++ {
		t := newT0 + float64(i)*dt
		j := int(math.Floor((t - prevT0) / prevDt))
		if j < 0 || j >= prevN {
			continue
		}
		histPW[i] = prev.PW[j]
	}
}

// overwriteTail overwrites the segment covered by tailSer into histPW.
// histPW represents [newT0, ...] on dt; tailSer represents [tailT0, ...] on dt.
func overwriteTail(histPW []float64, newT0, dt float64, tailSer gpufuse.Series) {
	if len(histPW) == 0 || dt <= 0 || tailSer.DtSec <= 0 || len(tailSer.PW) == 0 {
		return
	}
	if math.Abs(tailSer.DtSec-dt) >= 1e-9 {
		// Should not happen; if it does, we refuse to merge.
		return
	}

	tailT0 := tailSer.T0Sec
	for j := 0; j < len(tailSer.PW); j++ {
		t := tailT0 + float64(j)*dt
		i := int(math.Floor((t - newT0) / dt))
		if i < 0 || i >= len(histPW) {
			continue
		}
		histPW[i] = tailSer.PW[j]
	}
}

const (
	MetricGpuIdlePowerMW  analysis.MetricID = "gpu_idle_power_mw"
	MetricGpuDynPowerMW   analysis.MetricID = "gpu_dyn_power_mw"
	MetricGpuIdleEnergyMJ analysis.MetricID = "gpu_idle_energy_mj"
	MetricGpuDynEnergyMJ  analysis.MetricID = "gpu_dyn_energy_mj"
)

type GpuIdleDynamic struct {
	cfg idle.Config
}

func NewGpuIdleDynamic() *GpuIdleDynamic {
	return &GpuIdleDynamic{cfg: idle.DefaultConfig()}
}

func (m *GpuIdleDynamic) ID() analysis.MetricID { return "gpu_idle_dynamic" }

func (m *GpuIdleDynamic) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil && c.Gpu() != nil
}

func (m *GpuIdleDynamic) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil || c.Gpu() == nil {
		return nil
	}

	// Use exactly one duration source for consistency across total/idle/dynamic.
	dtSec := windowDurSec(c)
	if dtSec <= 0 {
		return nil
	}

	// Per-GPU utilization (u_sm,u_mem) from the latest tick mapping into corrected window.
	delayTicks := c.Mono.TicksForMsCeil(config.GpuDelayMs())
	uMap := latestGpuUtilInWindow(c, delayTicks)

	// We require both:
	// - total power gauge (for idle model)
	// - total window energy (for counter increment)
	totalPowerPoints := c.Store.ListByID(analysis.MetricID("gpu_power_mw"))
	totalEnergyWindowPoints := c.Store.ListByID(analysis.MetricID("gpu_energy_window_mj"))
	if len(totalPowerPoints) == 0 || len(totalEnergyWindowPoints) == 0 {
		return nil
	}

	// Build lookup uuid -> totalWindowMJ and uuid -> totalPowerMW (only kind="total").
	totalWindowMJByUUID := make(map[string]float64, 8)
	for _, p := range totalEnergyWindowPoints {
		uuid := ""
		kind := ""
		if p.Key.Labels != nil {
			uuid = p.Key.Labels["gpu_uuid"]
			kind = p.Key.Labels["kind"]
		}
		if uuid == "" {
			continue
		}
		if kind != "" && kind != "total" {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		totalWindowMJByUUID[uuid] = v
	}

	totalPowerMWByUUID := make(map[string]float64, 8)
	for _, p := range totalPowerPoints {
		uuid := ""
		kind := ""
		if p.Key.Labels != nil {
			uuid = p.Key.Labels["gpu_uuid"]
			kind = p.Key.Labels["kind"]
		}
		if uuid == "" {
			continue
		}
		if kind != "" && kind != "total" {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		totalPowerMWByUUID[uuid] = v
	}

	if len(totalWindowMJByUUID) == 0 || len(totalPowerMWByUUID) == 0 {
		return nil
	}

	now := time.Now()

	getEnergyState := func(uuid string) GpuEnergyState {
		key := analysis.Key(MetricGpuEnergyState, analysis.Labels{"gpu_uuid": uuid})
		v, ok := c.State.Get(key)
		if !ok {
			return GpuEnergyState{}
		}
		st, ok := v.(GpuEnergyState)
		if !ok {
			return GpuEnergyState{}
		}
		return st
	}
	putEnergyState := func(uuid string, st GpuEnergyState) {
		key := analysis.Key(MetricGpuEnergyState, analysis.Labels{"gpu_uuid": uuid})
		c.State.Set(key, st)
	}

	for uuid, totalWindowMJ := range totalWindowMJByUUID {
		if uuid == "" {
			continue
		}

		totalPowerMW, ok := totalPowerMWByUUID[uuid]
		if !ok {
			// Without power, we cannot estimate idle. Skip to avoid inconsistent updates.
			continue
		}

		// Idle model inputs.
		u1, u2 := uMap[uuid][0], uMap[uuid][1]

		modelName := "gpu_" + uuid
		mod := idle.GetOrInitVec2(c.State, modelName, m.cfg)
		if mod == nil {
			continue
		}

		_ = mod.Observe(u1, u2, totalPowerMW, now)
		betaMW, q := mod.Estimate()

		// Clamp idle power to [0, totalPowerMW].
		idleMW := betaMW
		if math.IsNaN(idleMW) || math.IsInf(idleMW, 0) || idleMW < 0 {
			idleMW = 0
		}
		if idleMW > totalPowerMW {
			idleMW = totalPowerMW
		}
		dynMW := totalPowerMW - idleMW
		if dynMW < 0 {
			dynMW = 0
		}

		// Window energy increments for idle/dynamic computed from power and the same dtSec.
		idleWindowMJ := idleMW * dtSec
		dynWindowMJ := dynMW * dtSec

		// Persist all three counters together.
		st := getEnergyState(uuid)
		st.TotalMJ += totalWindowMJ
		st.IdleMJ += idleWindowMJ
		st.DynamicMJ += dynWindowMJ

		if math.IsNaN(st.TotalMJ) || math.IsInf(st.TotalMJ, 0) || st.TotalMJ < 0 {
			st.TotalMJ = 0
		}
		if math.IsNaN(st.IdleMJ) || math.IsInf(st.IdleMJ, 0) || st.IdleMJ < 0 {
			st.IdleMJ = 0
		}
		if math.IsNaN(st.DynamicMJ) || math.IsInf(st.DynamicMJ, 0) || st.DynamicMJ < 0 {
			st.DynamicMJ = 0
		}

		putEnergyState(uuid, st)

		labelsTotal := analysis.Labels{"gpu_uuid": uuid, "kind": "total"}
		labelsIdle := analysis.Labels{"gpu_uuid": uuid, "kind": "idle"}
		labelsDyn := analysis.Labels{"gpu_uuid": uuid, "kind": "dynamic"}

		// Emit contract power gauges (total already exists, but re-emitting is fine; keep or remove as you prefer).
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(analysis.MetricID("gpu_power_mw"), labelsTotal), Window: c.Window, Unit: "mW", Value: totalPowerMW})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(analysis.MetricID("gpu_power_mw"), labelsIdle), Window: c.Window, Unit: "mW", Value: idleMW})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(analysis.MetricID("gpu_power_mw"), labelsDyn), Window: c.Window, Unit: "mW", Value: dynMW})

		// Emit contract cumulative energy counters.
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuEnergyMJ, labelsTotal), Window: c.Window, Unit: "mJ", Value: st.TotalMJ})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuEnergyMJ, labelsIdle), Window: c.Window, Unit: "mJ", Value: st.IdleMJ})
		c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricGpuEnergyMJ, labelsDyn), Window: c.Window, Unit: "mJ", Value: st.DynamicMJ})

		diag := config.GetIdleDiagnosticsEnabled()
		if diag {
			ql := analysis.Labels{"name": modelName, "gpu_uuid": uuid, "mode": q.Mode}
			c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelReady, ql), Window: c.Window, Unit: "bool", Value: boolToFloat(q.Ready)})
			c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBins, ql), Window: c.Window, Unit: "count", Value: float64(q.BinsPopulated)})
			c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelPoints, ql), Window: c.Window, Unit: "count", Value: float64(q.TotalPoints)})
			c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelMode, ql), Window: c.Window, Unit: "enum", Value: modeToFloat(q.Mode)})
			c.Sink.Emit(c.Ctx, analysis.Point{Key: analysis.Key(MetricIdleModelBetaMW, ql), Window: c.Window, Unit: "mW", Value: q.Beta})
		}
	}

	return nil
}

func latestGpuUtilInWindow(c *analysis.Cycle, delayTicks uint64) map[string][2]float64 {
	out := make(map[string][2]float64, 8)
	if c == nil || c.Gpu() == nil {
		return out
	}

	// Raw selection window: we want corrected time in [Window.StartMono,Window.EndMono]
	// corrected = raw - delayTicks => raw in window shifted forward
	rawW := c.Window.ShiftForward(delayTicks)

	seg1, seg2 := c.Gpu().ViewChrono()

	// Walk chronological segments, keep last per uuid.
	ingest := func(seg []ring.GpuTick) {
		for _, t := range seg {
			tRaw := t.SampleMeta.Mono
			if tRaw < rawW.StartMono || tRaw > rawW.EndMono {
				continue
			}
			for i := range t.Devices {
				d := t.Devices[i]
				uuid := d.UUID
				if uuid == "" {
					continue
				}
				u1 := clamp01(d.SMUtilPct / 100.0)
				u2 := clamp01(d.MemUtilPct / 100.0)
				out[uuid] = [2]float64{u1, u2}
			}
		}
	}

	ingest(seg1)
	ingest(seg2)
	return out
}
