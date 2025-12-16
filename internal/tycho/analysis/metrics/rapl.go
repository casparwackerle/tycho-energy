package analysismetrics

import (
	"fmt"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// RaplWindowEnergy is the Slice 0 test metric plugin.
// Engine must not know anything about this plugin.
type RaplWindowEnergy struct {
	emitAggregateOnly bool
}

func NewRaplWindowEnergy() *RaplWindowEnergy {
	return &RaplWindowEnergy{emitAggregateOnly: true}
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

	// Copy only in-window samples (best-effort).
	samples := analysisctx.FilterWindowChrono[ring.RaplTick](
		r,
		c.Window.StartMono,
		c.Window.EndMono,
		func(t ring.RaplTick) uint64 { return t.SampleMeta.Mono },
	)
	if len(samples) < 2 {
		// Not enough data to form a delta.
		return nil
	}

	// For each socket, take first and last counters in window.
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
			// advance last
			fl.last = ctrs
		}
	}

	// Aggregate across sockets: emit one metric per domain label.
	var pkgSum, coreSum, uncoreSum, dramSum uint64

	for _, fl := range perSocket {
		if fl == nil || !fl.ok {
			continue
		}
		pkgSum += deltaWrapTODO(fl.first.Pkg, fl.last.Pkg)
		coreSum += deltaWrapTODO(fl.first.Core, fl.last.Core)
		uncoreSum += deltaWrapTODO(fl.first.Uncore, fl.last.Uncore)
		dramSum += deltaWrapTODO(fl.first.DRAM, fl.last.DRAM)
	}

	emit := func(domain string, v uint64) {
		p := analysis.Point{
			Key:    analysis.Key(m.ID(), analysis.Labels{"domain": domain}),
			Window: c.Window,
			Unit:   "mJ",
			Value:  float64(v),
			Quality: &analysis.Quality{
				SamplesUsed: len(samples),
				Notes:       fmt.Sprintf("sockets=%d", len(perSocket)),
			},
		}
		c.Sink.Emit(c.Ctx, p)
	}

	emit("pkg", pkgSum)
	emit("core", coreSum)
	emit("uncore", uncoreSum)
	emit("dram", dramSum)

	return nil
}

// deltaWrapTODO computes end-start assuming monotonic uint64 counters.
// If wrap handling becomes important, implement it here (or use a shared helper later).
func deltaWrapTODO(start, end uint64) uint64 {
	if end >= start {
		return end - start
	}
	// TODO: proper wrap handling with known counter width/modulus.
	// For Slice 0: best-effort, treat as zero.
	return 0
}
