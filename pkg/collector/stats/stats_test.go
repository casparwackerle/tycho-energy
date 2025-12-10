package stats

import (
	"github.com/casparwackerle/tycho-energy/pkg/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Stats", func() {
	It("Test InitAvailableParamAndMetrics", func() {
		_, err := config.Initialize(".")
		Expect(err).NotTo(HaveOccurred())

		config.SetEnabledHardwareCounterMetrics(false)
		exp := []string{}
		Expect(len(GetProcessFeatureNames()) >= len(exp)).To(BeTrue())
	})
})
