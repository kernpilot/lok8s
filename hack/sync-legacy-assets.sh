#!/usr/bin/env bash
# sync-legacy-assets.sh — keep the frozen bash tree's data files identical to
# the embedded mirror (internal/assets/lok8s/**, the canonical copy since the
# eject model). The bash implementation and the parity harnesses read
# .lok8s/** from disk, so the two must not diverge; the Go test
# TestEmbeddedMirrorMatchesLegacyTree fails on any byte difference.
#
#   hack/sync-legacy-assets.sh                 mirror  → .lok8s   (the normal direction)
#   hack/sync-legacy-assets.sh --from-legacy   .lok8s  → mirror   (after `lo crds generate`,
#                                                                  or when a fix landed in .lok8s first)
#   hack/sync-legacy-assets.sh --check         diff only; exit 1 on divergence (no Go needed)
#
# The subtree list is the one in internal/assets/drift_test.go (`mirrored`).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIRROR="${ROOT}/internal/assets/lok8s"
LEGACY="${ROOT}/.lok8s"

SUBTREES=(
  addons
  drivers/lo/cluster
  drivers/kubeone/cluster
  drivers/capi/cluster
  libs/inventory/manifests
  chat
  VERSION
)

mode="to-legacy"
case "${1:-}" in
  "") ;;
  --from-legacy) mode="from-legacy" ;;
  --check) mode="check" ;;
  -h|--help) sed -n '2,14p' "${BASH_SOURCE[0]}"; exit 0 ;;
  *) echo "error: unknown argument: $1" >&2; exit 2 ;;
esac

# copy_tree SRC DST — replace DST with a byte-identical copy of SRC (a file
# or a directory). Ejected .lo-origin markers never belong to either tree.
copy_tree() {
  local src="$1" dst="$2"
  [[ -e "${src}" ]] || { echo "error: missing source: ${src}" >&2; return 1; }
  rm -rf "${dst}"
  mkdir -p "$(dirname "${dst}")"
  cp -R "${src}" "${dst}"
  find "${dst}" -name .lo-origin -type f -delete 2>/dev/null || true
}

rc=0
for sub in "${SUBTREES[@]}"; do
  case "${mode}" in
    to-legacy)   copy_tree "${MIRROR}/${sub}" "${LEGACY}/${sub}" ;;
    from-legacy) copy_tree "${LEGACY}/${sub}" "${MIRROR}/${sub}" ;;
    check)
      if ! diff -r --exclude=.lo-origin "${MIRROR}/${sub}" "${LEGACY}/${sub}" >/dev/null 2>&1; then
        echo "drift: ${sub} (internal/assets/lok8s vs .lok8s)" >&2
        diff -r --exclude=.lo-origin "${MIRROR}/${sub}" "${LEGACY}/${sub}" >&2 || true
        rc=1
      fi
      ;;
  esac
done

case "${mode}" in
  to-legacy)   echo "synced internal/assets/lok8s -> .lok8s (${#SUBTREES[@]} subtrees)" ;;
  from-legacy) echo "synced .lok8s -> internal/assets/lok8s (${#SUBTREES[@]} subtrees)" ;;
  check)       (( rc == 0 )) && echo "in sync: internal/assets/lok8s == .lok8s (${#SUBTREES[@]} subtrees)" ;;
esac
exit "${rc}"
