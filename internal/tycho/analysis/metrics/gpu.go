package analysismetrics

import (
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	MetricGpuEnergyMJ analysis.MetricID = "gpu_energy_mj"
	// Optional debug later:
	// MetricGpuPowerWAvg analysis.MetricID = "gpu_power_w_avg"
)

type GpuWindowEnergy struct {
	delayTicks uint64
	quantum    time.Duration
}

func NewGpuWindowEnergy(mono *clock.Mono) *GpuWindowEnergy {
	q := time.Duration(config.TimebaseQuantumMs()) * time.Millisecond
	if q <= 0 {
		q = time.Millisecond
	}
	var dt uint64
	if mono != nil {
		dt = mono.TicksForMsCeil(config.GpuDelayMs())
	}
	return &GpuWindowEnergy{
		delayTicks: dt,
		quantum:    q,
	}
}

func (m *GpuWindowEnergy) ID() analysis.MetricID { return "gpu_window_energy" }

func (m *GpuWindowEnergy) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Gpu() != nil
}

type gpuObs struct {
	Mono     uint64
	PowerW   float64
	PowerSrc string

	// Cum energy is considered usable only if CumOK is true.
	CumOK       bool
	CumEnergyMJ uint64
}

// Fusion-lite policy (simple and swappable).
// Prefer InstantPowerMilliW if > 0, else fall back to PowerMilliW if > 0.
func selectPowerW(dev ring.GpuSample) (float64, string, bool) {
	if dev.InstantPowerMilliW > 0 {
		return float64(dev.InstantPowerMilliW) / 1000.0, "instant", true
	}
	if dev.PowerMilliW > 0 {
		return float64(dev.PowerMilliW) / 1000.0, "avg", true
	}
	return 0, "", false
}

func shiftBackClamp0(mono, dt uint64) uint64 {
	if dt == 0 {
		return mono
	}
	if mono > dt {
		return mono - dt
	}
	return 0
}

func (m *GpuWindowEnergy) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil {
		return nil
	}
	r := c.Gpu()
	if r == nil {
		return nil
	}

	// Delay-corrected window for GPU samples.
	w := c.EffectiveWindowTicks(m.delayTicks)

	seg1, seg2 := r.ViewChrono()

	// Per-UUID predecessor and in-window slices (multi-stream correctness).
	prev := map[string]gpuObs{}
	prevSet := map[string]bool{}
	inWin := map[string][]gpuObs{}

	ingestTick := func(t ring.GpuTick) {
		// Apply fixed delay correction to sample time semantics.
		tCorr := shiftBackClamp0(t.SampleMeta.Mono, m.delayTicks)

		for i := range t.Devices {
			dev := t.Devices[i]
			uuid := dev.UUID
			if uuid == "" {
				continue
			}

			powerW, src, powerOK := selectPowerW(dev)

			// Collector contract (after your change):
			// - if cumulative energy is validated usable, CumEnergyMilliJ is a positive value
			// - if invalid/unvalidated, CumEnergyMilliJ is set to pointer-to-zero (or zero)
			cumOK := false
			cumMJ := uint64(0)
			if dev.CumEnergyMilliJ != nil && *dev.CumEnergyMilliJ > 0 {
				cumOK = true
				cumMJ = *dev.CumEnergyMilliJ
			}

			// Drop observation if neither signal exists.
			if !powerOK && !cumOK {
				continue
			}

			obs := gpuObs{
				Mono:        tCorr,
				PowerW:      powerW,
				PowerSrc:    src,
				CumOK:       cumOK,
				CumEnergyMJ: cumMJ,
			}

			if tCorr < w.StartMono {
				// keep latest < start per UUID
				if !prevSet[uuid] || tCorr >= prev[uuid].Mono {
					prev[uuid] = obs
					prevSet[uuid] = true
				}
				continue
			}
			if tCorr > w.EndMono {
				continue
			}
			inWin[uuid] = append(inWin[uuid], obs)
		}
	}

	for _, t := range seg1 {
		ingestTick(t)
	}
	for _, t := range seg2 {
		ingestTick(t)
	}

	// Emit per GPU UUID.
	for uuid, winSamples := range inWin {
		labels := analysis.Labels{"gpu_uuid": uuid}

		// Fast-path: cumulative energy delta (already in mJ) if usable samples exist.
		if energyMJ, ok := integrateFromCumulativeMJ(prevSet[uuid], prev[uuid], winSamples, w); ok {
			c.Sink.Emit(c.Ctx, analysis.Point{
				Key:    analysis.Key(MetricGpuEnergyMJ, labels),
				Window: w,
				Unit:   "mJ",
				Value:  float64(energyMJ),
				Quality: &analysis.Quality{
					// Here: number of raw in-window observations examined.
					SamplesUsed: len(winSamples),
					DelayTicks:  m.delayTicks,
				},
			})
			continue
		}

		// Power integration path (ZOH). Result is J, then converted to mJ.
		xs := winSamples
		if prevSet[uuid] && prev[uuid].PowerW > 0 {
			tmp := make([]gpuObs, 0, len(winSamples)+1)
			tmp = append(tmp, prev[uuid])
			tmp = append(tmp, winSamples...)
			xs = tmp
		}
		if len(xs) == 0 {
			continue
		}

		// Do not invent pre-window power:
		// If no predecessor at/before window start, begin at first sample time.
		start := w.StartMono
		if xs[0].Mono > start {
			start = xs[0].Mono
		}
		if w.EndMono <= start {
			continue
		}

		energyJ, intervals := analysisops.IntegrateHeldValueZOH(
			xs,
			start, w.EndMono,
			func(s gpuObs) uint64 { return s.Mono },
			func(s gpuObs) float64 { return s.PowerW },
			m.quantum, // do not use mono.Quantum()
		)
		energyMJ := energyJ * 1000.0

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricGpuEnergyMJ, labels),
			Window: w,
			Unit:   "mJ",
			Value:  energyMJ,
			Quality: &analysis.Quality{
				// Match Redfish convention: interval segments integrated.
				SamplesUsed: intervals,
				DelayTicks:  m.delayTicks,
			},
		})
	}

	return nil
}

// integrateFromCumulativeMJ computes window energy from cumulative counters in mJ.
// Requires usable samples (obs.CumOK == true).
func integrateFromCumulativeMJ(hasPrev bool, prev gpuObs, win []gpuObs, w analysis.Window) (uint64, bool) {
	// last: last usable cum sample in window
	var last gpuObs
	lastSet := false
	for i := range win {
		if !win[i].CumOK {
			continue
		}
		last = win[i]
		lastSet = true
	}
	if !lastSet {
		return 0, false
	}

	// first: prefer usable predecessor at/before start; else first usable in window
	var first gpuObs
	firstSet := false
	if hasPrev && prev.CumOK && prev.Mono <= w.StartMono {
		first = prev
		firstSet = true
	} else {
		for i := range win {
			if !win[i].CumOK {
				continue
			}
			first = win[i]
			firstSet = true
			break
		}
	}
	if !firstSet {
		return 0, false
	}

	// Need distinct times to avoid emitting nonsense.
	if last.Mono <= first.Mono {
		return 0, false
	}

	// Conservative: no wrap handling here. If it decreases, treat as unusable.
	if last.CumEnergyMJ < first.CumEnergyMJ {
		return 0, false
	}
	return last.CumEnergyMJ - first.CumEnergyMJ, true
}
