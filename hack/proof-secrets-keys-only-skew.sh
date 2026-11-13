#!/usr/bin/env bash
# proof-secrets-keys-only-skew.sh — proves that a Secret plugin OLDER than
# LOK8S_INTERNAL_SECRETS_KEYS_ONLY emits nothing for a keys-only render: it
# knows LOK8S_SECRETS_DISABLE only, stops on it, reads no store and runs no
# generator. So a new lo with an old exec plugin (.kustomize/ not updated)
# renders no Secret at all, never one with values.
#
# For each release tag it builds kustomize/cmd/secret from the tag's own
# tree (git archive of the local objects; the Go module cache or the Go
# proxy for the tag's go.sum), then runs it the way `lo build --keys-only`
# calls it: LOK8S_SECRETS_DISABLE=1 LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1, the
# spec on argv[1], PATH_SECRETS at a store with a plaintext value. The spec
# has a passwd key (store), a bash key (a script that leaves a marker) and a
# required env key. The plugin must exit 0 with an empty stdout; the store
# must be unchanged and the marker absent. The working tree's plugin runs
# last as the contrast: it must emit the keys, each value empty.
#
# NOT in CI: the CI checkout is shallow (no tags). Run by hand; the result
# goes into the pull request. LOK8S_SECRETS_DISABLE came with v0.2.0: an
# older plugin reads the store on every run, keys-only or not.
#
# Usage: hack/proof-secrets-keys-only-skew.sh [tag...]   (default: v0.2.0 v0.14.1)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tags=("$@")
(( ${#tags[@]} )) || tags=(v0.2.0 v0.14.1)

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# snapshot <dir>: name, size and mtime of every file.
snapshot() { (cd "${1}" && find . -type f -printf '%p %s %T@\n' | LC_ALL=C sort); }

marker="${work}/bash-ran"
spec="${work}/spec.yaml"
cat > "${spec}" <<EOF
apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata:
  name: app
  namespace: demo
passwd:
  PASSWD: 32
bash:
  BASH: "touch ${marker}; echo from-bash"
env:
  ENV_REQUIRED: { var: SKEW_PROOF_UNSET }
EOF
store="${work}/store"
mkdir -p "${store}"
printf 'store-canary\n' > "${store}/Secret.app.demo.PASSWD"
before="$(snapshot "${store}")"

# run_plugin <bin> <out>: the keys-only call, in a minimal environment.
run_plugin() {
  local rc=0
  (cd "${work}" && env -i PATH=/usr/bin:/bin HOME="${work}" \
    LOK8S_SECRETS_DISABLE=1 LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 PATH_SECRETS="${store}" \
    "${1}" "${spec}" > "${2}" 2> "${2}.err") || rc=$?
  echo "${rc}"
}

# build_plugin <tree> <bin>: the plugin of a kustomize module tree.
build_plugin() {
  (cd "${1}/kustomize" && GOWORK=off GOFLAGS=-mod=readonly CGO_ENABLED=0 \
    go build -trimpath -o "${2}" ./cmd/secret)
}

printf '%-10s %-6s %-14s %-14s %s\n' "plugin" "rc" "stdout bytes" "store" "bash marker"
for tag in "${tags[@]}"; do
  git -C "${ROOT}" rev-parse --verify --quiet "refs/tags/${tag}^{commit}" >/dev/null \
    || fail "tag ${tag} is not in this clone (git fetch --tags)"
  src="${work}/src-${tag}"
  mkdir -p "${src}"
  git -C "${ROOT}" archive "${tag}" kustomize | tar -x -C "${src}"
  build_plugin "${src}" "${work}/Secret-${tag}" || fail "${tag}: the plugin does not build"
  out="${work}/out-${tag}"
  rc="$(run_plugin "${work}/Secret-${tag}" "${out}")"
  store_state=unchanged
  [[ "$(snapshot "${store}")" == "${before}" ]] || store_state=CHANGED
  marker_state=absent
  [[ ! -e "${marker}" ]] || marker_state=PRESENT
  printf '%-10s %-6s %-14s %-14s %s\n' "${tag}" "${rc}" "$(wc -c < "${out}")" "${store_state}" "${marker_state}"
  [[ "${rc}" == 0 ]] || fail "${tag}: exit ${rc}: $(cat "${out}.err")"
  [[ ! -s "${out}" ]] || fail "${tag}: stdout is not empty"
  [[ "${store_state}" == unchanged ]] || fail "${tag}: the store changed"
  [[ "${marker_state}" == absent ]] || fail "${tag}: the bash: script ran"
done

# The contrast: the working tree's plugin emits the keys, each empty.
build_plugin "${ROOT}" "${work}/Secret-tree" || fail "the working tree's plugin does not build"
out="${work}/out-tree"
rc="$(run_plugin "${work}/Secret-tree" "${out}")"
store_state=unchanged
[[ "$(snapshot "${store}")" == "${before}" ]] || store_state=CHANGED
marker_state=absent
[[ ! -e "${marker}" ]] || marker_state=PRESENT
printf '%-10s %-6s %-14s %-14s %s\n' "tree" "${rc}" "$(wc -c < "${out}")" "${store_state}" "${marker_state}"
[[ "${rc}" == 0 ]] || fail "tree: exit ${rc}: $(cat "${out}.err")"
for key in BASH ENV_REQUIRED PASSWD; do
  grep -qxF "  ${key}: \"\"" "${out}" || fail "tree: the key ${key} with an empty value is missing"
done
grep -qF 'secrets.lok8s.dev/keys-only: "true"' "${out}" || fail "tree: the keys-only annotation is missing"
[[ "${store_state}" == unchanged && "${marker_state}" == absent ]] || fail "tree: the store changed or the script ran"
echo
echo "proof: every older plugin emitted nothing; the tree's plugin emitted the keys, each empty"
