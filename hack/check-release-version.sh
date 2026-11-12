#!/usr/bin/env bash
# check-release-version.sh — a release tag must name the version that the
# tree carries: internal/assets/lok8s/VERSION (the embedded mirror: the bash
# tree inside the binary, which `lo assets eject bash` writes) and its synced
# twin .lok8s/VERSION (the frozen tree and the source archive).
#
# v0.14.0 was tagged before its version references were bumped: the binary
# reported 0.14.0 (ldflags), but its bash tree carried VERSION 0.13.0, so a
# bash-routed `lo version` and the inventory it wrote said 0.13.0. The release
# workflow runs this before goreleaser and fails the release on a mismatch.
#
#   hack/check-release-version.sh v0.14.1
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tag="${1:?usage: check-release-version.sh <tag, e.g. v0.14.1>}"
if [[ ! "${tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "check-release-version: '${tag}' is not a release tag (vMAJOR.MINOR.PATCH)" >&2
  exit 1
fi
want="${tag#v}"

fail=0
for f in internal/assets/lok8s/VERSION .lok8s/VERSION; do
  got=""
  [[ -f "${ROOT}/${f}" ]] && got="$(tr -d '[:space:]' < "${ROOT}/${f}")"
  if [[ "${got}" != "${want}" ]]; then
    echo "check-release-version: ${f} reads '${got}', the tag ${tag} needs '${want}'" >&2
    fail=1
  fi
done
if (( fail )); then
  echo "  Set internal/assets/lok8s/VERSION, run hack/sync-legacy-assets.sh, and tag that commit." >&2
  exit 1
fi
echo "check-release-version: ${tag} equals both VERSION files"
