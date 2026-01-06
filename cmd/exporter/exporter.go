/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"

	//_ "net/http/pprof"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis"
	analysisexport "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/export"
	"github.com/casparwackerle/tycho-energy/internal/tycho/analysis/exporterhttp"
	analysismetrics "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/metrics"
	analysisregistry "github.com/casparwackerle/tycho-energy/internal/tycho/analysis/registry"
	"github.com/casparwackerle/tycho-energy/internal/tycho/calibration"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	bpfCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/bpf"
	gpuCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/gpu"
	raplCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/rapl"
	redfishCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/redfish"
	"github.com/casparwackerle/tycho-energy/internal/tycho/engine"
	meta "github.com/casparwackerle/tycho-energy/internal/tycho/metadata"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/bpf"
	"github.com/casparwackerle/tycho-energy/pkg/build"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/components"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/platform"

	"github.com/prometheus/client_golang/prometheus"

	"k8s.io/klog/v2"
)

const (

	// to change these msg, you also need to update the e2e test
	finishingMsg = "Exiting..."
	startedMsg   = "Started Kepler in %s"
)

type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type TLSServerConfig struct {
	TLSConfig TLSConfig `yaml:"tls_server_config"`
}

// AppConfig holds the configuration info for the application.
type AppConfig struct {
	BaseDir                      string
	Address                      string
	MetricsPath                  string
	EnableGPU                    bool
	EnableEBPFCgroupID           bool
	ExposeHardwareCounterMetrics bool
	EnableMSR                    bool
	Kubeconfig                   string
	ApiserverEnabled             bool
	RedfishCredFilePath          string
	ExposeEstimatedIdlePower     bool
	MachineSpecFilePath          string
	DisablePowerMeter            bool
	TLSFilePath                  string
}

var tychoEmpty = flag.Bool("tycho-empty", false, "start with Tycho empty baseline (no sensors, no eBPF, no manager)")

func newAppConfig() *AppConfig {
	// Initialize flags
	cfg := &AppConfig{}
	flag.StringVar(&cfg.BaseDir, "config-dir", config.BaseDir, "path to config base directory")
	flag.StringVar(&cfg.Address, "address", "0.0.0.0:8888", "bind address")
	flag.StringVar(&cfg.MetricsPath, "metrics-path", "/metrics", "metrics path")
	flag.BoolVar(&cfg.EnableGPU, "enable-gpu", false, "whether enable gpu (need to have libnvidia-ml installed)")
	flag.BoolVar(&cfg.EnableEBPFCgroupID, "enable-cgroup-id", true, "whether enable eBPF to collect cgroup id")
	flag.BoolVar(&cfg.ExposeHardwareCounterMetrics, "expose-hardware-counter-metrics", true, "whether expose hardware counter as prometheus metrics")
	flag.BoolVar(&cfg.EnableMSR, "enable-msr", false, "whether MSR is allowed to obtain energy data")
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file, if empty we use the in-cluster configuration")
	flag.BoolVar(&cfg.ApiserverEnabled, "apiserver", true, "if apiserver is disabled, we collect pod information from kubelet")
	flag.StringVar(&cfg.RedfishCredFilePath, "redfish-cred-file-path", "", "path to the redfish credential file")
	flag.BoolVar(&cfg.ExposeEstimatedIdlePower, "expose-estimated-idle-power", false, "Whether to expose the estimated idle power as a metric")
	flag.StringVar(&cfg.MachineSpecFilePath, "machine-spec", "", "path to the machine spec file in json format")
	flag.BoolVar(&cfg.DisablePowerMeter, "disable-power-meter", false, "whether manually disable power meter read and forcefully apply the estimator for node powers")
	flag.StringVar(&cfg.TLSFilePath, "web.config.file", "", "path to TLS web config file")

	return cfg
}

