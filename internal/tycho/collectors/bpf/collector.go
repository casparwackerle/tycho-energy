package bpfCollector

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

// Config holds dependencies/settings for the BPF collector (placeholder for now).
type Config struct{}

// Collector performs BPF collection (placeholder).
type Collector struct {
	cfg  Config
	name string
}

func New(cfg Config) *Collector {
	return &Collector{cfg: cfg, name: "bpf"}
}

// Collect is called by the engine at the scheduled tick timestamp.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	klog.Infof("Tycho collect[%s]: tick @ %s", c.name, ts.Format(time.RFC3339Nano))
}
