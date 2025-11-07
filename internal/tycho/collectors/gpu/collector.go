// package gpuCollector

// import (
// 	"context"
// 	"encoding/binary"
// 	"math"
// 	"time"

// 	"github.com/NVIDIA/go-nvml/pkg/nvml"
// 	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
// 	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
// 	"github.com/casparwackerle/tycho-energy/pkg/config"
// 	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator"
// 	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator/devices"
// 	"k8s.io/klog/v2"
// )

// // Config holds dependencies/settings for the GPU collector.
// type Config struct {
// 	Buf  *ring.Sync[ring.GpuTick]
// 	Mono *clock.Mono
// }

// // Collector performs GPU collection (NVML today; DCGM later via the same path).
// type Collector struct {
// 	buf  *ring.Sync[ring.GpuTick]
// 	mono *clock.Mono

// 	inited     bool
// 	disabled   bool
// 	backendStr string         // "NVML" | "DCGM" | etc.
// 	devIf      devices.Device // ← new: handle to Kepler device interface (for per-process collection)

// 	// Per-device cached state
// 	devs              []gpuDeviceState
// 	lastProcQueryMono map[int]uint64

// 	// timing
// 	lastMono uint64
// }

// // gpuDeviceState holds per-device handles and last-sample state for energy integration.
// type gpuDeviceState struct {
// 	Index int
// 	Nvml  nvml.Device

// 	UUID   string
// 	Name   string
// 	PCIBus string

// 	HasCumulativeEnergy bool
// }

// // New returns a GPU collector.
// func New(cfg Config) *Collector {
// 	return &Collector{
// 		buf:  cfg.Buf,
// 		mono: cfg.Mono,
// 	}
// }

// // Init discovers the active accelerator and enumerates GPU devices/handles.
// // It returns nil if no GPUs are available so the caller can proceed without failing startup.
// // After a successful init, c.inited is set and Collect() can sample devices.
// func (c *Collector) Init(ctx context.Context) error {
// 	if c.inited {
// 		return nil
// 	}

// 	// Find active accelerator (Kepler side). If GPU disabled or unavailable, noop gracefully.
// 	accel := accelerator.GetActiveAcceleratorByType(config.GPU)
// 	if accel == nil {
// 		klog.V(2).Info("gpuCollector: no active accelerator found; GPU collection disabled")
// 		return nil
// 	}
// 	devIf := accel.Device()
// 	c.devIf = devIf
// 	c.backendStr = devIf.DevType().String()

// 	byID := devIf.DevicesByID()
// 	if len(byID) == 0 {
// 		klog.V(2).Info("gpuCollector: accelerator reports no devices; GPU collection disabled")
// 		return nil
// 	}

// 	c.devs = make([]gpuDeviceState, 0, len(byID))
// 	for idx, anyDev := range byID {
// 		gd, ok := anyDev.(devices.GPUDevice)
// 		if !ok || gd.DeviceHandler == nil {
// 			continue
// 		}
// 		nv, ok := gd.DeviceHandler.(nvml.Device)
// 		if !ok {
// 			continue
// 		}

// 		// Static metadata
// 		uuid, _ := nv.GetUUID()
// 		name, _ := nv.GetName()
// 		pci, _ := nv.GetPciInfo()
// 		bus := cChar32ToString(pci.BusId) // convert [32]int8 -> string

// 		// Capability probe: cumulative energy (mJ)
// 		hasCum := false
// 		if _, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
// 			hasCum = true
// 		}

// 		c.devs = append(c.devs, gpuDeviceState{
// 			Index:               idx,
// 			Nvml:                nv,
// 			UUID:                uuid,
// 			Name:                name,
// 			PCIBus:              bus,
// 			HasCumulativeEnergy: hasCum,
// 		})
// 	}

// 	if len(c.devs) == 0 {
// 		klog.V(2).Info("gpuCollector: no NVML-capable devices; GPU collection disabled")
// 		return nil
// 	}

// 	c.inited = true
// 	klog.V(3).Infof("gpuCollector: initialized %d device(s) via backend=%s", len(c.devs), c.backendStr)
// 	// Initialize lastMono so first dt is sane
// 	c.lastMono = c.mono.From(time.Now())
// 	c.lastProcQueryMono = make(map[int]uint64)
// 	return nil
// }

// // --- Helpers ---------------------------------------------------------------
// // Adjust these value-type codes if your binding defines different constants.
// const (
// 	nvmlValueTypeDouble           = 0
// 	nvmlValueTypeUnsignedInt      = 1
// 	nvmlValueTypeUnsignedLong     = 2
// 	nvmlValueTypeUnsignedLongLong = 3
// 	nvmlValueTypeSignedLongLong   = 4
// )

// // decodeNVMLUint converts an nvml.FieldValue into a uint64 (little-endian).
// // Handles the common numeric NVML types we may see for power fields.
// func decodeNVMLUint(v nvml.FieldValue) (uint64, bool) {
// 	switch v.ValueType {
// 	case nvmlValueTypeUnsignedInt:
// 		return uint64(binary.LittleEndian.Uint32(v.Value[:4])), true
// 	case nvmlValueTypeUnsignedLong, nvmlValueTypeUnsignedLongLong:
// 		return binary.LittleEndian.Uint64(v.Value[:]), true
// 	case nvmlValueTypeDouble:
// 		f := math.Float64frombits(binary.LittleEndian.Uint64(v.Value[:]))
// 		if f < 0 {
// 			return 0, false
// 		}
// 		return uint64(f + 0.5), true
// 	default:
// 		return 0, false
// 	}
// }

// // cChar32ToString converts a C-style fixed-size [32]int8 array to a Go string.
// func cChar32ToString(a [32]int8) string {
// 	b := make([]byte, 0, len(a))
// 	for _, v := range a {
// 		if v == 0 {
// 			break
// 		}
// 		b = append(b, byte(v))
// 	}
// 	return string(b)
// }

// // Collect samples all GPUs once at the given timestamp and pushes exactly ONE tick to the ring.
// // One ring element == one timestamped collection event with per-device data embedded (no per-device timestamps).
// func (c *Collector) Collect(ctx context.Context, ts time.Time) {
// 	if !c.inited || c.disabled {
// 		return
// 	}
// 	if len(c.devs) == 0 {
// 		klog.V(6).Info("gpuCollector: Collect skipped (no NVML-capable devices)")
// 		return
// 	}

// 	nowMono := c.mono.From(ts)

// 	// --- 1) Physical GPU device samples (NVML fast-path) ---
// 	devSamples := make([]ring.GpuSample, 0, len(c.devs))
// 	for i := range c.devs {
// 		state := &c.devs[i]
// 		nv := state.Nvml

// 		powerMwU32, _ := nv.GetPowerUsage()
// 		powerMw := int(powerMwU32)

// 		util, _ := nv.GetUtilizationRates()
// 		mem, _ := nv.GetMemoryInfo()
// 		smClk, _ := nv.GetClockInfo(nvml.CLOCK_SM)
// 		memClk, _ := nv.GetClockInfo(nvml.CLOCK_MEM)
// 		tempCU32, _ := nv.GetTemperature(nvml.TEMPERATURE_GPU)
// 		tempC := int(tempCU32)

