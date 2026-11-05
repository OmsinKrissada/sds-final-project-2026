#!/usr/bin/env bash
# Prepare a Raspberry Pi for k3s: memory cgroups, no swap, hostname.
#   sudo cluster/pi-prep.sh [hostname]      e.g. sudo cluster/pi-prep.sh pi1
# Idempotent. Reboot afterwards if it says so.
set -euo pipefail

if [ "$(id -u)" != 0 ]; then echo "run as root: sudo $0 [hostname]" >&2; exit 1; fi
NEWNAME="${1:-}"
REBOOT=0

# 1. memory cgroups on the kernel command line (must stay ONE line)
if [ -f /boot/firmware/cmdline.txt ]; then CMDLINE=/boot/firmware/cmdline.txt
elif [ -f /boot/cmdline.txt ]; then CMDLINE=/boot/cmdline.txt
else echo "no cmdline.txt under /boot or /boot/firmware" >&2; exit 1; fi
if ! grep -q 'cgroup_enable=memory' "$CMDLINE" || ! grep -q 'cgroup_memory=1' "$CMDLINE"; then
  cp -n "$CMDLINE" "$CMDLINE.bak" || true
  # strip trailing whitespace/newline, append flags once, keep a single line
  line="$(tr -d '\n' < "$CMDLINE" | sed -e 's/[[:space:]]*$//' -e 's/ cgroup_enable=memory//g' -e 's/ cgroup_memory=1//g')"
  printf '%s cgroup_memory=1 cgroup_enable=memory\n' "$line" > "$CMDLINE"
  echo "updated $CMDLINE (backup: $CMDLINE.bak)"
  REBOOT=1
else
  echo "cgroup flags already present in $CMDLINE"
fi

# 2. swap off (now and permanently)
swapoff -a || true
if command -v dphys-swapfile >/dev/null 2>&1; then
  dphys-swapfile swapoff || true
  dphys-swapfile uninstall || true
  systemctl disable --now dphys-swapfile 2>/dev/null || true
fi
[ -f /etc/fstab ] && sed -i -E 's/^([^#].*[[:space:]]swap[[:space:]].*)$/# \1/' /etc/fstab
# systemd swap units (Ubuntu images)
systemctl mask swap.target 2>/dev/null || true
echo "swap disabled"

# 3. hostname (node names must be unique in the cluster)
if [ -n "$NEWNAME" ] && [ "$(hostname)" != "$NEWNAME" ]; then
  hostnamectl set-hostname "$NEWNAME"
  if grep -q '^127\.0\.1\.1' /etc/hosts; then
    sed -i -E "s/^127\.0\.1\.1.*/127.0.1.1\t$NEWNAME/" /etc/hosts
  else
    printf '127.0.1.1\t%s\n' "$NEWNAME" >> /etc/hosts
  fi
  echo "hostname set to $NEWNAME"
fi

if [ "$REBOOT" = 1 ]; then
  echo "REBOOT REQUIRED for cgroup changes: sudo reboot"
else
  echo "pi-prep done; no reboot needed. Next: cluster/agent.sh"
fi
