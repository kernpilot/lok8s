#!/bin/bash
# lok8s/ceph-osd — carve a Ceph OSD partition on a Hetzner CLOUD VM's boot disk.
#
# Cloud VMs (unlike the bare-metal worker's installimage) boot from a stock
# image whose root partition fills the disk after cloud-init's growpart. Nodes
# with this cloud.d module get `growpart: {mode: "off"}` in cloud-config, so the
# root fs stays at the image base; here we reclaim the full disk, create an OSD
# partition spanning everything past a fixed-size root, then grow root into the
# gap. The partition is left RAW for Rook (deviceFilter / devices).
#
# ⛔ PARTITION LABEL must NOT contain the substring "ceph": ceph-volume's
# inventory flags any partition whose GPT name contains "ceph" as a legacy
# ceph-disk member and REJECTS it ("Used by ceph-disk") → Rook never creates the
# OSD (hit live 2026-06-15 with label "ceph-osd"). So the label is `rook-osd`.
#
# Validated on cx43 / ubuntu-24.04 (root grows online; the OSD partition's
# kernel re-read happens at the next boot/partprobe — Rook consumes it then).
# Idempotent: no-op if the partition already exists.
set -euo pipefail
DEV="${CEPH_OSD_DEVICE:-/dev/sda}"
ROOT_GIB="${CEPH_OSD_ROOT_GIB:-40}"
base="$(basename "${DEV}")"

if lsblk -rno NAME "${DEV}" 2>/dev/null | grep -qx "${base}2"; then
  echo "lok8s/ceph-osd: ${DEV}2 already exists — skipping"
  exit 0
fi

echo "lok8s/ceph-osd: carving OSD partition on ${DEV} (root=${ROOT_GIB}GiB, rest=rook-osd)"
sgdisk -e "${DEV}"                                                # move backup GPT to disk end → full size usable
sgdisk -n "0:${ROOT_GIB}GiB:0" -t "0:8300" -c "0:rook-osd" "${DEV}"   # OSD partition: rootGiB → end (label avoids "ceph")
partprobe "${DEV}" 2>/dev/null || true
sleep 1
growpart "${DEV}" 1                                               # grow root partition into the base→rootGiB gap
resize2fs "${DEV}1"                                              # online-grow the (mounted) root fs
echo "lok8s/ceph-osd: done"
lsblk -o NAME,FSTYPE,SIZE,TYPE "${DEV}" || true