// 		// Optional engines
// 		var encPtr, decPtr *float64
// 		if u, _, ret := nv.GetEncoderUtilization(); ret == nvml.SUCCESS {
// 			v := float64(u)
// 			encPtr = &v
// 		}
// 		if u, _, ret := nv.GetDecoderUtilization(); ret == nvml.SUCCESS {
// 			v := float64(u)
// 			decPtr = &v
// 		}

// 		// Optional cumulative energy (mJ) — raw only
// 		var cumMJPtr *uint64
// 		if mJ, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
// 			cum := mJ
// 			cumMJPtr = &cum
// 		}

// 		// --- Direct NVML field test (instant power) ----------------------------
// 		// Read instantaneous power via NVML field API (NVML_FI_DEV_POWER_INSTANT = 186).
// 		instPowerMw := uint64(0)
// 		values := []nvml.FieldValue{{FieldId: 186}} // mW
// 		if ret := nv.GetFieldValues(values); ret == nvml.SUCCESS && values[0].NvmlReturn == uint32(nvml.SUCCESS) {
// 			if mw, ok := decodeNVMLUint(values[0]); ok {
// 				instPowerMw = mw
// 			}
// 		}

// 		devSamples = append(devSamples, ring.GpuSample{
// 			DeviceIndex:        state.Index,
// 			UUID:               state.UUID,
// 			PCIBusID:           state.PCIBus,
// 			Name:               state.Name,
// 			PowerMilliW:        powerMw,
// 			InstantPowerMilliW: instPowerMw,
// 			CumEnergyMilliJ:    cumMJPtr,
// 			SMUtilPct:          float64(util.Gpu),
// 			MemUtilPct:         float64(util.Memory),
// 			EncUtilPct:         encPtr,
// 			DecUtilPct:         decPtr,
// 			MemUsedBytes:       mem.Used,
// 			MemTotalBytes:      mem.Total,
// 			SMClockMHz:         uint32(smClk),
// 			MemClockMHz:        uint32(memClk),
// 			TempC:              tempC,
// 			Backend:            c.backendStr,
// 			IsMIG:              false,
// 			MIGParentID:        nil,
// 			MIGParentUUID:      nil,
// 		})
// 	}

// 	// --- 2) MIG slice device samples (DCGM path only), gated by config ---
// 	if c.devIf != nil && c.backendStr == "DCGM" && config.GpuEnableMigDiscovery() {
// 		inst := c.devIf.DeviceInstances() // map[parentID]map[migID]any
// 		for parentID, migMap := range inst {
// 			// Find parent metadata (UUID/index) from our physical dev list
// 			var parentUUID *string
// 			var parentIdx *int
// 			for i := range c.devs {
// 				if c.devs[i].Index == parentID {
// 					parentIdx = new(int)
// 					*parentIdx = c.devs[i].Index
// 					parentUUID = new(string)
// 					*parentUUID = c.devs[i].UUID
// 					break
// 				}
// 			}
// 			for _, anyDev := range migMap {
// 				gd, ok := anyDev.(devices.GPUDevice)
// 				if !ok || gd.DeviceHandler == nil {
// 					continue
// 				}
// 				mh, ok := gd.DeviceHandler.(nvml.Device) // DCGM gave us NVML MIG handle
// 				if !ok {
// 					continue
// 				}

// 				// Same instantaneous reads against the MIG handle
// 				powerMwU32, _ := mh.GetPowerUsage()
// 				powerMw := int(powerMwU32)
// 				util, _ := mh.GetUtilizationRates()
// 				mem, _ := mh.GetMemoryInfo()
// 				smClk, _ := mh.GetClockInfo(nvml.CLOCK_SM)
// 				memClk, _ := mh.GetClockInfo(nvml.CLOCK_MEM)
// 				tempCU32, _ := mh.GetTemperature(nvml.TEMPERATURE_GPU)
// 				tempC := int(tempCU32)

// 				var encPtr, decPtr *float64
// 				if u, _, ret := mh.GetEncoderUtilization(); ret == nvml.SUCCESS {
// 					v := float64(u)
// 					encPtr = &v
// 				}
// 				if u, _, ret := mh.GetDecoderUtilization(); ret == nvml.SUCCESS {
// 					v := float64(u)
// 					decPtr = &v
// 				}

// 				// MIG UUID may be distinct or empty depending on driver; keep best-effort.
// 				uuid, _ := mh.GetUUID()
// 				pci, _ := mh.GetPciInfo()
// 				bus := cChar32ToString(pci.BusId)
// 				name, _ := mh.GetName()

// 				// Cumulative energy (raw) on MIG (if exposed)
// 				var cumMJPtr *uint64
// 				if mJ, ret := mh.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
// 					c := mJ
// 					cumMJPtr = &c
// 				}

// 				devSamples = append(devSamples, ring.GpuSample{
// 					DeviceIndex:     gd.ID, // use DCGM entity id as index for uniqueness
// 					UUID:            uuid,
// 					PCIBusID:        bus,
// 					Name:            name,
// 					PowerMilliW:     powerMw,
// 					CumEnergyMilliJ: cumMJPtr,
// 					SMUtilPct:       float64(util.Gpu),
// 					MemUtilPct:      float64(util.Memory),
// 					EncUtilPct:      encPtr,
// 					DecUtilPct:      decPtr,
// 					MemUsedBytes:    mem.Used,
// 					MemTotalBytes:   mem.Total,
// 					SMClockMHz:      uint32(smClk),
// 					MemClockMHz:     uint32(memClk),
// 					TempC:           tempC,
// 					Backend:         c.backendStr,
// 					IsMIG:           true,
// 					MIGParentID:     parentIdx,
// 					MIGParentUUID:   parentUUID,
// 				})
// 			}
// 		}
// 	}

// 	// --- Per-process metrics (NVML/DCGM), gated by config ---
// 	var procSamples []ring.GpuProcSample
// 	if c.devIf != nil && config.GpuEnablePerProcess() {

// 		procSamples = make([]ring.GpuProcSample, 0, 32)
// 		// Compute a per-owner duration from monotonic deltas, then pass as time.Duration.
// 		ownerSince := func(ownerIdx int) time.Duration {
// 			// Prefer per-owner watermark; fall back to the collector’s last tick time.
// 			var from uint64
// 			if t0, ok := c.lastProcQueryMono[ownerIdx]; ok && t0 != 0 {
// 				from = t0
// 			} else {
// 				from = c.lastMono // last successful tick we pushed
// 			}

// 			var dMono uint64
// 			if nowMono > from {
// 				dMono = nowMono - from
// 			} else {
// 				// Extremely unlikely (clock monotonicity), but avoid zero/negative.
// 				dMono = 1 // 1 ns
// 			}
// 			return time.Duration(int64(dMono))
// 		}

