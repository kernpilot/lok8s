#!/usr/bin/env bash
# sync-legacy-assets.sh — keep the frozen bash tree (.lok8s/**) identical to
# the embedded mirror (internal/assets/lok8s/**, the canonical copy of the
# WHOLE tree since the eject model: the data half and the bash
# implementation). The parity harnesses read .lok8s/** from disk, so the two
# must not diverge; the Go test TestEmbeddedMirrorMatchesLegacyTree fails on
# any byte or executable-bit difference.
#
#   hack/sync-legacy-assets.sh                 mirror  → .lok8s   (the normal direction)
#   hack/sync-legacy-assets.sh --from-legacy   .lok8s  → mirror   (after `lo crds generate`,
#                                                                  or when a fix landed in .lok8s first)
#   hack/sync-legacy-assets.sh --check         diff only; exit 1 on divergence (no Go needed)
#
# The entry list is the one in internal/assets/assets_test.go (`mirrored`):
# every top-level entry of .lok8s. A new top-level entry is added to both.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MIRROR="${ROOT}/internal/assets/lok8s"
LEGACY="${ROOT}/.lok8s"

SUBTREES=(
  README.md
  VERSION
  addons
  chat
  drivers
  libs
  lo
  providers
  tilt
  utils
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
# or a directory), modes included (cp keeps the executable bit). Ejected
# .lo-origin markers never belong to either tree.
#
# The replace is a wipe + copy, so anything under DST that git does not
# track would go with it. Refuse in that case: an untracked file there is
# local work (an addon being drafted, a scratch values file), not drift.
copy_tree() {
  local src="$1" dst="$2"
  [[ -e "${src}" ]] || { echo "error: missing source: ${src}" >&2; return 1; }
  if [[ -e "${dst}" ]]; then
    local untracked
    untracked="$(git -C "${ROOT}" ls-files --others --exclude-standard -- "${dst}" | grep -v '/\.lo-origin$' || true)"
    if [[ -n "${untracked}" ]]; then
      echo "error: refusing to replace ${dst#"${ROOT}"/}: untracked files would be deleted:" >&2
      while IFS= read -r line; do printf '  %s\n' "${line}"; done <<<"${untracked}" >&2
      echo "       commit, move or delete them first" >&2
      return 1
    fi
  fi
  rm -rf "${dst}"
  mkdir -p "$(dirname "${dst}")"
  cp -R "${src}" "${dst}"
  find "${dst}" -name .lo-origin -type f -delete 2>/dev/null || true
}

# Every COMMITTED top-level entry on either side must be listed: an entry
# that is not would silently stay out of the mirror (and out of the
# binary). The committed tree (git ls-tree HEAD): untracked or staged
# files are local work or toolchain droppings, not tree.
listed() { local e; for e in "${SUBTREES[@]}"; do [[ "${e}" == "${1}" ]] && return 0; done; return 1; }
rc=0
for side in "${MIRROR}" "${LEGACY}"; do
  while IFS= read -r name; do
    [[ -n "${name}" ]] || continue
    if ! listed "${name}"; then
      echo "error: ${side#"${ROOT}"/}/${name} is not in SUBTREES (add it to hack/sync-legacy-assets.sh and assets_test.go)" >&2
      rc=1
    fi
  done < <(git -C "${ROOT}" ls-tree --name-only HEAD -- "${side#"${ROOT}"/}/" | sed -e "s|^${side#"${ROOT}"/}/||")
done
(( rc == 0 )) || exit "${rc}"
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
      # Executable bits: the same set of +x files on both sides.
      if ! diff <(cd "${MIRROR}" && find "${sub}" -type f -perm -u+x | sort) \
                <(cd "${LEGACY}" && find "${sub}" -type f -perm -u+x | sort) >/dev/null 2>&1; then
        echo "drift: ${sub} executable bits differ (internal/assets/lok8s vs .lok8s)" >&2
        diff <(cd "${MIRROR}" && find "${sub}" -type f -perm -u+x | sort) \
             <(cd "${LEGACY}" && find "${sub}" -type f -perm -u+x | sort) >&2 || true
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
