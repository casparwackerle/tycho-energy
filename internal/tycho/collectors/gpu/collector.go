package gpuCollector

import (
	"context"
	"encoding/binary"
	"math"
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

// Collector performs GPU collection (NVML today; DCGM later via the same path).
type Collector struct {
	buf  *ring.Sync[ring.GpuTick]
	mono *clock.Mono

	inited     bool
	disabled   bool
	backendStr string         // "NVML" | "DCGM" | etc.
	devIf      devices.Device // ← new: handle to Kepler device interface (for per-process collection)

	// Per-device cached state
	devs              []gpuDeviceState
	lastProcQueryMono map[int]uint64

	// timing
	lastMono uint64
}

// gpuDeviceState holds per-device handles and last-sample state for energy integration.
type gpuDeviceState struct {
	Index int
	Nvml  nvml.Device

	UUID   string
	Name   string
	PCIBus string

	HasCumulativeEnergy bool
}

// New returns a GPU collector.
func New(cfg Config) *Collector {
	return &Collector{
		buf:  cfg.Buf,
		mono: cfg.Mono,
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

// --- Helpers ---------------------------------------------------------------
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

// Collect samples all GPUs once at the given timestamp and pushes exactly ONE tick to the ring.
// One ring element == one timestamped collection event with per-device data embedded (no per-device timestamps).
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	if !c.inited || c.disabled {
		return
	}
	if len(c.devs) == 0 {
		klog.V(6).Info("gpuCollector: Collect skipped (no NVML-capable devices)")
		return
	}

	nowMono := c.mono.From(ts)

	// --- 1) Physical GPU device samples (NVML fast-path) ---
	devSamples := make([]ring.GpuSample, 0, len(c.devs))
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

		// --- Direct NVML field test (instant power) ----------------------------
		// Read instantaneous power via NVML field API (NVML_FI_DEV_POWER_INSTANT = 186).
		instPowerMw := uint64(0)
		values := []nvml.FieldValue{{FieldId: 186}} // mW
		if ret := nv.GetFieldValues(values); ret == nvml.SUCCESS && values[0].NvmlReturn == uint32(nvml.SUCCESS) {
			if mw, ok := decodeNVMLUint(values[0]); ok {
				instPowerMw = mw
			}
		}

		devSamples = append(devSamples, ring.GpuSample{
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

	// --- 2) MIG slice device samples (DCGM path only), gated by config ---
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
				mh, ok := gd.DeviceHandler.(nvml.Device) // DCGM gave us NVML MIG handle
				if !ok {
					continue
				}

				// Same instantaneous reads against the MIG handle
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

				// MIG UUID may be distinct or empty depending on driver; keep best-effort.
				uuid, _ := mh.GetUUID()
				pci, _ := mh.GetPciInfo()
				bus := cChar32ToString(pci.BusId)
				name, _ := mh.GetName()

				// Cumulative energy (raw) on MIG (if exposed)
				var cumMJPtr *uint64
				if mJ, ret := mh.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
					c := mJ
					cumMJPtr = &c
				}

				devSamples = append(devSamples, ring.GpuSample{
					DeviceIndex:     gd.ID, // use DCGM entity id as index for uniqueness
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

	// --- Per-process metrics (NVML/DCGM), gated by config ---
	var procSamples []ring.GpuProcSample
	if c.devIf != nil && config.GpuEnablePerProcess() {

		procSamples = make([]ring.GpuProcSample, 0, 32)
		// Compute a per-owner duration from monotonic deltas, then pass as time.Duration.
		ownerSince := func(ownerIdx int) time.Duration {
			// Prefer per-owner watermark; fall back to the collector’s last tick time.
			var from uint64
			if t0, ok := c.lastProcQueryMono[ownerIdx]; ok && t0 != 0 {
				from = t0
			} else {
				from = c.lastMono // last successful tick we pushed
			}

			var dMono uint64
			if nowMono > from {
				dMono = nowMono - from
			} else {
				// Extremely unlikely (clock monotonicity), but avoid zero/negative.
				dMono = 1 // 1 ns
			}
			return time.Duration(int64(dMono))
		}

		// Helper to append results and advance the monotonic watermark.
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
				procSamples = append(procSamples, ring.GpuProcSample{
					Pid:               p.Pid,
					TimeStampUS:       p.TimeStamp,   // backend’s own clock; treat as opaque
					ComputeUtil:       p.ComputeUtil, // %
					MemUtil:           p.MemUtil,     // %
					EncUtil:           p.EncUtil,     // %
					DecUtil:           p.DecUtil,     // %
					GpuIndex:          ownerIdx,
					GpuUUID:           ownerUUID,
					GpuInstanceID:     nil, // can be filled if DCGM exposes ids you want to surface
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
			m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
			if err != nil {
				klog.V(6).Infof("gpuCollector: per-process (phys %d) error: %v", state.Index, err)
				c.lastProcQueryMono[state.Index] = nowMono
				continue
			}
			appendProc(state.Index, state.UUID, m)
		}

		// (b) MIG instances (DCGM only)
		if c.backendStr == "DCGM" && config.GpuEnableMigDiscovery() {
			inst := c.devIf.DeviceInstances()
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
					ownerIdx := gd.ID // use DCGM entity id for uniqueness
					since := ownerSince(ownerIdx)
					m, err := c.devIf.ProcessResourceUtilizationPerDevice(gd, since)
					if err != nil {
						klog.V(6).Infof("gpuCollector: per-process (MIG %d) error: %v", ownerIdx, err)
						c.lastProcQueryMono[ownerIdx] = nowMono
						continue
					}
					appendProc(ownerIdx, parentUUID, m)
				}
			}
		}
	}
	// --- 4) Push one tick containing devices and per-process (if any) ---
	c.buf.Push(ring.GpuTick{
		SampleMeta: ring.SampleMeta{Mono: nowMono},
		Devices:    devSamples,
		Processes:  procSamples,
	})

	//klog.V(5).Infof("gpuCollector: collected 1 tick (%d device samples, %d proc samples)", len(devSamples), len(procSamples))
	c.lastMono = nowMono
}

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
