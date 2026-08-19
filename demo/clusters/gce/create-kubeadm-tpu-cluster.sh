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

# Creates a vanilla kubeadm Kubernetes cluster on GCE and installs the TPU DRA
# driver on it:
#   - one VM as the control plane
#   - one VM as a CPU worker, for anything that must not occupy a TPU
#   - one single-host TPU VM (a ct6e Compute Engine instance) joined as a worker
#
# The cluster is a plain upstream Kubernetes, so nothing labels the TPU node or
# knows what a TPU is. The driver probes the hardware and is only told the
# accelerator type, which cannot be observed from the host.
#
# Pods get real VPC addresses from a GCE alias IP range per node, carved out of a
# secondary range on the subnet, so there is no overlay and no cloud routes. The
# cloud-controller-manager reads those ranges back to set the node CIDRs, and
# brings Service type LoadBalancer and the zone/region topology labels with it.
#
# There is no SSH anywhere in this script. cloud-init runs the whole node
# bootstrap including 'kubeadm init' and 'kubeadm join', and each node reports
# back through GCE guest attributes -- an API on the metadata server, so it works
# even where SSH is blocked by policy. Everything after that is kubectl.
#
# The control-plane taint is left in place, so all workloads land on the two workers.
# Every node takes an ephemeral public IP (also its egress path for package installs).
# The API server is reachable from anywhere by default -- it is TLS + client-cert
# authenticated -- so a rotating client IP never locks you out. Set API_SOURCE_RANGE
# to restrict it.

set -euo pipefail

CURRENT_DIR="$(cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd)"
PROJECT_DIR="$(cd -- "${CURRENT_DIR}/../../.." &> /dev/null && pwd)"

PROJECT="${PROJECT:-$(gcloud config get-value project 2>/dev/null)}"
# Machine type availability and TPU capacity both vary by zone; the preflight
# below fails fast when the selected zone cannot satisfy them.
ZONE="${ZONE:-southamerica-west1-a}"
NAME_PREFIX="${NAME_PREFIX:-tpu-dra}"

# DRA is GA in 1.34; older versions need feature gates this script does not set.
K8S_VERSION="${K8S_VERSION:-1.34}"
# Pods are addressed from a secondary range of the subnet, each node gets an
# alias range of NODE_CIDR_MASK bits out of it.
POD_CIDR="${POD_CIDR:-10.244.0.0/16}"
POD_RANGE_NAME="${POD_RANGE_NAME:-pods}"
NODE_CIDR_MASK="${NODE_CIDR_MASK:-24}"
SUBNET_CIDR="${SUBNET_CIDR:-10.10.0.0/16}"

CCM_IMAGE="${CCM_IMAGE:-registry.k8s.io/cloud-provider-gcp/cloud-controller-manager:v35.0.0}"

CP_MACHINE_TYPE="${CP_MACHINE_TYPE:-e2-standard-4}"
CPU_MACHINE_TYPE="${CPU_MACHINE_TYPE:-e2-standard-8}"
# TPUs are ordinary Compute Engine instances now; the older 'gcloud compute tpus
# tpu-vm' API is deprecated. ct6e-standard-8t is a single-host v6e-8 slice, but
# v6e capacity is scarce -- ct6e-standard-1t is far easier to obtain.
# https://docs.cloud.google.com/tpu/docs/create-instance-compute
TPU_MACHINE_TYPE="${TPU_MACHINE_TYPE:-ct6e-standard-1t}"
TPU_IMAGE_FAMILY="${TPU_IMAGE_FAMILY:-ubuntu-accel-2204-amd64-tpu-v5e-v5p-v6e}"
TPU_IMAGE_PROJECT="${TPU_IMAGE_PROJECT:-ubuntu-os-accelerator-images}"

API_SOURCE_RANGE="${API_SOURCE_RANGE:-0.0.0.0/0}"

REGION="${ZONE%-*}"
NETWORK="${NAME_PREFIX}-net"
CP_NODE="${NAME_PREFIX}-cp"
CPU_NODE="${NAME_PREFIX}-cpu"
TPU_NODE="${NAME_PREFIX}-tpu"
KUBECONFIG_FILE="${KUBECONFIG_FILE:-${TMPDIR:-/tmp}/${NAME_PREFIX}.kubeconfig}"

