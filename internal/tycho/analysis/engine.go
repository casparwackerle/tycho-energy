package analysis

import (
	"context"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
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

	meta *metadata.Store
}

// NewEngine constructs the Slice 0 analysis engine.
// It is intentionally metric-agnostic: it only knows "build plan and run it".
func NewEngine(mono *clock.Mono, rings Rings, sink Sink, state *StateStore, planner Planner, cfg Config, meta *metadata.Store) *Engine {
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
		meta:    meta,
	}
}

// Collect matches engine.Manager.Register callback signature.
func (e *Engine) Collect(ctx context.Context, _ time.Time) {
	if e == nil || e.mono == nil || e.planner == nil || e.sink == nil {
		return
	}

	nowMono := e.mono.Now()
	win := SelectWindow(e.mono, e.cfg.WindowDuration, e.cfg.SafetyOffset)

	store := NewPointStore()
	sink := NewCollectingSink(e.sink, store)

	cycle := &Cycle{
		Ctx:     ctx,
		Mono:    e.mono,
		NowMono: nowMono,
		Window:  win,
		Policy:  ReadPolicy{SafetyOffsetTicks: e.mono.TicksForDurationCeil(e.cfg.SafetyOffset)},
		Rings:   e.rings,
		Sink:    sink,
		State:   e.state,
		Store:   store,
		Meta:    e.meta,
	}

	klog.V(2).Infof("[analysis] cycle now=%d window=%s", nowMono, win.String())

	plan := e.planner.BuildPlan(cycle)
	if plan == nil {
		klog.V(2).Infof("[analysis] no plan")
		return
	}

	if err := plan.Run(cycle); err != nil {
		klog.Warningf("[analysis] plan failed: %v", err)
	}
}
