package manager

import (
	"github.com/casparwackerle/tycho-energy/pkg/bpf"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Manager", func() {

	It("Should work properly", func() {
		_, err := config.Initialize(".")
		Expect(err).NotTo(HaveOccurred())
		bpfExporter := bpf.NewMockExporter(bpf.DefaultSupportedMetrics())
		CollectorManager := New(bpfExporter)
		err = CollectorManager.Start()
		Expect(err).NotTo(HaveOccurred())
	})

})
