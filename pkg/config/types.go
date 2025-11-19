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

const (
	CORE      = "core"
	DRAM      = "dram"
	UNCORE    = "uncore"
	PKG       = "package"
	GPU       = "gpu"
	OTHER     = "other"
	PLATFORM  = "platform"
	FREQUENCY = "frequency"

	// counter - attacher package
	CPUCycle       = "cpu_cycles"
	CPURefCycle    = "cpu_ref_cycles"
	CPUInstruction = "cpu_instructions"
	CacheMiss      = "cache_miss"

	// bpf - attacher package
	CPUTime       = "bpf_cpu_time_ms"
	PageCacheHit  = "bpf_page_cache_hit"
	IRQNetTXLabel = "bpf_net_tx_irq"
	IRQNetRXLabel = "bpf_net_rx_irq"
	IRQBlockLabel = "bpf_block_irq"

	// GPU
	GPUComputeUtilization = "gpu_compute_util"
	GPUMemUtilization     = "gpu_mem_util"

	// Energy Metrics
	// Absolute energy and power
	AbsEnergyInCore     = "abs_energy_in_core"
	AbsEnergyInDRAM     = "abs_energy_in_dram"
	AbsEnergyInUnCore   = "abs_energy_in_uncore"
	AbsEnergyInPkg      = "abs_energy_in_pkg"
	AbsEnergyInGPU      = "abs_energy_in_gpu"
	AbsEnergyInOther    = "abs_energy_in_other"
	AbsEnergyInPlatform = "abs_energy_in_platform"
	// Dynamic energy and power
	DynEnergyInCore     = "dyn_energy_in_core"
	DynEnergyInDRAM     = "dyn_energy_in_dram"
	DynEnergyInUnCore   = "dyn_energy_in_uncore"
	DynEnergyInPkg      = "dyn_energy_in_pkg"
	DynEnergyInGPU      = "dyn_energy_in_gpu"
	DynEnergyInOther    = "dyn_energy_in_other"
	DynEnergyInPlatform = "dyn_energy_in_platform"
	// Idle energy and power
	IdleEnergyInCore     = "idle_energy_in_core"
	IdleEnergyInDRAM     = "idle_energy_in_dram"
	IdleEnergyInUnCore   = "idle_energy_in_uncore"
	IdleEnergyInPkg      = "idle_energy_in_pkg"
	IdleEnergyInGPU      = "idle_energy_in_gpu"
	IdleEnergyInOther    = "idle_energy_in_other"
	IdleEnergyInPlatform = "idle_energy_in_platform"

	cGroupIDMinKernelVersion = 4.18
	// If this file is present, cgroups v2 is enabled on that node.
	cGroupV2Path   = "%s/fs/cgroup/cgroup.controllers"
	metricPathKey  = "METRIC_PATH"
	bindAddressKey = "BIND_ADDRESS"
	// model_parameter_attributes
	EstimatorEnabledKey      = "ESTIMATOR"
	LocalRegressorEnabledKey = "LOCAL_REGRESSOR"
	InitModelURLKey          = "INIT_URL"
	FixedTrainerNameKey      = "TRAINER"
	ModelFiltersKey          = "FILTERS"
	DefaultTrainerName       = "SGDRegressorTrainer"
	// Local defaults
	defaultMetricValue      = ""
	defaultNamespace        = "kepler"
	defaultModelServerPort  = "8100"
	defaultModelRequestPath = "/model"
	defaultMaxLookupRetry   = 500
	// MaxIRQ is the maximum number of IRQs to be monitored
	MaxIRQ = 10
	// defaultSamplePeriodSec is the time in seconds that the reader will wait before reading the metrics again
	defaultSamplePeriodSec       = 3
	defaultKubeConfig            = ""
	defaultBPFSampleRate         = 0
	defaultCPUArchOverride       = ""
	defaultExcludeSwapperProcess = false
	// model_parameter_prefix
	defaultNodePlatformPowerKey        = "NODE_TOTAL"
	defaultNodeComponentsPowerKey      = "NODE_COMPONENTS"
	defaultContainerPlatformPowerKey   = "CONTAINER_TOTAL"
	defaultContainerComponentsPowerKey = "CONTAINER_COMPONENTS"
	defaultProcessPlatformPowerKey     = "PROCESS_TOTAL"
	defaultProcessComponentsPowerKey   = "PROCESS_COMPONENTS"
	DefaultMachineSpecFilePath         = "/etc/kepler/models/machine/spec.json"
	defaultDCGMHostEngineEndpoint      = "localhost:5555"

	// Tycho Timing Config
	defaultTychoTimebaseQuantumMs  = 1
	defaultTychoBufferWindowSec    = 5   // will be overwritten if too shoft. length of the buffer in seconds. must be longer than longest measurement inverval (likely redfish)
	defaultTychoBufferMarginCycles = 3   // must be at least 1
	defaultTychoRaplPollMs         = 50  //must be equal or multiple of defaultTychoRaplPollMs
	defaultTychoRaplDelayMs        = 0   //must be equal or multiple of defaultTychoRaplPollMs, assumed to be 0
	defaultTychoBpfPollMs          = 50  //must be equal or multiple of defaultTychoRaplPollMs
	defaultTychoBpfDelayMs         = 0   //must be equal or multiple of defaultTychoRaplPollMs, assumed to be 0
	defaultTychoGpuPollMs          = 200 //must be equal or multiple of defaultTychoRaplPollMs
	defaultTychoGpuDelayMs         = 200 //must be equal or multiple of defaultTychoRaplPollMs
	defaultGpuPhaseAwareSampling   = true
	defaultTychoRedfishPollMs      = 1000  //must be equal or multiple of defaultTychoRaplPollMs. Realistically, Redfish publishes at much slower rates, often 15 or more seconds
	defaultTychoRedfishDelayMs     = 0     //must be equal or multiple of defaultTychoRaplPollMs, assumed to be 0
	defaultTychoRedfishHeartbeatMs = 15000 //realistically possible redish update period, often 15-20 seconds

	// Tycho calibration config

	//defaultTychoCalibrationInitIdleBudgetSec                 = 10 // in sec: Duration of (general) idle testing
	defaultTychoCalibrationInitRaplPollMinMs = 50 // in ms: Poll intervals cannot be shorter
	//defaultTychoCalibrationInitRaplIdleEnabled               = false
	defaultTychoCalibrationRedfishContinuousHeartbeatEnabled = true // continuously adjusts Redfish Heartbeat
	defaultTychoCalibrationInitRedfishPollEnabled            = false
	defaultTychoCalibrationInitRedfishPollBudgetSec          = 60  //in sec: Duration of Poll testing
	defaultTychoCalibrationInitRedfishPollMinMs              = 500 // in ms: Poll intervals cannot be shorter
	//defaultTychoCalibrationInitRedfishIdleEnabled            = false
	defaultTychoCalibrationInitGpuPollEnabled   = false
	defaultTychoCalibrationInitGpuPollBudgetSec = 30 //in sec: Duration of Poll testing
	defaultTychoCalibrationInitGpuPollMinMs     = 50 // in ms: Poll intervals cannot be shorter
	//defaultTychoCalibrationInitGpuDelayEnabled               = false
	//defaultTychoCalibrationInitGpuDelayBudgetSec             = 30 // in sec: Duration of Delay Testing
	//defaultTychoCalibrationInitGpuIdleEnabled = false

	// Tycho Analysis Config
	defaultTychoTrigger            = "timer" // "redfish" | "timer"
	defaultTychoTriggerIntervalSec = 15      // only used if trigger=timer
	defaultTychoDetectLongestDelay = false
	defaultTychoDelayAfterMs       = 200 //will be overwritten if there is a metric delay that is longer

	// Tycho Collector Config
	defaultTychoEnableRapl            = true
	defaultTychoEnableBpf             = true
	defaultTychoEnableGpu             = true
	defaultTychoEnableRedfish         = true
	defaultTychoPowercapBasePath      = "/sys/class/powercap"
	defaultTychoRaplDomains           = "package,core,dram,uncore"
	defaultTychoGPUPreferDCGM         = true
	defaultTychoGPUEnablePerProcess   = true
	defaultTychoGPUEnableMIGDiscovery = true
)

var BaseDir string = "/etc/kepler/kepler.config"
