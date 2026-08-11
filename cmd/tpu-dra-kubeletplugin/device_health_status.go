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
	"slices"
	"sync"
	"time"

	"k8s.io/dynamic-resource-allocation/kubeletplugin"
	"k8s.io/klog/v2"
)

// This file implements device health reporting (KEP-4680) for the TPU kubelet
// plugin. It bridges the TPU health checker -- whose device-file probing
// already drives ResourceSlice (re-)publication -- to the version-neutral
// [kubeletplugin.DRAPlugin] WatchHealthStatus API, so that the health of
// allocated TPUs surfaces in
// pod.status.containerStatuses[].allocatedResourcesStatus.

// healthBroadcaster fans TPU device health notifications out to all pending
// WatchHealthStatus subscriptions, each of which builds a fresh health
// snapshot when woken. The TPU health checker broadcasts after
// every probing pass, changed or not, so each broadcast is both an update and
// the heartbeat which keeps the kubelet's health data fresh: the kubelet
// reports device health as unknown when it is not refreshed within each
// device's health check timeout (30 seconds by default), and the checker's
// probing interval is well within that. A wedged checker stops broadcasting
// and the kubelet correctly decays the devices' health to unknown instead of
// trusting a stale report.
type healthBroadcaster struct {
	state    *DeviceState
	poolName string

	mu          sync.RWMutex
	subscribers []chan struct{}
}

func newHealthBroadcaster(state *DeviceState, poolName string) *healthBroadcaster {
	return &healthBroadcaster{
		state:    state,
		poolName: poolName,
	}
}

// buildReport snapshots the health of all TPU devices. A device whose device
// file is present is healthy; a device which fell off the dev filesystem is
// unhealthy (see TPUHealthChecker).
func (b *healthBroadcaster) buildReport() kubeletplugin.DeviceHealthReport {
	b.state.Lock()
	defer b.state.Unlock()

	var devices []kubeletplugin.DeviceHealth
	for name, dev := range b.state.allocatable {
		health := kubeletplugin.HealthStatusHealthy
		message := "TPU device file is present"
		if !dev.allocatable {
			health = kubeletplugin.HealthStatusUnhealthy
			message = "TPU device file is missing"
		}
		devices = append(devices, kubeletplugin.DeviceHealth{
			PoolName:    b.poolName,
			DeviceName:  name,
			Health:      health,
			LastUpdated: time.Now(),
			Message:     message,
		})
	}
	return kubeletplugin.DeviceHealthReport{Devices: devices}
}

// broadcast wakes all pending WatchHealthStatus subscriptions to send a fresh
// health report. The subscriber channels have capacity one and the send is
// non-blocking, so notifications coalesce: however many probing passes
// complete while a subscriber is busy, it wakes once and builds the latest
// snapshot at send time. No health data is queued here; the current state
// lives in DeviceState and this is only a notification.
func (b *healthBroadcaster) broadcast() {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, subscriber := range b.subscribers {
		select {
		case subscriber <- struct{}{}:
		default:
		}
	}
}

func (b *healthBroadcaster) subscribe() chan struct{} {
	// A capacity-one notification channel: notifications coalesce, and the
	// report is built fresh at send time (see broadcast).
	subscriber := make(chan struct{}, 1)
	b.mu.Lock()
	b.subscribers = append(b.subscribers, subscriber)
	b.mu.Unlock()
	return subscriber
}

func (b *healthBroadcaster) unsubscribe(subscriber chan struct{}) {
	b.mu.Lock()
	for i, s := range b.subscribers {
		if s == subscriber {
			b.subscribers = slices.Delete(b.subscribers, i, i+1)
			break
		}
	}
	b.mu.Unlock()
}

// WatchHealthStatus implements [kubeletplugin.DRAPlugin]. The kubeletplugin
// helper calls it whenever the kubelet subscribes to device health updates and
// takes care of translating the reports into the DRAResourceHealth gRPC API
// version that the kubelet supports.
func (d *driver) WatchHealthStatus(ctx context.Context, reports chan<- kubeletplugin.DeviceHealthReport) error {
	klog.V(4).Info("Kubelet subscribed to device health updates")

	subscriber := d.health.subscribe()
	defer func() {
		d.health.unsubscribe(subscriber)
		klog.V(4).Info("Kubelet unsubscribed from device health updates")
	}()

	select {
	case <-ctx.Done():
		return nil
	case reports <- d.health.buildReport():
	}

	// Send a fresh snapshot on every notification. Periodic resends (required
	// because the kubelet treats health data older than the health check
	// timeout as stale) are not generated here: the notifications are driven
	// by the TPU health checker's probing passes, so that a resend is
	// evidence the checker is actually alive.
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-subscriber:
			select {
			case <-ctx.Done():
				return nil
			case reports <- d.health.buildReport():
			}
		}
	}
}
