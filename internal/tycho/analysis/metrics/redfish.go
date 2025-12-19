package analysismetrics

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

const MetricRedfishSystemEnergyJ analysis.MetricID = "redfish_system_energy_j"

type RedfishWindowEnergy struct{}

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

	w := c.EffectiveWindowTicks(0)
	all := analysisctx.FilterWindowWithPrevChrono[ring.RedfishSample](
		r, w.StartMono, w.EndMono,
		func(s ring.RedfishSample) uint64 { return s.Mono },
	)

	// Build per-chassis prev + window slices.
	prev := map[string]ring.RedfishSample{}
	prevSet := map[string]bool{}
	inWin := map[string][]ring.RedfishSample{}

	for i := range all {
		s := all[i]
		ch := s.ChassisID
		m := s.Mono

		if m < w.StartMono {
			// keep latest < start per chassis
			if !prevSet[ch] || m >= prev[ch].Mono {
				prev[ch] = s
				prevSet[ch] = true
			}
			continue
		}
		if m > w.EndMono {
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

		energyJ, intervals := analysisops.IntegrateHeldValueZOH(
			xs,
			w.StartMono, w.EndMono,
			func(s ring.RedfishSample) uint64 { return s.Mono },
			func(s ring.RedfishSample) float64 { return s.PowerWatts },
			c.Mono.Quantum(),
		)

		labels := analysis.Labels{"chassis": chassis}

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricRedfishSystemEnergyJ, labels),
			Window: w,
			Unit:   "J",
			Value:  energyJ,
			Quality: &analysis.Quality{
				SamplesUsed: intervals, // interval segments integrated (not raw samples)
				DelayTicks:  0,
			},
		})
	}

	return nil
}
