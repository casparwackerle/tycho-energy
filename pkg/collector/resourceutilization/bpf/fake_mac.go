//go:build darwin
// +build darwin

package bpf

import (
	"github.com/casparwackerle/tycho-energy/pkg/bpf"
	"github.com/casparwackerle/tycho-energy/pkg/collector/stats"
)

func UpdateProcessBPFMetrics(bpfExporter bpf.Exporter, processStats map[uint64]*stats.ProcessStats) {

}