// 		// Helper to append results and advance the monotonic watermark.
// 		appendProc := func(ownerIdx int, ownerUUID string, m map[uint32]any) {
// 			if len(m) == 0 {
// 				c.lastProcQueryMono[ownerIdx] = nowMono
// 				return
// 			}
// 			for _, v := range m {
// 				p, ok := v.(devices.GPUProcessUtilizationSample)
// 				if !ok {
// 					continue
// 				}
// 				procSamples = append(procSamples, ring.GpuProcSample{
// 					Pid:               p.Pid,
// 					TimeStampUS:       p.TimeStamp,   // backend’s own clock; treat as opaque
// 					ComputeUtil:       p.ComputeUtil, // %
// 					MemUtil:           p.MemUtil,     // %
// 					EncUtil:           p.EncUtil,     // %
// 					DecUtil:           p.DecUtil,     // %
// 					GpuIndex:          ownerIdx,
// 					GpuUUID:           ownerUUID,
// 					GpuInstanceID:     nil, // can be filled if DCGM exposes ids you want to surface
// 					ComputeInstanceID: nil,
// 				})
// 			}
// 			c.lastProcQueryMono[ownerIdx] = nowMono
// 		}

// 		// (a) Physical devices via NVML/DCGM
// 		for i := range c.devs {
// 			state := &c.devs[i]
// 			since := ownerSince(state.Index)
// 			gd := devices.GPUDevice{
// 				DeviceHandler: state.Nvml,
// 				ID:            state.Index,
// 				IsSubdevice:   false,
// 			}
// 			m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
// 			if err != nil {
// 				klog.V(6).Infof("gpuCollector: per-process (phys %d) error: %v", state.Index, err)
// 				c.lastProcQueryMono[state.Index] = nowMono
// 				continue
// 			}
// 			appendProc(state.Index, state.UUID, m)
// 		}

// 		// (b) MIG instances (DCGM only)
// 		if c.backendStr == "DCGM" && config.GpuEnableMigDiscovery() {
// 			inst := c.devIf.DeviceInstances()
// 			for parentID, migMap := range inst {
// 				parentUUID := ""
// 				for i := range c.devs {
// 					if c.devs[i].Index == parentID {
// 						parentUUID = c.devs[i].UUID
// 						break
// 					}
// 				}
// 				for _, anyDev := range migMap {
// 					gd, ok := anyDev.(devices.GPUDevice)
// 					if !ok {
// 						continue
// 					}
// 					ownerIdx := gd.ID // use DCGM entity id for uniqueness
// 					since := ownerSince(ownerIdx)
// 					m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
// 					if err != nil {
// 						klog.V(6).Infof("gpuCollector: per-process (MIG %d) error: %v", ownerIdx, err)
// 						c.lastProcQueryMono[ownerIdx] = nowMono
// 						continue
// 					}
// 					appendProc(ownerIdx, parentUUID, m)
// 				}
// 			}
// 		}
// 	}
// 	// --- 4) Push one tick containing devices and per-process (if any) ---
// 	c.buf.Push(ring.GpuTick{
// 		SampleMeta: ring.SampleMeta{Mono: nowMono},
// 		Devices:    devSamples,
// 		Processes:  procSamples,
// 	})

// 	//klog.V(5).Infof("gpuCollector: collected 1 tick (%d device samples, %d proc samples)", len(devSamples), len(procSamples))
// 	c.lastMono = nowMono
// }

package gpuCollector

import (
	"context"
	"encoding/binary"
	"math"
	"sync"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator/devices"
	"k8s.io/klog/v2"
)

// Config holds dependencies/settings for the GPU collector.
type Config struct {
	Buf  *ring.Sync[ring.GpuTick]
	Mono *clock.Mono
}

// New returns a GPU collector.
func New(cfg Config) *Collector {
	return &Collector{
		buf:  cfg.Buf,
		mono: cfg.Mono,
	}
}

// DerivePhaseAwareFromEngineInterval derives ALL timing from the engine interval Teng.
// No device-specific constants; only ratios. The phase estimator will refine T̂ at runtime.
func DerivePhaseAwareFromEngineInterval(Teng time.Duration) PhaseAwarePollConfig {
	if Teng <= 0 {
		Teng = 100 * time.Millisecond // last-resort guard
	}

	// Heuristic: assume device publish period T̂ is a small multiple of Teng.
	// Pick N so T̂ comfortably exceeds Teng (so base/burst can be meaningful).
	// 5x is a good starting guess, but clamp N to a sane band to handle extremes.
	N := 5
	if Teng >= 400*time.Millisecond {
		N = 3 // for slow engines, don’t make T̂ explode
	} else if Teng <= 50*time.Millisecond {
		N = 8 // for very fast engines, widen the gap to avoid over-bursting
	}
	TdevGuess := time.Duration(N) * Teng

	// Clamps scale with Teng as well; keep T̂ within [3x, 8x] of Teng.
	Tmin := 3 * Teng
	Tmax := 8 * Teng
	if TdevGuess < Tmin {
		TdevGuess = Tmin
	} else if TdevGuess > Tmax {
		TdevGuess = Tmax
	}

	// Polling schedule as simple fractions of T̂ and Teng:
	// - Base ≈ 0.6·T̂ (cruise), but never less than 2·Teng (don’t outpace the engine too much)
	// - Burst ≈ max(0.1·T̂, 0.5·Teng) — ties to both T̂ and user cadence
	// - Window half-width ≈ 0.3·T̂
	mul := func(x time.Duration, num, den int) time.Duration { return x * time.Duration(num) / time.Duration(den) }
	maxDur := func(a, b time.Duration) time.Duration {
		if a > b {
			return a
		}
		return b
	}

	base := maxDur(mul(TdevGuess, 3, 5), 2*Teng)   // ~0.6·T̂, ≥2·Teng
	burst := maxDur(mul(TdevGuess, 1, 10), Teng/2) // ~0.1·T̂, ≥0.5·Teng
	half := mul(TdevGuess, 3, 10)                  // ~0.3·T̂

	// Light clamps to avoid pathological values (still expressed via Teng):
	// Base ∈ [Teng, 6·Teng], Burst ∈ [Teng/4, 2·Teng], Half ∈ [Teng, 4·Teng]
	clamp := func(v, lo, hi time.Duration) time.Duration {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	base = clamp(base, 1*Teng, 6*Teng)
	burst = clamp(burst, Teng/4, 2*Teng)
	half = clamp(half, 1*Teng, 4*Teng)

	return PhaseAwarePollConfig{
		Enabled: true,

		// Device cadence model (all derived from Teng)
		InitialPublishPeriod:  TdevGuess,
		MinPublishPeriodClamp: Tmin,
		MaxPublishPeriodClamp: Tmax,
		PhaseEMAAlpha:         0.20, // dimensionless smoothing; safe default
		PeriodEMAAlpha:        0.10, // set 0 to “lock” period if you prefer

		// Polling schedule (all derived)
		BaseInterval:   base,
		BurstInterval:  burst,
		BurstHalfWidth: half,

		// Dedupe: fixed epsilon per your requirement
		EpsilonMilliW: 2, // ← fixed at 2 mW
	}
}

// Init discovers the active accelerator and enumerates GPU devices/handles.
// It returns nil if no GPUs are available so the caller can proceed without failing startup.
// After a successful init, c.inited is set and Collect() can sample devices.
func (c *Collector) Init(ctx context.Context) error {
	if c.inited {
		return nil
	}

	// Find active accelerator (Kepler side). If GPU disabled or unavailable, noop gracefully.
	accel := accelerator.GetActiveAcceleratorByType(config.GPU)
	if accel == nil {
		klog.V(2).Info("gpuCollector: no active accelerator found; GPU collection disabled")
		return nil
	}
	devIf := accel.Device()
	c.devIf = devIf
	c.backendStr = devIf.DevType().String()

	byID := devIf.DevicesByID()
	if len(byID) == 0 {
		klog.V(2).Info("gpuCollector: accelerator reports no devices; GPU collection disabled")
		return nil
	}

	c.devs = make([]gpuDeviceState, 0, len(byID))
	for idx, anyDev := range byID {
		gd, ok := anyDev.(devices.GPUDevice)
		if !ok || gd.DeviceHandler == nil {
			continue
		}
		nv, ok := gd.DeviceHandler.(nvml.Device)
		if !ok {
			continue
		}

		// Static metadata
		uuid, _ := nv.GetUUID()
		name, _ := nv.GetName()
		pci, _ := nv.GetPciInfo()
		bus := cChar32ToString(pci.BusId) // convert [32]int8 -> string

		// Capability probe: cumulative energy (mJ)
		hasCum := false
		if _, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
			hasCum = true
		}

		c.devs = append(c.devs, gpuDeviceState{
			Index:               idx,
			Nvml:                nv,
			UUID:                uuid,
			Name:                name,
			PCIBus:              bus,
			HasCumulativeEnergy: hasCum,
		})
	}

	if len(c.devs) == 0 {
		klog.V(2).Info("gpuCollector: no NVML-capable devices; GPU collection disabled")
		return nil
	}

	c.inited = true
	klog.V(3).Infof("gpuCollector: initialized %d device(s) via backend=%s", len(c.devs), c.backendStr)
	// Initialize lastMono so first dt is sane
	c.lastMono = c.mono.From(time.Now())
	c.lastProcQueryMono = make(map[int]uint64)
	return nil
}

