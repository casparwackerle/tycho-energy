// file: internal/tycho/analysis/metrics/redifsh.go
package analysismetrics

import (
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

const (
	// Canonical system families
	MetricSystemPowerMW          analysis.MetricID = "system_power_mw"
	MetricSystemEnergyMJ         analysis.MetricID = "system_energy_mj"
	MetricSystemEnergyLocalMJ    analysis.MetricID = "system_energy_local_mj"    // state-only
	MetricSystemEnergyOffsetMJ   analysis.MetricID = "system_energy_offset_mj"   // state-only
	MetricResidualEnergyLocalMJ  analysis.MetricID = "residual_energy_local_mj"  // state-only
	MetricResidualEnergyOffsetMJ analysis.MetricID = "residual_energy_offset_mj" // state-only

	// Slice 10A provenance
	redfishSourceRaw       = "redfish_raw"
	redfishSourceCorrected = "redfish_corrected"

	systemKindTotal = "total"
)

type SystemRawFromRedfish struct {
	delayTicks uint64
}

func NewSystemRawFromRedfish() *SystemRawFromRedfish { return &SystemRawFromRedfish{} }

func (m *SystemRawFromRedfish) ID() analysis.MetricID { return "system_raw_from_redfish" }

func (m *SystemRawFromRedfish) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Sink != nil && c.Redfish() != nil && c.Mono != nil && c.State != nil
}

func (m *SystemRawFromRedfish) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Mono == nil || c.State == nil {
		return nil
	}
	r := c.Redfish()
	if r == nil {
		return nil
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()
	if monoQuantumSec <= 0 {
		return nil
	}

	// Window duration (seconds), inclusive-ish consistent with existing code base.
	windowSec := float64(c.Window.EndMono-c.Window.StartMono+1) * monoQuantumSec
	if windowSec <= 0 {
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
		// If corrected is ready for this chassis, do not emit raw canonical system metrics anymore.
		// This enforces: warmup uses raw; afterwards everything uses corrected.
		readyKey := analysis.Key(MetricFusionReady, analysis.Labels{"chassis": chassis})
		if v, ok := c.State.Get(readyKey); ok {
			if b, ok2 := v.(bool); ok2 && b {
				continue
			}
		}

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
			time.Duration(c.Mono.Quantum()),
		)

		energyMJWin := energyJ * 1000.0
		if energyMJWin < 0 {
			energyMJWin = 0
		}
		pMW := energyMJWin / windowSec

		labels := analysis.Labels{
			"chassis": chassis,
			"source":  redfishSourceRaw,
			"kind":    systemKindTotal,
		}

		// Accumulate counter in state (reset on restart is acceptable).
		energyKey := analysis.Key(MetricSystemEnergyMJ, labels)
		var prevCum float64
		if v, ok := c.State.Get(energyKey); ok {
			if f, ok2 := v.(float64); ok2 {
				prevCum = f
			}
		}
		newCum := prevCum + energyMJWin
		c.State.Set(energyKey, newCum)

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemPowerMW, labels),
			Window: c.Window,
			Unit:   "mW",
			Value:  pMW,
			Quality: &analysis.Quality{
				SamplesUsed: intervals,
				DelayTicks:  m.delayTicks,
			},
		})

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    analysis.Key(MetricSystemEnergyMJ, labels),
			Window: c.Window,
			Unit:   "mJ",
			Value:  newCum,
			Quality: &analysis.Quality{
				SamplesUsed: intervals,
				DelayTicks:  m.delayTicks,
			},
		})
	}

	return nil
}
