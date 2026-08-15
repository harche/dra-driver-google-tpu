# End to end tests

The suite creates a kind cluster with fake TPU devices, installs the driver and
exercises it. It drives the scripts under `demo/`, so a change that breaks the
quickstart breaks these tests.

## Running them

1. Install [bats](https://bats-core.readthedocs.io/en/stable/installation.html)
2. Install [kind](https://kind.sigs.k8s.io/)
3. Fetch the bats helper libraries:

```bash
git clone --depth 1 https://github.com/bats-core/bats-support.git tests/test_helper/bats-support
git clone --depth 1 https://github.com/bats-core/bats-assert.git tests/test_helper/bats-assert
```

4. Run them:

```bash
make test-e2e
```

The cluster is deleted afterwards and its logs are exported to `_artifacts/`.

## What they cover

The kind worker is labeled with the accelerator type and nothing else, so the
chip count, the topology and the device directory have to be discovered from the
fake `/dev/accel<n>` devices. The control plane node has no devices and no
labels, which covers the idle path.

The last test removes the accelerator label from the node and configures it on
the chart instead, leaving nothing in the cluster that describes the TPU. It
passes only if the driver discovers the hardware and publishes it on the
devices of the ResourceSlice.

## Writing tests

Prefer the assertions from
[bats-assert](https://github.com/bats-core/bats-assert): they report the actual
and the expected output, which makes a failure readable without a rerun.