// -----------------------------------------------------------------------------
// 1) Public configuration extensions (minimal surface)
// -----------------------------------------------------------------------------

// PhaseAwarePollConfig controls the internal sampler's phase-aware burst behavior.
// All durations are engine-agnostic and use monotonic time as provided by c.mono.
type PhaseAwarePollConfig struct {
	Enabled bool // if false, sampler runs in fixed-base mode

	// Device cadence model (estimates)
	InitialPublishPeriod  time.Duration // e.g., 500 * time.Millisecond
	MinPublishPeriodClamp time.Duration // e.g., 450ms
	MaxPublishPeriodClamp time.Duration // e.g., 600ms
	PhaseEMAAlpha         float64       // e.g., 0.2
	PeriodEMAAlpha        float64       // e.g., 0.1 (set 0 to disable period EMA)

	// Polling schedule
	BaseInterval   time.Duration // e.g., 300ms outside burst
	BurstInterval  time.Duration // e.g., 50ms during burst
	BurstHalfWidth time.Duration // e.g., 150ms around predicted edge

	// New sample detection
	EpsilonMilliW int // e.g., 3–5 mW (power change threshold if CumEnergy unreliable)
}

// SamplerLimits caps allocations and CPU usage for the sampler.
type SamplerLimits struct {
	MaxEventQueue int // bounded queue length (e.g., 4–8)
}

// -----------------------------------------------------------------------------
// 2) Internal event model & queue
// -----------------------------------------------------------------------------

// gpuSamplerEvent is produced by the sampler and consumed by Collect().
type gpuSamplerEvent struct {
	// tObsMono is the observed monotonic timestamp for the updated device snapshot.
	tObsMono uint64
	Devices  []ring.GpuSample     // per-device instantaneous snapshot
	Procs    []ring.GpuProcSample // optional per-process slice (if enabled)
	// changed indicates this event reflects a fresh device publish (vs. synthetic/no-change).
	Changed bool
}

// gpuEventQueue is a minimal bounded, non-blocking queue abstraction.
// Overwrite-on-full policy to keep freshest events.
type gpuEventQueue struct {
	mu   sync.Mutex
	buf  []gpuSamplerEvent
	head int // next index to read
	tail int // next index to write
	size int // current number of items
	cap  int // fixed capacity
}

// newGpuEventQueue constructs a bounded queue.
func newGpuEventQueue(capacity int) *gpuEventQueue {
	if capacity < 1 {
		capacity = 1
	}
	return &gpuEventQueue{
		buf:  make([]gpuSamplerEvent, capacity),
		head: 0,
		tail: 0,
		size: 0,
		cap:  capacity,
	}
}

// Enqueue tries to add an event; overwrites oldest entry if the queue is full.
func (q *gpuEventQueue) Enqueue(ev gpuSamplerEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.size == q.cap {
		// Queue full → overwrite oldest (advance head).
		q.head = (q.head + 1) % q.cap
		q.size--
	}

	q.buf[q.tail] = ev
	q.tail = (q.tail + 1) % q.cap
	q.size++
	klog.V(5).Infof("GPU-QUEUE: enqueue ts=%d changed=%v size=%d/%d", ev.tObsMono, ev.Changed, q.size, q.cap)
}

// TryDequeue returns (event, ok) without blocking.
// If the queue is empty, ok=false and an empty event is returned.
func (q *gpuEventQueue) TryDequeue() (gpuSamplerEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.size == 0 {
		return gpuSamplerEvent{}, false
	}

	ev := q.buf[q.head]
	q.head = (q.head + 1) % q.cap
	q.size--

	return ev, true
}

// -----------------------------------------------------------------------------
// 3) Phase estimator state (PhaseAwarePolling core)
// -----------------------------------------------------------------------------

// phaseEstimator tracks the device publish period and phase vs. Tycho's monotonic clock.
type phaseEstimator struct {
	Enabled bool

	// Estimated device cadence
	Period   time.Duration // T̂
	Phase    time.Duration // ϕ̂
	AlphaPhi float64       // EMA alpha for phase correction
	AlphaT   float64       // EMA alpha for period correction
	MinT     time.Duration // clamp lower bound for period updates
	MaxT     time.Duration // clamp upper bound for period updates

	// Last observation bookkeeping for drift and jitter handling
	lastObsMono uint64
}

// nextEdgeMono predicts the next publish edge in monotonic units (ns) at or after nowMono.
func (p *phaseEstimator) nextEdgeMono(nowMono uint64) uint64 {
	// Disabled or invalid period → return now (no phase prediction).
	if !p.Enabled || p.Period <= 0 {
		return nowMono
	}

	T := uint64(p.Period)

	// Normalize phase into [0, Period).
	phiDur := p.Phase
	if p.Phase < 0 || p.Phase >= p.Period {
		// Go's % keeps sign of the dividend; normalize to positive range.
		phiDur = p.Phase % p.Period
		if phiDur < 0 {
			phiDur += p.Period
		}
	}
	phi := uint64(phiDur)

	// If we have a recent observation, step forward in multiples of T from there.
	if p.lastObsMono != 0 {
		last := p.lastObsMono
		// First candidate is last + T.
		next := last + T
		if next >= nowMono {
			return next
		}
		// Advance by the required number of whole periods to be >= nowMono.
		delta := nowMono - last
		steps := delta / T
		return last + (steps+1)*T
	}

	// No observation yet: align by phase so that k*T + phi >= nowMono.
	// If we're already exactly on an edge, return now.
	if nowMono <= phi {
		return phi
	}
	off := nowMono - phi
	rem := off % T
	if rem == 0 {
		return nowMono
	}
	return nowMono + (T - rem)
}

