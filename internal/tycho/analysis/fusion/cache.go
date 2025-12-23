// file: internal/tycho/analysis/fusion/cache.go
package fusion

import "math"

func NewCache(chassis string, quantumTicks uint64, horizonBins int, startBin BinIndex) *Cache {
	if horizonBins < 1 {
		horizonBins = 1
	}
	c := &Cache{
		ChassisID:    chassis,
		QuantumTicks: quantumTicks,
		HorizonBins:  horizonBins,
		StartBin:     startBin,
		LastBin:      startBin - 1, // "empty"
		EpkgMJ:       make([]float64, horizonBins),
		EdramMJ:      make([]float64, horizonBins),
		EgpuMJ:       make([]float64, horizonBins),
		CPUInstr:     make([]float64, horizonBins),
		RedfishObs:   make([]RedfishObs, 0, 64),
	}
	return c
}

func (c *Cache) CloneShallow() *Cache {
	if c == nil {
		return nil
	}
	out := *c
	// Note: slices are shared; this is only for very local use.
	return &out
}

func (c *Cache) ResetArrays() {
	if c == nil {
		return
	}
	zero := func(x []float64) {
		for i := range x {
			x[i] = 0
		}
	}
	zero(c.EpkgMJ)
	zero(c.EdramMJ)
	zero(c.EgpuMJ)
	zero(c.CPUInstr)
	c.RedfishObs = c.RedfishObs[:0]
	c.LastBin = c.StartBin - 1
}

// EnsureHorizon ensures the cache arrays represent [startBin, startBin + HorizonBins - 1].
// It shifts overlapping data forward when possible; otherwise it resets.
func (c *Cache) EnsureHorizon(startBin BinIndex) {
	if c == nil {
		return
	}
	if c.HorizonBins <= 0 {
		c.HorizonBins = 1
	}
	// If already aligned, nothing to do.
	if c.StartBin == startBin {
		return
	}

	oldStart := c.StartBin
	oldEnd := oldStart + BinIndex(c.HorizonBins) - 1
	newStart := startBin
	newEnd := newStart + BinIndex(c.HorizonBins) - 1

	// Compute overlap.
	ovStart := maxBin(oldStart, newStart)
	ovEnd := minBin(oldEnd, newEnd)
	if ovEnd < ovStart {
		// No overlap: full reset.
		c.StartBin = newStart
		c.ResetArrays()
		return
	}

	// Shift by copying overlap into new arrays.
	n := c.HorizonBins
	newEpkg := make([]float64, n)
	newEdram := make([]float64, n)
	newEgpu := make([]float64, n)
	newInstr := make([]float64, n)

	for k := ovStart; k <= ovEnd; k++ {
		oi := int64(k - oldStart)
		ni := int64(k - newStart)
		if oi < 0 || oi >= int64(n) || ni < 0 || ni >= int64(n) {
			continue
		}
		newEpkg[ni] = c.EpkgMJ[oi]
		newEdram[ni] = c.EdramMJ[oi]
		newEgpu[ni] = c.EgpuMJ[oi]
		newInstr[ni] = c.CPUInstr[oi]
	}

	c.StartBin = newStart
	c.EpkgMJ = newEpkg
	c.EdramMJ = newEdram
	c.EgpuMJ = newEgpu
	c.CPUInstr = newInstr

	// Redfish observations are kept as a plain slice; easiest is to clear and reload per cycle.
	// (Still cheap relative to solver; and 6a does not need incremental obs maintenance.)
	c.RedfishObs = c.RedfishObs[:0]

	// Clamp LastBin into new horizon range.
	if c.LastBin < newStart-1 {
		c.LastBin = newStart - 1
	}
	if c.LastBin > newEnd {
		c.LastBin = newEnd
	}
}

func (c *Cache) idxOf(k BinIndex) (int, bool) {
	if c == nil || c.HorizonBins <= 0 {
		return 0, false
	}
	if k < c.StartBin {
		return 0, false
	}
	i := int64(k - c.StartBin)
	if i < 0 || i >= int64(c.HorizonBins) {
		return 0, false
	}
	return int(i), true
}

func (c *Cache) AddEpkg(k BinIndex, v float64) {
	if i, ok := c.idxOf(k); ok {
		c.EpkgMJ[i] += v
	}
}
func (c *Cache) AddEdram(k BinIndex, v float64) {
	if i, ok := c.idxOf(k); ok {
		c.EdramMJ[i] += v
	}
}
func (c *Cache) AddEgpu(k BinIndex, v float64) {
	if i, ok := c.idxOf(k); ok {
		c.EgpuMJ[i] += v
	}
}
func (c *Cache) AddCPUInstr(k BinIndex, v float64) {
	if i, ok := c.idxOf(k); ok {
		c.CPUInstr[i] += v
	}
}

// ZeroRange sets selected features to 0 for bins k in [kStart, kEnd] (clamped to horizon).
func (c *Cache) ZeroRange(kStart, kEnd BinIndex, zeroEpkg, zeroEdram, zeroEgpu, zeroCPUInstr bool) {
	if c == nil || c.HorizonBins <= 0 {
		return
	}
	// Clamp range to horizon.
	hStart := c.StartBin
	hEnd := c.StartBin + BinIndex(c.HorizonBins) - 1
	if kStart < hStart {
		kStart = hStart
	}
	if kEnd > hEnd {
		kEnd = hEnd
	}
	if kEnd < kStart {
		return
	}

	for k := kStart; k <= kEnd; k++ {
		i, ok := c.idxOf(k)
		if !ok {
			continue
		}
		if zeroEpkg {
			c.EpkgMJ[i] = 0
		}
		if zeroEdram {
			c.EdramMJ[i] = 0
		}
		if zeroEgpu {
			c.EgpuMJ[i] = 0
		}
		if zeroCPUInstr {
			c.CPUInstr[i] = 0
		}
	}
}

func (c *Cache) SetLastBin(k BinIndex) {
	if c == nil {
		return
	}
	// Ensure monotonic.
	if k > c.LastBin {
		c.LastBin = k
	}
}

// WindowSums sums bins overlapping [startMono, endMono] (inclusive-ish).
// This is used for diagnostics only.
func (c *Cache) WindowSums(startMono, endMono uint64) (epkg, edram, egpu, instr float64) {
	if c == nil || c.QuantumTicks == 0 || c.HorizonBins <= 0 {
		return 0, 0, 0, 0
	}
	if endMono <= startMono {
		return 0, 0, 0, 0
	}
	k0 := BinIndex(int64(startMono / c.QuantumTicks))
	k1 := BinIndex(int64(endMono / c.QuantumTicks))
	for k := k0; k <= k1; k++ {
		i, ok := c.idxOf(k)
		if !ok {
			continue
		}
		epkg += c.EpkgMJ[i]
		edram += c.EdramMJ[i]
		egpu += c.EgpuMJ[i]
		instr += c.CPUInstr[i]
	}
	// Defensive: avoid -0
	if math.Abs(epkg) < 1e-12 {
		epkg = 0
	}
	if math.Abs(edram) < 1e-12 {
		edram = 0
	}
	if math.Abs(egpu) < 1e-12 {
		egpu = 0
	}
	if math.Abs(instr) < 1e-12 {
		instr = 0
	}
	return epkg, edram, egpu, instr
}

func minBin(a, b BinIndex) BinIndex {
	if a < b {
		return a
	}
	return b
}
func maxBin(a, b BinIndex) BinIndex {
	if a > b {
		return a
	}
	return b
}
