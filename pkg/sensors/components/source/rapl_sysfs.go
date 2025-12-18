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

package source

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

const (
	// sysfs path templates
	energyFile         = "energy_uj"
	energyMaxRangeFile = "max_energy_range_uj"

	// RAPL number of events (core, dram and uncore)
	numRAPLEvents = 3

	// RAPL events
	dramEvent    = "dram"
	coreEvent    = "core"
	uncoreEvent  = "uncore"
	packageEvent = "package"
)

var (
	once                      sync.Once
	systemCollectionSupported bool
	eventPaths                map[string]map[string]string
)

// getEnergy returns the sum of the energy consumption of all sockets for a given event
func getEnergy(event string) (uint64, error) {
	energy := uint64(0)
	if hasEvent(event) {
		energyMap := readEventEnergy(event)
		for _, e := range energyMap {
			energy += e
		}
		return energy, nil
	}
	return energy, fmt.Errorf("could not read RAPL energy for %s", event)
}

func readEventEnergy(eventName string) map[string]uint64 {
	energy := map[string]uint64{}
	for pkID, subTree := range eventPaths {
		for event, path := range subTree {
			if strings.Index(event, eventName) != 0 {
				continue
			}
			var e uint64
			var err error
			var data []byte

			if data, err = os.ReadFile(path + energyFile); err != nil {
				klog.V(3).Infoln(err)
				continue
			}
			if e, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err != nil {
				klog.V(3).Infoln(err)
				continue
			}
			e /= 1000 /*mJ*/
			energy[pkID] = e
		}
	}
	return energy
}

// readEventEnergyUnwrappedMJ reads per-socket energy_uj for a domain, unwraps it in uJ,
// then returns absolute energy in mJ (uJ/1000).
//
// Unwrap rule (single wrap, no reset detection):
// if raw < prevRaw: wrapAdd += modulus; abs = wrapAdd + raw.
func (r *PowerSysfs) readEventEnergyUnwrappedMJ(eventName string) map[string]uint64 {
	energy := map[string]uint64{}

	if r == nil {
		return energy
	}

	// Ensure cache is initialized (best-effort).
	// In practice IsSystemCollectionSupported() already calls initUnwrapCache().
	r.unwrapOnce.Do(func() { r.initUnwrapCache() })

	for pkID, subTree := range eventPaths {
		for event, path := range subTree {
			if strings.Index(event, eventName) != 0 {
				continue
			}

			// State must exist (domain unsupported if we couldn't read modulus at init).
			stMap := r.state[pkID]
			if stMap == nil {
				continue
			}
			st := stMap[eventName]
			if st == nil || st.modulusUJ == 0 {
				// Domain unsupported (silent).
				continue
			}

			data, err := os.ReadFile(path + energyFile)
			if err != nil {
				klog.V(6).Infof("rapl-sysfs: pk=%s dom=%s read %s failed (%v)", pkID, eventName, energyFile, err)
				continue
			}

			rawUJ, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				klog.V(6).Infof("rapl-sysfs: pk=%s dom=%s parse %s failed (%v)", pkID, eventName, energyFile, err)
				continue
			}

			// Unwrap in uJ (no reset detection).
			if st.hasPrev && rawUJ < st.prevRawUJ {
				st.wrapAddUJ += st.modulusUJ
				klog.V(6).Infof("rapl-sysfs: WRAP pk=%s dom=%s prevUJ=%d rawUJ=%d modulusUJ=%d wrapAddUJ=%d",
					pkID, eventName, st.prevRawUJ, rawUJ, st.modulusUJ, st.wrapAddUJ)
			}
			st.prevRawUJ = rawUJ
			st.hasPrev = true

			absUJ := st.wrapAddUJ + rawUJ
			absMJ := absUJ / 1000

			energy[pkID] = absMJ
		}
	}
	return energy
}

func getMaxEnergyRange(eventName string) (uint64, error) {
	energy := uint64(0)
	if hasEvent(eventName) {
		for _, subTree := range eventPaths {
			for event, path := range subTree {
				if strings.Index(event, eventName) != 0 {
					continue
				}
				var e uint64
				var err error
				var data []byte

				if data, err = os.ReadFile(path + energyMaxRangeFile); err != nil {
					klog.V(3).Infoln(err)
					continue
				}
				if e, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64); err != nil {
					klog.V(3).Infoln(err)
					continue
				}
				e /= 1000 /*mJ*/
				return e, nil
			}
		}
	}
	return energy, fmt.Errorf("could not read RAPL energy max range for %s", eventName)
}

type PowerSysfs struct {
	unwrapOnce sync.Once

	// Per "package name" (e.g. "package-0") and domain ("package","core","uncore","dram")
	// we cache modulus and unwrap state.
	modulusUJ map[string]map[string]uint64
	state     map[string]map[string]*raplUnwrapState
}

type raplUnwrapState struct {
	hasPrev   bool
	prevRawUJ uint64
	wrapAddUJ uint64
	modulusUJ uint64
}

func (PowerSysfs) GetName() string {
	return "rapl-sysfs"
}

