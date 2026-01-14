package analysismetrics

import (
	"math"
	"sort"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/attribution"
	gpufuse "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/gpu"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/config"
)

// Output metric family (Prometheus sink prefix "tycho" yields tycho_*):
// tycho_workload_gpu_energy_mj{gpu_uuid,kind,namespace,pod,container}
const (
	MetricWorkloadGpuEnergyMJ analysis.MetricID = "workload_gpu_energy_mj"
)

type GpuWorkloadDynamic struct {
	delayTicks uint64
}

func NewGpuWorkloadDynamic(mono *clock.Mono) *GpuWorkloadDynamic {
	var dt uint64
	if mono != nil {
		dt = mono.TicksForMsCeil(config.GpuDelayMs())
	}
	return &GpuWorkloadDynamic{delayTicks: dt}
}

func (m *GpuWorkloadDynamic) ID() analysis.MetricID { return "gpu_workload_dynamic" }

func (m *GpuWorkloadDynamic) IsEnabled(c *analysis.Cycle) bool {
	// NOTE:
	// - c.Store is PointStore (cycle-local point cache).
	// - c.Meta is metadata.Store (PID -> ProcMeta, container/pod caches).
	return c != nil && c.Sink != nil && c.Store != nil && c.State != nil && c.Mono != nil && c.Gpu() != nil && c.Meta != nil
}

