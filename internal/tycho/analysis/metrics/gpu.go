// file: internal/tycho/analysis/metrics/gpu.go
package analysismetrics

import (
	"math"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	gpufuse "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/gpu"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	MetricGpuEnergyMJ analysis.MetricID = "gpu_energy_mj"
)

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
	if c == nil || c.Sink == nil || c.Gpu() == nil || c.Mono == nil {
		return nil
	}

	// Corrected analysis-window end (aligned to Cycle.Window, not safety offset).
	endTick := c.Window.EndMono
	endSec := tickToAbsSec(c.Mono, endTick)

	// Analysis window duration (seconds) derived from ticks.
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

			// Cumulative energy (mJ), optional.
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

	// Reconstruct per GPU UUID, maintain corrected history incrementally, emit window energy.
	for uuid, a := range byUUID {
		if uuid == "" || a == nil {
			continue
		}

		// Build/refresh the full history grid by copying previous series overlap.
		histPW := make([]float64, desiredN)

		prev, hasPrev := gpufuse.GetCorrectedSeries(c.State, uuid)
		if hasPrev && prev.DtSec > 0 && math.Abs(prev.DtSec-dtSec) < 1e-9 {
			// Snap desiredT0 to previous grid to avoid drift
			k := math.Round((desiredT0 - prev.T0Sec) / dtSec)
			desiredT0 = prev.T0Sec + k*dtSec
			if desiredT0 < 0 {
				desiredT0 = 0
			}
		}

		// Solve only the tail and overwrite that suffix in the history grid.
		tailSer, ok := gpufuse.Reconstruct(
			uuid,
			endSec,
			tailDur,
			tailDur, // apply constraints across the tail duration
			dtSec,
			a.inst,
			a.avg,
			a.cum,
			useEnergy,
		)
		if !ok || tailSer.DtSec <= 0 || len(tailSer.PW) == 0 {
			// If we cannot solve at all, keep whatever we copied and still try to emit energy if possible.
			// But if there is no previous overlap either, we have nothing.
			if !hasPrev {
				continue
			}
		} else {
			overwriteTail(histPW, desiredT0, dtSec, tailSer)
		}

		merged := gpufuse.Series{
			UUID:  uuid,
			T0Sec: desiredT0,
			DtSec: dtSec,
			PW:    histPW,
		}
		// Keep last-solve diagnostics if you want them internally; otherwise omit.
		merged.Last = tailSer.Last

		// Persist corrected history for future slices.
		gpufuse.PutCorrectedSeries(c.State, uuid, merged)

		// Emit window energy using the single canonical integrator.
		monoQuantumSec := c.Mono.Quantum().Seconds()
		energyMJ, okE := gpufuse.EnergyMJ(merged, c.Window, monoQuantumSec)
		if !okE {
			continue
		}

		labels := analysis.Labels{"gpu_uuid": uuid}
		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricGpuEnergyMJ, labels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  energyMJ,
			Quality: &analysis.Quality{
				SamplesUsed: a.rawTicks,
				DelayTicks:  m.delayTicks,
			},
		})
	}

	return nil
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
