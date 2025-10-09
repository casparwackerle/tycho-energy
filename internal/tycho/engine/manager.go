package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/pkg/bpf"
	"github.com/casparwackerle/tycho-energy/pkg/collector"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/kubernetes"
	exporter "github.com/casparwackerle/tycho-energy/pkg/metrics"
	"k8s.io/klog/v2"
)

// Manager is a lightweight scheduler that runs one aligned ticker per registered
// metric collector. It guarantees phase alignment via a shared base timestamp,
// but each collector runs in its own goroutine with its own cadence.
//
// Lifecycle:
//  1. Construct with NewManager()
//  2. Register collectors via Register(...) BEFORE Start()
//  3. Start(ctx) — blocks until ctx is cancelled; all tickers stop
//  4. Stop() is a no-op (kept for symmetry; Start is ctx-driven)
type Manager struct {
	// shared alignment epoch for all tickers; set at Start()
	base time.Time

	// registrations provided before Start()
	regs []registration

	// internal state
	mu      sync.Mutex
	started bool
	wg      sync.WaitGroup
}

type registration struct {
	name    string
	period  time.Duration
	enabled bool
	collect func(ctx context.Context, ts time.Time)
}

// NewManager creates an empty engine manager.
func NewManager() *Manager {
	return &Manager{}
}

// Register adds a collector callback with its cadence and enable flag.
// Must be called BEFORE Start(). Returns an error if called after Start().
func (m *Manager) Register(
	name string,
	period time.Duration,
	enabled bool,
	collect func(ctx context.Context, ts time.Time),
) error {
	if collect == nil {
		return errors.New("collect callback must not be nil")
	}
	if period <= 0 {
		return errors.New("period must be > 0")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return errors.New("engine already started; registration is closed")
	}

	m.regs = append(m.regs, registration{
		name:    name,
		period:  period,
		enabled: enabled,
		collect: collect,
	})
	return nil
}

// Start launches one aligned ticker per enabled registration and blocks
// until ctx is cancelled. On cancellation, all ticker goroutines stop and
// Start returns nil.
//
// Notes:
//   - Work inside each collect callback should be minimal and non-blocking
//     (e.g., push into a buffer/bus) to keep scheduling jitter low.
//   - All tickers share the same base epoch for deterministic phasing.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("engine already started")
	}
	m.started = true
	m.base = time.Now()
	regs := append([]registration(nil), m.regs...) // copy for safe iteration
	m.mu.Unlock()

	for _, r := range regs {
		if !r.enabled {
			klog.Infof("Tycho engine: %s disabled; skipping", r.name)
			continue
		}
		tkr, err := clock.NewAlignedTicker(nil, r.period, m.base)
		if err != nil {
			klog.Errorf("Tycho engine: failed to create %s ticker: %v", r.name, err)
			continue
		}

		klog.Infof("Tycho engine: starting %s ticker (period=%s, base=%s)",
			r.name, r.period, m.base.Format(time.RFC3339Nano))

		m.wg.Add(1)
		go func(name string, period time.Duration, collect func(context.Context, time.Time), t clock.AlignedTicker) {
			defer m.wg.Done()
			defer t.Stop()

			for {
				select {
				case <-ctx.Done():
					klog.Infof("Tycho engine: stopping %s ticker", name)
					return
				case ts := <-t.C():
					// Minimal, non-blocking work is crucial for low jitter.
					collect(ctx, ts)
				}
			}
		}(r.name, r.period, r.collect, tkr)
	}

	// Wait for cancellation, then for all workers to exit.
	<-ctx.Done()
	m.wg.Wait()
	klog.Infof("Tycho engine: context cancelled; all tickers stopped")
	return nil
}

// Stop is currently a no-op. Use context cancellation to stop Start().
func (m *Manager) Stop() {}

//----------------------------------------old code below

// NewManager returns a minimal manager with a default period.
// You can change the period later when we thread config through.
// func NewManager(periodMs int) *Manager {
// 	if periodMs <= 0 {
// 		periodMs = 1000 // default 1000 ms
// 	}
// 	return &Manager{periodMs: time.Duration(periodMs) * time.Millisecond}
// }

