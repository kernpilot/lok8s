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
ROOT_GIB="${CEPH_OSD_ROOT_GIB:-40}"

# Already carved? (a bluestore partition beyond the OS partitions)
if lsblk -rno FSTYPE "${DEV}" 2>/dev/null | grep -q 'ceph_bluestore'; then
  echo "lok8s/ceph-osd: an OSD partition already exists on ${DEV} — skipping"; exit 0
fi

TABLE="$(blkid -o value -s PTTYPE "${DEV}" 2>/dev/null || true)"
case "${TABLE}" in
  gpt)
    BASE="$(basename "${DEV}")"
    if lsblk -rno NAME "${DEV}" 2>/dev/null | grep -qx "${BASE}2"; then
      echo "lok8s/ceph-osd: ${DEV}2 already exists — skipping"; exit 0
    fi
    echo "lok8s/ceph-osd: GPT ${DEV} — carve OSD (root=${ROOT_GIB}GiB) + grow root"
    sgdisk -e "${DEV}"                                              # backup GPT → disk end, full size usable
    sgdisk -n "0:${ROOT_GIB}GiB:0" -t "0:8300" -c "0:rook-osd" "${DEV}"
    partprobe "${DEV}" 2>/dev/null || true; sleep 1
    growpart "${DEV}" 1
    resize2fs "${DEV}1"
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
