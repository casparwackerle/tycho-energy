// file: internal/tycho/analysis/metrics/redifsh.go
package analysismetrics

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const MetricRedfishSystemEnergyMJ analysis.MetricID = "redfish_system_energy_mj"

// Slice 10A: fixed provenance label for raw Redfish-derived system observation.
const redfishSourceRaw = "redfish_raw"

type RedfishWindowEnergy struct {
	delayTicks uint64
}

func NewRedfishWindowEnergy() *RedfishWindowEnergy { return &RedfishWindowEnergy{} }

func (m *RedfishWindowEnergy) ID() analysis.MetricID { return "redfish_window_energy" }

func (m *RedfishWindowEnergy) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Redfish() != nil && c.Mono != nil
}

func (m *RedfishWindowEnergy) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Mono == nil {
		return nil
	}
	r := c.Redfish()
	if r == nil {
		return nil
	}

	// Delay is configurable (often 0). Still use the same code path.
	m.delayTicks = c.Mono.TicksForMsCeil(config.RedfishDelayMs())

	// Raw-sample selection window (forward-shifted).
	wEff := c.EffectiveWindowTicks(m.delayTicks)

	all := analysisctx.FilterWindowWithPrevChrono[ring.RedfishSample](
		r, wEff.StartMono, wEff.EndMono,
		func(s ring.RedfishSample) uint64 { return s.Mono },
	)

	prev := map[string]ring.RedfishSample{}
	prevSet := map[string]bool{}
	inWin := map[string][]ring.RedfishSample{}

	for i := range all {
		s := all[i]
		ch := s.ChassisID
		t := s.Mono

		if t < wEff.StartMono {
			if !prevSet[ch] || t >= prev[ch].Mono {
				prev[ch] = s
				prevSet[ch] = true
			}
			continue
		}
		if t > wEff.EndMono {
			continue
		}
		inWin[ch] = append(inWin[ch], s)
	}

	for chassis, winSamples := range inWin {
		xs := winSamples
		if prevSet[chassis] {
			tmp := make([]ring.RedfishSample, 0, len(winSamples)+1)
			tmp = append(tmp, prev[chassis])
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
			func(s ring.RedfishSample) uint64 { return s.Mono },
			func(s ring.RedfishSample) float64 { return s.PowerWatts },
			c.Mono.Quantum(),
		)

		labels := analysis.Labels{
			"chassis": chassis,
			"source":  redfishSourceRaw,
		}
		energyMJ := energyJ * 1000.0

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricRedfishSystemEnergyMJ, labels),
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