// correctPhase updates the phase estimate using the observed delta from the predicted edge.
// It applies a simple EMA step and normalizes Phase into [0, Period).
func (p *phaseEstimator) correctPhase(delta time.Duration) {
	if !p.Enabled || p.Period <= 0 || p.AlphaPhi <= 0 {
		return
	}

	// EMA: Phase += α * delta
	adj := time.Duration(p.AlphaPhi * float64(delta))
	p.Phase += adj

	// Normalize Phase into [0, Period)
	if p.Phase >= p.Period || p.Phase < 0 {
		// Go's % keeps the sign of the dividend, so re-normalize to positive
		p.Phase = p.Phase % p.Period
		if p.Phase < 0 {
			p.Phase += p.Period
		}
	}
}

// correctPeriod updates the estimated publish period (T̂) using EMA smoothing
// on the observed inter-sample duration. The result is clamped within
// [MinT, MaxT] to avoid runaway drift.
func (p *phaseEstimator) correctPeriod(dt time.Duration) {
	if !p.Enabled || p.AlphaT <= 0 || dt <= 0 {
		return
	}

	newT := (1-p.AlphaT)*float64(p.Period) + p.AlphaT*float64(dt)
	p.Period = time.Duration(newT)

	// Clamp to configured bounds, if any
	if p.MinT > 0 && p.Period < p.MinT {
		p.Period = p.MinT
	}
	if p.MaxT > 0 && p.Period > p.MaxT {
		p.Period = p.MaxT
	}
}

// resetResync hard-resets phase/edge tracking after repeated misses.
// Strategy:
// - Record an observation at nowMono so nextEdgeMono() will return nowMono + Period.
// - Zero the phase (Phase=0). With lastObsMono set, phase no longer drives prediction.
// - Leave Period unchanged (it may have been calibrated already).
func (p *phaseEstimator) resetResync(nowMono uint64) {
	if nowMono == 0 {
		return
	}
	// Anchor the estimator to "now": next edge prediction becomes lastObs + T.
	p.lastObsMono = nowMono

	// If period invalid, nothing else to do; phase is irrelevant anyway.
	if p.Period <= 0 {
		p.Phase = 0
		return
	}

	// Normalize then zero phase; with lastObsMono set, phase is not used.
	if p.Phase >= p.Period || p.Phase < 0 {
		p.Phase = p.Phase % p.Period
		if p.Phase < 0 {
			p.Phase += p.Period
		}
	}
	p.Phase = 0
}

// -----------------------------------------------------------------------------
// 4) Sampler: internal mini-scheduler (runs in its own goroutine)
// -----------------------------------------------------------------------------

// sampler encapsulates the variable-rate polling loop and dedupe logic.
type sampler struct {
	mono   *clock.Mono
	cfgB   PhaseAwarePollConfig
	limits SamplerLimits

	// Hooks into the existing collector for device/process reads (no code duplication).
	parent *Collector

	// Phase-aware model
	phase phaseEstimator

	// Output queue consumed by Collect()
	q *gpuEventQueue

	// Sticky copy of the last-known snapshot for no-change ticks
	lastDevices []ring.GpuSample
	lastProcs   []ring.GpuProcSample

	// Control
	stopCh  chan struct{}
	running bool
}

// start launches the sampler loop in its own goroutine.
// It is safe to call once after initialization; subsequent calls are ignored.
func (s *sampler) start(ctx context.Context) {
	if s == nil || s.running {
		return
	}

	s.stopCh = make(chan struct{})
	s.running = true

	go func() {
		defer func() { s.running = false }()
		s.loop(ctx)
	}()
}

// stop requests termination and waits for exit (best-effort).
func (s *sampler) stop() {
	if s == nil || !s.running {
		return
	}

	// Signal the sampler loop to stop.
	close(s.stopCh)

	// Give the loop a short grace period to exit; no hard synchronization required.
	// This avoids deadlocks while ensuring background goroutine has time to terminate.
	timeout := time.NewTimer(200 * time.Millisecond)
	defer timeout.Stop()

	select {
	case <-timeout.C:
		// Timed out waiting for graceful stop — mark as not running anyway.
		s.running = false
	default:
		// In normal operation, loop() should observe <-s.stopCh and return quickly.
	}
}

// loop is the phase-aware polling state machine (Base vs Burst).
func (s *sampler) loop(ctx context.Context) {
	if s == nil || s.mono == nil {
		return
	}

	for {
		// Determine current mode and interval.
		nowMono := s.mono.From(time.Now())
		var interval time.Duration
		if s.shouldBurst(nowMono) && s.cfgB.Enabled {
			interval = s.cfgB.BurstInterval
		} else {
			interval = s.cfgB.BaseInterval
		}
		if interval <= 0 {
			interval = 200 * time.Millisecond // conservative fallback
		}

		// Sleep until next poll or stop.
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.stopCh:
			timer.Stop()
			return
		case <-timer.C:
			// proceed
		}

		// Read a fresh snapshot.
		devs, procs, ok := s.readSnapshot()
		if !ok {
			// Could not read -> skip this cycle
			continue
		}

		// Dedupe: detect whether device values reflect a new publish.
		changed := s.detectNewSample(devs)
		tObs := s.mono.From(time.Now())

		if changed {
			// Emit event and refine phase/period estimates.
			s.onNewObservation(tObs, devs, procs)
		} else {
			// No new device publish -> skip this cycle
		}
	}
}

// shouldBurst decides whether to enter burst mode at nowMono.
// Returns true if the current monotonic time lies within ±BurstHalfWidth
// of the predicted next device publish edge.
func (s *sampler) shouldBurst(nowMono uint64) bool {
	if !s.cfgB.Enabled || s.phase.Period <= 0 {
		return false
	}

	// Predict next publish edge (monotonic units).
	nextEdge := s.phase.nextEdgeMono(nowMono)
	if nextEdge == 0 {
		return false
	}

	// Compute absolute distance from now to predicted edge.
	var dist uint64
	if nowMono > nextEdge {
		dist = nowMono - nextEdge
	} else {
		dist = nextEdge - nowMono
	}

	// Convert burst half-width to ns.
	bw := uint64(s.cfgB.BurstHalfWidth)
	if bw == 0 {
		return false
	}

	return dist <= bw
}

