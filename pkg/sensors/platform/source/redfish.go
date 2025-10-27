/*
Copyright 2023.

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

// RedfishChassisModel is the struct for the physical components of a system.
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

// RedfishPowerModel is the struct for the power model
// Generated from Redfish examples and normalized to tolerate floats from BMCs.
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

// NOTE: A number of BMCs report decimals for ranges/thresholds; prefer float64.
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

// RedfishSystemPowerResult holds per-chassis cached power and metadata.
// Tycho will drive polling via PollOnce(); no internal ticker here.
type RedfishSystemPowerResult struct {
	chassis       string
	consumedWatts float64
	// timestamp is the last time Kepler's GetAbsEnergyFromPlatform() integrated this value.
	timestamp time.Time
	// newness tracking for Tycho:
	lastChange time.Time
	seq        uint64
	// Optional header metadata (populate if you decide to capture in redfish_util.go)
	lastETag   string
	sourceDate time.Time
}

// RedfishAccessInfo is the struct for the access model
type RedfishAccessInfo struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Host     string `json:"host"`
}

type RedFishClient struct {
	accessInfo    RedfishAccessInfo
	systems       []*RedfishSystemPowerResult
	probeInterval time.Duration // retained for compatibility; not used for a ticker anymore
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
	} else {
		klog.V(5).Infof("Initialized node credential")
		nodeName := os.Getenv("NODE_NAME")
		if nodeName == "" {
			nodeName = "localhost"
		}
		redfishCred, err := nodecred.GetNodeCredByNodeName(nodeName, "redfish")
		if err == nil {
			userName := redfishCred["redfish_username"]
			password := redfishCred["redfish_password"]
			host := redfishCred["redfish_host"]
			if host != "" {
				klog.V(5).Infof("Initialized redfish credential")
				probeInterval := config.GetRedfishProbeIntervalInSeconds()
				interval := time.Duration(probeInterval) * time.Second
				redfish := &RedFishClient{
					accessInfo:    RedfishAccessInfo{Username: userName, Password: password, Host: host},
					systems:       []*RedfishSystemPowerResult{},
					probeInterval: interval,
					mutex:         sync.Mutex{},
				}
				return redfish
			}
		} else {
			klog.V(1).Infof("%s", fmt.Sprintf("failed to get node credential: %v", err))
			return nil
		}
	}
	return nil
}

func (*RedFishClient) GetName() string {
	return "redfish"
}

// IsSystemCollectionSupported performs one-time discovery/fetch.
// It no longer starts an internal ticker; Tycho owns polling.
func (rf *RedFishClient) IsSystemCollectionSupported() bool {
	chassis, err := getRedfishChassis(rf.accessInfo)
	if err != nil {
		klog.Infof("failed to get redfish chassis info: %v\n", err)
		return false
	}

	// iterate each "Members" in the chassis and get the initial power info
	for index, member := range chassis.Members {
		// split the OdataID by delimiter "/" and get the chassis ID
		split := strings.Split(member.OdataID, "/")
		if len(split) < 2 {
			continue
		}
		id := split[len(split)-1]
		res := RedfishSystemPowerResult{}
		power, hdr, err := getRedfishPower(rf.accessInfo, id)
		if err == nil && len(power.PowerControl) > 0 {
			val := power.PowerControl[0].PowerConsumedWatts
			// capture header hints
			etag := hdr.Get("ETag")
			dateHdr := hdr.Get("Date")
			var srcDate time.Time
			if t, err := http.ParseTime(dateHdr); err == nil {
				srcDate = t
			}
			if index < len(rf.systems) {
				rf.systems[index].consumedWatts = val
				rf.systems[index].timestamp = time.Now()
				rf.systems[index].lastChange = rf.systems[index].timestamp
				rf.systems[index].lastETag = etag
				rf.systems[index].sourceDate = srcDate
			} else {
				res.chassis = id
				res.consumedWatts = val
				res.timestamp = time.Now()
				res.lastChange = res.timestamp
				res.lastETag = etag
				res.sourceDate = srcDate
				rf.systems = append(rf.systems, &res)
			}
			klog.V(5).Infof("power info: %+v\n", power)
		} else {
			klog.V(5).Infof("failed to get power info: %v\n", err)
		}
	}
	return len(rf.systems) > 0
}

// PollOnce fetches current power for all discovered systems and updates the cache.
// Tycho should call this at its desired cadence (e.g., every RedfishPollMs).
func (rf *RedFishClient) PollOnce() {
	if rf == nil || rf.systems == nil {
		return
	}
	for _, system := range rf.systems {
		power, hdr, err := getRedfishPower(rf.accessInfo, system.chassis)
		if err == nil && len(power.PowerControl) > 0 {
			newWatts := power.PowerControl[0].PowerConsumedWatts
			etag := hdr.Get("ETag")
			dateHdr := hdr.Get("Date")
			var srcDate time.Time
			if t, err := http.ParseTime(dateHdr); err == nil {
				srcDate = t
			}

			rf.mutex.Lock()
			if newWatts != system.consumedWatts {
				changed := false
				// 1) Header-driven newness (preferred)
				if etag != "" && etag != system.lastETag {
					system.lastETag = etag
					changed = true
				} else if !srcDate.IsZero() && srcDate.After(system.sourceDate) {
					system.sourceDate = srcDate
					changed = true
				}
				// 2) Value-driven newness (fallback)
				if !changed && newWatts != system.consumedWatts {
					changed = true
				}

				if changed {
					system.consumedWatts = newWatts
					system.lastChange = time.Now()
					system.seq++
				}
			}
			rf.mutex.Unlock()
		} else {
			klog.V(5).Infof("redfish poll failed for chassis=%s: %v", system.chassis, err)
		}
	}
}

// ForEachSystem safely iterates over current systems under lock.
func (rf *RedFishClient) ForEachSystem(f func(sys *RedfishSystemPowerResult)) {
	rf.mutex.Lock()
	defer rf.mutex.Unlock()
	for _, s := range rf.systems {
		f(s)
	}
}

// GetAbsEnergyFromPlatform returns energy delta (mJ) by integrating held power.
// This remains compatible with Kepler's integration path (if you still use it).
func (rf *RedFishClient) GetAbsEnergyFromPlatform() (map[string]float64, error) {
	if rf.systems == nil {
		return nil, nil
	}
	power := make(map[string]float64)
	for _, system := range rf.systems {
		rf.mutex.Lock()
		now := time.Now()
		// elapsed time since the last integration in seconds
		elapsed := now.Sub(system.timestamp).Seconds()
		system.timestamp = now
		klog.V(5).Infof("power info: %+v\n", system)
		// consumedWatts is instantaneous W; convert to mW and multiply by seconds → mJ
		power[system.chassis] = system.consumedWatts * 1000.0 * elapsed
		rf.mutex.Unlock()
	}
	return power, nil
}

// In pkg/sensors/platform/source/redfish.go
func (s *RedfishSystemPowerResult) Chassis() string  { return s.chassis }
func (s *RedfishSystemPowerResult) Sequence() uint64 { return s.seq }
func (s *RedfishSystemPowerResult) Watts() float64   { return s.consumedWatts }

// StopPower is a no-op now (no internal ticker).
func (rf *RedFishClient) StopPower() {}
