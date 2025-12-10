package stats

import (
	"github.com/casparwackerle/tycho-energy/pkg/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("ProcessMetric", func() {

	BeforeEach(func() {
		_, err := config.Initialize(".")
		Expect(err).NotTo(HaveOccurred())
	})

	It("Test ResetDeltaValues", func() {
		SetMockedCollectorMetrics()
		metrics := CreateMockedProcessStats(1)
		p := metrics[uint64(1)]
		p.ResetDeltaValues()
		Expect(p.ResourceUsage[config.CPUTime].SumAllDeltaValues()).To(Equal(uint64(0)))
	})
})