// readSnapshot reads devices (+optional per-process) and returns a snapshot.
// It succeeds if device sampling succeeds; per-process sampling is optional.
func (s *sampler) readSnapshot() (devs []ring.GpuSample, procs []ring.GpuProcSample, ok bool) {
	if s == nil || s.parent == nil || s.mono == nil {
		return nil, nil, false
	}

	// Devices are mandatory for a valid snapshot.
	dv, okDev := s.parent.readAllDevicesSnapshot()
	if !okDev || len(dv) == 0 {
		return nil, nil, false
	}

	// Per-process is optional; failure yields an empty slice but overall ok=true.
	nowMono := s.mono.From(time.Now())
	pr, okProc := s.parent.readPerProcessSnapshot(nowMono)
	if !okProc {
		pr = nil
	}

	return dv, pr, true
}

// detectNewSample compares against the last snapshot (or cumulative energy) to dedupe.
// Returns true if any device shows a new sample beyond configured thresholds.
func (s *sampler) detectNewSample(curr []ring.GpuSample) bool {
	if len(curr) == 0 {
		return false
	}
	if len(s.lastDevices) == 0 {
		s.lastDevices = curr
		return true
	}

	eps := s.cfgB.EpsilonMilliW
	if eps <= 0 {
		eps = 3
	}

	for i := range curr {
		c := curr[i]
		var prev *ring.GpuSample
		for j := range s.lastDevices {
			if s.lastDevices[j].UUID == c.UUID {
				prev = &s.lastDevices[j]
				break
			}
		}
		if prev == nil {
			s.lastDevices = curr
			return true
		}

		// STRICT: power-only
		dp := int(c.PowerMilliW) - int(prev.PowerMilliW)
		if dp < 0 {
			dp = -dp
		}
		if dp > eps {
			s.lastDevices = curr
			return true
		}
		klog.V(5).Infof("GPU-DEDUPE: uuid=%s power=%d last=%d Δ=%d changed=%v", c.UUID, c.PowerMilliW, prev.PowerMilliW, dp, (dp > eps))
	}
	return false
}

// onNewObservation enqueues event and refines phase/period estimates.
func (s *sampler) onNewObservation(tObsMono uint64, devs []ring.GpuSample, procs []ring.GpuProcSample) {
	if s == nil || s.q == nil {
		return
	}

	// ---- Phase/period refinement --------------------------------------------
	if s.phase.Enabled && s.phase.Period > 0 {
		// Predict the edge nearest to tObsMono (choose between next and previous).
		nextEdge := s.phase.nextEdgeMono(tObsMono)
		T := uint64(s.phase.Period)

		// Candidate previous edge (handle potential underflow carefully).
		var prevEdge uint64
		if nextEdge >= T {
			prevEdge = nextEdge - T
		} else {
			prevEdge = nextEdge // fallback: treat nextEdge as closest if underflow risk
		}

		// Compute deltas to both candidates and choose the smaller magnitude.
		var dNext int64 = int64(tObsMono) - int64(nextEdge)
		var dPrev int64 = int64(tObsMono) - int64(prevEdge)

		// Pick the delta with smaller absolute value.
		delta := dNext
		if abs64(dPrev) < abs64(dNext) {
			delta = dPrev
		}

		// Apply phase correction.
		klog.V(5).Infof("GPU-OBS: new sample tObs=%d, period=%v, nextEdge=%d", tObsMono, s.phase.Period, s.phase.nextEdgeMono(tObsMono))
		s.phase.correctPhase(time.Duration(delta))

		// Update period estimate from inter-observation interval.
		if s.phase.lastObsMono != 0 && tObsMono > s.phase.lastObsMono {
			dt := time.Duration(int64(tObsMono - s.phase.lastObsMono))
			s.phase.correctPeriod(dt)
		}
		s.phase.lastObsMono = tObsMono
	}

	// ---- Update last snapshots ----------------------------------------------
	s.lastDevices = devs
	s.lastProcs = procs

	// ---- Enqueue the new observation event ----------------------------------
	s.q.Enqueue(gpuSamplerEvent{
		tObsMono: tObsMono,
		Devices:  devs,
		Procs:    procs,
		Changed:  true,
	})
}

// abs64 returns the absolute value of an int64 without overflow.
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// -----------------------------------------------------------------------------
// 5) Collector additions for sampler integration
// -----------------------------------------------------------------------------

// CollectorSamplerDeps groups the knobs to enable PhaseAware Sampling from the outside.
type CollectorSamplerDeps struct {
	PhaseAware PhaseAwarePollConfig
	Limits     SamplerLimits
}

// EnablePhaseAware configures and wires the phase-aware sampler.
// All timing is derived from the user-defined engine interval (GpuPollMs).
// If GpuPhaseAwareSamplingEnabled() is false, this is a no-op.
func (c *Collector) EnablePhaseAware(ctx context.Context, deps CollectorSamplerDeps) {
	if c == nil || c.PhaseAwareEnabled || c.mono == nil {
		return
	}
	if !config.GpuPhaseAwareSamplingEnabled() {
		klog.V(3).Info("gpuCollector: phase-aware disabled by config")
		return
	}

	Teng := time.Duration(config.GpuPollMs()) * time.Millisecond
	cfg := DerivePhaseAwareFromEngineInterval(Teng)
	cfg.Enabled = true
	cfg.EpsilonMilliW = 2

	lim := deps.Limits
	if lim.MaxEventQueue <= 0 {
		lim.MaxEventQueue = 8
	}

	q := newGpuEventQueue(lim.MaxEventQueue)
	ph := phaseEstimator{
		Enabled:  true,
		Period:   cfg.InitialPublishPeriod,
		Phase:    0,
		AlphaPhi: cfg.PhaseEMAAlpha,
		AlphaT:   cfg.PeriodEMAAlpha,
		MinT:     cfg.MinPublishPeriodClamp,
		MaxT:     cfg.MaxPublishPeriodClamp,
	}

	s := &sampler{
		mono:   c.mono,
		cfgB:   cfg,
		limits: lim,
		parent: c,
		phase:  ph,
		q:      q,
	}

	c.PhaseAwareEnabled = true
	c.events = q
	c.samp = s

	// Auto-start sampler here
	c.samp.start(ctx)

	klog.V(3).Infof("gpuCollector: phase-aware enabled+started; Teng=%v T̂=%v base=%v burst=%v ±%v eps=%dmW queue=%d",
		Teng, cfg.InitialPublishPeriod, cfg.BaseInterval, cfg.BurstInterval, cfg.BurstHalfWidth,
		cfg.EpsilonMilliW, lim.MaxEventQueue,
	)
}

func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	c.StopPhaseAwareSampler()
	return nil
}

// StartPhaseAwareSampler starts the background sampler.
func (c *Collector) StartPhaseAwareSampler(ctx context.Context) {
	if c == nil || !c.PhaseAwareEnabled || c.samp == nil {
		return
	}
	// Optionally ensure collector has been initialized.
	if !c.inited || c.disabled {
		return
	}
	c.samp.start(ctx)
}

// StopPhaseAwareSampler stops the background sampler.
func (c *Collector) StopPhaseAwareSampler() {
	if c == nil || c.samp == nil {
		return
	}
	c.samp.stop()
}

