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
	// googlePCIVendorID is the PCI vendor identifier of Google. Vendor alone
	// does not mean TPU: Google also ships other devices under it, e.g. the
	// gVNIC network device (0x0042) or its NVMe controller (0x001f), see the
	// registry at https://admin.pci-ids.ucw.cz/read/PC/1ae0. Those are excluded
	// by knownNonTPUGoogleDeviceIDs below; the device id otherwise only decides
	// the generation (tpuGenerationFromPCIIDs), not whether it is a TPU, so an
	// unlisted future chip is still counted, just with an unknown generation.
	googlePCIVendorID = "0x1ae0"

	iommuGroupsPath = "sys/kernel/iommu_groups"
)

var (
	accelDeviceRegex = regexp.MustCompile(`^accel[0-9]+$`)
	vfioGroupRegex   = regexp.MustCompile(`^[0-9]+$`)

	// knownNonTPUGoogleDeviceIDs lists the PCI device ids Google ships under
	// its vendor id for devices that are not TPU chips, so that one of them
	// bound to vfio-pci is not mistaken for a TPU. Sourced from
	// https://admin.pci-ids.ucw.cz/read/PC/1ae0; add to this list if Google
	// registers a new non-TPU device there.
	knownNonTPUGoogleDeviceIDs = map[string]bool{
		"0x001f": true, // NVMe device
		"0x0042": true, // Compute Engine Virtual Ethernet (gVNIC)
		"0xabcd": true, // Airbrush Combined Paintbox IPU/Oscar Edge TPU (Pixel Neural Core)
	}
)

// tpuHardware describes the TPU chips found on the local host.
type tpuHardware struct {
	// devDirectory holds the character device of every chip.
	devDirectory string
	// deviceRegex matches the chip device names inside devDirectory.
	deviceRegex *regexp.Regexp
	chipCount   int
	// generation is the TPU generation read from the PCI ids of the vfio
	// devices, in the vocabulary AcceleratorGen (see util.go) uses. Empty when
	// the devices are not bound to vfio-pci (generations up to v4 use
	// /dev/accel instead) or their generation is not one
	// tpuGenerationFromPCIIDs recognizes yet.
	generation string
}

// tpuGenerationFamily is a TPU generation family this driver supports: every
// chip of a family shares one PCI device id (and, when that id is reused
// across families as v2 and v3 do, a subsystem id too).
type tpuGenerationFamily struct {
	pciDeviceID string
	// pciSubsystemID disambiguates a pciDeviceID reused by more than one
	// family. Leave empty when the device id alone is enough.
	pciSubsystemID string
	// generations lists every string AcceleratorGen (see util.go) can return
	// for a chip of this family. More than one when the same silicon takes
	// more than one form factor the PCI id cannot tell apart, e.g. v4 and
	// v4lite, or v5lite (v5e) and v5litepod: tpuGenerationFromPCIIDs then
	// resolves to the first, which completeLabelsFromHardware (util.go) only
	// uses to double check a label another source already provided, never to
	// pick between the two on its own.
	generations []string
}

// tpuGenerationFamilies is the single source of truth for the TPU generations
// this driver knows: it both tells hardware detection how to recognize a
// generation from its PCI ids (tpuGenerationFromPCIIDs) and tells
// AcceleratorGen (util.go) which generation strings are valid
// (isValidTPUGeneration), so a new generation only has to be added here.
//
// To add one: find its PCI device id under
// /sys/bus/pci/devices/<addr>/device on a host with the chip, or in Google's
// own tpu-info tool, the closest thing to an authoritative source for these
// ids: https://github.com/AI-Hypercomputer/cloud-accelerator-diagnostics/blob/main/tpu_info/tpu_info/device.py
var tpuGenerationFamilies = []tpuGenerationFamily{
	// v2 shares this device id (subsystem 0x004e) but is not supported.
	{pciDeviceID: "0x0027", pciSubsystemID: "0x004f", generations: []string{"v3"}},
	{pciDeviceID: "0x005e", generations: []string{"v4", "v4lite"}},
	{pciDeviceID: "0x0063", generations: []string{"v5lite", "v5litepod"}}, // v5e
	{pciDeviceID: "0x0062", generations: []string{"v5p"}},
	{pciDeviceID: "0x006f", generations: []string{"v6e"}},
	// {pciDeviceID: "0x0076", generations: []string{"v7x"}}, // Ironwood, not yet supported by this driver.
}

