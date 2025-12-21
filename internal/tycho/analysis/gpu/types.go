package gpu

// PowerObs is a power observation (W) at corrected time (seconds).
type PowerObs struct {
	TSec  float64
	Power float64 // W
}

// CumEnergyObs is a cumulative energy counter (mJ) at corrected time (seconds).
// Only included when the collector considers the value present and > 0.
type CumEnergyObs struct {
	TSec     float64
	EnergyMJ uint64 // mJ
}

// SolveInfo carries debug info about the last solve.
type SolveInfo struct {
	RowsInst   int
	RowsAvg    int
	RowsEnergy int

	Iter     int
	RelRes   float64
	Clamped  int
	MinRaw   float64
	MinFinal float64
}

// Series is the corrected, reconstructed power signal on a uniform grid.
type Series struct {
	UUID  string
	T0Sec float64 // start time in corrected seconds
	DtSec float64 // grid spacing in seconds
	PW    []float64

	// Debug / introspection
	Last SolveInfo
}