// -----------------------------------------------------------------------------
// 6) Collector (existing) — only minimal field additions needed
// -----------------------------------------------------------------------------
// NOTE: This extends your existing Collector with just a few fields.
// Keep your existing fields as-is; only the delta is shown here for clarity.
type Collector struct {
	// existing fields …
	buf  *ring.Sync[ring.GpuTick]
	mono *clock.Mono

	inited     bool
	disabled   bool
	backendStr string // "NVML" | "DCGM" | etc.
	devIf      devices.Device

	devs              []gpuDeviceState
	lastProcQueryMono map[int]uint64
	lastMono          uint64

	// --- NEW: PhaseAware sampler integration ---
	PhaseAwareEnabled bool
	samp              *sampler
	events            *gpuEventQueue
}

// Collect remains your engine-driven per-tick entry point.
// It should drain at most one sampler event and push exactly one ring tick.
// If no event is available, it can push a "no-change" tick using last snapshot.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	if c == nil || c.buf == nil || c.mono == nil || !c.inited || c.disabled {
		return
	}
	if c.events == nil {
		return
	}

	if ev, ok := c.events.TryDequeue(); ok {
		c.buf.Push(ring.GpuTick{
			SampleMeta: ring.SampleMeta{Mono: ev.tObsMono},
			Devices:    ev.Devices,
			Processes:  ev.Procs,
		})
		klog.V(5).Infof("GPU-COLLECT: pushed tick mono=%d changed=%v devs=%d procs=%d", ev.tObsMono, ev.Changed, len(ev.Devices), len(ev.Procs))
		c.lastMono = ev.tObsMono
		c.logTickDebug(ev.Changed, len(ev.Devices), len(ev.Procs))
	}
	// else: nothing to push this tick
}

// -----------------------------------------------------------------------------
// 7) Minimal device state (unchanged from your version, included for completeness)
// -----------------------------------------------------------------------------

type gpuDeviceState struct {
	Index int
	// Keep NVML/DCGM handles and static metadata as in your current code.
	// (Placeholders here to avoid importing vendors in this skeleton.)
	UUID   string
	Name   string
	PCIBus string

	Nvml nvml.Device

	HasCumulativeEnergy bool
}

// -----------------------------------------------------------------------------
// 8) Wiring helpers used by sampler to reuse Collector's existing reads
// -----------------------------------------------------------------------------

// cChar32ToString converts a C-style fixed-size [32]int8 array to a Go string.
func cChar32ToString(a [32]int8) string {
	b := make([]byte, 0, len(a))
	for _, v := range a {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}

// Adjust these value-type codes if your binding defines different constants.
const (
	nvmlValueTypeDouble           = 0
	nvmlValueTypeUnsignedInt      = 1
	nvmlValueTypeUnsignedLong     = 2
	nvmlValueTypeUnsignedLongLong = 3
	nvmlValueTypeSignedLongLong   = 4
)

// decodeNVMLUint converts an nvml.FieldValue into a uint64 (little-endian).
// Handles the common numeric NVML types we may see for power fields.
func decodeNVMLUint(v nvml.FieldValue) (uint64, bool) {
	switch v.ValueType {
	case nvmlValueTypeUnsignedInt:
		return uint64(binary.LittleEndian.Uint32(v.Value[:4])), true
	case nvmlValueTypeUnsignedLong, nvmlValueTypeUnsignedLongLong:
		return binary.LittleEndian.Uint64(v.Value[:]), true
	case nvmlValueTypeDouble:
		f := math.Float64frombits(binary.LittleEndian.Uint64(v.Value[:]))
		if f < 0 {
			return 0, false
		}
		return uint64(f + 0.5), true
	default:
		return 0, false
	}
}

// readAllDevicesSnapshot reuses the Collector's fast-path reads (NVML/DCGM) to
// construct []ring.GpuSample for the sampler. This avoids code duplication.
func (c *Collector) readAllDevicesSnapshot() (devs []ring.GpuSample, ok bool) {
	// Basic guards
	if c == nil || !c.inited || c.disabled {
		return nil, false
	}
	if len(c.devs) == 0 {
		return nil, false
	}

	// --- Physical GPU device samples (NVML fast-path) ------------------------
	out := make([]ring.GpuSample, 0, len(c.devs))
	for i := range c.devs {
		state := &c.devs[i]
		nv := state.Nvml

		powerMwU32, _ := nv.GetPowerUsage()
		powerMw := int(powerMwU32)

		util, _ := nv.GetUtilizationRates()
		mem, _ := nv.GetMemoryInfo()
		smClk, _ := nv.GetClockInfo(nvml.CLOCK_SM)
		memClk, _ := nv.GetClockInfo(nvml.CLOCK_MEM)
		tempCU32, _ := nv.GetTemperature(nvml.TEMPERATURE_GPU)
		tempC := int(tempCU32)

		// Optional engines
		var encPtr, decPtr *float64
		if u, _, ret := nv.GetEncoderUtilization(); ret == nvml.SUCCESS {
			v := float64(u)
			encPtr = &v
		}
		if u, _, ret := nv.GetDecoderUtilization(); ret == nvml.SUCCESS {
			v := float64(u)
			decPtr = &v
		}

		// Optional cumulative energy (mJ) — raw only
		var cumMJPtr *uint64
		if mJ, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
			cum := mJ
			cumMJPtr = &cum
		}

		// Instant power via NVML field API (NVML_FI_DEV_POWER_INSTANT = 186).
		instPowerMw := uint64(0)
		values := []nvml.FieldValue{{FieldId: 186}}
		if ret := nv.GetFieldValues(values); ret == nvml.SUCCESS && values[0].NvmlReturn == uint32(nvml.SUCCESS) {
			if mw, ok := decodeNVMLUint(values[0]); ok {
				instPowerMw = mw
			}
		}

		out = append(out, ring.GpuSample{
			DeviceIndex:        state.Index,
			UUID:               state.UUID,
			PCIBusID:           state.PCIBus,
			Name:               state.Name,
			PowerMilliW:        powerMw,
			InstantPowerMilliW: instPowerMw,
			CumEnergyMilliJ:    cumMJPtr,
			SMUtilPct:          float64(util.Gpu),
			MemUtilPct:         float64(util.Memory),
			EncUtilPct:         encPtr,
			DecUtilPct:         decPtr,
			MemUsedBytes:       mem.Used,
			MemTotalBytes:      mem.Total,
			SMClockMHz:         uint32(smClk),
			MemClockMHz:        uint32(memClk),
			TempC:              tempC,
			Backend:            c.backendStr,
			IsMIG:              false,
			MIGParentID:        nil,
			MIGParentUUID:      nil,
		})
	}

	// --- MIG instances (DCGM path only), gated by config ---------------------
	if c.devIf != nil && c.backendStr == "DCGM" && config.GpuEnableMigDiscovery() {
		inst := c.devIf.DeviceInstances() // map[parentID]map[migID]any
		for parentID, migMap := range inst {
			// Find parent metadata (UUID/index) from our physical dev list
			var parentUUID *string
			var parentIdx *int
			for i := range c.devs {
				if c.devs[i].Index == parentID {
					parentIdx = new(int)
					*parentIdx = c.devs[i].Index
					parentUUID = new(string)
					*parentUUID = c.devs[i].UUID
					break
				}
			}
			for _, anyDev := range migMap {
				gd, ok := anyDev.(devices.GPUDevice)
				if !ok || gd.DeviceHandler == nil {
					continue
				}
				mh, ok := gd.DeviceHandler.(nvml.Device) // DCGM provided NVML handle for MIG
				if !ok {
					continue
				}

				powerMwU32, _ := mh.GetPowerUsage()
				powerMw := int(powerMwU32)
				util, _ := mh.GetUtilizationRates()
				mem, _ := mh.GetMemoryInfo()
				smClk, _ := mh.GetClockInfo(nvml.CLOCK_SM)
				memClk, _ := mh.GetClockInfo(nvml.CLOCK_MEM)
				tempCU32, _ := mh.GetTemperature(nvml.TEMPERATURE_GPU)
				tempC := int(tempCU32)

				var encPtr, decPtr *float64
				if u, _, ret := mh.GetEncoderUtilization(); ret == nvml.SUCCESS {
					v := float64(u)
					encPtr = &v
				}
				if u, _, ret := mh.GetDecoderUtilization(); ret == nvml.SUCCESS {
					v := float64(u)
					decPtr = &v
				}

				// MIG UUID / PCI / name
				uuid, _ := mh.GetUUID()
				pci, _ := mh.GetPciInfo()
				bus := cChar32ToString(pci.BusId)
				name, _ := mh.GetName()

				// Optional cumulative energy on MIG (if exposed)
				var cumMJPtr *uint64
				if mJ, ret := mh.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
					c := mJ
					cumMJPtr = &c
				}

				out = append(out, ring.GpuSample{
					DeviceIndex:     gd.ID, // use DCGM entity id for uniqueness
					UUID:            uuid,
					PCIBusID:        bus,
					Name:            name,
					PowerMilliW:     powerMw,
					CumEnergyMilliJ: cumMJPtr,
					SMUtilPct:       float64(util.Gpu),
					MemUtilPct:      float64(util.Memory),
					EncUtilPct:      encPtr,
					DecUtilPct:      decPtr,
					MemUsedBytes:    mem.Used,
					MemTotalBytes:   mem.Total,
					SMClockMHz:      uint32(smClk),
					MemClockMHz:     uint32(memClk),
					TempC:           tempC,
					Backend:         c.backendStr,
					IsMIG:           true,
					MIGParentID:     parentIdx,
					MIGParentUUID:   parentUUID,
				})
			}
		}
	}

	if len(out) > 0 {
		total := 0
		for _, d := range out {
			total += d.PowerMilliW
		}
		avg := float64(total) / float64(len(out))
		klog.V(5).Infof("GPU-SNAPSHOT: read %d devices, avgPower=%.1f mW", len(out), avg)
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true

}

