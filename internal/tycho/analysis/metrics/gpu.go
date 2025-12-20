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

// Prefer InstantPowerMilliW if > 0, else PowerMilliW if > 0.
func selectPowerW(dev ring.GpuSample) (float64, string, bool) {
	if dev.InstantPowerMilliW > 0 {
		return float64(dev.InstantPowerMilliW) / 1000.0, "instant", true
	}
	if dev.PowerMilliW > 0 {
		return float64(dev.PowerMilliW) / 1000.0, "avg", true
	}
	return 0, "", false
}

func (m *GpuWindowEnergy) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil {
		return nil
	}
	r := c.Gpu()
	if r == nil {
		return nil
	}

	// Raw-sample selection window (forward-shifted).
	wEff := c.EffectiveWindowTicks(m.delayTicks)

	seg1, seg2 := r.ViewChrono()

	prev := map[string]gpuObs{}
	prevSet := map[string]bool{}
	inWin := map[string][]gpuObs{}

	ingestTick := func(t ring.GpuTick) {
		tRaw := t.SampleMeta.Mono // keep raw sample time; delay handled via wEff

		for i := range t.Devices {
			dev := t.Devices[i]
			uuid := dev.UUID
			if uuid == "" {
				continue
			}

			powerW, src, powerOK := selectPowerW(dev)

			cumOK := false
			cumMJ := uint64(0)
			if dev.CumEnergyMilliJ != nil && *dev.CumEnergyMilliJ > 0 {
				cumOK = true
				cumMJ = *dev.CumEnergyMilliJ
			}

			if !powerOK && !cumOK {
				continue
			}

			obs := gpuObs{
				Mono:        tRaw,
				PowerW:      powerW,
				PowerSrc:    src,
				CumOK:       cumOK,
				CumEnergyMJ: cumMJ,
			}

			if tRaw < wEff.StartMono {
				if !prevSet[uuid] || tRaw >= prev[uuid].Mono {
					prev[uuid] = obs
					prevSet[uuid] = true
				}
				continue
			}
			if tRaw > wEff.EndMono {
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

	for uuid, winSamples := range inWin {
		labels := analysis.Labels{"gpu_uuid": uuid}

		// Fast-path: cumulative energy delta (already in mJ).
		if energyMJ, ok := integrateFromCumulativeMJ(prevSet[uuid], prev[uuid], winSamples, wEff); ok {
			c.Sink.Emit(c.Ctx, analysis.Point{
				Key:    analysis.Key(MetricGpuEnergyMJ, labels),
				Window: c.Window, // corrected shared window
				Unit:   "mJ",
				Value:  float64(energyMJ),
				Quality: &analysis.Quality{
					SamplesUsed: len(winSamples),
					DelayTicks:  m.delayTicks,
				},
			})
			continue
		}

		// Power integration path (ZOH). Result is J, convert to mJ.
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

		// If no predecessor at/before window start, begin at first sample time.
		start := wEff.StartMono
		if xs[0].Mono > start {
			start = xs[0].Mono
		}
		if wEff.EndMono <= start {
			continue
		}

		energyJ, intervals := analysisops.IntegrateHeldValueZOH(
			xs,
			start, wEff.EndMono,
			func(s gpuObs) uint64 { return s.Mono },
			func(s gpuObs) float64 { return s.PowerW },
			m.quantum,
		)
		energyMJ := energyJ * 1000.0

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricGpuEnergyMJ, labels),
			Window: c.Window, // corrected shared window
			Unit:   "mJ",
			Value:  energyMJ,
			Quality: &analysis.Quality{
				SamplesUsed: intervals,
				DelayTicks:  m.delayTicks,
			},
		})
	}

	return nil
}

// integrateFromCumulativeMJ computes window energy from cumulative counters in mJ.
// Requires usable samples (obs.CumOK == true).
func integrateFromCumulativeMJ(hasPrev bool, prev gpuObs, win []gpuObs, wEff analysis.Window) (uint64, bool) {
	// last usable cum sample in window
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
	if hasPrev && prev.CumOK && prev.Mono <= wEff.StartMono {
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

	if last.Mono <= first.Mono {
		return 0, false
	}
	if last.CumEnergyMJ < first.CumEnergyMJ {
		return 0, false
	}
	return last.CumEnergyMJ - first.CumEnergyMJ, true
}
