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
	"errors"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/casparwackerle/tycho-energy/internal/tycho/calibration"
	"github.com/casparwackerle/tycho-energy/internal/tycho/clock"
	bpfCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/bpf"
	gpuCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/gpu"
	raplCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/rapl"
	redfishCollector "github.com/casparwackerle/tycho-energy/internal/tycho/collectors/redfish"
	"github.com/casparwackerle/tycho-energy/internal/tycho/engine"
	"github.com/casparwackerle/tycho-energy/internal/tycho/ring"
	"github.com/casparwackerle/tycho-energy/pkg/bpf"
	"github.com/casparwackerle/tycho-energy/pkg/build"
	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/metrics"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/accelerator"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/components"
	"github.com/casparwackerle/tycho-energy/pkg/sensors/platform"
	"gopkg.in/yaml.v3"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

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

func healthProbe(w http.ResponseWriter, req *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`ok`))
	if err != nil {
		klog.Fatalf("%s", fmt.Sprintf("failed to write response: %v", err))
	}
}

func main() {
	start := time.Now()
	klog.InitFlags(nil)
	appConfig := newAppConfig() // Initialize appConfig and define flags
	flag.Parse()                // Parse command-line flags

	if _, err := config.Initialize(appConfig.BaseDir); err != nil {
		klog.Fatalf("Failed to initialize config: %v", err)
	}

	klog.Infof("Kepler running on version: %s", build.Version)

	// prometheus
	registry := metrics.GetRegistry()
	registry.MustRegister(prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "kepler_exporter_build_info",
			Help: "A metric with a constant '1' value labeled by version, revision, branch, os and arch from which kepler_exporter was built.",
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

	//new eBPF exporter manager
	collMgr := engine.New(bpfExporter)
	if collMgr == nil {
		klog.Fatal("could not create a collector manager")
	}
	defer collMgr.Stop()

	// Tycho owns scheduling
	if err := collMgr.StatsCollector.Initialize(); err != nil {
		klog.Fatalf("failed to init stats collector: %v", err)
	}

	// calibrate if necessary
	needCal := config.CalibrationGpuPollEnabled() ||
		config.CalibrationGpuDelayEnabled() ||
		config.CalibrationRedfishPollEnabled() ||
		config.CalibrationRedfishDelayEnabled() ||
		config.CalibrationRaplDelayEnabled()

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

	// Central buffer manager
	bufMgr := ring.NewManager()

	// Compute per-metric sizes (use your config getters)
	winSec := config.BufferWindowSec() // e.g., 5.0
	bpfSz := ring.SizeForWindow(winSec, config.BpfPollMs())
	raplSz := ring.SizeForWindow(winSec, config.RaplPollMs())
	rfSz := ring.SizeForWindow(winSec, config.RedfishPollMs())
	gpuSz := ring.SizeForWindow(winSec, config.GpuPollMs())

	// Create synchronized typed rings
	bpfBuf := ring.GetOrCreateSync[ring.BpfTick](bufMgr, "bpf", bpfSz)
	raplBuf := ring.GetOrCreateSync[ring.RaplSample](bufMgr, "rapl", raplSz)
	rfBuf := ring.GetOrCreateSync[ring.RedfishSample](bufMgr, "redfish", rfSz)
	gpuBuf := ring.GetOrCreateSync[ring.GpuSample](bufMgr, "gpu", gpuSz)

	//start monotonic time
	mono := clock.NewMono(clock.DefaultSource, time.Duration(config.TimebaseQuantumMs())*time.Millisecond)

	// Start Engine
	eng := engine.NewManager()
	b := bpfCollector.New(bpfCollector.Config{Buf: bpfBuf, Mono: mono, Exp: bpfExporter})
	r := raplCollector.New(raplCollector.Config{Buf: raplBuf, Mono: mono})
	rf := redfishCollector.New(redfishCollector.Config{Buf: rfBuf, Mono: mono})
	g := gpuCollector.New(gpuCollector.Config{Buf: gpuBuf, Mono: mono})

	if err := g.Init(context.Background()); err != nil {
		klog.Errorf("gpuCollector init failed: %v", err)
	}

	_ = eng.Register("bpf", time.Duration(config.BpfPollMs())*time.Millisecond, config.EnableBpf(), b.Collect)
	_ = eng.Register("rapl", time.Duration(config.RaplPollMs())*time.Millisecond, config.EnableRapl(), r.Collect)
	_ = eng.Register("redfish", time.Duration(config.RedfishPollMs())*time.Millisecond, config.EnableRedfish(), rf.Collect)
	_ = eng.Register("gpu", time.Duration(config.GpuPollMs())*time.Millisecond, config.EnableGpu(), g.Collect)

	tycho_ctx, tycho_cancel := context.WithCancel(context.Background())
	go func() { _ = eng.Start(tycho_ctx) }()
	defer tycho_cancel()

	// // ----- Force one idle calibration after engine is running --------------------
	// go func() {
	// 	// Warm-up so BPF has enough ticks for the guard window
	// 	const warmup = 20 * time.Second
	// 	time.Sleep(warmup)

	// 	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	// 	defer cancel()
	// 	resIdle, err := calibration.RunIdleCalibration(ctx, mono, bpfBuf, raplBuf, rfBuf, gpuBuf)
	// 	if err != nil {
	// 		klog.Errorf("TYCHO-CAL: initial idle calibration error: %v", err)
	// 		return
	// 	}
	// 	calibration.Apply(resIdle)
	// 	klog.V(2).Infof("TYCHO-CAL: initial idle calibration applied: status=%v notes=%v", resIdle.Status, resIdle.Notes)
	// }()

	// // ----- Periodic re-calibration every 24h (idle-only) -------------------------
	// go func() {
	// 	ticker := time.NewTicker(24 * time.Hour)
	// 	defer ticker.Stop()
	// 	for {
	// 		select {
	// 		case <-ticker.C:
	// 			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	// 			resIdle, err := calibration.RunIdleCalibration(ctx, mono, bpfBuf, raplBuf, rfBuf, gpuBuf)
	// 			cancel()
	// 			if err != nil {
	// 				klog.Errorf("TYCHO-CAL: periodic idle calibration error: %v", err)
	// 				continue
	// 			}
	// 			calibration.Apply(resIdle)
	// 			klog.V(2).Infof("TYCHO-CAL: periodic idle calibration applied: status=%v notes=%v", resIdle.Status, resIdle.Notes)
	// 		case <-tycho_ctx.Done():
	// 			return
	// 		}
	// 	}
	// }()

	//-------------
	// tychoMgr = engine.NewManager(*tychoPeriodMs, collMgr)
	// go func() {
	// 	if err := tychoMgr.Start(tychoCtx); err != nil {
	// 		klog.Errorf("Tycho engine error: %v", err)
	// 	}
	// }()

	//--------------------------------------------------------------------------------
	// --- Tycho smoke test (remove later) ---
	// This ensures the new internal package is referenced and builds.
	// Start() is a no-op for now, so this won't change behavior.
	// tm := engine.NewManager(100)

	// // use a short-lived context; Start currently returns immediately anyway
	// tycho_ctx, tycho_cancel := context.WithCancel(context.Background())
	// defer tycho_cancel()

	// if err := tm.Start(tycho_ctx); err != nil {
	// 	klog.Errorf("Tycho smoke test: manager Start() error: %v", err)
	// } else {
	// 	klog.Infof("Tycho smoke test: manager Start() called (no-op)")
	// }

	//--------------------------------------------------------------------------------

	//starting a CollectorManager instance to collect data and report metrics
	// if startErr := m.Start(); startErr != nil {
	// 	klog.Infof("%s", fmt.Sprintf("failed to start : %v", startErr))
	// }

	metricPathConfig := config.GetMetricPath(appConfig.MetricsPath)
	bindAddressConfig := config.GetBindAddress(appConfig.Address)

	var certFile, keyFile string
	tlsConfigured := false

	// Retrieve the TLS config
	if appConfig.TLSFilePath != "" {
		configPath := appConfig.TLSFilePath

		configFile, err := os.Open(configPath)
		if err != nil {
			klog.Errorf("Error opening config file: %v\n", err)
		}
		defer configFile.Close()

		var tlsServerConfig TLSServerConfig
		decoder := yaml.NewDecoder(configFile)
		if err := decoder.Decode(&tlsServerConfig); err != nil {
			klog.Errorf("Error parsing config file: %v\n", err)
		}

		if tlsServerConfig.TLSConfig.CertFile != "" && tlsServerConfig.TLSConfig.KeyFile != "" {
			certFile = tlsServerConfig.TLSConfig.CertFile
			keyFile = tlsServerConfig.TLSConfig.KeyFile
			tlsConfigured = true
		}
	}

	handler := http.ServeMux{}
	reg := collMgr.PrometheusCollector.RegisterMetrics()
	handler.Handle(metricPathConfig, promhttp.HandlerFor(
		reg,
		promhttp.HandlerOpts{
			Registry: reg,
		},
	))
	handler.HandleFunc("/healthz", healthProbe)
	handler.HandleFunc("/", rootHandler(metricPathConfig))
	handler.HandleFunc("/debug/pprof/", http.DefaultServeMux.ServeHTTP)
	srv := &http.Server{
		Addr:    bindAddressConfig,
		Handler: &handler,
	}

	klog.Infof("starting to listen on %s", bindAddressConfig)
	errChan := make(chan error)

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if tlsConfigured {
			// Run server in TLS mode
			klog.Infof("Starting server with TLS")
			err = srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			// Fall back to non-TLS mode
			klog.Infof("Starting server without TLS")
			err = srv.ListenAndServe()
		}

		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errChan <- err
		}
	}()
	klog.Infof(startedMsg, time.Since(start))
	klog.Flush() // force flush to parse the start msg in the e2e test

	// Wait for an exit signal

	ctx := context.Background()
	select {
	case err := <-errChan:
		klog.Fatalf("%s", fmt.Sprintf("failed to listen and serve: %v", err))
	case <-signalChan:
		klog.Infof("Received shutdown signal")
		ctx, cancel := context.WithDeadline(ctx, time.Now().Add(5*time.Second))
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			klog.Fatalf("%s", fmt.Sprintf("failed to shutdown gracefully: %v", err))
		}
	}
	wg.Wait()
	klog.Infoln(finishingMsg)
	klog.FlushAndExit(klog.ExitFlushTimeout, 0)
}

func rootHandler(metricPathConfig string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, err := w.Write([]byte(`<html>
					<head><title>Energy Stats Exporter</title></head>
					<body>
					<h1>Energy Stats Exporter</h1>
					<p><a href="` + metricPathConfig + `">Metrics</a></p>
					</body>
					</html>`)); err != nil {
			klog.Errorf("%s", fmt.Sprintf("failed to write http response: %v", err))
		}
	}
}
