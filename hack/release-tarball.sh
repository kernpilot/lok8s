#!/usr/bin/env bash
# hack/release-tarball.sh — build the framework tarball a lok8s release ships
# next to the binaries: lok8s-<version>.tar.gz with .lok8s/, operator/crds/,
# operator/deploy/ and (when built) .kustomize/ under a lok8s-<version>/ prefix.
#
# Called by goreleaser (before hook, see .goreleaser.yaml) with the release
# version; the result lands in .release/ (NOT dist/ — goreleaser requires an
# empty dist/ after its before hooks) and is attached via release.extra_files.
#
# Usage: hack/release-tarball.sh <version>      e.g. v0.3.0
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

version="${1:-}"
[[ -n "${version}" ]] || { echo "usage: hack/release-tarball.sh <version>" >&2; exit 2; }

out_dir=".release"
out="${out_dir}/lok8s-${version}.tar.gz"
mkdir -p "${out_dir}"
rm -f "${out_dir}"/lok8s-*.tar.gz

# .kustomize/ holds the just-built plugins; include it only if present so a
# clean checkout never fails the tar on a missing path (same rule as the
# pre-goreleaser release workflow).
paths=(.lok8s/ operator/crds/ operator/deploy/)
[[ -d .kustomize ]] && paths+=(.kustomize/)

# --transform is GNU tar: the release runs on ubuntu; a local `make snapshot`
# on macOS needs gnu-tar (gtar) on PATH ahead of bsdtar.
tar -czf "${out}" "${paths[@]}" --transform "s,^,lok8s-${version}/,"
echo "release-tarball: ${out} ($(wc -c <"${out}") bytes)"
