/*
Copyright 2022.

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

package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

var (
	versionRegex = regexp.MustCompile(`^(\d+)\.(\d+).`)
	instance     *Config
	once         sync.Once
)

type Client interface {
	getUnixName() (unix.Utsname, error)
	getCgroupV2File() string
}

// Configuration structs
type KeplerConfig struct {
	KeplerNamespace              string
	EnabledEBPFCgroupID          bool
	EnabledGPU                   bool
	EnabledMSR                   bool
	EnableProcessStats           bool
	ExposeContainerStats         bool
	ExposeVMStats                bool
	ExposeHardwareCounterMetrics bool
	ExposeIRQCounterMetrics      bool
	ExposeBPFMetrics             bool
	ExposeComponentPower         bool
	ExposeIdlePowerMetrics       bool
	EnableAPIServer              bool
	MockACPIPowerPath            string
	MaxLookupRetry               int
	KubeConfig                   string
	BPFSampleRate                int
	EstimatorModel               string
	EstimatorSelectFilter        string
	CPUArchOverride              string
	MachineSpecFilePath          string
	ExcludeSwapperProcess        bool
}

type MetricsConfig struct {
	CoreUsageMetric    string
	DRAMUsageMetric    string
	UncoreUsageMetric  string
	GPUUsageMetric     string
	GeneralUsageMetric string
}

type RedfishConfig struct {
	CredFilePath           string
	ProbeIntervalInSeconds string
	SkipSSLVerify          bool
}

type ModelConfig struct {
	ModelServerEnable           bool
	ModelServerEndpoint         string
	ModelConfigValues           map[string]string
	NodePlatformPowerKey        string
	NodeComponentsPowerKey      string
	ContainerPlatformPowerKey   string
	ContainerComponentsPowerKey string
	ProcessPlatformPowerKey     string
	ProcessComponentsPowerKey   string
}

type LibvirtConfig struct {
	MetadataURI   string
	MetadataToken string
}

type HostConfig struct {
	ProcDir string
	SysDir  string
}

type TychoTimingConfig struct {
	TimebaseQuantumMs    int // resampling grid for aligned series
	BufferWindowSec      int
	BufferMarginCycles   int
	RaplPollMs           int
	RaplDelayMs          int
	BpfPollMs            int
	BpfDelayMs           int
	GpuPollMs            int
	GpuDelayMs           int
	RedfishPollMs        int // note: Redfish.RedfishProbeIntervalInSeconds still exists; this overrides if >0
	RedfishDelayMs       int
	RedfishHeartbeatMs   int
	RedfishAutoHeartbeat bool
}

type TychoCalibrationConfig struct {
	IdleBudgetSec                     int
	RaplPollMinMs                     int
	RaplDelayEnabled                  bool
	RaplDelayBudgetSec                int
	RaplIdleEnabled                   bool
	RedfishContinuousHeartbeatEnabled bool
	RedfishPollEnabled                bool
	RedfishPollBudgetSec              int
	RedfishPollMinMs                  int
	RedfishDelayEnabled               bool
	RedfishDelayBudgetSec             int
	RedfishIdleEnabled                bool
	GpuPollEnabled                    bool
	GpuPollBudgetSec                  int
	GpuPollMinMs                      int
	GpuDelayEnabled                   bool
	GpuDelayBudgetSec                 int
	GpuIdleEnabled                    bool
}

type TychoAnalysisConfig struct {
	Trigger            string // "redfish" | "timer"
	TriggerIntervalSec int    // used for "timer"
	DetectLongestDelay bool
	DelayAfterMs       int // used for "redfish"
}

type TychoCollectorConfig struct {
	EnableRapl    bool
	EnableBpf     bool // eBPF/software counters collection gate (separate from Kepler.ExposeBPFMetrics)
	EnableGpu     bool // separate from Kepler.EnabledGPU to distinguish model vs collector
	EnableRedfish bool
	// RAPL specifics
	PowercapBasePath string
	RaplDomains      string // comma list: package,core,dram,psys
	//GPU specifics
	PreferDCGM         bool // set to false to prefer NVML
	EnablePerProcess   bool
	EnableMIGDiscovery bool
}

type Config struct {
	ModelServerService     string
	KernelVersion          float32
	Kepler                 KeplerConfig
	SamplePeriodSec        uint64
	Model                  ModelConfig
	Metrics                MetricsConfig
	Redfish                RedfishConfig
	Libvirt                LibvirtConfig
	Host                   HostConfig
	DCGMHostEngineEndpoint string
	TychoTiming            TychoTimingConfig
	TychoCalibration       TychoCalibrationConfig
	TychoAnalysis          TychoAnalysisConfig
	TychoCollector         TychoCollectorConfig
}

// newConfig creates and returns a new Config instance.
func newConfig() (*Config, error) {
	absBaseDir, err := filepath.Abs(BaseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for config-dir: %s: %w", BaseDir, err)
	}

	s, err := os.Stat(absBaseDir)
	if os.IsNotExist(err) {
		// if the directory does not exist, create it
		if err := os.MkdirAll(absBaseDir, 0755); err != nil {
			return nil, fmt.Errorf("config-dir %s does not exist", BaseDir)
		}
		s, err = os.Stat(absBaseDir)
		if err != nil {
			return nil, fmt.Errorf("failed to stat config-dir %s: %w", BaseDir, err)
		}
	}
	if !s.IsDir() {
		return nil, fmt.Errorf("config-dir %s is not a directory", BaseDir)
	}

	return &Config{
		ModelServerService:     fmt.Sprintf("kepler-model-server.%s.svc.cluster.local", getConfig("KEPLER_NAMESPACE", defaultNamespace)),
		Kepler:                 getKeplerConfig(),
		SamplePeriodSec:        uint64(getIntConfig("SAMPLE_PERIOD_SEC", defaultSamplePeriodSec)),
		Model:                  getModelConfig(),
		Metrics:                getMetricsConfig(),
		Redfish:                getRedfishConfig(),
		Libvirt:                getLibvirtConfig(),
		DCGMHostEngineEndpoint: getConfig("NVIDIA_HOSTENGINE_ENDPOINT", defaultDCGMHostEngineEndpoint),
		KernelVersion:          float32(0),
		Host:                   getHostConfig(),
		TychoTiming:            getTychoTimingConfig(),
		TychoCalibration:       getTychoCalibrationConfig(),
		TychoAnalysis:          getTychoAnalysisConfig(),
		TychoCollector:         getTychoCollectorConfig(),
	}, nil
}

// Instance returns the singleton Config instance
func Instance() *Config {
	return instance
}

// Initialize initializes the global instance once and returns an error if
func Initialize(baseDir string) (*Config, error) {
	var err error
	once.Do(func() {
		BaseDir = baseDir
		instance, err = newConfig()
		ValidateTychoQuick()
		NormalizeTycho()
	})
	return instance, err
}

func getKeplerConfig() KeplerConfig {
	return KeplerConfig{
		KeplerNamespace:              getConfig("KEPLER_NAMESPACE", defaultNamespace),
		EnabledEBPFCgroupID:          getBoolConfig("ENABLE_EBPF_CGROUPID", true),
		EnabledGPU:                   getBoolConfig("ENABLE_GPU", false),
		EnabledMSR:                   getBoolConfig("ENABLE_MSR", false),
		EnableProcessStats:           getBoolConfig("ENABLE_PROCESS_METRICS", false),
		ExposeContainerStats:         getBoolConfig("EXPOSE_CONTAINER_METRICS", true),
		ExposeVMStats:                getBoolConfig("EXPOSE_VM_METRICS", true),
		ExposeHardwareCounterMetrics: getBoolConfig("EXPOSE_HW_COUNTER_METRICS", true),
		ExposeIRQCounterMetrics:      getBoolConfig("EXPOSE_IRQ_COUNTER_METRICS", true),
		ExposeBPFMetrics:             getBoolConfig("EXPOSE_BPF_METRICS", true),
		ExposeComponentPower:         getBoolConfig("EXPOSE_COMPONENT_POWER", true),
		ExposeIdlePowerMetrics:       getBoolConfig("EXPOSE_ESTIMATED_IDLE_POWER_METRICS", false),
		EnableAPIServer:              getBoolConfig("ENABLE_API_SERVER", false),
		MockACPIPowerPath:            getConfig("MOCK_ACPI_POWER_PATH", ""),
		MaxLookupRetry:               getIntConfig("MAX_LOOKUP_RETRY", defaultMaxLookupRetry),
		KubeConfig:                   getConfig("KUBE_CONFIG", defaultKubeConfig),
		BPFSampleRate:                getIntConfig("EXPERIMENTAL_BPF_SAMPLE_RATE", defaultBPFSampleRate),
		EstimatorModel:               getConfig("ESTIMATOR_MODEL", defaultMetricValue),
		EstimatorSelectFilter:        getConfig("ESTIMATOR_SELECT_FILTER", defaultMetricValue), // no filter
		CPUArchOverride:              getConfig("CPU_ARCH_OVERRIDE", defaultCPUArchOverride),
		ExcludeSwapperProcess:        getBoolConfig("EXCLUDE_SWAPPER_PROCESS", defaultExcludeSwapperProcess),
	}
}

func getTychoTimingConfig() TychoTimingConfig {
	return TychoTimingConfig{
		TimebaseQuantumMs:  getIntConfig("TYCHO_TIMEBASE_QUANTUM_MS", defaultTychoTimebaseQuantumMs),
		BufferWindowSec:    getIntConfig("TYCHO_BUFFER_WINDOW_SEC", defaultTychoBufferWindowSec),
		BufferMarginCycles: getIntConfig("TYCHO_BUFFER_MARGIN_CYCLES", defaultTychoBufferMarginCycles),
		RaplPollMs:         getIntConfig("TYCHO_RAPL_POLL_MS", defaultTychoRaplPollMs),
		RaplDelayMs:        getIntConfig("TYCHO_RAPL_DELAY_MS", defaultTychoRaplDelayMs),
		BpfPollMs:          getIntConfig("TYCHO_BPF_POLL_MS", defaultTychoBpfPollMs),
		BpfDelayMs:         getIntConfig("TYCHO_BPF_DELAY_MS", defaultTychoBpfDelayMs),
		GpuPollMs:          getIntConfig("TYCHO_GPU_POLL_MS", defaultTychoGpuPollMs),
		GpuDelayMs:         getIntConfig("TYCHO_GPU_DELAY_MS", defaultTychoGpuDelayMs),
		RedfishPollMs:      getIntConfig("TYCHO_REDFISH_POLL_MS", defaultTychoRedfishPollMs),
		RedfishDelayMs:     getIntConfig("TYCHO_REDFISH_DELAY_MS", defaultTychoRedfishDelayMs),
		RedfishHeartbeatMs: getIntConfig("TYCHO_REDFISH_HEARTBEAT_MAX_GAP_MS", defaultTychoRedfishHeartbeatMs),
	}
}

func getTychoCalibrationConfig() TychoCalibrationConfig {
	return TychoCalibrationConfig{
		IdleBudgetSec:                     getIntConfig("TYCHO_CALIBRATION_IDLE_BUDGET_SEC", defaultTychoCalibrationInitIdleBudgetSec),
		RaplPollMinMs:                     getIntConfig("TYCHO_CALIBRATION_RAPL_POLL_MIN_MS", defaultTychoCalibrationInitRaplPollMinMs),
		RaplDelayEnabled:                  getBoolConfig("TYCHO_CALIBRATION_RAPL_DELAY_ENABLE", defaultTychoCalibrationInitRaplDelayEnabled),
		RaplDelayBudgetSec:                getIntConfig("TYCHO_CALIBRATION_RAPL_DELAY_BUDGET_SEC", defaultTychoCalibrationInitRaplDelayBudgetSec),
		RaplIdleEnabled:                   getBoolConfig("TYCHO_CALIBRATION_RAPL_IDLE_ENABLE", defaultTychoCalibrationInitRaplIdleEnabled),
		RedfishContinuousHeartbeatEnabled: getBoolConfig("TYCHO_CALIBRATION_REDFISH_CONTINUOUS_HEARTBEAT_ENABLE", defaultTychoCalibrationRedfishContinuousHeartbeatEnabled),
		RedfishPollEnabled:                getBoolConfig("TYCHO_CALIBRATION_REDFISH_POLL_ENABLE", defaultTychoCalibrationInitRedfishPollEnabled),
		RedfishPollBudgetSec:              getIntConfig("TYCHO_CALIBRATION_REDFISH_POLL_BUDGET_SEC", defaultTychoCalibrationInitRedfishPollBudgetSec),
		RedfishPollMinMs:                  getIntConfig("TYCHO_CALIBRATION_REDFISH_POLL_MIN_MS", defaultTychoCalibrationInitRedfishPollMinMs),
		RedfishDelayEnabled:               getBoolConfig("TYCHO_CALIBRATION_REDFISH_DELAY_ENABLE", defaultTychoCalibrationInitRedfishDelayEnabled),
		RedfishDelayBudgetSec:             getIntConfig("TYCHO_CALIBRATION_REDFISH_DELAY_BUDGET_SEC", defaultTychoCalibrationInitRedfishDelayBudgetSec),
		RedfishIdleEnabled:                getBoolConfig("TYCHO_CALIBRATION_REDFISH_IDLE_ENABLE", defaultTychoCalibrationInitRedfishIdleEnabled),
		GpuPollEnabled:                    getBoolConfig("TYCHO_CALIBRATION_GPU_POLL_ENABLE", defaultTychoCalibrationInitGpuPollEnabled),
		GpuPollBudgetSec:                  getIntConfig("TYCHO_CALIBRATION_GPU_POLL_BUDGET_SEC", defaultTychoCalibrationInitGpuPollBudgetSec),
		GpuPollMinMs:                      getIntConfig("TYCHO_CALIBRATION_GPU_POLL_MIN_MS", defaultTychoCalibrationInitGpuPollMinMs),
		GpuDelayEnabled:                   getBoolConfig("TYCHO_CALIBRATION_GPU_DELAY_ENABLE", defaultTychoCalibrationInitGpuDelayEnabled),
		GpuDelayBudgetSec:                 getIntConfig("TYCHO_CALIBRATION_GPU_DELAY_BUDGET_SEC", defaultTychoCalibrationInitGpuDelayBudgetSec),
		GpuIdleEnabled:                    getBoolConfig("TYCHO_CALIBRATION_GPU_IDLE_ENABLE", defaultTychoCalibrationInitGpuIdleEnabled),
	}
}

func getTychoAnalysisConfig() TychoAnalysisConfig {
	return TychoAnalysisConfig{
		Trigger:            getConfig("TYCHO_ANALYSIS_TRIGGER", defaultTychoTrigger),
		TriggerIntervalSec: getIntConfig("TYCHO_ANALYSIS_EVERY_SEC", defaultTychoTriggerIntervalSec),
		DetectLongestDelay: getBoolConfig("TYCHO_ANALYSIS_DETECT_LONGEST_DELAY", defaultTychoDetectLongestDelay),
		DelayAfterMs:       getIntConfig("TYCHO_ANALYSIS_DELAY_AFTER_MS", defaultTychoDelayAfterMs),
	}
}
func getTychoCollectorConfig() TychoCollectorConfig {
	return TychoCollectorConfig{
		EnableRapl:         getBoolConfig("TYCHO_COLLECTOR_ENABLE_RAPL", defaultTychoEnableRapl),
		EnableBpf:          getBoolConfig("TYCHO_COLLECTOR_ENABLE_BPF", defaultTychoEnableBpf),
		EnableGpu:          getBoolConfig("TYCHO_COLLECTOR_ENABLE_GPU", defaultTychoEnableGpu),
		EnableRedfish:      getBoolConfig("TYCHO_COLLECTOR_ENABLE_REDFISH", defaultTychoEnableRedfish),
		PowercapBasePath:   getConfig("TYCHO_COLLECTOR_POWERCAP_BASE_PATH", defaultTychoPowercapBasePath),
		RaplDomains:        getConfig("TYCHO_COLLECTOR_RAPL_DOMAINS", defaultTychoRaplDomains),
		PreferDCGM:         getBoolConfig("TYCHO_COLLECTOR_GPU_PREFER_DCGM", defaultTychoGPUPreferDCGM),
		EnablePerProcess:   getBoolConfig("TYCHO_COLLECTOR_GPU_ENABLE_PERPROCESS", defaultTychoGPUEnablePerProcess),
		EnableMIGDiscovery: getBoolConfig("TYCHO_COLLECTOR_GPU_ENABL_MIGDISCOVERY", defaultTychoGPUEnableMIGDiscovery),
	}
}

func getMetricsConfig() MetricsConfig {
	return MetricsConfig{
		CoreUsageMetric:    getConfig("CORE_USAGE_METRIC", CPUInstruction),
		DRAMUsageMetric:    getConfig("DRAM_USAGE_METRIC", CacheMiss),
		UncoreUsageMetric:  getConfig("UNCORE_USAGE_METRIC", defaultMetricValue),
		GPUUsageMetric:     getConfig("GPU_USAGE_METRIC", GPUComputeUtilization),
		GeneralUsageMetric: getConfig("GENERAL_USAGE_METRIC", defaultMetricValue),
	}
}

func getRedfishConfig() RedfishConfig {
	return RedfishConfig{
		CredFilePath:           getConfig("REDFISH_CRED_FILE_PATH", ""),
		ProbeIntervalInSeconds: getConfig("REDFISH_PROBE_INTERVAL_IN_SECONDS", "60"),
		SkipSSLVerify:          getBoolConfig("REDFISH_SKIP_SSL_VERIFY", true),
	}
}

func ProcDir() string {
	return instance.Host.ProcDir
}

func SysDir() string {
	return instance.Host.SysDir
}

func getModelConfig() ModelConfig {
	return ModelConfig{
		ModelServerEnable:           getBoolConfig("MODEL_SERVER_ENABLE", false),
		ModelServerEndpoint:         setModelServerReqEndpoint(),
		ModelConfigValues:           GetModelConfigMap(),
		NodePlatformPowerKey:        getConfig("NODE_TOTAL_POWER_KEY", defaultNodePlatformPowerKey),
		NodeComponentsPowerKey:      getConfig("NODE_COMPONENTS_POWER_KEY", defaultNodeComponentsPowerKey),
		ContainerPlatformPowerKey:   getConfig("CONTAINER_TOTAL_POWER_KEY", defaultContainerPlatformPowerKey),
		ContainerComponentsPowerKey: getConfig("CONTAINER_COMPONENTS_POWER_KEY", defaultContainerComponentsPowerKey),
		ProcessPlatformPowerKey:     getConfig("PROCESS_TOTAL_POWER_KEY", defaultProcessPlatformPowerKey),
		ProcessComponentsPowerKey:   getConfig("PROCESS_COMPONENTS_POWER_KEY", defaultProcessComponentsPowerKey),
	}
}

func getHostConfig() HostConfig {
	return HostConfig{
		ProcDir: getConfig("PROC_DIR", "/proc"),
		SysDir:  getConfig("SYS_DIR", "/sys"),
	}
}

func getLibvirtConfig() LibvirtConfig {
	return LibvirtConfig{
		MetadataURI:   getConfig("LIBVIRT_METADATA_URI", ""),
		MetadataToken: getConfig("LIBVIRT_METADATA_TOKEN", "name"),
	}
}

// Helper functions
func getBoolConfig(configKey string, defaultBool bool) bool {
	defaultValue := "false"
	if defaultBool {
		defaultValue = "true"
	}
	return strings.ToLower(getConfig(configKey, defaultValue)) == "true"
}

func getIntConfig(configKey string, defaultInt int) int {
	defaultValue := strconv.Itoa(defaultInt)
	value, err := strconv.Atoi(getConfig(configKey, defaultValue))
	if err == nil {
		return value
	}
	return defaultInt
}

// getConfig returns the value of the key by first looking in the environment
// and then in the config file if it exists or else returns the default value.
func getConfig(key, defaultValue string) string {
	// env var takes precedence over config file
	if envValue, exists := os.LookupEnv(key); exists {
		return envValue
	}

	// return config file value if there is one
	configFile := filepath.Join(BaseDir, key)
	if value, err := os.ReadFile(configFile); err == nil {
		return strings.TrimSpace(bytes.NewBuffer(value).String())
	}

	return defaultValue
}

func setModelServerReqEndpoint() string {
	modelServerURL := getConfig("MODEL_SERVER_URL", "kepler-model-server")
	if modelServerURL == "kepler-model-server" {
		modelServerPort := strings.TrimSuffix(getConfig("MODEL_SERVER_PORT", defaultModelServerPort), "\n")
		modelServerURL = fmt.Sprintf("http://%s:%s", modelServerURL, modelServerPort)
	}
	modelReqPath := getConfig("MODEL_SERVER_MODEL_REQ_PATH", defaultModelRequestPath)
	return modelServerURL + modelReqPath
}

// return local path to power model weight
// e.g., /var/lib/kepler/data/model_weight/acpi_AbsPowerModel.json
func GetDefaultPowerModelURL(modelOutputType, energySource string) string {
	return fmt.Sprintf(`/var/lib/kepler/data/model_weight/%s_%sModel.json`, energySource, modelOutputType)
}

func logBoolConfigs() {
	if klog.V(5).Enabled() {
		klog.V(5).Infof("PROC_DIR: %s", instance.Host.ProcDir)
		klog.V(5).Infof("SYS_DIR: %s", instance.Host.SysDir)
		klog.V(5).Infof("ENABLE_EBPF_CGROUPID: %t", instance.Kepler.EnabledEBPFCgroupID)
		klog.V(5).Infof("ENABLE_GPU: %t", instance.Kepler.EnabledGPU)
		klog.V(5).Infof("ENABLE_PROCESS_METRICS: %t", instance.Kepler.EnableProcessStats)
		klog.V(5).Infof("EXPOSE_HW_COUNTER_METRICS: %t", instance.Kepler.ExposeHardwareCounterMetrics)
		klog.V(5).Infof("EXPOSE_IRQ_COUNTER_METRICS: %t", instance.Kepler.ExposeIRQCounterMetrics)
		klog.V(5).Infof("EXPOSE_BPF_METRICS: %t", instance.Kepler.ExposeBPFMetrics)
		klog.V(5).Infof("EXPOSE_COMPONENT_POWER: %t", instance.Kepler.ExposeComponentPower)
		klog.V(5).Infof("EXPOSE_ESTIMATED_IDLE_POWER_METRICS: %t. This only impacts when the power is estimated using pre-prained models. Estimated idle power is meaningful only when Kepler is running on bare-metal or with a single virtual machine (VM) on the node.", instance.Kepler.ExposeIdlePowerMetrics)
		klog.V(5).Infof("EXPERIMENTAL_BPF_SAMPLE_RATE: %d", instance.Kepler.BPFSampleRate)
		klog.V(5).Infof("EXCLUDE_SWAPPER_PROCESS: %t", instance.Kepler.ExcludeSwapperProcess)
	}
}

func logTychoConfigs() {
	klog.V(5).Infof("START TYCHO CONFIGS: ----------------------------------------")
	klog.V(5).Infof("TYCHO_TIMEBASE_QUANTUM_MS: %d", instance.TychoTiming.TimebaseQuantumMs)
	klog.V(5).Infof("TYCHO_BUFFER_WINDOW_SEC: %d", instance.TychoTiming.BufferWindowSec)
	klog.V(5).Infof("TYCHO_BUFFER_MARGIN_CYCLES: %d", instance.TychoTiming.BufferMarginCycles)
	klog.V(5).Infof("TYCHO_POLL/DELAY: RAPL: %d/%d, BPF: %d/%d, GPU: %d/%d, REDFISH: %d/%d",
		instance.TychoTiming.RaplPollMs, instance.TychoTiming.RaplDelayMs,
		instance.TychoTiming.BpfPollMs, instance.TychoTiming.BpfDelayMs,
		instance.TychoTiming.GpuPollMs, instance.TychoTiming.GpuDelayMs,
		instance.TychoTiming.RedfishPollMs, instance.TychoTiming.RedfishDelayMs)
	klog.V(5).Infof("TYCHO_REDFISH_HEARTBEAT_MAX_GAP_MS: %d", instance.TychoTiming.RedfishHeartbeatMs)
	klog.V(5).Infof("TYCHO_ANALYSIS_TRIGGER: %s", instance.TychoAnalysis.Trigger)
	klog.V(5).Infof("TYCHO_ANALYSIS_EVERY_SEC: %d", instance.TychoAnalysis.TriggerIntervalSec)
	klog.V(5).Infof("TYCHO_ANALYSIS_DETECT_LONGEST_DELAY: %t", instance.TychoAnalysis.DetectLongestDelay)
	klog.V(5).Infof("TYCHO_ANALYSIS_DELAY_AFTER_MS: %d", instance.TychoAnalysis.DelayAfterMs)
	klog.V(5).Infof("TYCHO_COLLECTOR_ENABLED: RAPL: %t, BPF: %t, GPU: %t, REDFISH: %t",
		instance.TychoCollector.EnableRapl,
		instance.TychoCollector.EnableBpf,
		instance.TychoCollector.EnableGpu,
		instance.TychoCollector.EnableRedfish)
	klog.V(5).Infof("TYCHO_COLLECTOR_POWERCAP_BASE_PATH: %s", instance.TychoCollector.PowercapBasePath)
	klog.V(5).Infof("TYCHO_COLLECTOR_RAPL_DOMAINS: %s", instance.TychoCollector.RaplDomains)
	klog.V(5).Infof("START TYCHO CALIBRATION CONFIGS")
	klog.V(5).Infof("IdleBudgetSec:%d", instance.TychoCalibration.IdleBudgetSec)
	klog.V(5).Infof("RAPL:  DelayEnabled=%t, DelayBudgetSec=%d, IdleEnabled=%t",
		instance.TychoCalibration.RaplDelayEnabled,
		instance.TychoCalibration.RaplDelayBudgetSec,
		instance.TychoCalibration.RaplIdleEnabled)
	klog.V(5).Infof("REDFISH: ContinuousHeartbeatEnabled=%t, PollEnabled=%t, PollBudgetSec=%d, DelayEnabled=%t, DelayBudgetSec=%d, IdleEnabled=%t",
		instance.TychoCalibration.RedfishContinuousHeartbeatEnabled,
		instance.TychoCalibration.RedfishPollEnabled,
		instance.TychoCalibration.RedfishPollBudgetSec,
		instance.TychoCalibration.RedfishDelayEnabled,
		instance.TychoCalibration.RedfishDelayBudgetSec,
		instance.TychoCalibration.RedfishIdleEnabled)
	klog.V(5).Infof("GPU: PollEnabled=%t, PollBudgetSec=%d, DelayEnabled=%t, DelayBudgetSec=%d, IdleEnabled=%t",
		instance.TychoCalibration.GpuPollEnabled,
		instance.TychoCalibration.GpuPollBudgetSec,
		instance.TychoCalibration.GpuDelayEnabled,
		instance.TychoCalibration.GpuDelayBudgetSec,
		instance.TychoCalibration.GpuIdleEnabled)
	klog.V(5).Infof("GPU: Prefer DCGM=%t, EnablePerProcess=%t, EnableMIGDiscovery=%t",
		instance.TychoCollector.PreferDCGM, instance.TychoCollector.EnablePerProcess, instance.TychoCollector.EnableMIGDiscovery)
	klog.V(5).Infof("STOP TYCHO CONFIGS: ----------------------------------------")

}

func ValidateTychoQuick() {
	t := &instance.TychoTiming
	a := &instance.TychoAnalysis
	c := &instance.TychoCollector

	// basic bounds
	if t.TimebaseQuantumMs <= 0 {
		klog.Fatalf("TYCHO: timebaseQuantumMs must be > 0 (got %d)", t.TimebaseQuantumMs)
	}
	clampNonNegative := func(p *int, name string) {
		if *p < 0 {
			klog.V(2).Infof("TYCHO: %s < 0 (%d) -> clamping to 0", name, *p)
			*p = 0
		}
	}
	for _, fld := range []struct {
		p *int
		n string
	}{
		{&t.RaplPollMs, "RaplPollMs"}, {&t.RaplDelayMs, "RaplDelayMs"},
		{&t.BpfPollMs, "BpfPollMs"}, {&t.BpfDelayMs, "BpfDelayMs"},
		{&t.GpuPollMs, "GpuPollMs"}, {&t.GpuDelayMs, "GpuDelayMs"},
		{&t.RedfishPollMs, "RedfishPollMs"}, {&t.RedfishDelayMs, "RedfishDelayMs"},
		{&t.RedfishHeartbeatMs, "RedfishHeartbeatMs"},
		{&t.BufferWindowSec, "BufferWindowSec"}, {&t.BufferWindowSec, "BufferWindowSec"},
		{&a.DelayAfterMs, "DelayAfterMs"},
	} {
		clampNonNegative(fld.p, fld.n)
	}
	if t.BufferWindowSec == 0 {
		t.BufferWindowSec = 1
	}

	// trigger coherence
	if a.Trigger == "redfish" && !c.EnableRedfish {
		klog.V(2).Infof("TYCHO: trigger=redfish but redfish disabled -> switching to timer")
		a.Trigger = "timer"
	}
	if a.Trigger == "timer" {
		if a.TriggerIntervalSec <= 0 {
			klog.V(2).Infof("TYCHO: trigger=timer but interval<=0 -> defaulting to 5s")
			a.TriggerIntervalSec = 5
		}
	} else if a.Trigger != "redfish" {
		klog.V(2).Infof("TYCHO: unknown trigger=%q -> defaulting to redfish", a.Trigger)
		a.Trigger = "redfish"
	}

	// redfish cadence sanity
	if t.RedfishHeartbeatMs < t.RedfishPollMs {
		klog.V(2).Infof("TYCHO: RedfishHeartbeatMs (%d) < RedfishPollMs (%d) -> raising to poll",
			t.RedfishHeartbeatMs, t.RedfishPollMs)
		t.RedfishHeartbeatMs = t.RedfishPollMs * 2
	}

	// at-least-one collector
	if !(c.EnableRapl || c.EnableBpf || c.EnableGpu || c.EnableRedfish) {
		klog.V(1).Infof("TYCHO: no collectors enabled; the agent will not collect telemetry")
	}

	// filesystem hint (non-fatal)
	if c.EnableRapl {
		if _, err := os.Stat(defaultTychoPowercapBasePath); err != nil {
			klog.V(1).Infof("TYCHO: powercap base path %q not found (RAPL may fail): %v",
				defaultTychoPowercapBasePath, err)
		}
	}
}

// Ensure plausible configuration values, adjust if necessary
func NormalizeTycho() {
	t := &instance.TychoTiming
	a := &instance.TychoAnalysis
	c := &instance.TychoCollector

	// helpers
	align := func(ms, q int) int {
		if q <= 0 {
			return ms
		}
		if r := ms % q; r != 0 {
			return ms + (q - r)
		}
		return ms
	}
	enabledMax := func(pairs ...struct {
		en bool
		v  int
	}) int {
		m := 0
		set := false
		for _, p := range pairs {
			if !p.en {
				continue
			}
			if !set || p.v > m {
				m, set = p.v, true
			}
		}
		if !set {
			return 0
		}
		return m
	}

	// Align everything to quantum
	q := t.TimebaseQuantumMs
	t.RaplPollMs = align(t.RaplPollMs, q)
	t.RaplDelayMs = align(t.RaplDelayMs, q)
	t.BpfPollMs = align(t.BpfPollMs, q)
	t.BpfDelayMs = align(t.BpfDelayMs, q)
	t.GpuPollMs = align(t.GpuPollMs, q)
	t.GpuDelayMs = align(t.GpuDelayMs, q)
	t.RedfishPollMs = align(t.RedfishPollMs, q)
	t.RedfishDelayMs = align(t.RedfishDelayMs, q)
	t.RedfishHeartbeatMs = align(t.RedfishHeartbeatMs, q)
	a.DelayAfterMs = align(a.DelayAfterMs, q)
	if t.BufferMarginCycles < 0 {
		t.BufferMarginCycles = 0
	}

	// Ensure DelayAfterMs ≥ longest per-source delay
	// longest per-source delay (enabled only)
	longestDelay := enabledMax(
		struct {
			en bool
			v  int
		}{c.EnableRapl, t.RaplDelayMs},
		struct {
			en bool
			v  int
		}{c.EnableBpf, t.BpfDelayMs},
		struct {
			en bool
			v  int
		}{c.EnableGpu, t.GpuDelayMs},
		struct {
			en bool
			v  int
		}{c.EnableRedfish, t.RedfishDelayMs},
	)
	if a.DelayAfterMs < longestDelay {
		klog.V(2).Infof("TYCHO: auto-increasing DelayAfterMs from %d to %d (longest source delay)",
			a.DelayAfterMs, longestDelay)
		a.DelayAfterMs = longestDelay
	}

	// Compute fast-sources requirement (acquisition + analysis fire)
	// acquisition duration: period + delay, enabled only
	fastSourcesMs := enabledMax(
		struct {
			en bool
			v  int
		}{c.EnableRapl, t.RaplPollMs + t.RaplDelayMs},
		struct {
			en bool
			v  int
		}{c.EnableBpf, t.BpfPollMs + t.BpfDelayMs},
		struct {
			en bool
			v  int
		}{c.EnableGpu, t.GpuPollMs + t.GpuDelayMs},
		struct {
			en bool
			v  int
		}{c.EnableRedfish, t.RedfishPollMs + t.RedfishDelayMs},
	)
	requiredMs := fastSourcesMs + a.DelayAfterMs

	// Compute Redfish coverage if enabled (slow publisher coverage)
	if c.EnableRedfish {
		if t.RedfishHeartbeatMs > requiredMs {
			requiredMs = t.RedfishHeartbeatMs + a.DelayAfterMs
		}
	}

	// Final buffer requirement: required + small safety
	minBufMs := (requiredMs * (1 + t.BufferMarginCycles)) + 100 // 100 ms safety
	if t.BufferWindowSec < minBufMs*1000 {
		newSec := (minBufMs + 999) / 1000 // ceil ms→s
		klog.V(2).Infof(
			"TYCHO: auto-increasing BufferWindowSec from %ds to %ds (required=%dms, fast+delayAfter=%dms, redfishCov=%dms, quantum=%dms)",
			t.BufferWindowSec, newSec, minBufMs, fastSourcesMs+a.DelayAfterMs, t.RedfishHeartbeatMs, q,
		)
		t.BufferWindowSec = newSec
		klog.V(2).Infof("TYCHO buffer calc: fast=%dms, delayAfter=%dms, redfishCov=%dms, safety=100ms => minBufMs=%dms -> %ds",
			fastSourcesMs, a.DelayAfterMs, t.RedfishHeartbeatMs, minBufMs, newSec)
	}

	// Optional compact summary
	klog.V(2).Infof(
		"TYCHO EFFECTIVE: poll={rapl:%dms,bpf:%dms,gpu:%dms,redfish:%dms} delay={rapl:%d,bpf:%d,gpu:%d,redfish:%d} fireDelay=%dms buffer=%ds quantum=%dms trigger=%s",
		t.RaplPollMs, t.BpfPollMs, t.GpuPollMs, t.RedfishPollMs,
		t.RaplDelayMs, t.BpfDelayMs, t.GpuDelayMs, t.RedfishDelayMs,
		a.DelayAfterMs, t.BufferWindowSec, q, a.Trigger,
	)
}

func LogConfigs() {
	klog.V(5).Infof("config-dir: %s", BaseDir)
	logBoolConfigs()
	logTychoConfigs()
}

func SetRedfishCredFilePath(credFilePath string) {
	instance.Redfish.CredFilePath = credFilePath
}

func SetRedfishProbeIntervalInSeconds(interval string) {
	instance.Redfish.ProbeIntervalInSeconds = interval
}

func SetRedfishSkipSSLVerify(skipSSLVerify bool) {
	instance.Redfish.SkipSSLVerify = skipSSLVerify
}

// SetEnabledEBPFCgroupID enables or disables eBPF code to collect cgroup ID
// based on kernel version and cgroup version.
// SetEnabledEBPFCgroupID enables the eBPF code to collect cgroup id if the system has kernel version > 4.18
func SetEnabledEBPFCgroupID(enabled bool) {
	// set to false if any config source set it to false
	enabled = enabled && instance.Kepler.EnabledEBPFCgroupID
	klog.Infoln("using gCgroup ID in the BPF program:", enabled)
	instance.KernelVersion = getKernelVersion(&realSystem{})
	klog.Infoln("kernel version:", instance.KernelVersion)
	if (enabled) && (instance.KernelVersion >= cGroupIDMinKernelVersion) && (isCGroupV2(&realSystem{})) {
		instance.Kepler.EnabledEBPFCgroupID = true
	} else {
		instance.Kepler.EnabledEBPFCgroupID = false
	}
}

// SetEnabledHardwareCounterMetrics enables the exposure of hardware counter metrics
func SetEnabledHardwareCounterMetrics(enabled bool) {
	// set to false is any config source set it to false
	instance.Kepler.ExposeHardwareCounterMetrics = enabled
}

// SetEnabledIdlePower allows enabling idle power exposure in Kepler's metrics. When direct power metrics access is available,
// idle power exposure is automatic. With pre-trained power models, awareness of implications is crucial.
// Estimated idle power is useful for bare-metal or single VM setups. In VM environments, accurately distributing idle power is tough due
// to unknown co-running VMs. Wrong division results in significant accuracy errors, duplicatiing the host idle power across all VMs.
// Container pre-trained models focus on dynamic power. Estimating idle power in limited information scenarios (like VMs) is complex.
// Idle power prediction is limited to bare-metal or single VM setups.
// Know the number of running VMs becomes crucial for achieving a fair distribution of idle power, particularly when following the GHG (Greenhouse Gas) protocol.
func SetEnabledIdlePower(enabled bool) {
	// set to true is any config source set it to true or if system power metrics are available
	instance.Kepler.ExposeIdlePowerMetrics = enabled
	if instance.Kepler.ExposeIdlePowerMetrics {
		klog.Infoln("The Idle power will be exposed. Are you running on Baremetal or using single VM per node?")
	}
}

// SetEnabledGPU enables the exposure of gpu metrics
func SetEnabledGPU(enabled bool) {
	instance.Kepler.EnabledGPU = enabled
}

func SetModelServerEnable(enabled bool) {
	instance.Model.ModelServerEnable = enabled || instance.Model.ModelServerEnable
}

// SetEnabledMSR enables the exposure of MSR metrics
func SetEnabledMSR(enabled bool) {
	instance.Kepler.EnabledMSR = enabled
}

// SetKubeConfig set kubeconfig file
func SetKubeConfig(k string) {
	instance.Kepler.KubeConfig = k
}

// SetEnableAPIServer enables Kepler to watch apiserver
func SetEnableAPIServer(enabled bool) {
	instance.Kepler.EnableAPIServer = enabled
}

func SetEstimatorConfig(modelName, selectFilter string) {
	instance.Kepler.EstimatorModel = modelName
	instance.Kepler.EstimatorSelectFilter = selectFilter
}

func SetModelServerEndpoint(serverEndpoint string) {
	instance.Model.ModelServerEndpoint = serverEndpoint
}

func SetMachineSpecFilePath(specFilePath string) {
	instance.Kepler.MachineSpecFilePath = specFilePath
}

// GetMachineSpec initializes a map of MachineSpecValues from MACHINE_SPEC
func GetMachineSpec() *MachineSpec {
	if instance.Kepler.MachineSpecFilePath != "" {
		if spec, err := readMachineSpec(instance.Kepler.MachineSpecFilePath); err == nil {
			return spec
		} else {
			klog.Warningf("failed to read spec from %s: %v, use default machine spec", instance.Kepler.MachineSpecFilePath, err)
		}
	}
	return getDefaultMachineSpec()
}

func GetMetricPath(cmdSet string) string {
	return getConfig(metricPathKey, cmdSet)
}

func GetBindAddress(cmdSet string) string {
	return getConfig(bindAddressKey, cmdSet)
}

func SetGPUUsageMetric(metric string) {
	instance.Metrics.GPUUsageMetric = metric
}

type realSystem struct{}

var _ Client = &realSystem{}

func (c *realSystem) getUnixName() (unix.Utsname, error) {
	var utsname unix.Utsname
	err := unix.Uname(&utsname)
	return utsname, err
}

func (c *realSystem) getCgroupV2File() string {
	return fmt.Sprintf(cGroupV2Path, SysDir())
}

func getKernelVersion(c Client) float32 {
	utsname, err := c.getUnixName()
	if err != nil {
		klog.V(4).Infoln("Failed to parse unix name")
		return -1
	}
	// per https://github.com/google/cadvisor/blob/master/machine/info.go#L164
	kv := utsname.Release[:bytes.IndexByte(utsname.Release[:], 0)]

	versionParts := versionRegex.FindStringSubmatch(string(kv))
	if len(versionParts) < 2 {
		klog.V(1).Infof("got invalid release version %q (expected format '4.3-1 or 4.3.2-1')\n", kv)
		return -1
	}
	major, err := strconv.Atoi(versionParts[1])
	if err != nil {
		klog.V(1).Infof("got invalid release major version %q\n", major)
		return -1
	}

	minor, err := strconv.Atoi(versionParts[2])
	if err != nil {
		klog.V(1).Infof("got invalid release minor version %q\n", minor)
		return -1
	}

	v, err := strconv.ParseFloat(fmt.Sprintf("%d.%d", major, minor), 32)
	if err != nil {
		klog.V(1).Infof("parse %d.%d got issue: %v", major, minor, err)
		return -1
	}
	return float32(v)
}

func isCGroupV2(c Client) bool {
	_, err := os.Stat(c.getCgroupV2File())
	return !os.IsNotExist(err)
}

// InitModelConfigMap initializes map of config from MODEL_CONFIG
func InitModelConfigMap() {
	if instance.Model.ModelConfigValues == nil {
		instance.Model.ModelConfigValues = GetModelConfigMap()
	}
}

// IsIdlePowerEnabled always return true if Kepler has access to system power metrics.
// However, if pre-trained power models are being used, Kepler should only expose metrics if the user is aware of the implications.
func IsIdlePowerEnabled() bool {
	return instance.Kepler.ExposeIdlePowerMetrics
}

// IsExposeProcessStatsEnabled returns false if process metrics are disabled to minimize overhead in the Kepler standalone mode.
func IsExposeProcessStatsEnabled() bool {
	return instance.Kepler.EnableProcessStats
}

// IsExposeContainerStatsEnabled returns false if container metrics are disabled to minimize overhead in the Kepler standalone mode.
func IsExposeContainerStatsEnabled() bool {
	return instance.Kepler.ExposeContainerStats
}

// IsExposeVMStatsEnabled returns false if VM metrics are disabled to minimize overhead.
func IsExposeVMStatsEnabled() bool {
	return instance.Kepler.ExposeVMStats
}

// IsExposeBPFMetricsEnabled returns false if BPF Metrics metrics are disabled to minimize overhead.
func IsExposeBPFMetricsEnabled() bool {
	return instance.Kepler.ExposeBPFMetrics
}

// IsExposeComponentPowerEnabled returns false if component power metrics are disabled to minimize overhead.
func IsExposeComponentPowerEnabled() bool {
	return instance.Kepler.ExposeComponentPower
}

func IsEnabledMSR() bool {
	return instance.Kepler.EnabledMSR
}

func IsModelServerEnabled() bool {
	return instance.Model.ModelServerEnable
}

func ModelServerEndpoint() string {
	return instance.Model.ModelServerEndpoint
}

func GetModelConfigMap() map[string]string {
	configMap := make(map[string]string)
	modelConfigStr := getConfig("MODEL_CONFIG", "")
	lines := strings.Fields(modelConfigStr)
	for _, line := range lines {
		values := strings.Split(line, "=")
		if len(values) == 2 {
			k := strings.TrimSpace(values[0])
			v := strings.TrimSpace(values[1])
			configMap[k] = v
		}
	}
	return configMap
}

func GetLibvirtMetadataURI() string {
	return instance.Libvirt.MetadataURI
}

func GetLibvirtMetadataToken() string {
	return instance.Libvirt.MetadataToken
}

func ExposeIRQCounterMetrics() bool {
	return instance.Kepler.ExposeIRQCounterMetrics
}

func GetBPFSampleRate() int {
	return instance.Kepler.BPFSampleRate
}

func GetRedfishCredFilePath() string {
	return instance.Redfish.CredFilePath
}

func GetRedfishProbeIntervalInSeconds() int {
	// convert string "redfishProbeIntervalInSeconds" to int
	probeInterval, err := strconv.Atoi(instance.Redfish.ProbeIntervalInSeconds)
	if err != nil {
		klog.Warning("failed to convert redfishProbeIntervalInSeconds to int", err)
		return 60
	}
	return probeInterval
}

func GetRedfishSkipSSLVerify() bool {
	return instance.Redfish.SkipSSLVerify
}

func GetMockACPIPowerPath() string {
	return instance.Kepler.MockACPIPowerPath
}

func ExposeHardwareCounterMetrics() bool {
	return instance.Kepler.ExposeHardwareCounterMetrics
}

func IsGPUEnabled() bool {
	return instance.Kepler.EnabledGPU
}

func SamplePeriodSec() uint64 {
	return instance.SamplePeriodSec
}

func TimebaseQuantumMs() int {
	return instance.TychoTiming.TimebaseQuantumMs
}

func BufferWindowSec() int {
	return instance.TychoTiming.BufferWindowSec
}

func BufferMarginCycles() int {
	return instance.TychoTiming.BufferMarginCycles
}

func RaplPollMs() int {
	return instance.TychoTiming.RaplPollMs
}

func RaplDelayMs() int {
	return instance.TychoTiming.RaplDelayMs
}

func SetRaplDelayMs(ms int) {
	instance.TychoTiming.RaplDelayMs = ms
}

func BpfPollMs() int {
	return instance.TychoTiming.BpfPollMs
}

func BpfDelayMs() int {
	return instance.TychoTiming.BpfDelayMs
}

func GpuPollMs() int {
	return instance.TychoTiming.GpuPollMs
}

func SetGpuPollMs(ms int) {
	instance.TychoTiming.GpuPollMs = ms
}

func GpuDelayMs() int {
	return instance.TychoTiming.GpuDelayMs
}

func SetGpuDelayMs(ms int) {
	instance.TychoTiming.GpuDelayMs = ms
}

func RedfishPollMs() int {
	return instance.TychoTiming.RedfishPollMs
}

func SetRedfishPollMs(ms int) {
	instance.TychoTiming.RedfishPollMs = ms
}

func RedfishDelayMs() int {
	return instance.TychoTiming.RedfishDelayMs
}

func SetRedfishDelayMs(ms int) {
	instance.TychoTiming.RedfishDelayMs = ms
}

func RedfishHeartbeatMs() int {
	return instance.TychoTiming.RedfishHeartbeatMs
}

func CalibrationIdleBudgetSec() int {
	return instance.TychoCalibration.IdleBudgetSec
}

func CalibrationRaplPollMinMs() int {
	return instance.TychoCalibration.RaplPollMinMs
}

func CalibrationRaplDelayEnabled() bool {
	return instance.TychoCalibration.RaplDelayEnabled
}

func CalibrationRaplDelayBudgetSec() int {
	return instance.TychoCalibration.RaplDelayBudgetSec
}

func CalibrationRaplIdleEnabled() bool {
	return instance.TychoCalibration.RaplIdleEnabled
}

func CalibrationREdfishContinuousHeartbeatEnabled() bool {
	return instance.TychoTiming.RedfishAutoHeartbeat
}

func CalibrationRedfishPollEnabled() bool {
	return instance.TychoCalibration.RedfishPollEnabled
}

func CalibrationRedfishPollBudgetSec() int {
	return instance.TychoCalibration.RedfishPollBudgetSec
}

func CalibrationRedfishPollMinMs() int {
	return instance.TychoCalibration.RedfishPollMinMs
}

func CalibrationRedfishDelayEnabled() bool {
	return instance.TychoCalibration.RedfishDelayEnabled
}

func CalibrationRedfishDelayBudgetSec() int {
	return instance.TychoCalibration.RedfishDelayBudgetSec
}

func CalibrationRedfishIdleEnabled() bool {
	return instance.TychoCalibration.RedfishIdleEnabled
}

func CalibrationGpuPollEnabled() bool {
	return instance.TychoCalibration.GpuPollEnabled
}

func CalibrationGpuPollBudgetSec() int {
	return instance.TychoCalibration.GpuPollBudgetSec
}

func CalibrationGpuPollMinMs() int {
	return instance.TychoCalibration.GpuPollMinMs
}

func CalibrationGpuDelayEnabled() bool {
	return instance.TychoCalibration.GpuDelayEnabled
}

func CalibrationGpuDelayBudgetSec() int {
	return instance.TychoCalibration.GpuDelayBudgetSec
}

func CalibrationGpuIdleEnabled() bool {
	return instance.TychoCalibration.GpuIdleEnabled
}

func Trigger() string {
	return instance.TychoAnalysis.Trigger
}

func TriggerIntervalSec() int {
	return instance.TychoAnalysis.TriggerIntervalSec
}

func DetectLongestDelay() bool {
	return instance.TychoAnalysis.DetectLongestDelay
}

func DelayAfterMs() int {
	return instance.TychoAnalysis.DelayAfterMs
}

func EnableRapl() bool {
	return instance.TychoCollector.EnableRapl
}

func EnableBpf() bool {
	return instance.TychoCollector.EnableBpf
}

func EnableGpu() bool {
	return instance.TychoCollector.EnableGpu
}

func EnableRedfish() bool {
	return instance.TychoCollector.EnableRedfish
}

func PowercapBasePath() string {
	return instance.TychoCollector.PowercapBasePath
}

func RaplDomains() string {
	return instance.TychoCollector.RaplDomains
}

func GPUPreferDcgm() bool {
	return instance.TychoCollector.PreferDCGM
}

func GpuEnablePerProcess() bool {
	return instance.TychoCollector.EnablePerProcess
}

func GpuEnableMigDiscovery() bool {
	return instance.TychoCollector.EnableMIGDiscovery
}

func CoreUsageMetric() string {
	return instance.Metrics.CoreUsageMetric
}

func DRAMUsageMetric() string {
	return instance.Metrics.DRAMUsageMetric
}

func GPUUsageMetric() string {
	return instance.Metrics.GPUUsageMetric
}

func CPUArchOverride() string {
	return instance.Kepler.CPUArchOverride
}

func GeneralUsageMetric() string {
	return instance.Metrics.GeneralUsageMetric
}

func KubeConfig() string {
	return instance.Kepler.KubeConfig
}

func EnabledEBPFCgroupID() bool {
	return instance.Kepler.EnabledEBPFCgroupID
}

func NodePlatformPowerKey() string {
	return instance.Model.NodePlatformPowerKey
}

func NodeComponentsPowerKey() string {
	return instance.Model.NodeComponentsPowerKey
}

func ContainerPlatformPowerKey() string {
	return instance.Model.ContainerPlatformPowerKey
}

func ModelConfigValues(k string) string {
	return instance.Model.ModelConfigValues[k]
}

func ContainerComponentsPowerKey() string {
	return instance.Model.ContainerComponentsPowerKey
}

func ProcessPlatformPowerKey() string {
	return instance.Model.ProcessPlatformPowerKey
}

func ProcessComponentsPowerKey() string {
	return instance.Model.ProcessComponentsPowerKey
}

func IsAPIServerEnabled() bool {
	return instance.Kepler.EnableAPIServer
}

func BPFHwCounters() []string {
	return []string{CPUCycle, CPUInstruction, CacheMiss, CPURefCycle}
}

func BPFSwCounters() []string {
	return []string{CPUTime, IRQNetTXLabel, IRQNetRXLabel, IRQBlockLabel, PageCacheHit}
}

func DCGMHostEngineEndpoint() string {
	return instance.DCGMHostEngineEndpoint
}

func ExcludeSwapperProcess() bool {
	return instance.Kepler.ExcludeSwapperProcess
}
