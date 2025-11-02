package calibration

import "github.com/casparwackerle/tycho-energy/internal/tycho/ring"

// Span-based coverage checks using mono-ns

func hasWindowBpfMono(buf *ring.Sync[ring.BpfTick], m0, m1 uint64) bool {
	src := buf.SnapshotChrono()
	if len(src) == 0 {
		return false
	}
	first := src[0].SampleMeta.Mono
	last := src[len(src)-1].SampleMeta.Mono
	return first <= m0 && last >= m1
}

func hasWindowRedfishMono(buf *ring.Sync[ring.RedfishSample], m0, m1 uint64) bool {
	src := buf.SnapshotChrono()
	if len(src) == 0 {
		return false
	}
	first := src[0].SampleMeta.Mono
	last := src[len(src)-1].SampleMeta.Mono
	return first <= m0 && last >= m1
}

func hasWindowGpuMono(buf *ring.Sync[ring.GpuTick], m0, m1 uint64) bool {
	src := buf.SnapshotChrono()
	if len(src) == 0 {
		return false
	}
	first := src[0].SampleMeta.Mono
	last := src[len(src)-1].SampleMeta.Mono
	return first <= m0 && last >= m1
}

func hasWindowRaplMono(buf *ring.Sync[ring.RaplSample], m0, m1 uint64) bool {
	src := buf.SnapshotChrono()
	if len(src) == 0 {
		return false
	}
	first := src[0].SampleMeta.Mono
	last := src[len(src)-1].SampleMeta.Mono
	return first <= m0 && last >= m1
}
