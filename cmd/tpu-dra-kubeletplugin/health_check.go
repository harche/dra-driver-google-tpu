/*
 * Copyright The Kubernetes Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"os"
	"path/filepath"
	"time"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
)

const (
	tpuCheckInterval = 10 * time.Second
)

var (
	Healthy   = resourceapi.DeviceAttribute{StringValue: ptr.To("Healthy")}
	Unhealthy = resourceapi.DeviceAttribute{StringValue: ptr.To("Unhealthy")}
)

// TPUHealthChecker checks the health of TPUs. Currently the health checker is limit
// to just checking for the existence of device in the `dev` directory.
type TPUHealthChecker struct {
	devices      map[string]AllocatableDevice
	devDirectory string
	state        *DeviceState
	stop         chan bool
	tpuGen       string
	// broadcaster notifies WatchHealthStatus subscribers after every
	// probing pass so they send a fresh health report (see
	// device_health_status.go).
	broadcaster *healthBroadcaster
}

// NewTPUHealthChecker returns a TPUHealthChecker object for a given device name.
func NewTPUHealthChecker(devices AllocatableDevices, state *DeviceState, devDirectory string, tpuGen string, broadcaster *healthBroadcaster) *TPUHealthChecker {
	hc := &TPUHealthChecker{
		devices:      make(map[string]AllocatableDevice),
		state:        state,
		devDirectory: devDirectory,
		stop:         make(chan bool),
		tpuGen:       tpuGen,
		broadcaster:  broadcaster,
	}

	// Cloning the device map to avoid interfering with the device manager
	for id, d := range devices {
		hc.devices[id] = *d
	}

	return hc
}

// Start creates a goRoutine that monitors the devDir for changes.
func (hc *TPUHealthChecker) Start() error {
	klog.Info("Starting TPU Health Checker")

	for name, device := range hc.devices {
		klog.Infof("Healthchecker receives device %s, device %v+", name, device)
	}

	go func() {
		if err := hc.monitorDevDir(); err != nil {
			klog.Errorf("TPUHealthChecker monitorDevDir error: %v", err)
		}
	}()

	return nil
}

func (hc *TPUHealthChecker) deviceExists(deviceId string) (bool, error) {
	deviceIDs := []string{deviceId}

	for _, id := range deviceIDs {
		if _, err := os.Stat(filepath.Join(hc.devDirectory, id)); err == nil {
			continue
		} else if os.IsNotExist(err) {
			return false, nil
		} else {
			klog.Errorf("deviceExists check unexpected error: %v", err)
			return false, nil
		}
	}

	return true, nil
}

// monitorDevDir monitors the dev/ directory to see if all the TPU devices are present.
func (hc *TPUHealthChecker) monitorDevDir() error {
	ticker := time.NewTicker(tpuCheckInterval)
	for {
		select {
		case <-hc.stop:
			close(hc.stop)
			ticker.Stop()
			return nil
		case <-ticker.C:
			for id, d := range hc.devices {
				val, _ := hc.deviceExists(id)
				if val && !d.allocatable {
					klog.Infof("Device %s is now healthy.", id)
					d.allocatable = true
					hc.devices[id] = d
					if err := hc.state.UpdateHealth(id, true); err != nil {
						klog.Errorf("failed to update health for device %s: %v", id, err)
					}
				} else if !val && d.allocatable {
					klog.Warningf("Device %s is now unhealthy.", id)
					d.allocatable = false
					hc.devices[id] = d
					if err := hc.state.UpdateHealth(id, false); err != nil {
						klog.Errorf("failed to update health for device %s: %v", id, err)
					}
				}
			}
			// Report to the kubelet on every pass, changed or not: the send
			// is the heartbeat which keeps the kubelet's health data fresh
			// (KEP-4680), and vouches only for what this pass verified.
			if hc.broadcaster != nil {
				hc.broadcaster.broadcast()
			}
		}
	}
}

// Stop the listening go routine.
func (hc *TPUHealthChecker) Stop() {
	hc.stop <- true
	<-hc.stop
}
