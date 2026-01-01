// file: internal/tycho/analysis/idle/config.go
package idle

import "time"

type Config struct {
	// Util bins for scalar models: [0, UMax] partitioned into BinWidth.
	UMax     float64
	BinWidth float64

	// Per-bin storage cap.
	KPerBin int

	// Quantile per bin (0.10 = q10).
	Quantile float64

	// Refit policy.
	MinBinsPopulated int
	MinTotalPoints   int
	MinNewPoints     int
	RefitEvery       time.Duration

	// Stability gate.
	EpsUScalar float64
	EpsUVec    float64

	// Guardrail.
	DeltaMin float64
}

func DefaultConfig() Config {
	return Config{
		UMax:     0.50,
		BinWidth: 0.05,
		KPerBin:  64,
		Quantile: 0.10,

		MinBinsPopulated: 6,
		MinTotalPoints:   60,
		MinNewPoints:     10,
		RefitEvery:       60 * time.Second,

		EpsUScalar: 0.05,
		EpsUVec:    0.10,

		DeltaMin: 0,
	}
}
