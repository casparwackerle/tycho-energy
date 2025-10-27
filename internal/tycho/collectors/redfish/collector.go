package redfishCollector

import (
	"context"
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/platform/source"
	"k8s.io/klog/v2"
)

// Config mirrors the pattern of other collectors (e.g., RAPL).
type Config struct {
	Buf  *ring.Sync[ring.RedfishSample]
	Mono *clock.Mono
}

// Collector owns a passive Redfish client and small per-chassis state.
type Collector struct {
	buf    *ring.Sync[ring.RedfishSample]
	mono   *clock.Mono
	client *source.RedFishClient

	// Heartbeat (adaptive & per chassis): emit if no new sample by this duration.
	expectFixed time.Duration            // baseline from config
	expectDyn   map[string]time.Duration // adaptive, per chassis

	// Per-chassis book-keeping
	lastSeq       map[string]uint64
	lastEmit      map[string]time.Time
	lastEmittedW  map[string]float64
	lastSeqTime   map[string]time.Time       // when we last observed seq advance
	interArrivals map[string][]time.Duration // sliding window for median
}

// Internal tunables (not user-config)
const (
	iaWindow    = 9               // keep last N inter-arrival gaps
	expectMin   = 2 * time.Second // clamp lower bound
	expectMax   = 60 * time.Second
	adaptFactor = 3.0 / 2.0 // 1.5 × median
)

func New(cfg Config) *Collector {
	c := &Collector{
		buf:           cfg.Buf,
		mono:          cfg.Mono,
		expectFixed:   time.Duration(config.RedfishExpectedChangeMs()) * time.Millisecond,
		expectDyn:     map[string]time.Duration{},
		lastSeq:       map[string]uint64{},
		lastEmit:      map[string]time.Time{},
		lastEmittedW:  map[string]float64{},
		lastSeqTime:   map[string]time.Time{},
		interArrivals: map[string][]time.Duration{},
	}

	// Create Redfish client (passive; no internal ticker)
	rf := source.NewRedfishClient()
	if rf == nil {
		klog.Warning("redfish: no credentials/host; collector remains inactive")
		return c
	}

	// One-shot discovery (no background goroutines)
	if !rf.IsSystemCollectionSupported() {
		klog.Warning("redfish: system not supported or discovery failed")
		return c
	}
	c.client = rf

	klog.Infof("redfish: init expectFixed=%v pollMs=%d",
		c.expectFixed, config.RedfishPollMs())

	return c
}

// Collect is called by Tycho's engine at the global cadence (e.g., every 1s).
// It polls once, then emits per-chassis when:
//   - the BMC produced a new sample (seq advanced), OR
//   - the heartbeat window elapsed (carry-forward last value).
//
// Every push includes SourceTime (if provided by BMC), CollectorTime, and FreshnessMs.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	select {
	case <-ctx.Done():
		return
	default:
	}
	if c.client == nil {
		return
	}

	start := time.Now()

	// Update the passive cache with a single pull at Tycho cadence.
	c.client.PollOnce()

	now := time.Now()
	c.client.ForEachSystem(func(sys *source.RedfishSystemPowerResult) {
		chassis := sys.Chassis()
		seq := sys.Sequence()
		watts := sys.Watts()
		srcTime := sys.SourceDate() // zero if unknown

		// If seq advanced, update inter-arrival stats (for adaptive heartbeat).
		if prev, ok := c.lastSeq[chassis]; !ok || prev != seq {
			// Inter-arrival measurement
			if prevOK, ok2 := c.lastSeqTime[chassis]; ok2 {
				ia := now.Sub(prevOK)
				if ia > 0 {
					list := c.interArrivals[chassis]
					list = append(list, ia)
					if len(list) > iaWindow {
						list = list[len(list)-iaWindow:]
					}
					c.interArrivals[chassis] = list

					// Recompute adaptive expect window as 1.5 × median
					if med := medianDuration(list); med > 0 {
						ad := clampDur(time.Duration(adaptFactor*float64(med)), expectMin, expectMax)
						c.expectDyn[chassis] = ad
					}
				}
			}
			c.lastSeqTime[chassis] = now
		}

		// Determine the effective expect window for this chassis
		expect := c.expectFixed
		if d, ok := c.expectDyn[chassis]; ok && d > 0 {
			expect = d
		}
		if expect <= 0 {
			expect = 15 * time.Second // conservative default if config is zero
		}

		// Decide whether to emit:
		emit := false
		reason := "seq"

		// (A) Emit on new BMC sample (seq advanced)
		if c.lastSeq[chassis] != seq {
			emit = true
		}

		// (B) Emit heartbeat if the window elapsed (carry-forward last value)
		if !emit {
			last := c.lastEmit[chassis]
			if last.IsZero() || now.Sub(last) >= expect {
				emit = true
				reason = "heartbeat"
			}
		}

		if !emit {
			return
		}

		// Freshness: time from BMC-provided SourceTime to now (if available)
		var freshness time.Duration
		if !srcTime.IsZero() {
			freshness = now.Sub(srcTime)
			if freshness < 0 {
				freshness = 0
			}
		}

		// Prepare sample with monotonic tick and timestamps
		s := ring.RedfishSample{
			SampleMeta:    ring.SampleMeta{Mono: c.mono.From(ts)},
			ChassisID:     chassis,
			PowerWatts:    watts,
			Seq:           seq,     // may be the same in heartbeat case
			SourceTime:    srcTime, // zero if BMC didn't provide Date
			CollectorTime: now,
			FreshnessMs:   float64(freshness.Milliseconds()),
		}

		c.buf.Push(s)
		c.lastEmit[chassis] = now
		c.lastSeq[chassis] = seq
		c.lastEmittedW[chassis] = watts

		klog.V(5).Infof(
			"redfish: %s chassis=%s seq=%d watts=%.3f freshness=%v expect=%v (in %v)",
			reason, chassis, seq, watts, freshness, expect, time.Since(start),
		)
	})
}

func medianDuration(xs []time.Duration) time.Duration {
	if len(xs) == 0 {
		return 0
	}
	tmp := make([]time.Duration, len(xs))
	copy(tmp, xs)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i] < tmp[j] })
	n := len(tmp)
	if n%2 == 1 {
		return tmp[n/2]
	}
	// even -> average middle two
	return (tmp[n/2-1] + tmp[n/2]) / 2
}

func clampDur(d, lo, hi time.Duration) time.Duration {
	if d < lo {
		return lo
	}
	if d > hi {
		return hi
	}
	return d
}