// // Start spins an aligned ticker and periodically logs jitter.
// // It returns when ctx is cancelled.
// func (m *Manager) Start(ctx context.Context) error {
// 	// Align to "now" for the skeleton; later we'll use a shared epoch.
// 	t, err := clock.NewAlignedTicker(nil, m.periodMs, time.Time{})
// 	if err != nil {
// 		return err
// 	}
// 	defer t.Stop()

// 	klog.Infof("Tycho engine: started ticker (period=%s)", m.periodMs)

// 	var (
// 		count  int64
// 		maxJit time.Duration
// 	)

// 	for {
// 		select {
// 		case <-ctx.Done():
// 			klog.Infof("Tycho engine: stopping (context cancelled)")
// 			return nil

// 		case ts := <-t.C():
// 			// ts is the scheduled/aligned tick time; jitter is how late we are.
// 			jitter := time.Since(ts)
// 			if jitter > maxJit {
// 				maxJit = jitter
// 			}
// 			count++

// 			// Light logging: every 10 ticks, report status.
// 			if count%100 == 0 {
// 				klog.V(5).Infof("Tycho engine: ticks=%d last_jitter=%s max_jitter=%s", count, jitter, maxJit)
// 				// reset max to see peaks per-window
// 				maxJit = 0
// 			}
// 		}
// 	}
// }

// // Stop is currently a no-op; Start exits on ctx cancellation.
// func (m *Manager) Stop() {}

type CollectorManager struct {
	// StatsCollector is responsible to collect resource and energy consumption metrics and calculate them when needed
	StatsCollector *collector.Collector

	// PrometheusCollector implements the external Collector interface provided by the Prometheus client
	PrometheusCollector *exporter.PrometheusExporter

	// Watcher register in the kubernetes apiserver to watch for pod events to add or remove it from the ContainerStats map
	Watcher *kubernetes.ObjListWatcher
}

func New(bpfExporter bpf.Exporter) *CollectorManager {
	var err error
	manager := &CollectorManager{}
	supportedMetrics := bpfExporter.SupportedMetrics()
	manager.StatsCollector = collector.NewCollector(bpfExporter)
	manager.PrometheusCollector = exporter.NewPrometheusExporter(supportedMetrics)
	// the collector and prometheusExporter share structures and collections
	manager.PrometheusCollector.NewProcessCollector(manager.StatsCollector.ProcessStats)
	manager.PrometheusCollector.NewContainerCollector(manager.StatsCollector.ContainerStats)
	manager.PrometheusCollector.NewVMCollector(manager.StatsCollector.VMStats)
	manager.PrometheusCollector.NewNodeCollector(&manager.StatsCollector.NodeStats)
	// configure the watcher
	if manager.Watcher, err = kubernetes.NewObjListWatcher(supportedMetrics); err != nil {
		klog.Errorf("could not create the watcher, %v", err)
	}
	manager.Watcher.Mx = &manager.PrometheusCollector.Mx
	manager.Watcher.ContainerStats = manager.StatsCollector.ContainerStats
	if err = manager.Watcher.Run(); err != nil {
		klog.Errorf("could not run the watcher %v", err)
	}
	return manager
}

// original kepler CollectorManager
func (m *CollectorManager) Start() error {
	if err := m.StatsCollector.Initialize(); err != nil {
		return err
	}

	samplePeriod := time.Duration((config.SamplePeriodSec()) * uint64(time.Second))

	go func() {
		ticker := time.NewTicker(samplePeriod)
		for {
			// wait x seconds before updating the metrics
			<-ticker.C

			// acquire the lock to wait prometheus finish the metric collection before updating the metrics
			m.PrometheusCollector.Mx.Lock()
			m.StatsCollector.Update()
			m.PrometheusCollector.Mx.Unlock()
		}
	}()

	return nil
}

func (m *CollectorManager) Stop() {
	m.Watcher.ShutDownWithDrain()
}