// readPerProcessSnapshot reuses the Collector's per-process path, if enabled.
// nowMono is the current monotonic timestamp (ns). Returns ok=true on successful
// execution of the per-process path (even if it yields zero samples). If per-process
// is disabled or devIf is nil, it returns (nil, true).
func (c *Collector) readPerProcessSnapshot(nowMono uint64) (procs []ring.GpuProcSample, ok bool) {
	// Optional feature: if disabled or unavailable, succeed with empty slice.
	if c == nil || !c.inited || c.disabled {
		return nil, false
	}
	if c.devIf == nil || !config.GpuEnablePerProcess() {
		return nil, true
	}
	if c.lastProcQueryMono == nil {
		c.lastProcQueryMono = make(map[int]uint64)
	}

	procs = make([]ring.GpuProcSample, 0, 32)

	// Helper: compute "since" duration for a given owner (device or MIG entity).
	ownerSince := func(ownerIdx int) time.Duration {
		var from uint64
		if t0, ok := c.lastProcQueryMono[ownerIdx]; ok && t0 != 0 {
			from = t0
		} else if c.lastMono != 0 {
			from = c.lastMono
		} else {
			// Fallback: use a small non-zero duration to avoid zero/negative
			from = nowMono
		}

		var dMono uint64
		if nowMono > from {
			dMono = nowMono - from
		} else {
			dMono = 1 // 1 ns minimum
		}
		return time.Duration(int64(dMono))
	}

	appendProc := func(ownerIdx int, ownerUUID string, m map[uint32]any) {
		if len(m) == 0 {
			c.lastProcQueryMono[ownerIdx] = nowMono
			return
		}
		for _, v := range m {
			p, ok := v.(devices.GPUProcessUtilizationSample)
			if !ok {
				continue
			}
			procs = append(procs, ring.GpuProcSample{
				Pid:               p.Pid,
				TimeStampUS:       p.TimeStamp,   // backend-provided timestamp (opaque)
				ComputeUtil:       p.ComputeUtil, // %
				MemUtil:           p.MemUtil,     // %
				EncUtil:           p.EncUtil,     // %
				DecUtil:           p.DecUtil,     // %
				GpuIndex:          ownerIdx,
				GpuUUID:           ownerUUID,
				GpuInstanceID:     nil,
				ComputeInstanceID: nil,
			})
		}
		c.lastProcQueryMono[ownerIdx] = nowMono
	}

	// (a) Physical devices via NVML/DCGM
	for i := range c.devs {
		state := &c.devs[i]
		since := ownerSince(state.Index)
		gd := devices.GPUDevice{
			DeviceHandler: state.Nvml,
			ID:            state.Index,
			IsSubdevice:   false,
		}
		// ProcessResourceUtilizationPerDevice is provided by the accelerator Device interface.
		m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
		if err != nil {
			// Advance watermark to avoid tight loop on repeated errors.
			c.lastProcQueryMono[state.Index] = nowMono
			continue
		}
		appendProc(state.Index, state.UUID, m)
	}

	// (b) MIG instances (DCGM only)
	if c.backendStr == "DCGM" && config.GpuEnableMigDiscovery() {
		inst := c.devIf.DeviceInstances() // map[parentID]map[migID]any
		for parentID, migMap := range inst {
			parentUUID := ""
			for i := range c.devs {
				if c.devs[i].Index == parentID {
					parentUUID = c.devs[i].UUID
					break
				}
			}
			for _, anyDev := range migMap {
				gd, ok := anyDev.(devices.GPUDevice)
				if !ok {
					continue
				}
				ownerIdx := gd.ID // DCGM entity id
				since := ownerSince(ownerIdx)
				m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
				if err != nil {
					c.lastProcQueryMono[ownerIdx] = nowMono
					continue
				}
				appendProc(ownerIdx, parentUUID, m)
			}
		}
	}

	return procs, true
}

// -----------------------------------------------------------------------------
// 9) Lightweight logging hooks (optional but handy for validation)
// -----------------------------------------------------------------------------

// logTickDebug emits a periodic debug line for ring push verification.
// It logs at verbosity level 3 to avoid cluttering normal output.
func (c *Collector) logTickDebug(changed bool, numDev, numProc int) {
	if c == nil {
		return
	}

	status := "no-change"
	if changed {
		status = "changed"
	}

	klog.V(3).Infof("gpuCollector: pushed tick (%s, %d devices, %d procs)", status, numDev, numProc)
}
