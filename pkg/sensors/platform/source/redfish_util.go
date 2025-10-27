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
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/casparwackerle/tycho-energy/pkg/config"
	"k8s.io/klog/v2"
)

// getRedfishModel performs a single HTTP GET to the given endpoint and decodes JSON
// into the provided model. It tolerates unknown fields and uses a bounded timeout.
// NOTE: If you decide to capture headers (ETag/Date) later, this function can be
// refactored to return http.Header without changing the higher-level logic.
func getRedfishModel(access RedfishAccessInfo, endpoint string, model interface{}) (http.Header, error) {
	username := access.Username
	password := access.Password
	host := access.Host

	// Transport with optional TLS verify skip (for lab hardware / self-signed certs)
	transport := &http.Transport{}
	if config.GetRedfishSkipSSLVerify() {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	client := &http.Client{
		Transport: transport,
		// Hard cap to avoid hung sockets even if caller forgets a context timeout.
		Timeout: 30 * time.Second,
	}

	url := host + endpoint
	req, err := http.NewRequest("GET", url, http.NoBody)
	if err != nil {
		return nil, err
	}

	// Per-call timeout via context (in addition to client-level timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req = req.WithContext(ctx)

	// Headers
	req.Header.Add("OData-Version", "4.0")
	req.Header.Add("Accept", "application/json")
	req.Header.Set("User-Agent", "kepler") // keep for compatibility
	req.Header.Set("Connection", "keep-alive")
	req.SetBasicAuth(username, password)

	// Execute request
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		// Drain to allow connection reuse, then close.
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			klog.V(0).Infof("Failed to discard response body: %v", err)
		}
		_ = resp.Body.Close()
	}()

	// Status check
	if resp.StatusCode != http.StatusOK {
		return resp.Header, fmt.Errorf("server returned status: %v", resp.Status)
	}

	// Decode JSON strictly (unknown fields logged at V(6) and ignored)
	dec := json.NewDecoder(resp.Body)
	dec.DisallowUnknownFields()

	var returnErr error
	if err := dec.Decode(model); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			// Ignore unknown fields but surface them at high verbosity for diagnostics.
			fieldName := strings.TrimPrefix(err.Error(), "json: unknown field ")
			klog.V(6).Infof("Redfish response contains unknown field %s at %s", fieldName, endpoint)
		} else {
			returnErr = err
			klog.V(5).Infof("Failed to decode response at %s: %v", endpoint, err)
		}
	}

	return resp.Header, returnErr
}

func getRedfishChassis(access RedfishAccessInfo) (*RedfishChassisModel, error) {
	var chassis RedfishChassisModel
	if _, err := getRedfishModel(access, "/redfish/v1/Chassis", &chassis); err != nil {
		klog.V(1).Infof("Failed to get chassis: %v", err)
		return nil, err
	}
	return &chassis, nil
}

func getRedfishPower(access RedfishAccessInfo, chassis string) (*RedfishPowerModel, http.Header, error) {
	var power RedfishPowerModel
	hdr, err := getRedfishModel(access, "/redfish/v1/Chassis/"+chassis+"/Power#/PowerControl", &power)
	if err != nil {
		klog.V(1).Infof("Failed to get power: %v", err)
		return nil, hdr, err
	}
	return &power, hdr, nil
}
