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

# The chart ships a ValidatingAdmissionPolicy that restricts the driver
# ServiceAccount to ResourceSlices for its own node. The driver publishes via
# resource.k8s.io/v1, so the policy has to match that version or it never sees
# the traffic and the restriction is inert. These tests impersonate the
# ServiceAccount with the authentication.kubernetes.io/node-name extra that
# ServiceAccountTokenPodNodeInfo stamps on a pod token and check that own-node
# writes are allowed while cross-node ones and identities with no node
# association are denied. No TPU is needed, the policy runs at admission.

load 'test_helper/bats-support/load'
load 'test_helper/bats-assert/load'

NAMESPACE="dra-driver-google-tpu"
DAEMONSET="dra-driver-google-tpu-kubeletplugin"

# The kind cluster has one worker and a control plane; the policy only cares
# about the node names, not about which one carries the fake devices.
own_node() { echo "${CLUSTER_NAME}-worker"; }
other_node() { echo "${CLUSTER_NAME}-control-plane"; }

driver_sa() {
  local account
  account=$(kubectl get daemonset --namespace "$NAMESPACE" "$DAEMONSET" \
    -o jsonpath='{.spec.template.spec.serviceAccountName}')
  echo "system:serviceaccount:$NAMESPACE:$account"
}

# kubectl impersonating the ServiceAccount as it appears in a pod on NODE.
as_driver_on() {
  local node=$1
  shift
  kubectl --as "$(driver_sa)" \
    --as-user-extra "authentication.kubernetes.io/node-name=$node" "$@"
}

# The same identity without a node association. A real token always carries some
# extra, so one unrelated key is set and only node-name is missing, which is the
# case the policy rejects outright.
as_driver_without_node() {
  kubectl --as "$(driver_sa)" \
    --as-user-extra "authentication.kubernetes.io/credential-id=JTI=vap-e2e" "$@"
}

# A v1 ResourceSlice manifest, the version the driver publishes.
slice_manifest() {
  cat <<EOF
apiVersion: resource.k8s.io/v1
kind: ResourceSlice
metadata:
  name: $1
spec:
  driver: tpu.google.com
  nodeName: $2
  pool:
    name: $2
    generation: 1
    resourceSliceCount: 1
EOF
}

# The pipe has to live inside a function so that "run" (which takes a single
# command) sees the whole thing, matching how the rest of the suite wraps
# pipelines. create_slice NAME NODE creates a slice as the driver on its own
# node; create_slice_without_node does the same as the identity with no node.
create_slice() {
  slice_manifest "$1" "$2" | as_driver_on "$(own_node)" create -f -
}
create_slice_without_node() {
  slice_manifest "$1" "$2" | as_driver_without_node create -f -
}

# A newly installed policy takes a moment to become active in the apiserver.
# Wait until a request without a node association is denied by the policy
# itself; any other outcome (RBAC not yet propagated, request accepted) keeps
# waiting so a slow apiserver is not mistaken for a broken policy.
setup_file() {
  for _ in $(seq 30); do
    output=$(create_slice_without_node vap-e2e-canary "$(own_node)" 2>&1) && {
      kubectl delete resourceslice vap-e2e-canary --ignore-not-found
      sleep 2
      continue
    }
    [[ "$output" == *"no node association"* ]] && return 0
    sleep 2
  done
  echo "the node-restriction policy never became active: $output" >&2
  return 1
}

teardown() {
  kubectl delete resourceslice \
    vap-e2e-own-node vap-e2e-other-node vap-e2e-admin-owned vap-e2e-canary vap-e2e-no-node \
    --ignore-not-found >/dev/null 2>&1 || true
}

@test "impersonation carries the ServiceAccount and its node name" {
  # A broken --as flag would run every request as the admin user or without the
  # node extra, and the policy checks below would then pass or fail for the
  # wrong reason, so the identity is verified before anything is asserted on it.
  run as_driver_on "$(own_node)" auth whoami \
    -o jsonpath='{.status.userInfo.username}'
  assert_success
  assert_output "$(driver_sa)"

  run as_driver_on "$(own_node)" auth whoami \
    -o "jsonpath={.status.userInfo.extra.authentication\.kubernetes\.io/node-name[0]}"
  assert_success
  assert_output "$(own_node)"
}

@test "the driver may create a ResourceSlice for its own node" {
  run create_slice vap-e2e-own-node "$(own_node)"
  assert_success
}

@test "the driver may not create a ResourceSlice for another node" {
  run create_slice vap-e2e-other-node "$(other_node)"
  assert_failure
  assert_output --partial "may not modify"
}

@test "the driver may not delete another node's ResourceSlice" {
  # DELETE is validated against oldObject, so the slice is created by the admin
  # user first and the driver on the worker is then denied its removal.
  slice_manifest vap-e2e-admin-owned "$(other_node)" | kubectl create -f -

  run as_driver_on "$(own_node)" delete resourceslice vap-e2e-admin-owned
  assert_failure
  assert_output --partial "may not modify"
}

@test "the driver may delete a ResourceSlice for its own node" {
  create_slice vap-e2e-own-node "$(own_node)"

  run as_driver_on "$(own_node)" delete resourceslice vap-e2e-own-node
  assert_success
}

@test "a request with no node association is denied" {
  run create_slice_without_node vap-e2e-no-node "$(own_node)"
  assert_failure
  assert_output --partial "no node association"
}
