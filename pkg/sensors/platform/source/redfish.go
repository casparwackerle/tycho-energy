package source

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/casparwackerle/tycho-energy/pkg/config"
	"github.com/casparwackerle/tycho-energy/pkg/nodecred"
	"k8s.io/klog/v2"
)

// ---------- Redfish model types (float64-tolerant) ----------

type RedfishChassisModel struct {
	OdataContext string `json:"@odata.context"`
	OdataID      string `json:"@odata.id"`
	OdataType    string `json:"@odata.type"`
	Description  string `json:"Description"`
	Members      []struct {
		OdataID string `json:"@odata.id"`
	} `json:"Members"`
	MembersOdataCount int    `json:"Members@odata.count"`
	Name              string `json:"Name"`
}

type RedfishPowerModel struct {
	OdataType     string          `json:"@odata.type,omitempty"`
	ID            string          `json:"Id,omitempty"`
	Name          string          `json:"Name,omitempty"`
	PowerControl  []PowerControl  `json:"PowerControl,omitempty"`
	Voltages      []Voltages      `json:"Voltages,omitempty"`
	PowerSupplies []PowerSupplies `json:"PowerSupplies,omitempty"`
	Actions       Actions         `json:"Actions,omitempty"`
	OdataID       string          `json:"@odata.id,omitempty"`
}

type PowerMetrics struct {
	IntervalInMin        int     `json:"IntervalInMin,omitempty"`
	MinConsumedWatts     float64 `json:"MinConsumedWatts,omitempty"`
	MaxConsumedWatts     float64 `json:"MaxConsumedWatts,omitempty"`
	AverageConsumedWatts float64 `json:"AverageConsumedWatts,omitempty"`
}

type PowerLimit struct {
	LimitInWatts   int    `json:"LimitInWatts,omitempty"`
	LimitException string `json:"LimitException,omitempty"`
	CorrectionInMs int    `json:"CorrectionInMs,omitempty"`
}

type RelatedItem struct {
	OdataID string `json:"@odata.id,omitempty"`
}

type Status struct {
	State  string `json:"State,omitempty"`
	Health string `json:"Health,omitempty"`
}

type PowerControl struct {
	OdataID             string        `json:"@odata.id,omitempty"`
	MemberID            string        `json:"MemberId,omitempty"`
	Name                string        `json:"Name,omitempty"`
	PowerConsumedWatts  float64       `json:"PowerConsumedWatts,omitempty"`
	PowerRequestedWatts int           `json:"PowerRequestedWatts,omitempty"`
	PowerAvailableWatts int           `json:"PowerAvailableWatts,omitempty"`
	PowerCapacityWatts  int           `json:"PowerCapacityWatts,omitempty"`
	PowerAllocatedWatts int           `json:"PowerAllocatedWatts,omitempty"`
	PowerMetrics        PowerMetrics  `json:"PowerMetrics,omitempty"`
	PowerLimit          PowerLimit    `json:"PowerLimit,omitempty"`
	RelatedItem         []RelatedItem `json:"RelatedItem,omitempty"`
	Status              Status        `json:"Status,omitempty"`
}

// Many BMCs use decimals for thresholds/ranges → use float64 consistently.
type Voltages struct {
	OdataID                   string        `json:"@odata.id,omitempty"`
	MemberID                  string        `json:"MemberId,omitempty"`
	Name                      string        `json:"Name,omitempty"`
	SensorNumber              float64       `json:"SensorNumber,omitempty"`
	Status                    Status        `json:"Status,omitempty"`
	ReadingVolts              float64       `json:"ReadingVolts,omitempty"`
	UpperThresholdNonCritical float64       `json:"UpperThresholdNonCritical,omitempty"`
	UpperThresholdCritical    float64       `json:"UpperThresholdCritical,omitempty"`
	UpperThresholdFatal       float64       `json:"UpperThresholdFatal,omitempty"`
	LowerThresholdNonCritical float64       `json:"LowerThresholdNonCritical,omitempty"`
	LowerThresholdCritical    float64       `json:"LowerThresholdCritical,omitempty"`
	LowerThresholdFatal       float64       `json:"LowerThresholdFatal,omitempty"`
	MinReadingRange           float64       `json:"MinReadingRange,omitempty"`
	MaxReadingRange           float64       `json:"MaxReadingRange,omitempty"`
	PhysicalContext           string        `json:"PhysicalContext,omitempty"`
	RelatedItem               []RelatedItem `json:"RelatedItem,omitempty"`
}

