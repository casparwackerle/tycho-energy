package analysisexport

import (
	"context"
	"math"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
)

// TruncatingSink truncates Point.Value toward zero before forwarding.
// It does not touch Quality, labels, units, etc.
type TruncatingSink struct {
	Downstream analysis.Sink
}

func NewTruncatingSink(downstream analysis.Sink) *TruncatingSink {
	return &TruncatingSink{Downstream: downstream}
}

func (s *TruncatingSink) Emit(ctx context.Context, p analysis.Point) {
	p.Value = math.Trunc(p.Value)
	if s.Downstream != nil {
		s.Downstream.Emit(ctx, p)
	}
}
