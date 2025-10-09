package engine

import (
	"context"
	"time"
)

type Collector interface {
	Name() string
	Period() time.Duration
	Enabled() bool
	Collect(ctx context.Context, ts time.Time) // do minimal work, push to buffer/bus
}