func (r *PowerSysfs) IsSystemCollectionSupported() bool {
	// use a hard code to reduce escapes to heap
	// there are parts of code invokes this function
	// use once to reduce IO
	once.Do(func() {
		eventPaths = detectEventPaths(config.SysDir())
		_, err := os.ReadFile(config.SysDir() + "/class/powercap/intel-rapl/intel-rapl:0/energy_uj")
		systemCollectionSupported = (err == nil)
	})

	if !systemCollectionSupported {
		return false
	}

	// Initialize unwrap state + modulus cache once per PowerSysfs instance.
	// This does not change behavior yet; it just prepares cached modulus for later steps.
	if r != nil {
		r.unwrapOnce.Do(func() {
			r.initUnwrapCache()
		})
	}

	return true
}

func (r *PowerSysfs) initUnwrapCache() {
	// Defensive init (PowerSysfs can be zero-value)
	if r.modulusUJ == nil {
		r.modulusUJ = make(map[string]map[string]uint64)
	}
	if r.state == nil {
		r.state = make(map[string]map[string]*raplUnwrapState)
	}

	// Domains we care about (names must match existing constants)
	domains := []string{packageEvent, coreEvent, uncoreEvent, dramEvent}

	// For each package ID and each domain, try to read max_energy_range_uj once.
	for pkID, subTree := range eventPaths {
		if _, ok := r.modulusUJ[pkID]; !ok {
			r.modulusUJ[pkID] = make(map[string]uint64)
		}
		if _, ok := r.state[pkID]; !ok {
			r.state[pkID] = make(map[string]*raplUnwrapState)
		}

		for _, dom := range domains {
			var (
				found bool
				path  string
			)

			// Find the sysfs subtree entry for this domain (prefix match like existing code).
			for ev, p := range subTree {
				if strings.Index(ev, dom) == 0 {
					found = true
					path = p
					break
				}
			}
			if !found {
				continue
			}

			data, err := os.ReadFile(path + energyMaxRangeFile)
			if err != nil {
				// Missing domain should be silent.
				klog.V(6).Infof("rapl-sysfs: pk=%s dom=%s no %s (%v)", pkID, dom, energyMaxRangeFile, err)
				continue
			}

			raw, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if err != nil {
				klog.V(6).Infof("rapl-sysfs: pk=%s dom=%s parse %s failed (%v)", pkID, dom, energyMaxRangeFile, err)
				continue
			}

			// IMPORTANT: store modulus in uJ (no /1000 here). Unwrap happens in uJ.
			r.modulusUJ[pkID][dom] = raw
			r.state[pkID][dom] = &raplUnwrapState{modulusUJ: raw}

			klog.V(6).Infof("rapl-sysfs: unwrap cache init pk=%s dom=%s modulusUJ=%d", pkID, dom, raw)
		}
	}
}

func (r *PowerSysfs) GetAbsEnergyFromDram() (uint64, error) {
	return getEnergy(dramEvent)
}

func (r *PowerSysfs) GetAbsEnergyFromCore() (uint64, error) {
	return getEnergy(coreEvent)
}

func (r *PowerSysfs) GetAbsEnergyFromUncore() (uint64, error) {
	return getEnergy(uncoreEvent)
}

func (r *PowerSysfs) GetAbsEnergyFromPackage() (uint64, error) {
	return getEnergy(packageEvent)
}

func (r *PowerSysfs) GetAbsEnergyFromNodeComponents() map[int]NodeComponentsEnergy {
	packageEnergies := make(map[int]NodeComponentsEnergy)

	// IMPORTANT: use unwrapped absolute counters (still reported as mJ to keep existing contract).
	pkgEnergies := r.readEventEnergyUnwrappedMJ(packageEvent)
	coreEnergies := r.readEventEnergyUnwrappedMJ(coreEvent)
	dramEnergies := r.readEventEnergyUnwrappedMJ(dramEvent)
	uncoreEnergies := r.readEventEnergyUnwrappedMJ(uncoreEvent)

	for pkgID, pkgEnergy := range pkgEnergies {
		coreEnergy := coreEnergies[pkgID]
		dramEnergy := dramEnergies[pkgID]
		uncoreEnergy := uncoreEnergies[pkgID]
		splits := strings.Split(pkgID, "-")
		i, _ := strconv.Atoi(splits[len(splits)-1])
		packageEnergies[i] = NodeComponentsEnergy{
			Core:   coreEnergy,
			DRAM:   dramEnergy,
			Uncore: uncoreEnergy,
			Pkg:    pkgEnergy,
		}
	}

	return packageEnergies
}

func (r *PowerSysfs) StopPower() {
}

func (r *PowerSysfs) GetMaxEnergyRangeFromDram() (uint64, error) {
	return getMaxEnergyRange(dramEvent)
}

func (r *PowerSysfs) GetMaxEnergyRangeFromCore() (uint64, error) {
	return getMaxEnergyRange(coreEvent)
}

func (r *PowerSysfs) GetMaxEnergyRangeFromUncore() (uint64, error) {
	return getMaxEnergyRange(uncoreEvent)
}

func (r *PowerSysfs) GetMaxEnergyRangeFromPackage() (uint64, error) {
	return getMaxEnergyRange(packageEvent)
}
