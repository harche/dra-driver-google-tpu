/*
 * Copyright 2026 The Kubernetes Authors.
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
	"strings"
	"testing"
)

// fakeDevDir creates a directory containing the given fake TPU device files.
func fakeDevDir(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func enumerate(t *testing.T, nodeName, tpuGen string, devNames ...string) AllocatableDevices {
	t.Helper()
	t.Setenv("NODE_NAME", nodeName)
	tm := &tpuManager{
		tpuGen:       tpuGen,
		DevDirectory: fakeDevDir(t, devNames...),
		tpuChipCount: len(devNames),
	}
	devs, err := tm.enumerateAllPossibleTpuDevices()
	if err != nil {
		t.Fatalf("enumerateAllPossibleTpuDevices: %v", err)
	}
	if len(devs) != len(devNames) {
		t.Fatalf("got %d devices, want %d", len(devs), len(devNames))
	}
	return devs
}

func TestDeviceUUIDsUniquePerChip(t *testing.T) {
	// Enumeration recognizes the accel<n> devices outside the fixed vfio path, so
	// the fake node exposes four accel-named chips.
	devs := enumerate(t, "node-a", "v4", "accel0", "accel1", "accel2", "accel3")
	seen := map[string]string{}
	for name, d := range devs {
		if !strings.HasPrefix(d.UUID, "tpu-") || len(d.UUID) != len("tpu-")+36 {
			t.Errorf("device %s: malformed uuid %q", name, d.UUID)
		}
		if prev, dup := seen[d.UUID]; dup {
			t.Errorf("devices %s and %s share uuid %s", prev, name, d.UUID)
		}
		seen[d.UUID] = name
	}
}

func TestDeviceUUIDsStableAcrossRestarts(t *testing.T) {
	names := []string{"accel0", "accel1", "accel2", "accel3"}
	first := enumerate(t, "node-a", "v4", names...)
	second := enumerate(t, "node-a", "v4", names...)
	for name, d := range first {
		if got := second[name].UUID; got != d.UUID {
			t.Errorf("device %s: uuid changed across enumerations: %s -> %s", name, d.UUID, got)
		}
	}
}

func TestDeviceUUIDsDifferAcrossNodes(t *testing.T) {
	names := []string{"accel0", "accel1"}
	a := enumerate(t, "node-a", "v4", names...)
	b := enumerate(t, "node-b", "v4", names...)
	for name, d := range a {
		if b[name].UUID == d.UUID {
			t.Errorf("device %s: same uuid %s on different nodes", name, d.UUID)
		}
	}
}