type InputRanges struct {
	InputType      string  `json:"InputType,omitempty"`
	MinimumVoltage float64 `json:"MinimumVoltage,omitempty"`
	MaximumVoltage float64 `json:"MaximumVoltage,omitempty"`
	OutputWattage  float64 `json:"OutputWattage,omitempty"`
}

type PowerSupplies struct {
	OdataID              string        `json:"@odata.id,omitempty"`
	MemberID             string        `json:"MemberId,omitempty"`
	Name                 string        `json:"Name,omitempty"`
	Status               Status        `json:"Status,omitempty"`
	PowerSupplyType      string        `json:"PowerSupplyType,omitempty"`
	LineInputVoltageType string        `json:"LineInputVoltageType,omitempty"`
	LineInputVoltage     float64       `json:"LineInputVoltage,omitempty"`
	PowerCapacityWatts   float64       `json:"PowerCapacityWatts,omitempty"`
	LastPowerOutputWatts float64       `json:"LastPowerOutputWatts,omitempty"`
	Model                string        `json:"Model,omitempty"`
	Manufacturer         string        `json:"Manufacturer,omitempty"`
	FirmwareVersion      string        `json:"FirmwareVersion,omitempty"`
	SerialNumber         string        `json:"SerialNumber,omitempty"`
	PartNumber           string        `json:"PartNumber,omitempty"`
	SparePartNumber      string        `json:"SparePartNumber,omitempty"`
	InputRanges          []InputRanges `json:"InputRanges,omitempty"`
	RelatedItem          []RelatedItem `json:"RelatedItem,omitempty"`
}

type PowerPowerSupplyReset struct {
	Target string `json:"target,omitempty"`
}

type Actions struct {
	PowerPowerSupplyReset PowerPowerSupplyReset `json:"#Power.PowerSupplyReset,omitempty"`
}

// ---------- Cached system state (per chassis) ----------

type RedfishSystemPowerResult struct {
	chassis       string
	consumedWatts float64

	// Last time GetAbsEnergyFromPlatform() integrated this value.
	timestamp time.Time

	// Newness tracking for Tycho (header-driven preferred):
	lastETag   string
	sourceDate time.Time
	lastChange time.Time
	seq        uint64
}

// Access credentials
type RedfishAccessInfo struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Host     string `json:"host"`
}

type RedFishClient struct {
	accessInfo    RedfishAccessInfo
	systems       []*RedfishSystemPowerResult
	probeInterval time.Duration // retained for compatibility; no ticker used
	mutex         sync.Mutex
}

func NewRedfishClient() *RedFishClient {
	credPath := config.GetRedfishCredFilePath()
	if credPath == "" {
		klog.Infof("failed to get redfish credential file path")
		return nil
	}
	if err := nodecred.InitNodeCredImpl(map[string]string{"redfish_cred_file_path": credPath}); err != nil {
		klog.Infof("%s", fmt.Sprintf("failed to initialize node credential: %v", err))
		return nil
	}

	klog.V(5).Infof("Initialized node credential")
	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		nodeName = "localhost"
	}
	redfishCred, err := nodecred.GetNodeCredByNodeName(nodeName, "redfish")
	if err != nil {
		klog.V(1).Infof("%s", fmt.Sprintf("failed to get node credential: %v", err))
		return nil
	}

	userName := redfishCred["redfish_username"]
	password := redfishCred["redfish_password"]
	host := redfishCred["redfish_host"]
	if host == "" {
		return nil
	}

	klog.V(5).Infof("Initialized redfish credential")
	probeInterval := config.GetRedfishProbeIntervalInSeconds()
	interval := time.Duration(probeInterval) * time.Second

	return &RedFishClient{
		accessInfo:    RedfishAccessInfo{Username: userName, Password: password, Host: host},
		systems:       []*RedfishSystemPowerResult{},
		probeInterval: interval,
		mutex:         sync.Mutex{},
	}
}

func (*RedFishClient) GetName() string { return "redfish" }