func (m *GpuWorkloadDynamic) Run(c *analysis.Cycle) error {
	if c == nil || c.Sink == nil || c.Store == nil || c.State == nil || c.Mono == nil || c.Gpu() == nil || c.Meta == nil {
		return nil
	}
	if !c.Window.IsValid() {
		return nil
	}

	monoQuantumSec := c.Mono.Quantum().Seconds()
	if monoQuantumSec <= 0 {
		return nil
	}

	// Corrected-time window bounds in seconds.
	winStartSec := float64(c.Window.StartMono) * monoQuantumSec
	winEndSec := float64(c.Window.EndMono) * monoQuantumSec
	if winEndSec <= winStartSec {
		return nil
	}
	dtWinSec := winEndSec - winStartSec

	// Dynamic window budget: MetricGpuPowerMW{kind="dynamic"} is mean power (mW) over the window.
	// Multiply by window duration to get dynamic energy delta (mJ).
	dynMWByUUID := loadGpuPowerMWByKind(c.Store, "dynamic")
	if len(dynMWByUUID) == 0 {
		return nil
	}

	// To cover corrected window [Start, End], we need raw ticks in [Start+delay, End+delay].
	rawW := c.Window.ShiftForward(m.delayTicks)

	seg1, seg2 := c.Gpu().ViewChrono()

	// Each GPU process tick provides a per-GPU snapshot: PID -> ComputeUtil(%).
	// We apply hold-last-value over time.
	type pidUtilMap map[uint32]float64 // util weight (0..100)
	type evt struct {
		tSec float64
		u    pidUtilMap
	}

	eventsByUUID := map[string][]evt{}
	lastBeforeByUUID := map[string]evt{}
	hasLastBefore := map[string]bool{}

	ingestTick := func(t ring.GpuTick) {
		tRaw := t.SampleMeta.Mono
		if tRaw == 0 {
			return
		}

		// Convert to corrected-time seconds.
		tCorr := tRaw
		if tCorr > m.delayTicks {
			tCorr = tCorr - m.delayTicks
		} else {
			tCorr = 0
		}
		tSec := float64(tCorr) * monoQuantumSec

		// Build per-UUID snapshot.
		if len(t.Processes) == 0 {
			return
		}
		byUUID := map[string]pidUtilMap{}
		for i := range t.Processes {
			ps := t.Processes[i]
			uuid := ps.GpuUUID
			if uuid == "" {
				continue
			}

			w := float64(ps.ComputeUtil)
			if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
				w = 0
			}
			if w > 100 {
				w = 100
			}

			mm := byUUID[uuid]
			if mm == nil {
				mm = make(pidUtilMap, 8)
				byUUID[uuid] = mm
			}

			pid := ps.Pid
			if cur, ok := mm[pid]; !ok || w > cur {
				mm[pid] = w
			}
		}

		for uuid, mm := range byUUID {
			e := evt{tSec: tSec, u: mm}

			if tSec <= winStartSec {
				// last snapshot at or before start
				if !hasLastBefore[uuid] || e.tSec >= lastBeforeByUUID[uuid].tSec {
					lastBeforeByUUID[uuid] = e
					hasLastBefore[uuid] = true
				}
				continue
			}
			if tSec > winEndSec {
				// ignore for now; hold-last is enough for [start,end]
				continue
			}
			eventsByUUID[uuid] = append(eventsByUUID[uuid], e)
		}
	}

	// Only scan ticks inside the raw selection window (fast and sufficient for v1).
	for _, t := range seg1 {
		if t.SampleMeta.Mono >= rawW.StartMono && t.SampleMeta.Mono <= rawW.EndMono {
			ingestTick(t)
		}
	}
	for _, t := range seg2 {
		if t.SampleMeta.Mono >= rawW.StartMono && t.SampleMeta.Mono <= rawW.EndMono {
			ingestTick(t)
		}
	}

	// Accumulate per-workload per-GPU energy deltas (mJ) for this window.
	type accKey struct {
		uuid      string
		namespace string
		pod       string
		container string
	}
	deltaByKey := map[accKey]float64{}
	systemDeltaByUUID := map[string]float64{}

	emitCounter := func(uuid, ns, pod, ctr string, delta float64) {
		labels := analysis.Labels{
			"gpu_uuid":  uuid,
			"kind":      "dynamic",
			"namespace": ns,
			"pod":       pod,
			"container": ctr,
		}
		k := analysis.Key(MetricWorkloadGpuEnergyMJ, labels)

		prevV, _ := c.State.Get(k)
		prev, _ := prevV.(float64)

		next := prev + delta
		if math.IsNaN(next) || math.IsInf(next, 0) || next < 0 {
			next = 0
		}
		c.State.Set(k, next)

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    k,
			Window: c.Window,
			Unit:   "mJ",
			Value:  next,
		})
	}

	// emitCounterAbs sets the counter to an absolute value (monotonic guarded).
	// This is used for system idle, which must mirror tycho_gpu_energy_mj{kind="idle"} exactly.
	emitCounterAbs := func(uuid, ns, pod, ctr, kind string, abs float64) {
		labels := analysis.Labels{
			"gpu_uuid":  uuid,
			"kind":      kind,
			"namespace": ns,
			"pod":       pod,
			"container": ctr,
		}
		k := analysis.Key(MetricWorkloadGpuEnergyMJ, labels)

		prevV, _ := c.State.Get(k)
		prev, _ := prevV.(float64)

		next := abs
		if math.IsNaN(next) || math.IsInf(next, 0) || next < 0 {
			next = 0
		}
		// Guard monotonicity (should not trigger if upstream is correct).
		if next < prev {
			next = prev
		}
		c.State.Set(k, next)

		c.Sink.Emit(c.Ctx, analysis.Point{
			Key:    k,
			Window: c.Window,
			Unit:   "mJ",
			Value:  next,
		})
	}

	// Attribute per GPU UUID.
	for uuid, dynMW := range dynMWByUUID {
		if uuid == "" {
			continue
		}

		ser, ok := gpufuse.GetCorrectedSeries(c.State, uuid)
		if !ok || ser.DtSec <= 0 || len(ser.PW) == 0 {
			continue
		}

		// Total window energy delta from corrected series.
		totalWindowMJ, okT := energyMJBetween(ser, winStartSec, winEndSec)
		if !okT || math.IsNaN(totalWindowMJ) || math.IsInf(totalWindowMJ, 0) || totalWindowMJ < 0 {
			totalWindowMJ = 0
		}

		// Dynamic window energy delta.
		dynWindowMJ := dynMW * dtWinSec
		if math.IsNaN(dynWindowMJ) || math.IsInf(dynWindowMJ, 0) || dynWindowMJ < 0 {
			dynWindowMJ = 0
		}
		if dynWindowMJ <= 0 {
			systemDeltaByUUID[uuid] += 0
			continue
		}

		// If we cannot derive total structure, route dynamic to system.
		if totalWindowMJ <= 0 {
			systemDeltaByUUID[uuid] += dynWindowMJ
			continue
		}

		// Scale corrected interval energy by window dynamic fraction.
		f := dynWindowMJ / totalWindowMJ
		if f < 0 {
			f = 0
		}
		if f > 1 {
			f = 1
		}

		evs := eventsByUUID[uuid]
		if len(evs) > 0 {
			sort.Slice(evs, func(i, j int) bool { return evs[i].tSec < evs[j].tSec })
		}

		curUtil := pidUtilMap(nil)
		if hasLastBefore[uuid] {
			curUtil = lastBeforeByUUID[uuid].u
		} else {
			curUtil = make(pidUtilMap)
		}

		prevT := winStartSec

		applyInterval := func(a, b float64, util pidUtilMap) {
			if b <= a {
				return
			}
			intervalTotalMJ, okI := energyMJBetween(ser, a, b)
			if !okI || intervalTotalMJ <= 0 {
				return
			}

			intervalDynMJ := intervalTotalMJ * f
			if intervalDynMJ <= 0 || math.IsNaN(intervalDynMJ) || math.IsInf(intervalDynMJ, 0) {
				return
			}

			var sumW float64
			for _, w := range util {
				if w > 0 {
					sumW += w
				}
			}
			if sumW <= 0 {
				systemDeltaByUUID[uuid] += intervalDynMJ
				return
			}

			var assigned float64
			for pid32, w := range util {
				if w <= 0 {
					continue
				}
				share := intervalDynMJ * (w / sumW)
				if share <= 0 {
					continue
				}

				pid := uint64(pid32)
				procID, cgid, okProc := attribution.ResolveProc(c.Meta, pid)
				var procIDPtr *attribution.ProcID
				if okProc {
					tmp := procID
					procIDPtr = &tmp
				}

				wk, okW := attribution.ResolveWorkload(c.Meta, procIDPtr, cgid)
				if !okW || wk.IsZero() {
					systemDeltaByUUID[uuid] += share
					continue
				}

				key := accKey{uuid: uuid, namespace: wk.Namespace, pod: wk.Pod, container: wk.Container}
				deltaByKey[key] += share
				assigned += share
			}

			rem := intervalDynMJ - assigned
			if rem > 1e-9 {
				systemDeltaByUUID[uuid] += rem
			}
		}

		for _, e := range evs {
			t := e.tSec
			if t <= winStartSec {
				curUtil = e.u
				continue
			}
			if t > winEndSec {
				break
			}
			applyInterval(prevT, t, curUtil)
			curUtil = e.u
			prevT = t
		}
		applyInterval(prevT, winEndSec, curUtil)
	}

	// Emit workload series.
	for k, d := range deltaByKey {
		if math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
			continue
		}
		if d < 1e-12 {
			d = 0
		}
		emitCounter(k.uuid, k.namespace, k.pod, k.container, d)
	}

	// Emit __system__ series always for each GPU UUID where a dynMW existed.
	for uuid := range dynMWByUUID {
		d := systemDeltaByUUID[uuid]
		if math.IsNaN(d) || math.IsInf(d, 0) || d < 0 {
			d = 0
		}
		emitCounter(uuid, attribution.SystemNamespace, attribution.SystemPod, attribution.SystemContainer, d)
	}

	// Emit __system__ idle series (completeness):
	// tycho_workload_gpu_energy_mj{kind="idle"} mirrors tycho_gpu_energy_mj{kind="idle"} per gpu_uuid.
	idleMJByUUID := loadGpuEnergyMJByKind(c.Store, "idle")
	for uuid, idleAbs := range idleMJByUUID {
		if uuid == "" {
			continue
		}
		emitCounterAbs(
			uuid,
			attribution.SystemNamespace,
			attribution.SystemPod,
			attribution.SystemContainer,
			"idle",
			idleAbs,
		)
	}

	return nil
}

