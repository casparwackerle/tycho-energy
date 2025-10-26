package gpuCollector

import (
	"context"
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
	Buf  *ring.Sync[ring.GpuSample]
	Mono *clock.Mono
}

// Collector performs GPU collection (NVML today; DCGM later via the same path).
type Collector struct {
	buf  *ring.Sync[ring.GpuSample]
	mono *clock.Mono

	inited     bool
	disabled   bool
	backendStr string // "NVML" | "DCGM" | etc.

	// Per-device cached state
	devs []gpuDeviceState

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
	LastMilliJ          uint64
	LastMilliW          int
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

		// Prime last values (best-effort)
		var lastW int
		if wU32, ret := nv.GetPowerUsage(); ret == nvml.SUCCESS {
			lastW = int(wU32)
		}
		var lastMJ uint64
		if hasCum {
			if mJ, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
				lastMJ = mJ
			}
		}

		c.devs = append(c.devs, gpuDeviceState{
			Index:               idx,
			Nvml:                nv,
			UUID:                uuid,
			Name:                name,
			PCIBus:              bus,
			HasCumulativeEnergy: hasCum,
			LastMilliJ:          lastMJ,
			LastMilliW:          lastW,
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
	return nil
}

// Collect samples all GPUs once at the given timestamp and pushes a GpuSample per device.
func (c *Collector) Collect(ctx context.Context, ts time.Time) {
	if !c.inited || c.disabled {
		return
	}

	nowMono := c.mono.From(ts)
	var dtMs float64
	if c.lastMono != 0 && nowMono > c.lastMono {
		dtMs = float64(nowMono-c.lastMono) / 1e6
	}
	if dtMs < 1.0 {
		dtMs = 1.0
	}

	totalEnergy := uint64(0)
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

		var encPtr, decPtr *float64
		if u, _, ret := nv.GetEncoderUtilization(); ret == nvml.SUCCESS {
			val := float64(u)
			encPtr = &val
		}
		if u, _, ret := nv.GetDecoderUtilization(); ret == nvml.SUCCESS {
			val := float64(u)
			decPtr = &val
		}

		var dEμJ uint64
		if state.HasCumulativeEnergy {
			if mJ, ret := nv.GetTotalEnergyConsumption(); ret == nvml.SUCCESS {
				var delta uint64
				if mJ >= state.LastMilliJ {
					delta = mJ - state.LastMilliJ
				}
				dEμJ = delta * 1000
				state.LastMilliJ = mJ
			} else {
				dEμJ = integrateTrapezoidMicroJ(state.LastMilliW, powerMw, dtMs)
			}
		} else {
			dEμJ = integrateTrapezoidMicroJ(state.LastMilliW, powerMw, dtMs)
		}
		totalEnergy += dEμJ

		sample := ring.GpuSample{
			SampleMeta:          ring.SampleMeta{Mono: nowMono},
			DeviceIndex:         state.Index,
			UUID:                state.UUID,
			PCIBusID:            state.PCIBus,
			Name:                state.Name,
			PowerMilliW:         powerMw,
			SMUtilPct:           float64(util.Gpu),
			MemUtilPct:          float64(util.Memory),
			EncUtilPct:          encPtr,
			DecUtilPct:          decPtr,
			MemUsedBytes:        mem.Used,
			MemTotalBytes:       mem.Total,
			SMClockMHz:          uint32(smClk),
			MemClockMHz:         uint32(memClk),
			TempC:               tempC,
			EnergyMicroJ:        dEμJ,
			HasCumulativeEnergy: state.HasCumulativeEnergy,
			Backend:             c.backendStr,
		}
		c.buf.Push(sample)
		state.LastMilliW = powerMw
	}

	klog.V(5).Infof("gpuCollector: collected %d GPU samples (totalEnergyΔ=%.3f mJ)",
		len(c.devs), float64(totalEnergy)/1000.0)

	c.lastMono = nowMono
}

func integrateTrapezoidMicroJ(prevMilliW, currMilliW int, dtMs float64) uint64 {
	μJ := ((float64(prevMilliW) + float64(currMilliW)) / 2.0) * dtMs * 1000.0
	if μJ < 0 {
		return 0
	}
	return uint64(math.Round(μJ))
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
