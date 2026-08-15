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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"k8s.io/klog/v2"
)

const (
	// googlePCIVendorID is the PCI vendor identifier of Google, it is used to
	// tell TPUs apart from any other device bound to the vfio-pci driver.
	googlePCIVendorID = "0x1ae0"

	iommuGroupsPath = "sys/kernel/iommu_groups"
)

var (
	accelDeviceRegex = regexp.MustCompile(`^accel[0-9]+$`)
	vfioGroupRegex   = regexp.MustCompile(`^[0-9]+$`)
)

// tpuHardware describes the TPU chips found on the local host.
type tpuHardware struct {
	// devDirectory holds the character device of every chip.
	devDirectory string
	// deviceRegex matches the chip device names inside devDirectory.
	deviceRegex *regexp.Regexp
	chipCount   int
}

// probeTPUHardware looks for TPU chips on the local host so that the driver
// does not have to trust the node labels. TPU generations up to v4 expose one
// /dev/accel<n> device per chip, later ones are bound to vfio-pci and expose
// one /dev/vfio/<iommu group> device per chip. root is the host root, it is
// only meant to be overridden by tests.
func probeTPUHardware(root string) (*tpuHardware, error) {
	if count := countAccelDevices(filepath.Join(root, devDirectory)); count > 0 {
		return &tpuHardware{
			devDirectory: devDirectory,
			deviceRegex:  accelDeviceRegex,
			chipCount:    count,
		}, nil
	}

	if count := countVfioTPUDevices(root); count > 0 {
		return &tpuHardware{
			devDirectory: devDirectoryVfio,
			deviceRegex:  vfioGroupRegex,
			chipCount:    count,
		}, nil
	}

	return nil, fmt.Errorf("no TPU device found in %s or %s: %w", devDirectory, devDirectoryVfio, errNoTPUDetected)
}

// countAccelDevices counts the /dev/accel<n> devices exposed by the TPU driver.
func countAccelDevices(devDir string) int {
	entries, err := os.ReadDir(devDir)
	if err != nil {
		klog.V(3).Infof("Cannot read %s: %v", devDir, err)
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && accelDeviceRegex.MatchString(entry.Name()) {
			count++
		}
	}
	return count
}

// countVfioTPUDevices counts the /dev/vfio/<iommu group> devices whose IOMMU
// group contains a Google PCI device. Other devices bound to vfio-pci also show
// up in /dev/vfio and must not be counted as TPU chips.
func countVfioTPUDevices(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, devDirectoryVfio))
	if err != nil {
		klog.V(3).Infof("Cannot read %s: %v", devDirectoryVfio, err)
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !vfioGroupRegex.MatchString(entry.Name()) {
			continue
		}
		if isGoogleIOMMUGroup(root, entry.Name()) {
			count++
		}
	}
	return count
}

// isGoogleIOMMUGroup reports whether any PCI device of the IOMMU group is made
// by Google.
func isGoogleIOMMUGroup(root, group string) bool {
	devices, err := os.ReadDir(filepath.Join(root, iommuGroupsPath, group, "devices"))
	if err != nil {
		klog.V(3).Infof("Cannot read the devices of IOMMU group %s: %v", group, err)
		return false
	}
	for _, device := range devices {
		vendor, err := os.ReadFile(filepath.Join(root, iommuGroupsPath, group, "devices", device.Name(), "vendor"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(vendor)) == googlePCIVendorID {
			return true
		}
	}
	return false
}
