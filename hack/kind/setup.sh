#!/usr/bin/env bash
set -euo pipefail

CLUSTER_NAME="${KIND_CLUSTER_NAME:-crema-dev}"
REGISTRY_NAME="${KIND_REGISTRY_NAME:-crema-registry}"
REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"

echo "==> Starting local OCI registry (${REGISTRY_NAME}:${REGISTRY_PORT})"
if [ "$(docker inspect -f '{{.State.Running}}' "${REGISTRY_NAME}" 2>/dev/null || true)" != "true" ]; then
  docker rm -f "${REGISTRY_NAME}" >/dev/null 2>&1 || true
  docker run -d --restart=always -p "127.0.0.1:${REGISTRY_PORT}:5000" --name "${REGISTRY_NAME}" registry:2
fi

if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  echo "==> Creating kind cluster ${CLUSTER_NAME}"
  cat <<EOF | kind create cluster --name "${CLUSTER_NAME}" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
EOF
fi

echo "==> Connecting registry to kind network and configuring containerd plainHTTP mirror"
docker network connect "kind" "${REGISTRY_NAME}" 2>/dev/null || true

for node in $(kind get nodes --name "${CLUSTER_NAME}"); do
  docker exec "${node}" mkdir -p "/etc/containerd/certs.d/${REGISTRY_NAME}:5000"
  cat <<EOF | docker exec -i "${node}" tee "/etc/containerd/certs.d/${REGISTRY_NAME}:5000/hosts.toml" >/dev/null
server = "http://${REGISTRY_NAME}:5000"

[host."http://${REGISTRY_NAME}:5000"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
EOF
done

echo "==> Patching mock nvidia.com/gpu=4 extended resource on worker node"
WORKER_NODE="${CLUSTER_NAME}-worker"
kubectl --context "kind-${CLUSTER_NAME}" proxy --port=0 --address=127.0.0.1 >/tmp/crema-kubectl-proxy.log 2>&1 &
PROXY_PID=$!
trap 'kill ${PROXY_PID} >/dev/null 2>&1 || true' EXIT
sleep 2
PROXY_PORT=$(sed -n 's/.*127.0.0.1:\([0-9]*\).*/\1/p' /tmp/crema-kubectl-proxy.log | head -n1)
if [ -n "${PROXY_PORT}" ]; then
  curl -fsS -X PATCH "http://127.0.0.1:${PROXY_PORT}/api/v1/nodes/${WORKER_NODE}/status" \
    -H "Content-Type: application/json-patch+json" \
    -d '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"4"},{"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"4"}]' >/dev/null
fi

echo "==> Applying default SnapshotRegistry (${REGISTRY_NAME}:5000/workspaces)"
kubectl --context "kind-${CLUSTER_NAME}" apply -f config/crd/bases/
cat <<EOF | kubectl --context "kind-${CLUSTER_NAME}" apply -f -
apiVersion: crema.kubebottle.io/v1alpha1
kind: SnapshotRegistry
metadata:
  name: default
spec:
  repository: ${REGISTRY_NAME}:5000/workspaces
  plainHTTP: true
EOF

echo "==> kind cluster ${CLUSTER_NAME} is ready!"
