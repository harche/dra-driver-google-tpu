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
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeFiles creates every path relative to root with the given content.
func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("cannot create %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("cannot create %s: %v", path, err)
		}
	}
}

func TestProbeTPUHardware(t *testing.T) {
	tests := []struct {
		name           string
		files          map[string]string
		wantDevDir     string
		wantChips      int
		wantGeneration string
		wantNoTPUErr   bool
	}{
		{
			name: "accel devices",
			files: map[string]string{
				"dev/accel0": "",
				"dev/accel1": "",
				"dev/accel2": "",
				"dev/accel3": "",
				"dev/null":   "",
			},
			wantDevDir: devDirectory,
			wantChips:  4,
		},
		{
			name: "vfio devices of a recognized tpu generation",
			files: map[string]string{
				"dev/vfio/vfio": "",
				"dev/vfio/12":   "",
				"dev/vfio/13":   "",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/vendor": googlePCIVendorID + "\n",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/device": "0x006f\n", // v6e
				"sys/kernel/iommu_groups/13/devices/0000:00:06.0/vendor": googlePCIVendorID + "\n",
				"sys/kernel/iommu_groups/13/devices/0000:00:06.0/device": "0x006f\n", // v6e
			},
			wantDevDir:     devDirectoryVfio,
			wantChips:      2,
			wantGeneration: "v6e",
		},
		{
			name: "vfio device of an unrecognized generation is still counted",
			files: map[string]string{
				"dev/vfio/12": "",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/vendor": googlePCIVendorID + "\n",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/device": "0x00ff\n", // not in tpuGenerationFromPCIIDs yet
			},
			wantDevDir: devDirectoryVfio,
			wantChips:  1,
		},
		{
			name: "known non-tpu google device is ignored",
			files: map[string]string{
				"dev/vfio/12": "",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/vendor": googlePCIVendorID + "\n",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/device": "0x0042\n", // gVNIC
			},
			wantNoTPUErr: true,
		},
		{
			name: "vfio devices of another vendor are ignored",
			files: map[string]string{
				"dev/vfio/12": "",
				"sys/kernel/iommu_groups/12/devices/0000:00:05.0/vendor": "0x10de\n",
			},
			wantNoTPUErr: true,
		},
		{
			name:         "no device",
			files:        map[string]string{"dev/null": ""},
			wantNoTPUErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeFiles(t, root, tt.files)

			got, err := probeTPUHardware(root)
			if tt.wantNoTPUErr {
				if !errors.Is(err, errNoTPUDetected) {
					t.Fatalf("probeTPUHardware() error = %v, want %v", err, errNoTPUDetected)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.devDirectory != tt.wantDevDir {
				t.Errorf("probeTPUHardware() devDirectory = %s, want %s", got.devDirectory, tt.wantDevDir)
			}
			if got.chipCount != tt.wantChips {
				t.Errorf("probeTPUHardware() chipCount = %d, want %d", got.chipCount, tt.wantChips)
			}
			if got.generation != tt.wantGeneration {
				t.Errorf("probeTPUHardware() generation = %q, want %q", got.generation, tt.wantGeneration)
			}
		})
	}
}

func TestTpuGenerationFromPCIIDs(t *testing.T) {
	tests := []struct {
		name      string
		device    string
		subsystem string
		want      string
		wantOK    bool
	}{
		{name: "v3", device: "0x0027", subsystem: "0x004f", want: "v3", wantOK: true},
		{name: "v2 is not supported by this driver", device: "0x0027", subsystem: "0x004e", wantOK: false},
		{name: "v4", device: "0x005e", want: "v4", wantOK: true},
		{name: "v5e", device: "0x0063", want: "v5lite", wantOK: true},
		{name: "v5p", device: "0x0062", want: "v5p", wantOK: true},
		{name: "v6e", device: "0x006f", want: "v6e", wantOK: true},
		{name: "unrecognized device id", device: "0xffff", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tpuGenerationFromPCIIDs(tt.device, tt.subsystem)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("tpuGenerationFromPCIIDs(%q, %q) = (%q, %v), want (%q, %v)", tt.device, tt.subsystem, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSingleHostTopology(t *testing.T) {
	tests := []struct {
		chipCount int
		want      string
	}{
		{chipCount: 1, want: "1x1"},
		{chipCount: 2, want: "1x2"},
		{chipCount: 4, want: "2x2"},
		{chipCount: 8, want: "2x4"},
		{chipCount: 16, want: ""},
	}

	for _, tt := range tests {
		if got := singleHostTopology(tt.chipCount); got != tt.want {
			t.Errorf("singleHostTopology(%d) = %q, want %q", tt.chipCount, got, tt.want)
		}
	}
}

func TestCompleteLabelsFromHardware(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		hardware *tpuHardware
		want     map[string]string
	}{
		{
			name:     "unlabeled node",
			labels:   map[string]string{AcceleratorLabel: "tpu-v6e-slice"},
			hardware: &tpuHardware{devDirectory: devDirectoryVfio, chipCount: 8},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "8",
				TopologyLabel:         "2x4",
			},
		},
		{
			name: "labels win over the derived topology",
			labels: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "4x8",
			},
			hardware: &tpuHardware{devDirectory: devDirectoryVfio, chipCount: 4},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v6e-slice",
				AcceleratorCountLabel: "4",
				TopologyLabel:         "4x8",
			},
		},
		{
			name:     "unknown chip count keeps the topology unset",
			labels:   map[string]string{AcceleratorLabel: "tpu-v4-podslice"},
			hardware: &tpuHardware{devDirectory: devDirectory, chipCount: 3},
			want: map[string]string{
				AcceleratorLabel:      "tpu-v4-podslice",
				AcceleratorCountLabel: "3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completeLabelsFromHardware(tt.labels, tt.hardware)
			for key, want := range tt.want {
				if tt.labels[key] != want {
					t.Errorf("label %s = %q, want %q", key, tt.labels[key], want)
				}
			}
			if len(tt.labels) != len(tt.want) {
				t.Errorf("completeLabelsFromHardware() got = %v, want %v", tt.labels, tt.want)
			}
		})
	}
}
