/*
Copyright 2026 The Kubernetes Authors.

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
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const foreignDriverName = "example.com/nic"

func tpuDeviceState(chipCount int, deviceNames ...string) *DeviceState {
	allocatable := AllocatableDevices{}
	for i, name := range deviceNames {
		allocatable[name] = &AllocatableDevice{
			UUID:        name,
			name:        name,
			index:       i,
			allocatable: true,
		}
	}
	return &DeviceState{
		cdi:         &CDIHandler{},
		allocatable: allocatable,
		tm: &tpuManager{
			DevDirectory: "/dev",
			devices:      allocatable,
			tpuChipCount: chipCount,
		},
	}
}

func claimWithResults(results ...resourceapi.DeviceRequestAllocationResult) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim", Namespace: "default", UID: "claim-uid"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{Results: results},
			},
		},
	}
}

func result(driver, pool, device, request string) resourceapi.DeviceRequestAllocationResult {
	return resourceapi.DeviceRequestAllocationResult{
		Driver:  driver,
		Pool:    pool,
		Device:  device,
		Request: request,
	}
}

// A ResourceClaim can be satisfied by more than one driver. The TPU plugin must
// apply its full-chip count, its allocatable lookup and its CDI edits to the
// results it owns, and leave the rest alone.
func TestPrepareDevicesIgnoresForeignAllocationResults(t *testing.T) {
	tests := []struct {
		name        string
		state       *DeviceState
		results     []resourceapi.DeviceRequestAllocationResult
		wantErr     bool
		wantDevices []string
	}{
		{
			name:  "all results owned by this driver",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "tpus"),
			},
			wantDevices: []string{"tpu0", "tpu1"},
		},
		{
			// Without the ownership filter the foreign entry pushes the result
			// count past tpuChipCount, so a complete TPU allocation is rejected
			// because the workload also asked for a NIC.
			name:  "full chip set alongside a device owned by another driver",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "tpus"),
				result(foreignDriverName, "nic-pool", "nic0", "nics"),
			},
			wantDevices: []string{"tpu0", "tpu1"},
		},
		{
			// Device names are only unique within a driver's pools. A foreign
			// device that happens to be called "tpu0" must not be resolved
			// against this driver's allocatable map or prepared as a TPU.
			name:  "another driver uses a device name that also exists here",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(DriverName, "tpu-pool", "tpu1", "tpus"),
				result(foreignDriverName, "nic-pool", "tpu0", "nics"),
			},
			wantDevices: []string{"tpu0", "tpu1"},
		},
		{
			// The partial-allocation error still has to fire. A foreign result
			// must not stand in for a missing TPU and make the count add up.
			name:  "partial TPU allocation padded by a foreign result",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(DriverName, "tpu-pool", "tpu0", "tpus"),
				result(foreignDriverName, "nic-pool", "nic0", "nics"),
			},
			wantErr: true,
		},
		{
			name:  "only foreign results",
			state: tpuDeviceState(2, "tpu0", "tpu1"),
			results: []resourceapi.DeviceRequestAllocationResult{
				result(foreignDriverName, "nic-pool", "nic0", "nics"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := tt.state.prepareDevices(claimWithResults(tt.results...))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("prepareDevices() = %d devices, want an error", len(prepared))
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareDevices() error = %v, want nil", err)
			}
			if len(prepared) != len(tt.wantDevices) {
				t.Fatalf("prepareDevices() prepared %d devices, want %d", len(prepared), len(tt.wantDevices))
			}
			got := make(map[string]bool, len(prepared))
			for _, device := range prepared {
				got[device.DeviceName] = true
			}
			for _, want := range tt.wantDevices {
				if !got[want] {
					t.Errorf("prepareDevices() did not prepare %q; prepared %v", want, got)
				}
			}
		})
	}
}
