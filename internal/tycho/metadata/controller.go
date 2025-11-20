package metadata

import (
	"context"
	"time"

	"k8s.io/klog/v2"

	cfg "github.com/casparwackerle/tycho-energy/pkg/config"
)

// MonoSource is the minimal interface the metadata controller needs from
// Tycho's monotonic clock implementation.
type MonoSource interface {
	From(ts time.Time) uint64
}

type Config struct {
	Mono             MonoSource
	MaxAge           time.Duration
	ProcessInterval  time.Duration
	KubeletInterval  time.Duration
	EnableCADvisor   bool
	CADvisorInterval time.Duration
}

// Controller orchestrates metadata collection from various sources
// (process/cgroup, kubelet, cAdvisor) and manages the Store lifecycle.
//
// It is intended to be called periodically by Tycho's global engine,
// using wall-clock timestamps from the aligned tickers.
type Controller struct {
	store *Store
	cfg   Config

	processCollector  *processCollector
	kubeletCollector  *kubeletCollector
	cadvisorCollector *cadvisorCollector

	// Last execution times in wall-clock time.
	lastProcessScan  time.Time
	lastKubeletPoll  time.Time
	lastCADvisorPoll time.Time
}

// New constructs a metadata Controller using Tycho's global configuration.
//
// All durations come from the config package; no hard-coded time values here.
// Assumed config helpers (you will implement them in pkg/config):
//   - BufferWindowSec() float64
//   - MetadataProcessIntervalSec() uint64
//   - MetadataKubeletIntervalSec() uint64
//   - MetadataCADvisorIntervalSec() uint64
//   - EnableMetadataCADvisor() bool
func New(mono MonoSource) *Controller {
	winSec := cfg.BufferWindowSec() // already exists; used as MaxAge

	cfg := Config{
		Mono:             mono,
		MaxAge:           time.Duration(winSec * int(time.Second)),
		ProcessInterval:  time.Duration(cfg.MetadataProcessIntervalSec()) * time.Second,
		KubeletInterval:  time.Duration(cfg.MetadataKubeletIntervalSec()) * time.Second,
		EnableCADvisor:   cfg.EnableMetadataCADvisor(),
		CADvisorInterval: time.Duration(cfg.MetadataCADvisorIntervalSec()) * time.Second,
	}

	klog.Infof("metadata.New: MaxAge=%s, processInterval=%s, kubeletInterval=%s, enableCADvisor=%v, cadvisorInterval=%s",
		cfg.MaxAge, cfg.ProcessInterval, cfg.KubeletInterval, cfg.EnableCADvisor, cfg.CADvisorInterval)

	return NewController(cfg)
}

// NewController constructs a new metadata controller and its underlying store.
func NewController(cfg Config) *Controller {
	store := NewStore(cfg.MaxAge)

	ctrl := &Controller{
		store: store,
		cfg:   cfg,
		// Collectors are initialized with references to the store.
		processCollector:  newProcessCollector(cfg, store),
		kubeletCollector:  newKubeletCollector(cfg, store),
		cadvisorCollector: nil,
	}

	if cfg.EnableCADvisor {
		ctrl.cadvisorCollector = newCADvisorCollector(cfg, store)
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

// Collect advances the metadata controller by a single time step.
//
// The engine.Manager is expected to call this periodically, passing the
// aligned wall-clock timestamp used for all collectors. The controller
// itself handles sub-scheduling of process/kubelet/cAdvisor work.
func (c *Controller) Collect(ctx context.Context, ts time.Time) {
	var mono uint64
	if c.cfg.Mono != nil {
		mono = c.cfg.Mono.From(ts)
	}

	klog.V(3).Infof("[metadata] tick ts=%s mono=%d", ts.Format(time.RFC3339Nano), mono)

	// Process metadata
	if c.shouldRun(ts, c.lastProcess, c.cfg.ProcessInterval) {
		klog.V(4).Infof("[metadata] processCollector scheduled at ts=%s mono=%d", ts.Format(time.RFC3339Nano), mono)
		c.lastProcess = ts
		// TODO: c.proc.Collect(ctx, ts, mono)
	}

	// Kubelet metadata
	if c.cfg.KubeletInterval > 0 && c.shouldRun(ts, c.lastKubelet, c.cfg.KubeletInterval) {
		klog.V(4).Infof("[metadata] kubeletCollector scheduled at ts=%s mono=%d", ts.Format(time.RFC3339Nano), mono)
		c.lastKubelet = ts
		// TODO: c.kube.Collect(ctx, ts, mono)
	}

	// Simple GC cadence: tie to process interval for now.
	if c.shouldRun(ts, c.lastGC, c.cfg.ProcessInterval) {
		klog.V(5).Infof("[metadata] GC scheduled at ts=%s mono=%d", ts.Format(time.RFC3339Nano), mono)
		c.lastGC = ts
		// TODO: c.store.GC(ts)
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

func (c *Controller) shouldRun(now, last time.Time, interval time.Duration) bool {
	if interval <= 0 {
		// Treat non-positive intervals as "never".
		return false
	}
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}
