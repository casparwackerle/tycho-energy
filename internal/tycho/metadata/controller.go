package metadata

import (
	"context"
	"sync"
	"time"

	"k8s.io/klog/v2"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
)

// MonoSource is the minimal interface the metadata controller needs from
// Tycho's monotonic clock implementation.
type MonoSource interface {
	From(ts time.Time) uint64
}

// config
type Config struct {
	Mono            MonoSource
	MaxAge          time.Duration
	ProcessInterval time.Duration
	KubeletInterval time.Duration
}

// Controller orchestrates metadata collection from various sources
// (process/cgroup, kubelet) and manages the Store lifecycle.
//
// It is intended to be called periodically by Tycho's global engine,
// using wall-clock timestamps from the aligned tickers.
type Controller struct {
	mu sync.Mutex

	store *Store
	cfg   Config

	processCollector *processCollector
	kubeletCollector *kubeletCollector

	// Last execution times in wall-clock time.
	lastProcessScan time.Time
	lastKubeletPoll time.Time
	lastGC          time.Time

	// GC cadence (non-configurable):
	//   gcInterval = 5 * max(ProcessInterval, KubeletInterval)
	gcInterval time.Duration
}

// New constructs a metadata Controller using Tycho's global configuration.
//
// All durations come from the config package; no hard-coded time values here.
func New(mono MonoSource) *Controller {
	winSec := cfg.BufferWindowSec() // float64 seconds

	metaCfg := Config{
		Mono:            mono,
		MaxAge:          time.Duration(winSec) * time.Second,
		ProcessInterval: time.Duration(cfg.MetadataProcessIntervalSec()) * time.Second,
		KubeletInterval: time.Duration(cfg.MetadataKubeletIntervalSec()) * time.Second,
	}

	// GC policy: 5 * max(proc, kubelet), ignoring kubelet if disabled.
	base := metaCfg.ProcessInterval
	if metaCfg.KubeletInterval > base {
		base = metaCfg.KubeletInterval
	}
	if base <= 0 {
		base = 1 * time.Second // safety fallback
	}
	gcInterval := 5 * base

	klog.Infof(
		"metadata.New: MaxAge=%s, processInterval=%s, kubeletInterval=%s, gcInterval=%s",
		metaCfg.MaxAge,
		metaCfg.ProcessInterval,
		metaCfg.KubeletInterval,
		gcInterval,
	)

	ctrl := NewController(metaCfg)
	ctrl.gcInterval = gcInterval
	return ctrl
}

// NewController constructs a new metadata controller and its underlying store.
func NewController(cfg Config) *Controller {
	store := NewStore(cfg.MaxAge)

	base := cfg.ProcessInterval
	if cfg.KubeletInterval > base {
		base = cfg.KubeletInterval
	}
	if base <= 0 {
		base = 1 * time.Second
	}

	ctrl := &Controller{
		store:            store,
		cfg:              cfg,
		processCollector: newProcessCollector(cfg, store),
		kubeletCollector: newKubeletCollector(cfg, store),
		gcInterval:       5 * base,
	}
	return ctrl
}

// Store exposes read-only access to the underlying metadata store for
// other Tycho subsystems (e.g., analysis). Mutations should only happen
// via the controller and collector logic.
func (c *Controller) Store() *Store {
	return c.store
}

// Name can be used to implement Tycho's engine.Collector interface.
func (c *Controller) Name() string {
	return "metadata"
}

// RefreshIfStale performs a best-effort, cycle-start-aligned metadata refresh
// (process scan and kubelet poll) for workload attribution.
//
// Purpose:
//   - Prefer refreshing metadata at the beginning of an analysis cycle,
//     so newly-started processes and pods are visible to attribution logic.
//   - Preserve correctness and bounded overhead by respecting configured
//     collection intervals.
//
// Semantics:
//   - Decisions are made using wall-clock time (consistent with Collect).
//   - Each collector is executed at most once per configured interval.
//   - A small lead window may pull an almost-due refresh into the cycle start
//     to improve phase alignment.
//   - A small slack window suppresses duplicate work if a periodic metadata
//     tick already ran close to the cycle boundary.
//   - Garbage collection is never triggered by this method.
//   - The refresh is best-effort: failures are logged but never returned.
//   - last* timestamps are updated when a refresh attempt is made (even on
//     partial failure) to avoid retry loops.
//
// Guarantees:
//   - No change to workload attribution semantics.
//   - No guarantee that metadata is always fully up to date at analysis time.
//   - No increase in steady-state metadata collection frequency.

