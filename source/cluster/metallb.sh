#!/usr/bin/env bash
# Install MetalLB v0.14.8 (native manifest) and the L2 address pool.
# Needs a working kubectl (export KUBECONFIG=/etc/rancher/k3s/k3s.yaml on the VM).
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
VERSION=v0.14.8
export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

kubectl apply -f "https://raw.githubusercontent.com/metallb/metallb/$VERSION/config/manifests/metallb-native.yaml"

echo "waiting for MetalLB controller and speakers..."
kubectl -n metallb-system rollout status deployment/controller --timeout=180s
kubectl -n metallb-system rollout status daemonset/speaker --timeout=180s

# The validating webhook may lag behind the rollout; retry the pool a few times.
for i in $(seq 1 15); do
  if kubectl apply -f "$DIR/metallb-pool.yaml"; then
    echo "MetalLB ready: pool 192.168.0.240-192.168.0.250 (gateway VIP 192.168.0.240)"
    exit 0
  fi
  echo "webhook not ready yet, retry $i/15"; sleep 4
done
echo "failed to apply metallb-pool.yaml" >&2
exit 1
