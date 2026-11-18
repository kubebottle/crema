<p align="center">
  <img src="assets/crema-mascot.png" alt="crema mascot" width="220" />
</p>

# crema

> *Crema is the thin golden layer on top of an espresso — just like the thin writable filesystem layer sitting on top of a heavy GPU base image.*

Declarative GPU Workspace management for Kubernetes.

## Why

In GPU development environments, users routinely install Python packages, tweak dotfiles, and keep local tools in the container's root filesystem. On standard Kubernetes, changing GPU counts (e.g. `0 → 1 → 4` GPUs), suspending to release idle GPUs, or rescheduling a Pod wipes that writable layer clean.

**crema** bridges the gap between stateful dev machines and Kubernetes Pods:

- **Stateful GPU Scaling**: Suspend your workspace to save its rootfs, change GPU allocation (`0 ↔ 1 ↔ 4`), and resume with all installed packages and dotfiles intact.
- **Suspend & Resume**: Release expensive GPU resources when idle (`spec.suspend: true`) and resume from the latest committed OCI image later.
- **Orthogonal Storage**: Rootfs environment deltas are persisted as incremental `zstd` OCI layers; large datasets, code, and model weights stay on PVCs (`spec.volumeClaimTemplates`).
- **Kubernetes Native**: Uses standard `corev1.PodTemplateSpec` (supporting `/dev/shm` `emptyDir`, `runtimeClassName`, probes, and secrets), `containerd` `DiffService.Compare`, and standard OCI registries.

Inspired by [gpu-docker-api](https://github.com/mayooot/gpu-docker-api), redesigned for Kubernetes.

## Architecture

> See [docs/design.md](docs/design.md) for the full architecture, CRD specification, and state machine details.

![crema architecture: the controller reconciles Kubernetes resources, the node agent commits rootfs changes to an OCI registry, and containerd restores images while PVC data remains separate.](assets/architecture.svg)

The controller owns **desired state**; the node agent captures **filesystem state**. Before deleting a workspace Pod during `suspend: true`, `crema-agent` freezes the container via `containerd`, extracts only the writable delta layer, compresses it with `zstd`, and publishes an immutable OCI image (`@sha256:...`). When resumed (`suspend: false`), `kubelet` pulls the saved image and starts a fresh Pod with soft node affinity toward the previous node.

- **Rootfs state** lives in incremental OCI images (file-level diffs, including `.wh.*` whiteouts for deleted files).
- **Persistent data** stays on PVCs and is not included in rootfs snapshots.
- **Suspend/resume restarts processes**; it persists the filesystem environment, not RAM, running processes, or CUDA contexts.

## Quickstart (Local `kind`, No Physical GPU Required)

This walkthrough runs `crema` locally on `kind` using an in-cluster OCI registry (`crema-registry:5000`) and a **simulated `nvidia.com/gpu` extended resource** on the worker node — **no physical GPU hardware is required**.

### Prerequisites

- `docker`, `kind`, `kubectl`, `go` (`1.23+`)

### 1. Create `kind` Cluster + Local Registry + Mock GPU

```bash
# Starts registry:2 on 127.0.0.1:5001, creates kind cluster "crema-dev",
# configures containerd plainHTTP mirror, patches nvidia.com/gpu=4 on worker,
# and installs CRDs + default SnapshotRegistry.
./hack/kind/setup.sh

# Push a base image (alpine:3.20) to the local registry for offline-friendly testing
docker pull alpine:3.20
docker tag alpine:3.20 127.0.0.1:5001/library/alpine:3.20
docker push 127.0.0.1:5001/library/alpine:3.20
```

### 2. Build & Deploy `crema-controller` and `crema-agent` from Source

```bash
make kind-deploy
kubectl --context kind-crema-dev -n crema-system get pods
```

Wait until `crema-controller-manager` and `crema-agent` Pods are `1/1 Running`.

### 3. Run the Demo Workspace (No Physical GPU)

Apply [config/samples/crema_v1alpha1_gpuworkspace_kind_nogpu.yaml](config/samples/crema_v1alpha1_gpuworkspace_kind_nogpu.yaml) (starts with `nvidia.com/gpu: "0"`):

```bash
kubectl --context kind-crema-dev apply -f config/samples/crema_v1alpha1_gpuworkspace_kind_nogpu.yaml
kubectl --context kind-crema-dev get gws demo-workspace -w
```

Once `PHASE` is `Running`, mutate the container rootfs (write a dotfile, install a tool, and delete `/etc/motd` from the base image to test OCI whiteout):

```bash
kubectl --context kind-crema-dev exec demo-workspace-ws -- sh -c '
  echo "export CREMA_OK=1" > /root/.profile &&
  printf "#!/bin/sh\necho CREMA_TOOL_OK\n" > /usr/local/bin/my-tool &&
  chmod +x /usr/local/bin/my-tool &&
  rm -f /etc/motd
'
```

### 4. Suspend, Scale `nvidia.com/gpu` `0 → 1`, and Resume

`crema` requires suspending before changing compute/GPU resources:

```bash
# 1) Suspend to snapshot rootfs and release compute
kubectl --context kind-crema-dev patch gws demo-workspace --type=merge -p '{"spec":{"suspend":true}}'
kubectl --context kind-crema-dev get gws demo-workspace -w
# Wait for PHASE=Suspended, SEQ=1 (demo-workspace-ws Pod is deleted)

# 2) Scale nvidia.com/gpu from 0 to 1 and resume
kubectl --context kind-crema-dev patch gws demo-workspace --type=json \
  -p '[{"op":"replace","path":"/spec/suspend","value":false},{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/nvidia.com~1gpu","value":"1"}]'
kubectl --context kind-crema-dev get gws demo-workspace -w
# Wait for PHASE=Running, SEQ=1
```

Verify that your tool, dotfile, and deleted `/etc/motd` all survived recreation on the GPU worker node:

```bash
kubectl --context kind-crema-dev exec demo-workspace-ws -- sh -c \
  'my-tool && cat /root/.profile && test ! -e /etc/motd && echo WHITEOUT_OK'
```

### Clean Up

```bash
kubectl --context kind-crema-dev delete gws demo-workspace
# Optional: kind delete cluster --name crema-dev && docker rm -f crema-registry
```

## Status

🚀 **MVP Implemented & Verified** — `crema-controller`, `crema-agent`, `envtest` suite, and local `kind` E2E verified.

## License

[Apache-2.0](LICENSE)