# Deleting on failure destroys the evidence needed to debug it, so it is opt-in;
# otherwise the exit trap prints the exact teardown command.
CLEANUP_ON_FAILURE="${CLEANUP_ON_FAILURE:-false}"

if [[ -z "${PROJECT}" ]]; then
    echo "ERROR: set PROJECT or run 'gcloud config set project <id>'" >&2
    exit 1
fi

GCLOUD_COMMON=(--project="${PROJECT}" --zone="${ZONE}")
USER_DATA=""

log() { echo -e "\n=== $* ===" >&2; }

# retry ATTEMPTS DELAY CMD...
retry() {
    local attempts=$1 delay=$2 n=1
    shift 2
    until "$@"; do
        if (( n >= attempts )); then
            echo "ERROR: failed after ${attempts} attempts: $*" >&2
            return 1
        fi
        n=$(( n + 1 ))
        sleep "${delay}"
    done
}

teardown() {
    log "Deleting all ${NAME_PREFIX} resources"
    gcloud compute instances delete "${CP_NODE}" "${CPU_NODE}" "${TPU_NODE}" "${GCLOUD_COMMON[@]}" --quiet || true
    rm -f "${KUBECONFIG_FILE}"

    if ! gcloud compute networks describe "${NETWORK}" --project="${PROJECT}" >/dev/null 2>&1; then
        return 0
    fi

    # Some projects re-add firewall rules to a VPC on a timer, so a rule can appear
    # between listing them and deleting the network. Retry the pair rather than fail.
    local rules subnets entry
    for _ in 1 2 3; do
        rules=()
        mapfile -t rules < <(gcloud compute firewall-rules list --project="${PROJECT}" \
            --filter="network=${NETWORK}" --format="value(name)" 2>/dev/null)
        if (( ${#rules[@]} > 0 )); then
            gcloud compute firewall-rules delete "${rules[@]}" --project="${PROJECT}" --quiet || true
        fi
        # The VPC may hold subnets in more than one region if ZONE was changed.
        subnets=()
        mapfile -t subnets < <(gcloud compute networks subnets list --project="${PROJECT}" \
            --network="${NETWORK}" --format="value(name,region.basename())" 2>/dev/null)
        for entry in "${subnets[@]}"; do
            gcloud compute networks subnets delete "${entry%%$'\t'*}" --project="${PROJECT}" \
                --region="${entry##*$'\t'}" --quiet || true
        done
        if gcloud compute networks delete "${NETWORK}" --project="${PROJECT}" --quiet; then
            return 0
        fi
        sleep 15
    done
    echo "WARNING: ${NETWORK} still exists; re-run with --delete" >&2
}

on_exit() {
    local rc=$?
    [[ -n "${USER_DATA}" ]] && rm -f "${USER_DATA}"
    (( rc == 0 )) && return 0
    if [[ "${CLEANUP_ON_FAILURE}" == "true" ]]; then
        echo "Provisioning failed (exit ${rc}); CLEANUP_ON_FAILURE=true, removing resources" >&2
        teardown
    else
        cat >&2 <<WARN

########################################################################
# Provisioning FAILED (exit ${rc}). Resources may exist and are billing.
# Delete them with:
#
#   PROJECT=${PROJECT} ZONE=${ZONE} $0 --delete
#
# Or re-run with CLEANUP_ON_FAILURE=true to do this automatically.
########################################################################
WARN
    fi
    return "${rc}"
}
trap on_exit EXIT

if [[ "${1:-}" == "--delete" ]]; then
    teardown
    exit 0
fi

# Nodes publish progress here instead of us polling them over SSH.
guest_attr() {
    gcloud compute instances get-guest-attributes "$1" "${GCLOUD_COMMON[@]}" \
        --query-path="tpu-dra/$2" --format="value(value)" 2>/dev/null
}

node_ready() { [[ "$(guest_attr "$1" status)" == "ready" ]]; }

wait_node() {
    local name=$1
    log "Waiting for ${name} to finish cloud-init bootstrap"
    if ! retry 90 20 node_ready "${name}"; then
        echo "ERROR: ${name} never reported ready." >&2
        echo "       Serial console: gcloud compute instances get-serial-port-output ${name} ${GCLOUD_COMMON[*]}" >&2
        return 1
    fi
}

#######################################
# cloud-init user-data, identical on every node; role comes from metadata.
#######################################
# Cloud TPU rejects metadata.user-data unless it parses as YAML, so the bootstrap
# ships inside a #cloud-config document rather than as a bare shebang script.
BOOTSTRAP_SRC=$(mktemp)
cat >"${BOOTSTRAP_SRC}" <<'EOS'
#!/bin/bash
# Node bootstrap, run once by cloud-init at first boot. Installs the runtime,
# then either initialises the control plane or joins it -- no SSH required.
exec >/var/log/tpu-dra-bootstrap.log 2>&1
set -x
set -o pipefail

MD="http://metadata.google.internal/computeMetadata/v1"
md() { curl -fsS -H "Metadata-Flavor: Google" --max-time 10 "${MD}/$1"; }
# Guest attributes are how this node reports back; the provisioning script polls them.
put_attr() {
    curl -fsS -X PUT --data "$2" -H "Metadata-Flavor: Google" \
        "${MD}/instance/guest-attributes/tpu-dra/$1"
}
fail() { put_attr status "failed: $1"; exit 1; }

APT=(apt-get -o DPkg::Lock::Timeout=900 -y)
retry() {
    local n=1
    until "$@"; do
        if (( n >= 30 )); then return 1; fi
        n=$(( n + 1 ))
        sleep 10
    done
}

ROLE=$(md instance/attributes/tpu-dra-role)
TOKEN=$(md instance/attributes/tpu-dra-token)
POD_CIDR=$(md instance/attributes/tpu-dra-pod-cidr)
EXTERNAL_IP=$(md instance/network-interfaces/0/access-configs/0/external-ip)

put_attr status "bootstrapping"

retry "${APT[@]}" update || fail apt-update
retry "${APT[@]}" install apt-transport-https ca-certificates curl gpg containerd || fail apt-install

swapoff -a
sed -i '/[[:space:]]swap[[:space:]]/s/^/#/' /etc/fstab

modprobe overlay
modprobe br_netfilter
cat >/etc/modules-load.d/k8s.conf <<'EOF'
overlay
br_netfilter
EOF
cat >/etc/sysctl.d/99-k8s.conf <<'EOF'
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF
sysctl --system

# CDI is how the DRA driver injects TPU device nodes into the workload container.
# containerd 2.x (config version 3) enables it by default; version 2 must opt in.
mkdir -p /etc/containerd
containerd config default >/etc/containerd/config.toml || fail containerd-config
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
if [[ "$(awk -F'= *' '/^version/{print $2; exit}' /etc/containerd/config.toml)" == "2" ]]; then
    sed -i "/^\[plugins\.[\"']io\.containerd\.grpc\.v1\.cri[\"']\]$/a\\  enable_cdi = true\n  cdi_spec_dirs = [\"/etc/cdi\", \"/var/run/cdi\"]" /etc/containerd/config.toml
fi
systemctl restart containerd || fail containerd-restart
systemctl enable containerd

install -m 0755 -d /etc/apt/keyrings
retry curl -fsSL "https://pkgs.k8s.io/core:/stable:/v__K8S_VERSION__/deb/Release.key" -o /tmp/k8s-release.key || fail k8s-key
gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg /tmp/k8s-release.key || fail k8s-key-dearmor
chmod 0644 /etc/apt/keyrings/kubernetes-apt-keyring.gpg
echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v__K8S_VERSION__/deb/ /" \
    >/etc/apt/sources.list.d/kubernetes.list

retry "${APT[@]}" update || fail apt-update-k8s
retry "${APT[@]}" install kubelet kubeadm kubectl || fail k8s-install
apt-mark hold kubelet kubeadm kubectl
systemctl enable kubelet

# cloud-provider=external hands node addresses, topology labels and the node
# CIDR to the cloud-controller-manager. Until it runs, nodes carry the
# uninitialized taint, which is why it is deployed as soon as the API is up.
if [[ "${ROLE}" == "cp" ]]; then
    put_attr status "kubeadm-init"
    # The external IP SAN is what makes the published kubeconfig usable off-VPC.
    cat >/etc/kubernetes/kubeadm-init.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
bootstrapTokens:
- token: "${TOKEN}"
  ttl: "0"
nodeRegistration:
  kubeletExtraArgs:
  - name: cloud-provider
    value: external
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
apiServer:
  certSANs:
  - 127.0.0.1
  - localhost
  - "${EXTERNAL_IP}"
controllerManager:
  extraArgs:
  # The node CIDRs come from the GCE alias ranges through the
  # cloud-controller-manager, nothing is allocated in cluster.
  - name: allocate-node-cidrs
    value: "false"
networking:
  podSubnet: "${POD_CIDR}"
EOF
    kubeadm init --config /etc/kubernetes/kubeadm-init.yaml \
        --ignore-preflight-errors=NumCPU || fail kubeadm-init
    # Published so the provisioning script can build a kubeconfig without SSH.
    put_attr kubeconfig "$(base64 -w0 /etc/kubernetes/admin.conf)" || fail publish-kubeconfig
else
    CP_IP=$(md instance/attributes/tpu-dra-cp-ip)
    put_attr status "kubeadm-join"
    cat >/etc/kubernetes/kubeadm-join.yaml <<EOF
apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: "${CP_IP}:6443"
    token: "${TOKEN}"
    unsafeSkipCAVerification: true
nodeRegistration:
  kubeletExtraArgs:
  - name: cloud-provider
    value: external
EOF
    # The API server may still be coming up; kubeadm join is retried by the caller.
    retry kubeadm join --config /etc/kubernetes/kubeadm-join.yaml || fail kubeadm-join
fi

put_attr status "ready"
EOS
sed -i "s/__K8S_VERSION__/${K8S_VERSION}/g" "${BOOTSTRAP_SRC}"

USER_DATA=$(mktemp)
{
    echo "#cloud-config"
    echo "write_files:"
    echo "  - path: /opt/tpu-dra-bootstrap.sh"
    echo "    permissions: '0755'"
    echo "    content: |"
    sed 's/^/      /' "${BOOTSTRAP_SRC}"
    echo "runcmd:"
    echo "  - [/opt/tpu-dra-bootstrap.sh]"
} >"${USER_DATA}"
rm -f "${BOOTSTRAP_SRC}"

if command -v python3 >/dev/null && ! python3 -c "import sys,yaml; yaml.safe_load(open(sys.argv[1]))" "${USER_DATA}" 2>/dev/null; then
    echo "ERROR: generated cloud-config is not valid YAML; Cloud TPU would reject it" >&2
    exit 1
fi

#######################################
# Preflight
#######################################
# Machine type availability differs sharply between zones (e.g. us-central2-b has
# no e2 family at all), so check before creating anything. Retried because a
# transient API error here is indistinguishable from a genuinely absent type.
require_machine_type() {
    retry 3 5 gcloud compute machine-types describe "$1" "${GCLOUD_COMMON[@]}" >/dev/null 2>&1 && return
    local available
    available=$(gcloud compute machine-types list --zones="${ZONE}" --project="${PROJECT}" \
        --format="value(name)" 2>/dev/null | sed 's/-[0-9]*[a-z]*$//' | sort -u | tr '\n' ' ')
    if [[ -z "${available}" ]]; then
        echo "ERROR: could not list machine types in ${ZONE}; check credentials/API access" >&2
    else
        echo "ERROR: machine type '$1' is not available in ${ZONE}. Available: ${available}" >&2
    fi
    return 1
}

log "Checking machine types are available in ${ZONE}"
require_machine_type "${CP_MACHINE_TYPE}"
require_machine_type "${CPU_MACHINE_TYPE}"
require_machine_type "${TPU_MACHINE_TYPE}"

#######################################
# Network
#######################################
log "Creating network ${NETWORK}"
if ! gcloud compute networks describe "${NETWORK}" --project="${PROJECT}" >/dev/null 2>&1; then
    gcloud compute networks create "${NETWORK}" --project="${PROJECT}" --subnet-mode=custom
fi

# Checked separately from the network: subnets are regional, so pointing ZONE at
# another region needs a new subnet under the existing VPC.
if ! gcloud compute networks subnets describe "${NETWORK}" --project="${PROJECT}" \
        --region="${REGION}" >/dev/null 2>&1; then
    gcloud compute networks subnets create "${NETWORK}" \
        --project="${PROJECT}" --network="${NETWORK}" \
        --region="${REGION}" --range="${SUBNET_CIDR}" \
        --secondary-range "${POD_RANGE_NAME}=${POD_CIDR}"
elif ! gcloud compute networks subnets describe "${NETWORK}" --project="${PROJECT}" \
        --region="${REGION}" --format="value(secondaryIpRanges[].rangeName)" |
        grep -qw "${POD_RANGE_NAME}"; then
    # A subnet left over from a run that predates alias ranges.
    gcloud compute networks subnets update "${NETWORK}" \
        --project="${PROJECT}" --region="${REGION}" \
        --add-secondary-ranges "${POD_RANGE_NAME}=${POD_CIDR}"
fi

ensure_firewall() {
    local name=$1
    shift
    gcloud compute firewall-rules describe "${name}" --project="${PROJECT}" >/dev/null 2>&1 && return
    gcloud compute firewall-rules create "${name}" --project="${PROJECT}" --network="${NETWORK}" "$@"
}

# Cluster traffic (API server, etcd, kubelet, pod networking) stays inside the VPC.
ensure_firewall "${NETWORK}-internal" --allow=tcp,udp,icmp --source-ranges="${SUBNET_CIDR},${POD_CIDR}"
ensure_firewall "${NETWORK}-api" --allow=tcp:6443 --source-ranges="${API_SOURCE_RANGE}"
# Probes of the load balancers the cloud-controller-manager creates.
ensure_firewall "${NETWORK}-health-checks" --allow=tcp --source-ranges="35.191.0.0/16,130.211.0.0/22"

#######################################
# Nodes
#######################################
# Shared by every node so workers can join without fetching anything from the
# control plane first. Metadata is project-visible, which is acceptable for a
# disposable cluster; a long-lived one should pre-generate a CA instead.
# openssl rather than 'tr </dev/urandom | head', which dies of SIGPIPE under pipefail.
JOIN_TOKEN="$(openssl rand -hex 3).$(openssl rand -hex 8)"

# create_vm NAME MACHINE_TYPE ROLE [EXTRA_ARGS...]
create_vm() {
    local name=$1 machine_type=$2 role=$3
    shift 3
    if gcloud compute instances describe "${name}" "${GCLOUD_COMMON[@]}" >/dev/null 2>&1; then
        return
    fi
    local metadata="enable-guest-attributes=TRUE,tpu-dra-role=${role},tpu-dra-token=${JOIN_TOKEN}"
    metadata="${metadata},tpu-dra-pod-cidr=${POD_CIDR}"
    if [[ "${role}" != "cp" ]]; then
        metadata="${metadata},tpu-dra-cp-ip=${CP_INTERNAL_IP}"
    fi
    # The alias range is what the pods of this node are addressed from; the VPC
    # routes it to the VM, so no overlay and no --can-ip-forward are needed.
    gcloud compute instances create "${name}" "${GCLOUD_COMMON[@]}" \
        --machine-type="${machine_type}" \
        --boot-disk-size=100GB \
        --network-interface="subnet=${NETWORK},aliases=${POD_RANGE_NAME}:/${NODE_CIDR_MASK}" \
        --scopes=cloud-platform \
        --tags="${NAME_PREFIX}" \
        --metadata="${metadata}" \
        --metadata-from-file=user-data="${USER_DATA}" \
        "$@"
}

log "Creating control-plane VM ${CP_NODE}"
create_vm "${CP_NODE}" "${CP_MACHINE_TYPE}" cp \
    --image-family=ubuntu-2204-lts --image-project=ubuntu-os-cloud

# Workers need this in their metadata, so the control plane must exist first.
CP_INTERNAL_IP=$(gcloud compute instances describe "${CP_NODE}" "${GCLOUD_COMMON[@]}" \
    --format="value(networkInterfaces[0].networkIP)")
log "Control plane internal IP: ${CP_INTERNAL_IP}"

log "Creating TPU VM ${TPU_NODE} (${TPU_MACHINE_TYPE})"
# Created before the CPU worker: TPU capacity is the scarce resource, and a
# stockout here should not leave an extra VM running.
TPU_EXTRA=(--image-family="${TPU_IMAGE_FAMILY}" --image-project="${TPU_IMAGE_PROJECT}"
           --maintenance-policy=TERMINATE)
# Only the 8-chip type requires one thread per core.
[[ "${TPU_MACHINE_TYPE}" == "ct6e-standard-8t" ]] && TPU_EXTRA+=(--threads-per-core=1)
create_vm "${TPU_NODE}" "${TPU_MACHINE_TYPE}" worker "${TPU_EXTRA[@]}"

log "Creating CPU worker VM ${CPU_NODE}"
create_vm "${CPU_NODE}" "${CPU_MACHINE_TYPE}" worker \
    --image-family=ubuntu-2204-lts --image-project=ubuntu-os-cloud

#######################################
# Cluster
#######################################
wait_node "${CP_NODE}"

log "Fetching kubeconfig to ${KUBECONFIG_FILE}"
CP_EXTERNAL_IP=$(gcloud compute instances describe "${CP_NODE}" "${GCLOUD_COMMON[@]}" \
    --format="value(networkInterfaces[0].accessConfigs[0].natIP)")
guest_attr "${CP_NODE}" kubeconfig | base64 -d >"${KUBECONFIG_FILE}"
sed -i "s#server: https://.*#server: https://${CP_EXTERNAL_IP}:6443#" "${KUBECONFIG_FILE}"
chmod 600 "${KUBECONFIG_FILE}"
export KUBECONFIG="${KUBECONFIG_FILE}"
retry 30 5 kubectl version --request-timeout=5s

log "Installing the cloud-controller-manager"
# Nodes stay tainted as uninitialized until it runs, so it comes before anything
# else. It also sets the node CIDRs from the GCE alias ranges.
sed -e "s#__CCM_IMAGE__#${CCM_IMAGE}#" -e "s#__POD_CIDR__#${POD_CIDR}#" \
    "${CURRENT_DIR}/cloud-provider-gcp.yaml" | retry 5 10 kubectl apply -f -

log "Installing kindnet CNI"
# kindnet installs the CNI plugins and addresses pods out of the node CIDR, which
# here is the GCE alias range, so the VPC routes pod traffic without an overlay.
# Retried patiently: raw.githubusercontent.com answers 429 often enough to fail a
# short retry loop.
retry 10 30 kubectl create -f https://raw.githubusercontent.com/kubernetes-sigs/kindnet/refs/heads/main/install-kindnet.yaml

wait_node "${TPU_NODE}"
wait_node "${CPU_NODE}"
retry 60 10 kubectl wait --for=condition=Ready nodes --all --timeout=30s

# Proof that the cloud IPAM is what assigned the pod ranges.
log "Node CIDRs allocated from the GCE alias ranges"
kubectl get nodes -o custom-columns='NAME:.metadata.name,PODCIDR:.spec.podCIDR,PROVIDER:.spec.providerID'

# The chip count, the device directory and the single-host topology are probed
# from the hardware by the driver. The accelerator type cannot be observed on the
# host and no cloud controller labels the node here, so it comes from the machine
# type. It is set for the whole DaemonSet: nodes without a TPU stay idle.
case "${TPU_MACHINE_TYPE}" in
    ct6e-*) TPU_ACCELERATOR=tpu-v6e-slice ;;
    ct5lp-*) TPU_ACCELERATOR=tpu-v5-lite-podslice ;;
    ct5p-*) TPU_ACCELERATOR=tpu-v5p-slice ;;
    ct4p-*) TPU_ACCELERATOR=tpu-v4-podslice ;;
    *) echo "ERROR: unknown accelerator type for ${TPU_MACHINE_TYPE}; add it here" >&2; exit 1 ;;
esac

log "Installing the TPU DRA driver for ${TPU_ACCELERATOR}"
( cd "${PROJECT_DIR}" && ./demo/scripts/install-dra-driver.sh \
    --set kubeletPlugin.tpu.accelerator="${TPU_ACCELERATOR}" )

log "Cluster state"
kubectl get nodes -o wide
kubectl get pods -n dra-driver-google-tpu
# Real uuid/tpuGen attributes here prove the driver is talking to actual hardware.
slices_published() { [[ -n "$(kubectl get resourceslice -o jsonpath='{.items[0].spec.devices[0].name}' 2>/dev/null)" ]]; }
retry 30 10 slices_published
kubectl get resourceslice -o yaml

cat >&2 <<EOF

Cluster is up.

  export KUBECONFIG=${KUBECONFIG_FILE}
  kubectl get nodes

The API server is at ${CP_EXTERNAL_IP}:6443, open to ${API_SOURCE_RANGE}.

Run a workload that claims a TPU:

  kubectl apply -f ${PROJECT_DIR}/demo/specs/tpu-test.yaml

Tear down when finished (TPU VMs bill while they exist):

  PROJECT=${PROJECT} ZONE=${ZONE} $0 --delete
EOF