// One-time discovery. Tycho owns polling cadence.
func (rf *RedFishClient) IsSystemCollectionSupported() bool {
	chassis, err := getRedfishChassis(rf.accessInfo)
	if err != nil {
		klog.Infof("failed to get redfish chassis info: %v", err)
		return false
	}

	for _, member := range chassis.Members {
		parts := strings.Split(member.OdataID, "/")
		if len(parts) < 2 {
			continue
		}
		id := parts[len(parts)-1]

		var res RedfishSystemPowerResult
		power, hdr, err := getRedfishPower(rf.accessInfo, id)
		if err != nil || len(power.PowerControl) == 0 {
			klog.V(5).Infof("failed to get power info for chassis=%s: %v", id, err)
			continue
		}

		val := power.PowerControl[0].PowerConsumedWatts
		etag := hdr.Get("ETag")
		dateHdr := hdr.Get("Date")
		var srcDate time.Time
		if t, err := http.ParseTime(dateHdr); err == nil {
			srcDate = t
		}

		res.chassis = id
		res.consumedWatts = val
		res.timestamp = time.Now()
		res.lastChange = res.timestamp
		res.lastETag = etag
		res.sourceDate = srcDate

		rf.systems = append(rf.systems, &res)
		klog.V(5).Infof("redfish init: chassis=%s watts=%.3f etag=%q date=%v", id, val, etag, srcDate)
	}

	return len(rf.systems) > 0
}

// PollOnce fetches current power for all discovered systems and updates the cache.
// Tycho should call this at RedfishPollMs cadence.
func (rf *RedFishClient) PollOnce() {
	if rf == nil || rf.systems == nil {
		return
	}

	for _, system := range rf.systems {
		power, hdr, err := getRedfishPower(rf.accessInfo, system.chassis)
		if err != nil || len(power.PowerControl) == 0 {
			klog.V(5).Infof("redfish poll failed for chassis=%s: %v", system.chassis, err)
			continue
		}

		newWatts := power.PowerControl[0].PowerConsumedWatts
		etag := hdr.Get("ETag")
		dateHdr := hdr.Get("Date")
		var srcDate time.Time
		if t, err := http.ParseTime(dateHdr); err == nil {
			srcDate = t
		}

		rf.mutex.Lock()
		changed := false

		// 1) Header-driven newness
		if etag != "" && etag != system.lastETag {
			system.lastETag = etag
			changed = true
		}
		if !srcDate.IsZero() && srcDate.After(system.sourceDate) {
			system.sourceDate = srcDate
			changed = true
		}

		// 2) Value-driven fallback
		if !changed && newWatts != system.consumedWatts {
			changed = true
		}

		if changed {
			system.consumedWatts = newWatts
			system.lastChange = time.Now()
			system.seq++
		}
		rf.mutex.Unlock()
	}
}

// Safe iteration under lock
func (rf *RedFishClient) ForEachSystem(f func(sys *RedfishSystemPowerResult)) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()
	for _, s := range rf.systems {
		f(s)
	}
}

// Kepler-compat energy integration (mJ) from cached instantaneous power.
func (rf *RedFishClient) GetAbsEnergyFromPlatform() (map[string]float64, error) {
	if rf.systems == nil {
		return nil, nil
	}
	out := make(map[string]float64, len(rf.systems))
	rf.mutex.Lock()
	defer rf.mutex.Unlock()

	now := time.Now()
	for _, system := range rf.systems {
		elapsed := now.Sub(system.timestamp).Seconds()
		system.timestamp = now
		// W * 1000 * s = mJ
		out[system.chassis] = system.consumedWatts * 1000.0 * elapsed
		klog.V(5).Infof("redfish integrate: chassis=%s watts=%.3f elapsed=%.3fs", system.chassis, system.consumedWatts, elapsed)
	}
	return out, nil
}

// Lightweight getters used by Tycho's collector
func (s *RedfishSystemPowerResult) Chassis() string       { return s.chassis }
func (s *RedfishSystemPowerResult) Sequence() uint64      { return s.seq }
func (s *RedfishSystemPowerResult) Watts() float64        { return s.consumedWatts }
func (s *RedfishSystemPowerResult) SourceDate() time.Time { return s.sourceDate }

// No-op (no internal ticker anymore)
func (rf *RedFishClient) StopPower() {}
