package calibration

type Results struct {
	// GPU
	GpuBestPollMS *int
	GpuDelayMS    *int
	GpuIdleP5     *float64

	// Redfish
	RedfishBestPollMS *int
	RedfishDelayMS    *int
	RedfishIdleP5     *float64

	// RAPL
	RaplDelayMS *int
	RaplIdleP5  *float64

	// Diagnostics
	Notes  map[string]string
	Status map[string]string
}
