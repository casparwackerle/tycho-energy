// file: internal/tycho/analysis/fusion/types.go
package fusion

import "fmt"

// BinIndex is an index on the system fusion grid.
type BinIndex int64

func (k BinIndex) String() string { return fmt.Sprintf("%d", int64(k)) }

type RedfishKernel string

const (
	KernelInstant       RedfishKernel = "instant"
	KernelAvg1sTrailing RedfishKernel = "avg1s_trailing"
)

// RedfishObs is a solver-ready observation placeholder.
// Slice 6a only standardizes and stores; Slice 6b applies kernels to the grid.
type RedfishObs struct {
	ChassisID string
	MonoCorr  uint64 // corrected tick (event time), clamped at 0
	ValueMW   float64
	Kernel    RedfishKernel
	KernelMs  int
}

// Cache holds the fusion-grid-aligned feature history for one chassis.
type Cache struct {
	ChassisID string

	QuantumTicks uint64
	HorizonBins  int

	// StartBin is the BinIndex for index 0 in the arrays.
	StartBin BinIndex

	// LastBin is the latest bin that has been computed/filled.
	LastBin BinIndex

	// Per-bin features. Units:
	// - E*MJ: mJ per fusion bin
	// - CPUInstr: raw instruction count per bin
	EpkgMJ   []float64
	EdramMJ  []float64
	EgpuMJ   []float64
	CPUInstr []float64

	// Optional validity bitset could be added later; keep simple for now.
	// We treat missing metrics as 0 contributions.

	// Stored standardized Redfish observations over the horizon.
	// Slice 6b will decide how to project them to the grid.
	RedfishObs []RedfishObs
}
