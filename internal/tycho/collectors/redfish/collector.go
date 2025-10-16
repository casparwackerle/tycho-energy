package redfishCollector

import (
	"context"
	"time"

	"k8s.io/klog/v2"
)

type Config struct{}

type Collector struct {
	cfg  Config
	name string
}

func New(cfg Config) *Collector {
	return &Collector{cfg: cfg, name: "redfish"}
}

func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	klog.Infof("Tycho collect[%s]: tick @ %s", c.name, ts.Format(time.RFC3339Nano))
}