// func healthProbe(w http.ResponseWriter, req *http.Request) {
// 	w.WriteHeader(http.StatusOK)
// 	_, err := w.Write([]byte(`ok`))
// 	if err != nil {
// 		klog.Fatalf("%s", fmt.Sprintf("failed to write response: %v", err))
// 	}
// }

func main() {
	//start := time.Now()
	klog.InitFlags(nil)
	appConfig := newAppConfig() // Initialize appConfig and define flags
	flag.Parse()                // Parse command-line flags

	if _, err := config.Initialize(appConfig.BaseDir); err != nil {
		klog.Fatalf("Failed to initialize config: %v", err)
	}

	klog.Infof("Kepler running on version: %s", build.Version)

	// // prometheus
	// registry := metrics.GetRegistry()
	// registry.MustRegister(prometheus.NewGaugeFunc(
	// 	prometheus.GaugeOpts{
	// 		Name: "kepler_exporter_build_info",
	// 		Help: "A metric with a constant '1' value labeled by version, revision, branch, os and arch from which kepler_exporter was built.",
	// 		ConstLabels: prometheus.Labels{
	// 			"branch":   build.Branch,
	// 			"revision": build.Revision,
	// 			"version":  build.Version,
	// 			"os":       build.OS,
	// 			"arch":     build.Arch,
	// 		},
	// 	},
	// 	func() float64 { return 1 },
	// ))

	platform.SetIsSystemCollectionSupported(!appConfig.DisablePowerMeter)
	components.SetIsSystemCollectionSupported(!appConfig.DisablePowerMeter)

	config.SetEnabledEBPFCgroupID(appConfig.EnableEBPFCgroupID)
	config.SetEnabledHardwareCounterMetrics(appConfig.ExposeHardwareCounterMetrics)
	config.SetEnabledGPU(appConfig.EnableGPU)
	config.SetEnabledMSR(appConfig.EnableMSR)
	config.SetEnabledIdlePower(appConfig.ExposeEstimatedIdlePower)

	config.SetKubeConfig(appConfig.Kubeconfig)
	config.SetEnableAPIServer(appConfig.ApiserverEnabled)

	// set redfish credential file path
	if appConfig.RedfishCredFilePath != "" {
		config.SetRedfishCredFilePath(appConfig.RedfishCredFilePath)
	}

	if appConfig.MachineSpecFilePath != "" {
		config.SetMachineSpecFilePath(appConfig.MachineSpecFilePath)
	}

	config.LogConfigs()
	if !*tychoEmpty {
		components.InitPowerImpl()
		defer components.StopPower()
		platform.InitPowerImpl()
		defer platform.StopPower()
		raplCollector.PrintAvailableRaplDomains()
	} else {
		// In empty mode, ensure nothing tries to read HW power:
		config.SetEnabledHardwareCounterMetrics(false)
		config.SetEnabledMSR(false)
		config.SetEnableAPIServer(false) // optional: avoid kube API deps early on
		config.SetEnabledGPU(false)
	}

	// Optional: GPU accelerator registry (only when GPU is enabled and not empty).
	if !*tychoEmpty && config.EnableGpu() {
		r := accelerator.GetRegistry()
		if a, err := accelerator.New(config.GPU, true); err == nil {
			r.MustRegister(a)
		} else {
			klog.Errorf("failed to init GPU accelerators: %v", err)
		}
		defer accelerator.Shutdown()
	}

	// new eBPF exporter
	bpfExporter, err := bpf.NewExporter()
	if err != nil {
		klog.Fatalf("failed to create eBPF exporter: %v", err)
	}
	defer bpfExporter.Detach()

	// new eBPF exporter manager
	collMgr := engine.New(bpfExporter)
	if collMgr == nil {
		klog.Fatal("could not create a collector manager")
	}
	defer collMgr.Stop()

	// Tycho owns scheduling
	if err := collMgr.StatsCollector.Initialize(); err != nil {
		klog.Fatalf("failed to init stats collector: %v", err)
	}

	// start monotonic time
	mono := clock.NewMono(clock.DefaultSource, time.Duration(config.TimebaseQuantumMs())*time.Millisecond)

	// calibrate if necessary
	needCal := config.CalibrationGpuPollEnabled() || config.CalibrationRedfishPollEnabled()
	if needCal {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		klog.V(2).Info("TYCHO-CAL: starting calibration")
		res, err := calibration.RunPollCalibration(ctx) // uses its own temp mono/buffers/collectors
		if err != nil {
			klog.Errorf("TYCHO-CAL: calibration failed: %v", err)
		} else {
			calibration.Apply(res) // logs details at V(5) and updates config
			klog.V(2).Info("TYCHO-CAL: calibration applied")
		}
		config.ValidateTychoQuick()
		config.NormalizeTycho()
	}

	// Snapshot enable flags after empty-mode overrides + (optional) calibration normalize.
	enableBpf := config.EnableBpf()
	enableRapl := config.EnableRapl()
	enableRedfish := config.EnableRedfish()
	enableGpu := config.EnableGpu()

	klog.Infof("[tycho] enabled collectors: bpf=%v rapl=%v redfish=%v gpu=%v", enableBpf, enableRapl, enableRedfish, enableGpu)

	// Central buffer manager
	bufMgr := ring.NewManager()

	// Compute per-metric sizes (use your config getters)
	winSec := config.BufferWindowSec() // e.g., 5.0
	bpfSz := ring.SizeForWindow(winSec, config.BpfPollMs())
	raplSz := ring.SizeForWindow(winSec, config.RaplPollMs())
	rfSz := ring.SizeForWindow(winSec, config.RedfishPollMs())
	gpuSz := ring.SizeForWindow(winSec, config.GpuPollMs())

	// Create synchronized typed rings.
	// NOTE: We keep buffer creation unconditional (safe for downstream deps like calibration.Init),
	// but we gate collectors + analysis plugins so "disabled" means "no work + no emits".
	bpfBuf := ring.GetOrCreateSync[ring.BpfTick](bufMgr, "bpf", bpfSz)
	raplBuf := ring.GetOrCreateSync[ring.RaplTick](bufMgr, "rapl", raplSz)
	rfBuf := ring.GetOrCreateSync[ring.RedfishSample](bufMgr, "redfish", rfSz)
	gpuBuf := ring.GetOrCreateSync[ring.GpuTick](bufMgr, "gpu", gpuSz)

	calibration.Init(calibration.CalibDeps{
		Mono: mono,
		Bpf:  bpfBuf,
		Rapl: raplBuf,
		Rf:   rfBuf,
		Gpu:  gpuBuf,
	})

	// Start Engine
	eng := engine.NewManager()

	// Construct collectors only when enabled.
	// This is important for GPU because EnablePhaseAware may spawn its own goroutines.
	var (
		b  *bpfCollector.Collector
		r  *raplCollector.Collector
		rf *redfishCollector.Collector
		g  *gpuCollector.Collector
	)

	if enableBpf {
		b = bpfCollector.New(bpfCollector.Config{Buf: bpfBuf, Mono: mono, Exp: bpfExporter})
		_ = eng.Register("bpf", time.Duration(config.BpfPollMs())*time.Millisecond, true, b.Collect)
	}

	if enableRapl {
		r = raplCollector.New(raplCollector.Config{Buf: raplBuf, Mono: mono})
		_ = eng.Register("rapl", time.Duration(config.RaplPollMs())*time.Millisecond, true, r.Collect)
	}

	if enableRedfish {
		rf = redfishCollector.New(redfishCollector.Config{Buf: rfBuf, Mono: mono})
		_ = eng.Register("redfish", time.Duration(config.RedfishPollMs())*time.Millisecond, true, rf.Collect)
	}

	if enableGpu {
		g = gpuCollector.New(gpuCollector.Config{Buf: gpuBuf, Mono: mono})

		ctxGPU, cancelGPU := context.WithCancel(context.Background())
		defer cancelGPU()

		if err := g.Init(context.Background()); err != nil {
			klog.Errorf("gpuCollector init failed: %v", err)
		} else {
			g.EnablePhaseAware(ctxGPU, gpuCollector.CollectorSamplerDeps{}) // only when GPU enabled
			defer g.Close()
		}

		_ = eng.Register("gpu", time.Duration(config.GpuPollMs())*time.Millisecond, true, g.Collect)
	}

	// Metadata is always enabled (as in your original code).
	m := meta.New(mono)
	_ = eng.Register("metadata", time.Duration(config.MetadataEnginePeriodSec())*time.Second, true, m.Collect)

	//--------------------------------------------------
	// Analysis plan: register only metrics that are enabled.
	// This makes disabling immediate (no stale window emissions).
	analysisreg := analysisregistry.New()

	if enableRapl {
		analysisreg.Register(analysismetrics.NewRaplTotals(mono))
	}
	if enableBpf {
		analysisreg.Register(analysismetrics.NewBpfSystemMetrics())
	}
	if enableRedfish {
		//analysisreg.Register(analysismetrics.NewRedfishWindowEnergy()) //only do this if fusion is not available
	}
	if enableRapl && enableBpf {
		analysisreg.Register(analysismetrics.NewRaplIdleDynamic())
	}
	if enableGpu {
		analysisreg.Register(analysismetrics.NewGpuWindowEnergy(mono))
		analysisreg.Register(analysismetrics.NewGpuIdleDynamic())
	}

	// metric fusion only when all metrics are available.
	if enableRedfish && enableRapl && enableBpf {
		analysisreg.Register(analysismetrics.NewFusionSubstrate())
		analysisreg.Register(analysismetrics.NewFusionModel())
		analysisreg.Register(analysismetrics.NewSystemRawFromRedfish())
		analysisreg.Register(analysismetrics.NewResidual())
		//analysisreg.Register(analysismetrics.NewSystemResidualIdleDynamic())
	}

	// Residual only makes sense with Redfish system energy and RAPL.
	if enableRedfish && enableRapl && config.GetFusionDiagnosticsEnabled() {
		//analysisreg.Register(analysismetrics.NewSystemResidualEnergy())
	}

	// --- Analysis sinks ---
	logSink := analysisexport.NewLogSink()

	// Prometheus sink for analysis points (registered into the existing registry later).
	tychoPromSink := analysisexport.NewPrometheusSink(analysisexport.PrometheusConfig{
		Prefix:      "tycho", // becomes "tycho_"
		EnableDebug: false,   // window ticks + quality gauges
	})

	// Fan-out so logs stay useful while you bring up Prometheus.
	// sink := analysisexport.NewMultiSink(logSink, analysisexport.NewTruncatingSink(tychoPromSink))
	sink := analysisexport.NewMultiSink(logSink, tychoPromSink)

	// Create Tycho-owned Prometheus registry + HTTP server
	reg := prometheus.NewRegistry()

	reg.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "tycho_exporter_build_info",
			Help: "Constant 1 with build labels.",
			ConstLabels: prometheus.Labels{
				"branch":   build.Branch,
				"revision": build.Revision,
				"version":  build.Version,
				"os":       build.OS,
				"arch":     build.Arch,
			},
		},
		func() float64 { return 1 },
	))

	// Register Tycho analysis collector into this registry
	reg.MustRegister(tychoPromSink.Collector())

	// TLS optional
	tlsCfg, _ := exporterhttp.LoadTLSFromWebConfig(appConfig.TLSFilePath)

	metricPathConfig := config.GetMetricPath(appConfig.MetricsPath)
	bindAddressConfig := config.GetBindAddress(appConfig.Address)

	httpSrv := exporterhttp.New(exporterhttp.Config{
		Address:       bindAddressConfig,
		MetricsPath:   metricPathConfig,
		EnableHealthz: true,
		EnableRoot:    true,
		EnablePprof:   true,
		TLS:           tlsCfg,
	}, reg)

	httpSrv.Start()

	analysisEng := analysis.NewEngine(
		mono,
		analysis.Rings{Rapl: raplBuf, Bpf: bpfBuf, Redfish: rfBuf, Gpu: gpuBuf},
		sink,
		analysis.NewStateStore(),
		analysisreg,
		analysis.Config{
			WindowDuration: 5 * time.Second,
			SafetyOffset:   500 * time.Millisecond,
		},
	)

	// Run analysis periodically (Slice 0: 5s cadence).
	_ = eng.Register("analysis", 5*time.Second, true, analysisEng.Collect)
	//--------------------------------------------------

	tychoCtx, tychoCancel := context.WithCancel(context.Background())
	go func() { _ = eng.Start(tychoCtx) }()
	defer tychoCancel()

	// Run one cumulative-energy validation after the engine is warm (GPU only)
	if enableGpu {
		go func() {
			warmup := time.Duration(config.BufferWindowSec()) * time.Second
			time.Sleep(warmup)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()

			// --- Cumulative-energy validation (per device) ---
			snap := gpuBuf.SnapshotChrono()
			if diagMap, ok := calibration.CumEnergyValidationPerDeviceFromSnap(ctx, mono, snap); ok {
				// Persist in calibration’s own store (if you need it elsewhere)
				calibration.SetCumEnergyDiag(diagMap)

				// Build a simple validity map for the GPU collector (to avoid import cycles)
				validMap := make(map[string]bool, len(diagMap))
				valid, invalid := 0, 0
				for uuid, d := range diagMap {
					validMap[uuid] = d.Valid
					if d.Valid {
						valid++
					} else {
						invalid++
					}
					klog.V(3).Infof(
						"TYCHO-CAL: cumulativeEnergy[%s]: valid=%t samples=%d cumReads=%d monoViol=%d win=%.1fs Einteg=%.3fJ Ecum=%.3fJ relErr=%.3f",
						uuid, d.Valid, d.Samples, d.CumReads, d.MonotonicViolations,
						d.WindowSeconds, d.IntegratedJ, d.CumulativeDeltaJ, d.RelativeError,
					)
				}

				if g != nil {
					g.SetCumEnergyDiag(validMap) // matches gpuCollector signature
				}
				klog.V(2).Infof(
					"TYCHO-CAL: cumulative energy validation done: devices_valid=%d devices_invalid=%d (warmup≈%.0fs)",
					valid, invalid, warmup.Seconds(),
				)
			} else {
				klog.V(2).Infof(
					"TYCHO-CAL: cumulative energy validation skipped (insufficient GPU data; warmup≈%.0fs)",
					warmup.Seconds(),
				)
			}
		}()
	}

	httpSrv.RunUntilSignal()

	klog.Infoln(finishingMsg)

	// Stop the scheduler loop and collectors deterministically.
	// (Do this explicitly because FlushAndExit will skip defers.)
	tychoCancel()
	eng.Stop()           // only if you have it; if not, omit
	collMgr.Stop()       // explicit, since defer will not run
	bpfExporter.Detach() // explicit, since defer will not run

	klog.Flush()
	return
	//klog.FlushAndExit(klog.ExitFlushTimeout, 0)

}

// func rootHandler(metricPathConfig string) http.HandlerFunc {
// 	return func(w http.ResponseWriter, r *http.Request) {
// 		if _, err := w.Write([]byte(`<html>
// 					<head><title>Energy Stats Exporter</title></head>
// 					<body>
// 					<h1>Energy Stats Exporter</h1>
// 					<p><a href="` + metricPathConfig + `">Metrics</a></p>
// 					</body>
// 					</html>`)); err != nil {
// 			klog.Errorf("%s", fmt.Sprintf("failed to write http response: %v", err))
// 		}
// 	}
// }
