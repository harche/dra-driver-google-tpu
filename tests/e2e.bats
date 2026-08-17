#!/usr/bin/env bats

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

load 'test_helper/bats-support/load'
load 'test_helper/bats-assert/load'

NAMESPACE="dra-driver-google-tpu"
DAEMONSET="dra-driver-google-tpu-kubeletplugin"
PLUGIN_CONTAINER="tpu-dra-plugin"

# The kind cluster has one worker holding the fake TPU devices and a control
# plane with none, so both the active and the idle path are covered.
tpu_node() { echo "${CLUSTER_NAME}-worker"; }
plain_node() { echo "${CLUSTER_NAME}-control-plane"; }

plugin_pod_on() {
  kubectl get pods --namespace "$NAMESPACE" --field-selector "spec.nodeName=$1" \
    -o jsonpath='{.items[0].metadata.name}'
}

# The devices of a ResourceSlice come from a map, so they are sorted here to
# compare them.
slice_devices() {
  kubectl get resourceslice -o jsonpath="{.items[?(@.spec.nodeName=='$1')].spec.devices[*].name}" |
    tr ' ' '\n' | sort | xargs
}

# device_attribute NODE ATTRIBUTE TYPE, the property the driver discovered as a
# claim sees it.
device_attribute() {
  kubectl get resourceslice \
    -o jsonpath="{.items[?(@.spec.nodeName=='$1')].spec.devices[0].attributes.$2.$3}"
}

# slice_uuids NODE lists the uuid attribute of every device on the node, sorted.
slice_uuids() {
  kubectl get resourceslice \
    -o jsonpath="{.items[?(@.spec.nodeName=='$1')].spec.devices[*].attributes.uuid.string}" |
    tr ' ' '\n' | sort | xargs
}

# distinct_uuids NODE counts the distinct device uuids on the node; the uuid is
# derived per chip, so it equals the chip count.
distinct_uuids() {
  slice_uuids "$1" | tr ' ' '\n' | sort -u | grep -c .
}

# tpu_prefixed_uuids NODE counts the device uuids on the node that keep the
# "tpu-" prefix of the published format.
tpu_prefixed_uuids() {
  slice_uuids "$1" | tr ' ' '\n' | grep -c '^tpu-'
}

# assert_eventually WANT COMMAND... retries until the command prints WANT, then
# asserts one last time so that a failure reports both values.
assert_eventually() {
  local want=$1
  shift
  for _ in $(seq 30); do
    [[ "$("$@" 2>/dev/null)" == "$want" ]] && break
    sleep 2
  done
  run "$@"
  assert_output "$want"
}

teardown() {
  if [[ -z "${BATS_TEST_COMPLETED:-}" ]]; then
    dump_debug_info
  fi
}

dump_debug_info() {
  echo "--- Test failed. Dumping debug information ---"
  kubectl get nodes --show-labels
  kubectl get pods --namespace "$NAMESPACE" -o wide
  for pod in $(kubectl get pods --namespace "$NAMESPACE" -o name); do
    echo "--- $pod ---"
    kubectl logs --namespace "$NAMESPACE" "$pod" -c "$PLUGIN_CONTAINER" --tail=50 || true
  done
  kubectl get resourceslice -o yaml || true
  kubectl get resourceclaim --all-namespaces -o yaml || true
  echo "--- End of debug information ---"
}

@test "the plugin runs on every node of the cluster" {
  local nodes
  nodes=$(kubectl get nodes -o name | wc -l)

  run kubectl get daemonset --namespace "$NAMESPACE" "$DAEMONSET" -o jsonpath='{.status.numberReady}'
  assert_success
  assert_output "$nodes"
}

@test "the plugin stays idle on a node without TPU" {
  local pod
  pod=$(plugin_pod_on "$(plain_node)")
  assert [ -n "$pod" ]

  run kubectl get pod --namespace "$NAMESPACE" "$pod" \
    -o jsonpath="{.status.containerStatuses[?(@.name=='$PLUGIN_CONTAINER')].restartCount}"
  assert_success
  assert_output "0"

  run kubectl logs --namespace "$NAMESPACE" "$pod" -c "$PLUGIN_CONTAINER"
  assert_success
  assert_output --partial "No TPU detected"

  # An idle node must not advertise devices.
  assert_eventually "" slice_devices "$(plain_node)"
}

