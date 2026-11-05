#!/usr/bin/env bash
# Join a Raspberry Pi (arm64) to the k3s cluster as an agent.
#   sudo cluster/agent.sh https://192.168.0.10:6443 <token>
#   (or K3S_URL=... K3S_TOKEN=... sudo -E cluster/agent.sh)
# Optional: IFACE=eth0 to pin flannel to the wired interface.
set -euo pipefail

if [ "$(id -u)" != 0 ]; then echo "run as root: sudo -E $0 ..." >&2; exit 1; fi
K3S_URL="${1:-${K3S_URL:-}}"
K3S_TOKEN="${2:-${K3S_TOKEN:-}}"
if [ -z "$K3S_URL" ] || [ -z "$K3S_TOKEN" ]; then
  echo "usage: $0 <K3S_URL e.g. https://192.168.0.10:6443> <K3S_TOKEN>" >&2; exit 1
fi

# --- memory cgroup check ----------------------------------------------------
# k3s/kubelet needs the memory cgroup controller. On Raspberry Pi OS and Ubuntu
# for Pi it is disabled by default and must be enabled on the kernel command
# line. cluster/pi-prep.sh does that; a reboot is required afterwards.
mem_enabled="$(awk '$1=="memory"{print $4}' /proc/cgroups 2>/dev/null || true)"
if [ "$mem_enabled" != "1" ]; then
  if [ -f /boot/firmware/cmdline.txt ]; then CMDLINE=/boot/firmware/cmdline.txt   # Bookworm / Ubuntu 22.04+
  elif [ -f /boot/cmdline.txt ]; then CMDLINE=/boot/cmdline.txt                  # older Raspberry Pi OS
  else CMDLINE="(cmdline.txt not found)"; fi
  cat >&2 <<MSG
ERROR: the memory cgroup is not enabled (/proc/cgroups shows memory=$mem_enabled).
Fix: run cluster/pi-prep.sh, or append this to the SINGLE line in $CMDLINE:
    cgroup_memory=1 cgroup_enable=memory
then reboot and re-run this script. (Raspberry Pi OS: /boot/firmware/cmdline.txt on
Bookworm, /boot/cmdline.txt on Bullseye; Ubuntu for Pi: /boot/firmware/cmdline.txt.)
MSG
  exit 1
fi
if swapon --show | grep -q .; then
  echo "WARNING: swap is active; kubelet may refuse to start. Run cluster/pi-prep.sh." >&2
fi

FLANNEL_ARGS=()
[ -n "${IFACE:-}" ] && FLANNEL_ARGS=(--flannel-iface "$IFACE")

curl -sfL https://get.k3s.io | K3S_URL="$K3S_URL" K3S_TOKEN="$K3S_TOKEN" INSTALL_K3S_EXEC="agent" sh -s - \
  --kubelet-arg node-status-update-frequency=5s \
  "${FLANNEL_ARGS[@]}"

echo "k3s-agent installed; check on the server with: kubectl get nodes -o wide"