func (c *Controller) RefreshIfStale(ctx context.Context, ts time.Time) {
	if c == nil {
		return
	}

	// slack: prevents duplicate work if the periodic metadata tick already ran
	// very close to the analysis-cycle start.
	const slack = 250 * time.Millisecond

	// lead: allows pulling a collector slightly early so it runs at cycle start
	// when it is "almost due" anyway (phase alignment without increasing frequency much).
	const lead = 250 * time.Millisecond

	var mono uint64
	if c.cfg.Mono != nil {
		mono = c.cfg.Mono.From(ts)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Decide whether to run a collector at analysis-cycle start.
	//
	// Rules:
	// - If disabled -> skip.
	// - If never ran before -> run (first).
	// - If it ran within slack of this cycle start -> skip (recent/dedup).
	// - Otherwise, enforce minimum spacing with interval gate:
	//     * run if due (ts-last >= interval)                 -> reason "due"
	//     * run if almost due ((last+interval)-ts <= lead)   -> reason "lead"
	//     * else skip                                        -> reason "wait"
	shouldRunAtCycleStart := func(last time.Time, interval time.Duration) (run bool, reason string) {
		if interval <= 0 {
			return false, "disabled"
		}
		if last.IsZero() {
			return true, "first"
		}

		// Dedup: already ran very close to this cycle boundary (likely via periodic tick).
		delta := ts.Sub(last)
		if delta < 0 {
			// Defensive: should not happen with aligned wall time.
			delta = -delta
		}
		if delta <= slack {
			return false, "recent"
		}

		// Interval gate: do not run more frequently than interval.
		dueAt := last.Add(interval)

		// Due (or overdue): run.
		if !ts.Before(dueAt) {
			return true, "due"
		}

		// Almost due: pull it into this cycle start for alignment.
		if dueAt.Sub(ts) <= lead {
			return true, "lead"
		}

		return false, "wait"
	}

	// ---- Proc refresh (cycle-start aligned, interval-bounded) ----
	if run, reason := shouldRunAtCycleStart(c.lastProcessScan, c.cfg.ProcessInterval); run {
		klog.V(4).Infof("[meta] refresh proc scan (reason=%s) ts=%s mono=%d",
			reason, ts.Format(time.RFC3339Nano), mono)

		c.lastProcessScan = ts
		c.processCollector.Collect(ctx, ts, mono)

	} else {
		klog.V(4).Infof("[meta] refresh proc scan skipped (reason=%s) ts=%s",
			reason, ts.Format(time.RFC3339Nano))
	}

	// ---- Kubelet refresh (cycle-start aligned, interval-bounded) ----
	if c.cfg.KubeletInterval > 0 {
		if run, reason := shouldRunAtCycleStart(c.lastKubeletPoll, c.cfg.KubeletInterval); run {
			klog.V(4).Infof("[meta] refresh kubelet poll (reason=%s) ts=%s mono=%d",
				reason, ts.Format(time.RFC3339Nano), mono)

			c.lastKubeletPoll = ts
			c.kubeletCollector.Collect(ctx, ts, mono)
		} else {
			klog.V(4).Infof("[meta] refresh kubelet poll skipped (reason=%s) ts=%s",
				reason, ts.Format(time.RFC3339Nano))
		}
	}
}

// Collect advances the metadata controller by a single time step.
//
// The engine.Manager is expected to call this periodically, passing the
// aligned wall-clock timestamp used for all collectors.
func (c *Controller) Collect(ctx context.Context, ts time.Time) {
	if c == nil {
		return
	}

	var mono uint64
	if c.cfg.Mono != nil {
		mono = c.cfg.Mono.From(ts)
	}

	klog.V(4).Infof("[metadata] tick ts=%s mono=%d",
		ts.Format(time.RFC3339Nano), mono)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Process metadata
	if c.shouldRun(ts, c.lastProcessScan, c.cfg.ProcessInterval) {
		klog.V(4).Infof("[metadata] processCollector scheduled at ts=%s mono=%d",
			ts.Format(time.RFC3339Nano), mono)
		c.lastProcessScan = ts
		c.processCollector.Collect(ctx, ts, mono)
	}

	// Kubelet metadata
	if c.cfg.KubeletInterval > 0 &&
		c.shouldRun(ts, c.lastKubeletPoll, c.cfg.KubeletInterval) {
		klog.V(4).Infof("[metadata] kubeletCollector scheduled at ts=%s mono=%d",
			ts.Format(time.RFC3339Nano), mono)
		c.lastKubeletPoll = ts
		c.kubeletCollector.Collect(ctx, ts, mono)
	}

	// GC cadence: decoupled from proc scan
	if c.shouldRun(ts, c.lastGC, c.gcInterval) {
		klog.V(6).Infof("[metadata] Garbage Collection scheduled at ts=%s mono=%d",
			ts.Format(time.RFC3339Nano), mono)

		c.lastGC = ts
		droppedProcs, droppedContainers, droppedPods := c.store.GC(ts)
		if klog.V(6).Enabled() && (droppedProcs+droppedContainers+droppedPods) > 0 {
			klog.V(6).Infof(
				"[metadata] Garbage Collection removed procs=%d containers=%d pods=%d",
				droppedProcs, droppedContainers, droppedPods,
			)
		}
	}
}

func (c *Controller) shouldRun(now, last time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return false
	}
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}