@test "the chips are discovered from the hardware, not from the labels" {
  # The kind node is labeled with the accelerator type only; the chip count and
  # the single host topology come from the devices themselves.
  assert_eventually "4" device_attribute "$(tpu_node)" "chipCount" "int"
  assert_eventually "2x2" device_attribute "$(tpu_node)" "topology" "string"
}

@test "a resourceslice is published for the TPU node" {
  assert_eventually "accel0 accel1 accel2 accel3" slice_devices "$(tpu_node)"

  run device_attribute "$(tpu_node)" "tpuGen" "string"
  assert_success
  assert_output "v4"

  run device_attribute "$(tpu_node)" "accelerator" "string"
  assert_success
  assert_output "tpu-v4-podslice"
}

@test "every chip on the node gets its own uuid" {
  # Each chip publishes its own uuid, so the node's four chips have four distinct
  # values, each keeping the tpu- prefixed format.
  assert_eventually "accel0 accel1 accel2 accel3" slice_devices "$(tpu_node)"
  assert_eventually "4" distinct_uuids "$(tpu_node)"
  assert_eventually "4" tpu_prefixed_uuids "$(tpu_node)"
}

@test "the driver does not write to the node" {
  # Selection happens on the device attributes of the claim, the driver has no
  # business labelling the node and no permission to do so.
  run kubectl get node "$(tpu_node)" -o go-template='{{range $k, $v := .metadata.labels}}{{$k}}{{"\n"}}{{end}}'
  assert_success
  refute_output --partial "tpu.google.com/chip-count"
  refute_output --partial "tpu.google.com/topology"

  local account
  account=$(kubectl get daemonset --namespace "$NAMESPACE" "$DAEMONSET" \
    -o jsonpath='{.spec.template.spec.serviceAccountName}')
  if kubectl auth can-i patch nodes --quiet \
      --as "system:serviceaccount:$NAMESPACE:$account" 2>/dev/null; then
    fail "the driver must not be allowed to patch nodes"
  fi
}

@test "a workload claiming TPUs gets the devices and the environment" {
  kubectl apply -f "$PROJECT_DIR/demo/specs/tpu-test.yaml"
  kubectl wait --timeout=120s --for=condition=ready pods --namespace tpu-test -l app=pod

  run kubectl exec --namespace tpu-test tpu-pod0 -- sh -c 'ls /dev | grep -c accel'
  assert_success
  assert_output "4"

  run kubectl exec --namespace tpu-test tpu-pod0 -- printenv TPU_ACCELERATOR_TYPE
  assert_success
  assert_output "v4-8"

  run kubectl exec --namespace tpu-test tpu-pod0 -- printenv TPU_TOPOLOGY
  assert_success
  assert_output "2x2"

  kubectl delete -f "$PROJECT_DIR/demo/specs/tpu-test.yaml" --wait --timeout=90s
}

# Kept last: it takes the accelerator label away from the node.
@test "the plugin works on a node with no TPU label at all" {
  kubectl label node "$(tpu_node)" "tpu.google.com/accelerator-"

  "$PROJECT_DIR/demo/scripts/install-dra-driver.sh" \
    --set kubeletPlugin.containers.networkOptimizer.enabled=false \
    --set kubeletPlugin.containers.logCollector.enabled=false \
    --set kubeletPlugin.containers.vbarControlAgent.enabled=false \
    --set kubeletPlugin.tolerations[1].operator=Exists \
    --set kubeletPlugin.tpu.accelerator=tpu-v4-podslice
  kubectl rollout status "daemonset/$DAEMONSET" --namespace "$NAMESPACE" --timeout=180s

  # Nothing in the cluster describes the TPU any more.
  assert_eventually "tpu-v4-podslice" device_attribute "$(tpu_node)" "accelerator" "string"
  assert_eventually "4" device_attribute "$(tpu_node)" "chipCount" "int"
  assert_eventually "accel0 accel1 accel2 accel3" slice_devices "$(tpu_node)"

  # A cluster wide accelerator type must not disturb the nodes without a TPU.
  run kubectl get daemonset --namespace "$NAMESPACE" "$DAEMONSET" -o jsonpath='{.status.numberReady}'
  assert_success
  assert_output "$(kubectl get nodes -o name | wc -l)"
}
