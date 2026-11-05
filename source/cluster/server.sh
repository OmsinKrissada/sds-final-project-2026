#!/usr/bin/env bash
# Install the k3s server on the Ubuntu VM (run with sudo / as root).
#   IFACE=ens18 sudo -E cluster/server.sh
# The server is tainted CriticalAddonsOnly:NoExecute so no app pod runs on it;
# traefik and servicelb are disabled (MetalLB provides LoadBalancer IPs).
set -euo pipefail

if [ "$(id -u)" != 0 ]; then echo "run as root: sudo -E $0" >&2; exit 1; fi

IFACE="${IFACE:-$(ip -4 route show default | awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}')}"
[ -n "$IFACE" ] || { echo "cannot detect LAN interface; set IFACE=<name>" >&2; exit 1; }
IP="$(ip -4 -o addr show dev "$IFACE" | awk '{print $4}' | cut -d/ -f1 | head -1)"
echo "using interface $IFACE ($IP)"

curl -sfL https://get.k3s.io | INSTALL_K3S_EXEC="server" sh -s - \
  --disable traefik \
  --disable servicelb \
  --node-taint CriticalAddonsOnly=true:NoExecute \
  --flannel-iface "$IFACE" \
  --kube-controller-manager-arg node-monitor-grace-period=20s \
  --write-kubeconfig-mode 644

echo "waiting for node to register..."
for _ in $(seq 1 60); do
  k3s kubectl get nodes >/dev/null 2>&1 && break
  sleep 2
done
k3s kubectl get nodes

TOKEN="$(cat /var/lib/rancher/k3s/server/node-token)"
cat <<MSG

=== k3s server ready ===
Kubeconfig : /etc/rancher/k3s/k3s.yaml   (export KUBECONFIG=/etc/rancher/k3s/k3s.yaml)
Join token : $TOKEN

On each Raspberry Pi (after cluster/pi-prep.sh and a reboot):
  sudo cluster/agent.sh https://$IP:6443 $TOKEN
or from this repo: make cluster-agent K3S_URL=https://$IP:6443 K3S_TOKEN=$TOKEN
MSG