// loadGpuPowerMWByKind builds uuid -> power(mW) from MetricGpuPowerMW{kind="<kind>", gpu_uuid}.
func loadGpuPowerMWByKind(store *analysis.PointStore, kind string) map[string]float64 {
	out := make(map[string]float64, 8)
	if store == nil {
		return out
	}
	ps := store.ListByID(MetricGpuPowerMW)
	for _, p := range ps {
		if p.Key.Labels == nil {
			continue
		}
		uuid := p.Key.Labels["gpu_uuid"]
		k := p.Key.Labels["kind"]
		if uuid == "" || k != kind {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		out[uuid] = v
	}
	return out
}

// loadGpuEnergyMJByKind builds uuid -> energy(mJ) from MetricGpuEnergyMJ{kind="<kind>", gpu_uuid}.
// NOTE: MetricGpuEnergyMJ is a monotonic counter in your pipeline (GpuIdleDynamic persists st.*MJ).
func loadGpuEnergyMJByKind(store *analysis.PointStore, kind string) map[string]float64 {
	out := make(map[string]float64, 8)
	if store == nil {
		return out
	}
	ps := store.ListByID(MetricGpuEnergyMJ)
	for _, p := range ps {
		if p.Key.Labels == nil {
			continue
		}
		uuid := p.Key.Labels["gpu_uuid"]
		k := p.Key.Labels["kind"]
		if uuid == "" || k != kind {
			continue
		}
		v := p.Value
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			v = 0
		}
		out[uuid] = v
	}
	return out
}

// energyMJBetween integrates corrected series over [aSec,bSec] using ZOH on the grid (W*s).
// Returns mJ.
func energyMJBetween(ser gpufuse.Series, aSec, bSec float64) (float64, bool) {
	if ser.DtSec <= 0 || len(ser.PW) == 0 || bSec <= aSec {
		return 0, false
	}
	serStart := ser.T0Sec
	serEnd := ser.T0Sec + float64(len(ser.PW))*ser.DtSec

	a := aSec
	b := bSec
	if a < serStart {
		a = serStart
	}
	if b > serEnd {
		b = serEnd
	}
	if b <= a {
		return 0, false
	}

	var eJ float64

	i0 := int(math.Floor((a - ser.T0Sec) / ser.DtSec))
	if i0 < 0 {
		i0 = 0
	}
	if i0 >= len(ser.PW) {
		return 0, false
	}

	t := a
	for i := i0; i < len(ser.PW); i++ {
		t0 := ser.T0Sec + float64(i)*ser.DtSec
		t1 := t0 + ser.DtSec
		if t1 <= t {
			continue
		}
		segA := math.Max(t, t0)
		segB := math.Min(b, t1)
		if segB > segA {
			p := ser.PW[i]
			if p < 0 {
				p = 0
			}
			eJ += p * (segB - segA)
		}
		if t1 >= b {
			break
		}
	}

	return eJ * 1000.0, true
}
