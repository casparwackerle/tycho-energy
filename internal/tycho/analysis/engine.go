package analysis

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"k8s.io/klog/v2"
)

type Config struct {
	WindowDuration time.Duration
	SafetyOffset   time.Duration
}

type Engine struct {
	mono    *clock.Mono
	rings   Rings
	sink    Sink
	state   *StateStore
	planner Planner
	cfg     Config
}

// NewEngine constructs the Slice 0 analysis engine.
// It is intentionally metric-agnostic: it only knows "build plan and run it".
func NewEngine(mono *clock.Mono, rings Rings, sink Sink, state *StateStore, planner Planner, cfg Config) *Engine {
	if state == nil {
		state = NewStateStore()
	}
	return &Engine{
		mono:    mono,
		rings:   rings,
		sink:    sink,
		state:   state,
		planner: planner,
		cfg:     cfg,
	}
}

// Collect matches engine.Manager.Register callback signature.
// The ts argument is provided by the aligned ticker but Slice 0 uses mono.Now()
// to define a monotonic window that is consistent across all metrics.
func (e *Engine) Collect(ctx context.Context, _ time.Time) {
	if e == nil || e.mono == nil || e.planner == nil || e.sink == nil {
		return
	}

	nowMono := e.mono.Now()
	win := SelectWindow(e.mono, e.cfg.WindowDuration, e.cfg.SafetyOffset)

	cycle := &Cycle{
		Ctx:     ctx,
		Mono:    e.mono,
		NowMono: nowMono,
		Window:  win,
		Policy:  ReadPolicy{SafetyOffset: e.cfg.SafetyOffset},
		Rings:   e.rings,
		Sink:    e.sink,
		State:   e.state,
	}

	klog.V(3).Infof("[analysis] cycle now=%d window=%s", nowMono, win.String())

	plan := e.planner.BuildPlan(cycle)
	if plan == nil {
		klog.V(3).Infof("[analysis] no plan")
		return
	}

	if err := plan.Run(cycle); err != nil {
		klog.Warningf("[analysis] plan failed: %v", err)
	}
}
