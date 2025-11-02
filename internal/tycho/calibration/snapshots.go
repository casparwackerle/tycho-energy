package calibration

import (
	"sort"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
)

// ---- Sync wrapper passthrough (ensure you have this in ring/sync.go) --------
// func (s *Sync[T]) SnapshotChrono() []T { s.mu.RLock(); defer s.mu.RUnlock(); return s.ring.SnapshotChrono() }

// ---- Redfish ---------------------------------------------------------------

func snapshotRedfishAll(buf *ring.Sync[ring.RedfishSample]) []ring.RedfishSample {
	return buf.SnapshotChrono()
}

func snapshotRedfishMono(buf *ring.Sync[ring.RedfishSample], m0, m1 uint64) []ring.RedfishSample {
	src := buf.SnapshotChrono()
	out := make([]ring.RedfishSample, 0, len(src))
	for i := range src {
		m := src[i].SampleMeta.Mono
		if m >= m0 && m <= m1 {
			out = append(out, src[i])
		}
	}
	return out
}

// ---- GPU -------------------------------------------------------------------

func snapshotGpuAll(buf *ring.Sync[ring.GpuSample]) []ring.GpuSample {
	return buf.SnapshotChrono()
}

func snapshotGpuMono(buf *ring.Sync[ring.GpuSample], m0, m1 uint64) []ring.GpuSample {
	src := buf.SnapshotChrono()
	out := make([]ring.GpuSample, 0, len(src))
	for i := range src {
		m := src[i].SampleMeta.Mono
		if m >= m0 && m <= m1 {
			out = append(out, src[i])
		}
	}
	return out
}

// ---- RAPL ------------------------------------------------------------------

func snapshotRaplAll(buf *ring.Sync[ring.RaplSample]) []ring.RaplSample {
	return buf.SnapshotChrono()
}

func snapshotRaplMono(buf *ring.Sync[ring.RaplSample], m0, m1 uint64) []ring.RaplSample {
	src := buf.SnapshotChrono()
	out := make([]ring.RaplSample, 0, len(src))
	for i := range src {
		m := src[i].SampleMeta.Mono
		if m >= m0 && m <= m1 {
			out = append(out, src[i])
		}
	}
	return out
}

// ---- BPF -------------------------------------------------------------------

func snapshotBpfAll(buf *ring.Sync[ring.BpfTick]) []ring.BpfTick {
	return buf.SnapshotChrono()
}

func snapshotBpfMono(buf *ring.Sync[ring.BpfTick], m0, m1 uint64) []ring.BpfTick {
	src := buf.SnapshotChrono()
	out := make([]ring.BpfTick, 0, len(src))
	for i := range src {
		m := src[i].SampleMeta.Mono
		if m >= m0 && m <= m1 {
			out = append(out, src[i])
		}
	}
	return out
}

// Tail snapshot for guard window (e.g., last 10s)
func snapshotBpfTailMono(buf *ring.Sync[ring.BpfTick], tail time.Duration, mono *clock.Mono) []ring.BpfTick {
	m1 := mono.Now()
	m0 := m1 - uint64(tail.Nanoseconds())
	return snapshotBpfMono(buf, m0, m1)
}

// Optional: ensure monotonic order (defensive)
func sortByMono[T any](items []T, get func(i T) uint64) {
	sort.Slice(items, func(i, j int) bool { return get(items[i]) < get(items[j]) })
}
