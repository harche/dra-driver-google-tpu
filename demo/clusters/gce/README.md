# Testing on GCE

Notes from actually running `create-kubeadm-tpu-cluster.sh` end to end, beyond
what the [quickstart](../../../README.md#path-c-vanilla-kubernetes-on-gce)
covers: checking quota before you start, recovering from the errors you are
likely to hit, and proving a workload can not just mount a chip but drive it.

## 1. Check what TPU quota the project actually has

The script defaults to `ct6e-standard-1t` (a single-host v6e slice), but v6e
capacity is scarce and plenty of projects have none at all. Check before
running anything, rather than debugging a stockout after the control plane and
network are already up:

```bash
gcloud compute regions describe REGION --project=PROJECT --format="json(quotas)" |
  python3 -c "
import json, sys
for q in json.load(sys.stdin)['quotas']:
    if 'TPU' in q['metric']:
        print(q['metric'], q['limit'], q['usage'])
"
```

A project with no v6e quota does not show a `..._V6E_...` metric at all, at any
limit, in any region -- it is not that the limit is `0.0`, the metric itself is
absent until quota is granted. `TPU_LITE_PODSLICE_V5` (v5e) is a much more
commonly available fallback and exercises the same code paths; the machine
type it needs is `ct5lp-hightpu-1t`.

Override the machine type and, if needed, the zone (not every zone offers every
machine type):

```bash
PROJECT=my-project ZONE=us-central1-a TPU_MACHINE_TYPE=ct5lp-hightpu-1t \
  ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh
```

The script derives the accelerator type from the machine type on its own (see
the `case` in `create-kubeadm-tpu-cluster.sh`), so no other flag changes.

## 2. A transient error creating the TPU VM is common, retrying is normal

```
ERROR: (gcloud.compute.instances.create) Could not fetch resource:
 - Internal error. Please try again or contact Google Support. (Code: '...')
```

This came back with a *different* error code on each of three attempts in the
same zone, which pointed at zone-level flakiness rather than a one-off. The
script is idempotent -- it skips the network and any VM that already exists --
so re-running it only retries what failed:

```bash
PROJECT=my-project ZONE=us-central1-a TPU_MACHINE_TYPE=ct5lp-hightpu-1t \
  ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh
```

If it keeps failing, tear down and try another zone that offers the same
machine type (`gcloud compute machine-types list --filter="name=ct5lp-hightpu-1t"
--format="value(zone)"`):

```bash
PROJECT=my-project ZONE=us-central1-a ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh --delete
```

## 3. Pulling the driver image from a private registry

As the quickstart's note says, a plain Ubuntu node has no credential helper for
Artifact Registry or `gcr.io`, so containerd only ever attempts an *anonymous*
pull. A private image fails like this, with the driver container stuck in
`ImagePullBackOff` while the two sidecars pull fine:

```
Failed to pull image "...": failed to resolve image: failed to authorize:
failed to fetch anonymous token: ... 403 Forbidden
```

Two ways to fix it:

- **Image pull secret** (works with any private repo): create a
  `kubernetes.io/dockerconfigjson` secret from `gcloud auth print-access-token`
  and reference it from `imagePullSecrets` -- keeps the image
  private, needs refreshing when the token expires.
- **A public repo, scoped to just the test image** (what was used here): push
  to a dedicated Artifact Registry repository and grant `allUsers` read on
  *that repository only*, never on a shared one that also holds unrelated
  images:

  ```bash
  gcloud artifacts repositories create tpu-dra-test \
    --project=my-project --location=us-central1 --repository-format=docker
  gcloud artifacts repositories add-iam-policy-binding tpu-dra-test \
    --project=my-project --location=us-central1 \
    --member=allUsers --role=roles/artifactregistry.reader

  REGISTRY=us-central1-docker.pkg.dev/my-project/tpu-dra-test \
    IMAGE=dra-driver-google-tpu TAG=test \
    ./demo/scripts/build-driver-image.sh
  REGISTRY=us-central1-docker.pkg.dev/my-project/tpu-dra-test \
    IMAGE=dra-driver-google-tpu TAG=test \
    ./demo/scripts/push-driver-image.sh
  ```

  Then point the cluster at it before running the create script:

  ```bash
  REGISTRY=us-central1-docker.pkg.dev/my-project/tpu-dra-test IMAGE=dra-driver-google-tpu TAG=test \
    PROJECT=my-project ZONE=us-central1-a TPU_MACHINE_TYPE=ct5lp-hightpu-1t \
    ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh
  ```

  Delete the repository once done; nothing else depends on it.

## 4. What a real v5e chip looks like once it comes up

Hardware probing and the PCI id cross-check (see `hardware.go`) work the same
on a real chip as on the fake devices the kind e2e tests use:

```console
$ kubectl logs -n dra-driver-google-tpu -l app.kubernetes.io/name=dra-driver-google-tpu -c tpu-dra-plugin
Found 1 TPU chips in /dev/vfio
Discovered TPU map[tpu.google.com/accelerator:tpu-v5-lite-podslice] from driver configuration
Node is labeled with accelerator "tpu-v5-lite-podslice" (generation v5litepod) but its PCI id says generation v5lite
Assuming the single host topology 1x1 for 1 TPU chips
```

The warning is expected, not a bug: v5e's "device" and "podslice" form factors
are the same silicon and the same PCI id, so `tpuGenerationFromPCIIDs` cannot
tell them apart and resolves to one of them (see the comment on
`tpuGenerationFamily` in `hardware.go`). It only double checks a label another
source already provided and never blocks anything.

```console
$ kubectl get resourceslice -o yaml
    devices:
    - name: "0"
      attributes:
        accelerator: {string: tpu-v5-lite-podslice}
        brand:       {string: Google}
        chipCount:   {int: 1}
        index:       {int: 0}
        topology:    {string: 1x1}
        tpuGen:      {string: v5litepod}
        uuid:        {string: tpu-585695ce-c265-152a-e04a-88b343209584}
```

## 5. Proving a workload can not just mount the chip but drive it

`demo/specs/tpu-test.yaml` proves the device node and TPU environment variables
are injected; it never opens the chip. `demo/specs/tpu-jax-test.yaml` goes
further: it claims the TPU, installs `tpu-info` and JAX, and runs a real matrix
multiplication on the chip so a wrong PCI id, a missing mount or a broken CDI
spec would show up as a JAX error rather than passing silently.

```bash
kubectl apply -f demo/specs/tpu-jax-test.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Running pod/tpu-jax-pod -n tpu-jax-test --timeout=90s
kubectl logs -n tpu-jax-test tpu-jax-pod -f
```

```console
Libtpu version: 0.0.42.1
Accelerator type: v5e

TPU Chips
| Chip        | Type         | Devices | PID |
|-------------|--------------|---------|-----|
| /dev/vfio/0 | TPU v5e chip | 1       | N/A |

jax devices: [TpuDevice(id=0, process_index=0, coords=(0,0,0), core_on_chip=0)]
matmul sum: 1.0737418e+09
backend: tpu
```

`tpu-info`'s own PCI scan (`GOOGLE_PCI_VENDOR_ID` + device id, the same
technique `tpuGenerationFromPCIIDs` in `hardware.go` uses) independently agrees
with the driver: a real v5e chip, one device.

Without `IPC_LOCK` and `SYS_RESOURCE` on the container, this fails instead with
`FAILED_PRECONDITION: TPU initialization failed: Couldn't mmap: Resource
temporarily unavailable` -- the container runtime's default `RLIMIT_MEMLOCK` is
too small for libtpu to lock the memory it needs. This is a property of the
workload's container, not something the driver's CDI spec sets, so any real TPU
workload needs it.

Delete the namespace when done; the DRA driver only allocates from a released
claim, and this VM has just the one chip:

```bash
kubectl delete namespace tpu-jax-test
```

## 6. Tear down

TPU VMs bill while they exist:

```bash
PROJECT=my-project ZONE=us-central1-a ./demo/clusters/gce/create-kubeadm-tpu-cluster.sh --delete
```
