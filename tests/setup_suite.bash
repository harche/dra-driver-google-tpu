#!/usr/bin/env bash

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

set -e

# The suite drives the scripts under demo/ rather than defining its own cluster,
# so a change that breaks the quickstart breaks these tests.
function setup_suite {
  export BATS_TEST_TIMEOUT=300

  PROJECT_DIR="$(cd -- "${BATS_TEST_DIRNAME}/.." &>/dev/null && pwd)"
  export PROJECT_DIR

  # Defines KIND_CLUSTER_NAME, CONTAINER_TOOL and the driver image coordinates.
  source "${PROJECT_DIR}/demo/clusters/kind/scripts/common.sh"
  export CLUSTER_NAME="${KIND_CLUSTER_NAME}"
  if [[ "${CONTAINER_TOOL}" != "docker" ]]; then
    export KIND_EXPERIMENTAL_PROVIDER="${CONTAINER_TOOL}"
  fi

  mkdir -p "${PROJECT_DIR}/_artifacts"
  rm -rf "${PROJECT_DIR:?}/_artifacts/"*

  # create-cluster.sh loads the driver image into the cluster when it exists
  # locally, so it has to be built first.
  make -C "${PROJECT_DIR}" image-build
  "${PROJECT_DIR}/demo/clusters/kind/create-cluster.sh"

  # The Google Cloud helper containers are disabled: their images are only
  # published for Google Cloud and nothing they do applies to a fake device on a
  # kind node. The extra toleration puts a plugin on the control plane node too,
  # which has no TPU and is expected to stay idle.
  "${PROJECT_DIR}/demo/scripts/install-dra-driver.sh" \
    --set kubeletPlugin.containers.networkOptimizer.enabled=false \
    --set kubeletPlugin.containers.logCollector.enabled=false \
    --set kubeletPlugin.containers.vbarControlAgent.enabled=false \
    --set kubeletPlugin.tolerations[1].operator=Exists

  kubectl rollout status daemonset/dra-driver-google-tpu-kubeletplugin \
    --namespace dra-driver-google-tpu --timeout=180s

  # The rollout only waits for the container to start, the driver probes the
  # hardware and publishes afterwards.
  for _ in $(seq 60); do
    [[ -n "$(kubectl get resourceslice -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)" ]] && return 0
    sleep 2
  done
  echo "no resourceslice was published" >&2
  return 1
}

function teardown_suite {
  kind export logs "${PROJECT_DIR}/_artifacts" --name "${CLUSTER_NAME}" || true
  kind delete cluster --name "${CLUSTER_NAME}"
}
