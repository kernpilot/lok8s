#!/bin/bash
# lok8s/ceph-osd — carve a raw Ceph OSD partition on a node's OS disk.
#
# Handles both node shapes:
#   • Cloud VM (GPT): root fills the disk after cloud-init growpart, so nodes with
#     this module also get `growpart: {mode: "off"}`; here we reclaim the full disk,
#     carve an OSD partition past a fixed-size root, then grow root into the gap.
#   • Bare-metal (installimage, MBR/msdos): root is already sized by installimage;
#     we add ONE logical partition spanning the free tail (no root grow).
#
# ⛔ GPT label must NOT contain the substring "ceph" — ceph-volume rejects any
# partition labelled like a legacy ceph-disk member ("Used by ceph-disk"); the
# label is `rook-osd`. The partition is left RAW for Rook (deviceFilter/devices).
# Idempotent: no-op if an OSD partition already exists.
set -euo pipefail

# OS disk: explicit override, else the disk carrying / (handles nvmeXnYpZ and sdXN).
DEV="${CEPH_OSD_DEVICE:-}"
if [[ -z "${DEV}" ]]; then
  ROOT_SRC="$(findmnt -n -o SOURCE / 2>/dev/null || true)"
  DEV="$(printf '%s' "${ROOT_SRC}" | sed -E 's/p?[0-9]+$//')"
fi
[[ -b "${DEV}" ]] || { echo "lok8s/ceph-osd: OS disk '${DEV:-?}' is not a block device — skipping"; exit 0; }
ROOT_GIB="${CEPH_OSD_ROOT_GIB:-60}"   # cloud-VM root grows to this; 60 leaves headroom for containerd

# Already carved? (a bluestore partition beyond the OS partitions)
if lsblk -rno FSTYPE "${DEV}" 2>/dev/null | grep -q 'ceph_bluestore'; then
  echo "lok8s/ceph-osd: an OSD partition already exists on ${DEV} — skipping"; exit 0
fi

TABLE="$(blkid -o value -s PTTYPE "${DEV}" 2>/dev/null || true)"
case "${TABLE}" in
  gpt)
    # Idempotent + shape-agnostic: skip if our labelled OSD partition already exists.
    if lsblk -rno PARTLABEL "${DEV}" 2>/dev/null | grep -qx 'rook-osd'; then
      echo "lok8s/ceph-osd: a rook-osd partition already exists on ${DEV} — skipping"; exit 0
    fi
    sgdisk -e "${DEV}"                                              # backup GPT → disk end, full size usable
    # Cloud VM = one small root to grow; bare-metal installimage = several large OS
    # partitions already sized (carve OSD in the free tail). Count only DATA partitions
    # >2GiB so the GPT bios_grub (~1M) + EFI (~256M) helpers don't count — otherwise a
    # cloud VM's sda1+sda14+sda15 looked like "bare-metal", root never grew, and the CPs
    # were left on the image's ~4GiB root → DiskPressure → evicted apiserver.
    NBIG="$(lsblk -rbno SIZE,TYPE "${DEV}" 2>/dev/null | awk '$2=="part" && $1+0 > 2147483648 {n++} END{print n+0}')"
    if [[ "${NBIG}" -le 1 ]]; then
      echo "lok8s/ceph-osd: GPT cloud ${DEV} — carve OSD past ${ROOT_GIB}GiB root + grow root"
      sgdisk -n "0:${ROOT_GIB}GiB:0" -t "0:8300" -c "0:rook-osd" "${DEV}"
      partprobe "${DEV}" 2>/dev/null || true; sleep 1
      growpart "${DEV}" 1
      resize2fs "${DEV}1"
    else
      echo "lok8s/ceph-osd: GPT bare-metal ${DEV} (${NBIG} data parts) — carve OSD in the free tail"
      sgdisk -n "0:0:0" -t "0:8300" -c "0:rook-osd" "${DEV}"        # next free num, largest free block
      partprobe "${DEV}" 2>/dev/null || true; sleep 1
    fi
    ;;
  dos|msdos)
    echo "lok8s/ceph-osd: MBR ${DEV} — add a raw OSD partition in the free tail"
    # Start right after the last existing partition (parted machine output: num:start:end:…).
    LAST_END="$(parted -sm "${DEV}" unit MB print 2>/dev/null | awk -F: 'NR>2 && $1 ~ /^[0-9]+$/ {e=$3} END{print e}')"
    LAST_END="${LAST_END%MB}"
    [[ -n "${LAST_END}" ]] || LAST_END="$(( ROOT_GIB * 1000 ))"
    parted -s -a optimal "${DEV}" mkpart logical "${LAST_END}MB" 100%
    partprobe "${DEV}" 2>/dev/null || true; sleep 1
    NP="$(lsblk -rno NAME "${DEV}" | tail -1)"
    wipefs -a "/dev/${NP}" 2>/dev/null || true
    ;;
  *)
    echo "lok8s/ceph-osd: unrecognised partition table '${TABLE:-none}' on ${DEV} — skipping"; exit 0
    ;;
esac
echo "lok8s/ceph-osd: done"
lsblk -o NAME,FSTYPE,SIZE,TYPE "${DEV}" 2>/dev/null || true
