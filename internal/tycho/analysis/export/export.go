package analysisexport

import "github.com/casparwackerle/tycho-energy/internal/tycho/analysis"

// Sink is implemented by concrete exporters (Slice 0: log sink).
// It is exactly analysis.Sink to keep the engine/contracts in analysis.
type Sink = analysis.Sink
