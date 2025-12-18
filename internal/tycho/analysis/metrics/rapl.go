package analysismetrics

import (
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

// RaplWindowEnergy is the Slice 0+ metric plugin.
// Source-agnostic: expects normalized, non-wrapping absolute counters from the collector pipeline.
type RaplWindowEnergy struct {
	mono       *clock.Mono
	delayTicks uint64
}

func NewRaplWindowEnergy(mono *clock.Mono) *RaplWindowEnergy {
	return &RaplWindowEnergy{
		mono:       mono,
		delayTicks: mono.TicksForMsCeil(config.RaplDelayMs()),
	}
}

func (m *RaplWindowEnergy) ID() analysis.MetricID { return "rapl_energy_mj" }

func (m *RaplWindowEnergy) IsEnabled(c *analysis.Cycle) bool {
	return c != nil && c.Rapl() != nil
}

func (m *RaplWindowEnergy) Run(c *analysis.Cycle) error {
	r := c.Rapl()
	if r == nil {
		return nil
	}

	// Compute effective window ONCE for this metric source and reuse it.
	w := c.EffectiveWindowTicks(m.delayTicks)

	// Read only in-window samples (best-effort).
	samples := analysisctx.FilterWindowChrono[ring.RaplTick](
		r,
		w.StartMono,
		w.EndMono,
		func(t ring.RaplTick) uint64 { return t.SampleMeta.Mono },
	)
	if len(samples) < 2 {
		return nil
	}

	// For each socket: first/last counters.
	type firstLast struct {
		first ring.RaplDomainCounters
		last  ring.RaplDomainCounters
		ok    bool
	}
	perSocket := map[int]*firstLast{}

	for _, tick := range samples {
		for socketID, ctrs := range tick.Sockets {
			fl, ok := perSocket[socketID]
			if !ok {
				perSocket[socketID] = &firstLast{first: ctrs, last: ctrs, ok: true}
				continue
			}
			fl.last = ctrs
		}
	}

	// Aggregate deltas across sockets (non-wrapping monotonic semantics).
	var pkgSum, coreSum, uncoreSum, dramSum uint64
	for _, fl := range perSocket {
		if fl == nil || !fl.ok {
			continue
		}
		pkgSum += analysisops.DeltaU64(fl.first.Pkg, fl.last.Pkg)
		coreSum += analysisops.DeltaU64(fl.first.Core, fl.last.Core)
		uncoreSum += analysisops.DeltaU64(fl.first.Uncore, fl.last.Uncore)
		dramSum += analysisops.DeltaU64(fl.first.DRAM, fl.last.DRAM)
	}

	// Minimal, structured quality: no duplication inside Notes.
	q := &analysis.Quality{
		SamplesUsed: len(samples),
		SocketsUsed: len(perSocket),
		DelayTicks:  m.delayTicks,
		Notes:       "", // keep empty unless you have truly exceptional context
	}

	emit := func(domain string, v uint64) {
		p := analysis.Point{
			Key:     analysis.Key(m.ID(), analysis.Labels{"domain": domain}),
			Window:  w,
			Unit:    "mJ",
			Value:   float64(v),
			Quality: q,
		}
		c.Sink.Emit(c.Ctx, p)
	}

	emit("pkg", pkgSum)
	emit("core", coreSum)
	emit("uncore", uncoreSum)
	emit("dram", dramSum)
	return nil
}
