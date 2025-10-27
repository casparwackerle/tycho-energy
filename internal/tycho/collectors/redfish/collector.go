package redfishCollector

import (
	"context"
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
	buf       *ring.Sync[ring.RedfishSample]
	mono      *clock.Mono
	client    *source.RedFishClient
	expectDur time.Duration        // heartbeat interval when value unchanged
	lastSeq   map[string]uint64    // last emitted seq per chassis
	lastEmit  map[string]time.Time // last emit time per chassis
	supported bool                 // discovery succeeded
}

func New(cfg Config) *Collector {
	c := &Collector{
		buf:       cfg.Buf,
		mono:      cfg.Mono,
		expectDur: time.Duration(config.RedfishExpectedChangeMs()) * time.Millisecond,
		lastSeq:   map[string]uint64{},
		lastEmit:  map[string]time.Time{},
	}

	// Create Redfish client (passive; no internal ticker)
	rf := source.NewRedfishClient()
	if rf == nil {
		klog.Warning("redfish: no credentials/host; collector remains inactive")
		return c
	}

	// One-shot discovery (like RAPL gating). No background goroutines.
	if rf.IsSystemCollectionSupported() {
		c.client = rf
		c.supported = true
	} else {
		klog.Warning("redfish: system not supported or discovery failed")
	}
	return c
}

// Collect is called by Tycho's engine at the global cadence (e.g., every 1s).
// It polls once, then emits per-chassis **only when the source actually updated**,
// with an optional heartbeat after RedfishExpectedChangeMs().
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if !c.supported || c.client == nil {
		return
	}

	start := time.Now()

	// Update the passive cache with a single pull at Tycho cadence.
	c.client.PollOnce()

	now := time.Now()
	c.client.ForEachSystem(func(sys *source.RedfishSystemPowerResult) {
		chassis := sys.Chassis() // tiny getters added in source package
		seq := sys.Sequence()
		watts := sys.Watts()

		// Emit only if new BMC sample arrived (seq advanced)...
		if c.lastSeq[chassis] != seq {
			s := ring.RedfishSample{
				SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)},
				ChassisID:  chassis,
				PowerWatts: watts,
				Seq:        seq,
			}
			c.buf.Push(s)
			klog.V(5).Infof("redfish: pushed sample chassis=%s seq=%d watts=%.3f (in %v)", chassis, seq, watts, time.Since(start))

			c.lastSeq[chassis] = seq
			c.lastEmit[chassis] = now
			return
		}

		// ...or push a heartbeat if the expected update window elapsed.
		if c.expectDur > 0 && now.Sub(c.lastEmit[chassis]) >= c.expectDur {
			s := ring.RedfishSample{
				SampleMeta: ring.SampleMeta{Mono: c.mono.From(ts)},
				ChassisID:  chassis,
				PowerWatts: watts,
				Seq:        seq, // unchanged
			}
			c.buf.Push(s)
			klog.V(5).Infof("redfish: heartbeat chassis=%s seq=%d watts=%.3f (in %v)", chassis, seq, watts, time.Since(start))

			c.lastEmit[chassis] = now
		}
	})
}
