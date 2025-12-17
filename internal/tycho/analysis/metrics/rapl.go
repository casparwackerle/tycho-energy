package analysismetrics

import (
	"fmt"
	"sync"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisctx "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/context"
	analysisops "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/operators"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/components/source"
	"k8s.io/klog/v2"
)

// RaplWindowEnergy is the Slice 0+ metric plugin.
// Now includes: fixed per-source delay + wrap-aware delta via analysis/ops.
type RaplWindowEnergy struct {
	mono       *clock.Mono
	delayTicks uint64

	// wrap modulus in mJ per domain (same across sockets). Cached lazily.
	once        sync.Once
	modPkgMJ    uint64
	modCoreMJ   uint64
	modUncoreMJ uint64
	modDramMJ   uint64
	modErr      error
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

	// Load wrap modulus (mJ) lazily once.
	m.once.Do(func() { m.loadModulusMJ() })

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

	// Aggregate deltas across sockets.
	var pkgSum, coreSum, uncoreSum, dramSum uint64
	for _, fl := range perSocket {
		if fl == nil || !fl.ok {
			continue
		}
		pkgSum += analysisops.DeltaWrapU64(fl.first.Pkg, fl.last.Pkg, m.modPkgMJ)
		klog.V(2).Infof(" modPkgMj = %d", m.modPkgMJ)

		coreSum += analysisops.DeltaWrapU64(fl.first.Core, fl.last.Core, m.modCoreMJ)
		uncoreSum += analysisops.DeltaWrapU64(fl.first.Uncore, fl.last.Uncore, m.modUncoreMJ)
		dramSum += analysisops.DeltaWrapU64(fl.first.DRAM, fl.last.DRAM, m.modDramMJ)
	}

	notes := fmt.Sprintf("samples=%d sockets=%d delayTicks=%d", len(samples), len(perSocket), m.delayTicks)
	if m.modErr != nil {
		notes += fmt.Sprintf(" wrapModErr=%v", m.modErr)
	}

	emit := func(domain string, v uint64) {
		p := analysis.Point{
			Key:    analysis.Key(m.ID(), analysis.Labels{"domain": domain}),
			Window: w,
			Unit:   "mJ",
			Value:  float64(v),
			Quality: &analysis.Quality{
				SamplesUsed: len(samples),
				Notes:       notes,
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

// loadModulusMJ reads max_energy_range_uj for each domain via the components-backed sysfs helpers,
// then converts to mJ (divide by 1000).
func (m *RaplWindowEnergy) loadModulusMJ() {
	sys := &source.PowerSysfs{}

	// Best-effort: if a domain doesn't exist (common), modulus stays 0.
	// We intentionally do NOT record an error to avoid noisy logs.
	get := func(fn func() (uint64, error)) uint64 {
		mJ, err := fn()
		if err != nil {
			return 0
		}
		return mJ
	}

	m.modPkgMJ = get(sys.GetMaxEnergyRangeFromPackage)
	m.modCoreMJ = get(sys.GetMaxEnergyRangeFromCore)
	m.modUncoreMJ = get(sys.GetMaxEnergyRangeFromUncore)
	m.modDramMJ = get(sys.GetMaxEnergyRangeFromDram)
}
