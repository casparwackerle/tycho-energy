package sensors

import (
	"testing"

	"github.com/casparwackerle/tycho-energy/pkg/config"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPower(t *testing.T) {
	if _, err := config.Initialize("."); err != nil {
		t.Fatal(err)
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "Power Suite")
}
