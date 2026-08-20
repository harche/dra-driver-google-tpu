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

NAMESPACE="$1"

if [ -z "${NAMESPACE}" ]; then
  echo "Usage: $0 <namespace>"
  exit 1
fi

echo "Checking TPU device injection for pods in namespace '${NAMESPACE}'..."
echo ""

pods=$(kubectl get pod --namespace="${NAMESPACE}" --output=jsonpath='{.items[*].metadata.name}' 2>/dev/null)
if [ -z "${pods}" ]; then
  echo "No pods found in namespace '${NAMESPACE}'."
  exit 1
fi

for pod in ${pods}; do
  for ctr in $(kubectl get pod "${pod}" --namespace="${NAMESPACE}" -o jsonpath='{.spec.containers[*].name}'); do
    echo "=== Pod: ${pod} | Container: ${ctr} ==="
    kubectl exec "${pod}" -c "${ctr}" --namespace="${NAMESPACE}" -- sh -c '
      found=0
      if [ -d /dev/vfio ]; then
        chips=$(ls -1 /dev/vfio 2>/dev/null | grep -E "^[0-9]+$" | awk "{print \"/dev/vfio/\" \$1}" | tr "\n" " ")
        if [ -n "$chips" ]; then
          echo "TPU Chips (VFIO): ${chips}"
          found=1
        fi
      fi
      if ls /dev/accel* >/dev/null 2>&1; then
        echo "TPU Chips (Accel): $(ls -1 /dev/accel* 2>/dev/null | tr "\n" " ")"
        found=1
      fi
      if [ $found -eq 0 ]; then
        echo "No TPU device nodes found!"
      fi
      echo "TPU Environment Variables:"
      env | grep -E "^TPU_" | sort || echo "  (None)"
    '
    echo ""
  done
done
