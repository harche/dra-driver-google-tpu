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
	"context"
	"testing"
	"time"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
)

func newHealthTestDriver() *driver {
	state := &DeviceState{
		allocatable: AllocatableDevices{
			"tpu-0": &AllocatableDevice{name: "tpu-0", allocatable: true},
			"tpu-1": &AllocatableDevice{name: "tpu-1", allocatable: false},
		},
	}
	return &driver{
		deviceState: state,
		health:      newHealthBroadcaster(state, "node1"),
	}
}

func healthByDevice(report kubeletplugin.DeviceHealthReport) map[string]kubeletplugin.DeviceHealth {
	byDevice := make(map[string]kubeletplugin.DeviceHealth)
	for _, dev := range report.Devices {
		byDevice[dev.DeviceName] = dev
	}
	return byDevice
}

// TestBuildReport verifies that a report covers all devices, mapping a
// present device file to healthy and a missing one to unhealthy.
func TestBuildReport(t *testing.T) {
	d := newHealthTestDriver()

	report := d.health.buildReport()
	if len(report.Devices) != 2 {
		t.Fatalf("expected 2 devices in report, got %d", len(report.Devices))
	}

	byDevice := healthByDevice(report)
	if got := byDevice["tpu-0"]; got.Health != kubeletplugin.HealthStatusHealthy {
		t.Errorf("tpu-0: expected %q, got %q", kubeletplugin.HealthStatusHealthy, got.Health)
	}
	if got := byDevice["tpu-1"]; got.Health != kubeletplugin.HealthStatusUnhealthy {
		t.Errorf("tpu-1: expected %q, got %q", kubeletplugin.HealthStatusUnhealthy, got.Health)
	}
	for name, dev := range byDevice {
		if dev.PoolName != "node1" {
			t.Errorf("%s: expected pool %q, got %q", name, "node1", dev.PoolName)
		}
		if dev.Message == "" {
			t.Errorf("%s: expected a message", name)
		}
	}
}

// TestWatchHealthStatus verifies the initial report on subscription, that
// broadcasts (the health checker's per-pass heartbeat) are forwarded, and
// that the watch ends cleanly on context cancellation.
func TestWatchHealthStatus(t *testing.T) {
	d := newHealthTestDriver()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reports := make(chan kubeletplugin.DeviceHealthReport)
	done := make(chan error, 1)
	go func() {
		done <- d.WatchHealthStatus(ctx, reports)
	}()

	// The initial snapshot is sent without any broadcast.
	select {
	case report := <-reports:
		if len(report.Devices) != 2 {
			t.Errorf("initial report: expected 2 devices, got %d", len(report.Devices))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for initial health report")
	}

	// A health transition followed by a broadcast reaches the subscriber.
	d.deviceState.Lock()
	d.deviceState.allocatable["tpu-1"].allocatable = true
	d.deviceState.Unlock()
	d.health.broadcast()

	select {
	case report := <-reports:
		if got := healthByDevice(report)["tpu-1"]; got.Health != kubeletplugin.HealthStatusHealthy {
			t.Errorf("tpu-1 after recovery: expected %q, got %q", kubeletplugin.HealthStatusHealthy, got.Health)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for broadcast health report")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("WatchHealthStatus returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for WatchHealthStatus to return")
	}

	if len(d.health.subscribers) != 0 {
		t.Errorf("expected subscriber to be removed, %d remain", len(d.health.subscribers))
	}
}