// isValidTPUGeneration reports whether v is a generation AcceleratorGen (see
// util.go) can return, i.e. one listed in tpuGenerationFamilies.
func isValidTPUGeneration(v string) bool {
	for _, family := range tpuGenerationFamilies {
		for _, generation := range family.generations {
			if generation == v {
				return true
			}
		}
	}
	return false
}

// tpuGenerationFromPCIIDs returns the TPU generation matching the PCI device
// id and subsystem id of a Google TPU chip, in the vocabulary AcceleratorGen
// (see util.go) uses, or false if this driver does not recognize the ids yet.
// A false here does not mean the device is not a TPU, see
// knownNonTPUGoogleDeviceIDs for the devices this driver knows are not.
func tpuGenerationFromPCIIDs(device, subsystem string) (string, bool) {
	for _, family := range tpuGenerationFamilies {
		if family.pciDeviceID != device {
			continue
		}
		if family.pciSubsystemID != "" && family.pciSubsystemID != subsystem {
			continue
		}
		return family.generations[0], true
	}
	return "", false
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

	if count, generation := countVfioTPUDevices(root); count > 0 {
		return &tpuHardware{
			devDirectory: devDirectoryVfio,
			deviceRegex:  vfioGroupRegex,
			chipCount:    count,
			generation:   generation,
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
// group contains a Google TPU chip, and returns the generation of those chips
// when every one of them agrees on it (an empty generation is a valid
// agreement: it means none of them could be told apart, see
// tpuGenerationOfIOMMUGroup). Other devices bound to vfio-pci also show up in
// /dev/vfio and must not be counted as TPU chips.
func countVfioTPUDevices(root string) (int, string) {
	entries, err := os.ReadDir(filepath.Join(root, devDirectoryVfio))
	if err != nil {
		klog.V(3).Infof("Cannot read %s: %v", devDirectoryVfio, err)
		return 0, ""
	}
	count := 0
	var generation string
	sawGeneration := false
	mixed := false
	for _, entry := range entries {
		if entry.IsDir() || !vfioGroupRegex.MatchString(entry.Name()) {
			continue
		}
		gen, ok := tpuGenerationOfIOMMUGroup(root, entry.Name())
		if !ok {
			continue
		}
		count++
		switch {
		case !sawGeneration:
			generation = gen
			sawGeneration = true
		case generation != gen:
			mixed = true
		}
	}
	if mixed {
		klog.Warningf("IOMMU groups in %s report more than one TPU generation, ignoring all of them", devDirectoryVfio)
		return count, ""
	}
	return count, generation
}

// tpuGenerationOfIOMMUGroup reports whether the IOMMU group holds a Google TPU
// chip and, if this driver recognizes its PCI ids, its generation. A device
// with Google's vendor id that is not in knownNonTPUGoogleDeviceIDs is assumed
// to be a TPU of an unrecognized generation rather than ignored, so that a
// chip this driver predates is still counted; completeLabelsFromHardware (see
// util.go) then falls back to the usual sources for its accelerator string.
func tpuGenerationOfIOMMUGroup(root, group string) (string, bool) {
	devices, err := os.ReadDir(filepath.Join(root, iommuGroupsPath, group, "devices"))
	if err != nil {
		klog.V(3).Infof("Cannot read the devices of IOMMU group %s: %v", group, err)
		return "", false
	}
	for _, device := range devices {
		devicePath := filepath.Join(root, iommuGroupsPath, group, "devices", device.Name())
		vendor, err := os.ReadFile(filepath.Join(devicePath, "vendor"))
		// Sysfs always reports lowercase hex, EqualFold is just future proofing.
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(vendor)), googlePCIVendorID) {
			continue
		}
		deviceIDRaw, err := os.ReadFile(filepath.Join(devicePath, "device"))
		if err != nil {
			continue
		}
		deviceID := strings.TrimSpace(string(deviceIDRaw))
		if knownNonTPUGoogleDeviceIDs[deviceID] {
			continue
		}
		// Absent on devices that do not set a subsystem id, which is fine:
		// tpuGenerationFromPCIIDs only needs it to disambiguate a reused id.
		subsystem, _ := os.ReadFile(filepath.Join(devicePath, "subsystem_device"))
		gen, ok := tpuGenerationFromPCIIDs(deviceID, strings.TrimSpace(string(subsystem)))
		if !ok {
			klog.V(3).Infof("Google PCI device %s in IOMMU group %s has an unrecognized device id %s, assuming it is a TPU of an unknown generation; add it to tpuGenerationFromPCIIDs if it is not", device.Name(), group, deviceID)
		}
		return gen, true
	}
	return "", false
}
