#!/usr/bin/env bash
# proof-secrets-keys-only-skew.sh — proves that `lo build --keys-only`
# refuses every Secret plugin released before the keys-only run, and that
# its check of the plugin does nothing with any of them.
#
# For each release tag that ships the plugin (kustomize/cmd/secret), the
# script builds the plugin from the tag's own tree (git archive of the local
# objects; the Go module cache or the Go proxy for the tag's go.sum). Then:
#
# 1. The probe, the way lo runs it (Go: checkKeysOnlyPlugin; bash:
#    build::_keys_only_capable): `Secret --capabilities` in an empty
#    temporary directory, with an empty stdin, PATH_SECRETS at a path in
#    that directory that does not exist, LOK8S_SECRETS_DISABLE=1 and
#    LOK8S_INTERNAL_SECRETS_KEYS_ONLY=0. A second run points PATH_SECRETS
#    at a store with a plaintext value. The plugin must not print the line
#    keys-only. The directory and HOME must stay empty, and the store must
#    not change.
# 2. lo refuses the plugin: a fixture project with the plugin in its
#    .kustomize/ (a wrapper logs each call) and a kustomize stub that logs
#    a build. `lo build --keys-only` in the Go exec pipeline (LO_RENDER=exec)
#    and in the bash tree must fail with the refusal, print nothing on
#    stdout and never start the kustomize build. The plugin gets the probe
#    only, and the domain's store does not change.
# 3. The keys-only call without the probe (LOK8S_SECRETS_DISABLE=1
#    LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1, the spec on argv[1]), the second
#    line of defence. A plugin with the store-free switch (v0.2.0 on) stops
#    on it and emits nothing. A plugin without the switch would read the
#    store; only the probe keeps lo from running it, so this call is not
#    made for it.
#
# The working tree's plugin runs last as the contrast: the probe prints
# keys-only, lo starts the render, and the keys-only call emits each key
# with an empty value.
#
# NOT in CI: the CI checkout is shallow (no tags). Run by hand; the result
# goes into the pull request.
#
# Usage: hack/proof-secrets-keys-only-skew.sh [tag...]
#   default: every tag that ships kustomize/cmd/secret. LO_BIN names the Go
#   lo to drive (default: bin/lo; make build).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LO_BIN="${LO_BIN:-${ROOT}/bin/lo}"
[[ -x "${LO_BIN}" ]] || { echo "error: ${LO_BIN} not built (make build)" >&2; exit 2; }
[[ -x "${ROOT}/.bin/kustomize" ]] || { echo "error: the pinned toolchain is missing under ${ROOT}/.bin (b install)" >&2; exit 2; }
tags=("$@")
if (( ${#tags[@]} == 0 )); then
  while read -r tag; do
    if git -C "${ROOT}" cat-file -e "${tag}:kustomize/cmd/secret/main.go" 2>/dev/null; then
      tags+=("${tag}")
    fi
  done < <(git -C "${ROOT}" tag -l 'v*' | sort -V)
fi
(( ${#tags[@]} )) || { echo "error: no release tag in this clone (git fetch --tags)" >&2; exit 2; }

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# snapshot <dir>: name, size and mtime of every file.
snapshot() { (cd "${1}" && find . -type f -printf '%p %s %T@\n' | LC_ALL=C sort); }

# build_plugin <tree> <bin>: the plugin of a kustomize module tree.
build_plugin() {
  (cd "${1}/kustomize" && GOWORK=off GOFLAGS=-mod=readonly CGO_ENABLED=0 \
    go build -trimpath -o "${2}" ./cmd/secret)
}

# The spec of the keys-only call (check 3): a passwd key (the store), a
# bash key (a script that leaves a marker) and a required env key.
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
store_before="$(snapshot "${store}")"

# probe <bin> <store|-> <out>: check 1, one run. "-" puts PATH_SECRETS at
# a path in the probe directory that does not exist (as lo does). Prints
# "<rc> <state>": the state is "clean" when the probe directory and HOME
# are empty after the run and the store did not change.
probe() {
  local bin="${1}" st="${2}" out="${3}" dir home rc=0 state=clean
  dir="$(mktemp -d "${work}/probe-XXXXXX")"
  home="$(mktemp -d "${work}/home-XXXXXX")"
  [[ "${st}" != "-" ]] || st="${dir}/store"
  (cd "${dir}" && env -i PATH=/usr/bin:/bin HOME="${home}" TMPDIR="${dir}" \
    PATH_SECRETS="${st}" LOK8S_SECRETS_DISABLE=1 LOK8S_INTERNAL_SECRETS_KEYS_ONLY=0 \
    "${bin}" --capabilities < /dev/null > "${out}" 2> "${out}.err") || rc=$?
  [[ -z "$(ls -A "${dir}")" && -z "$(ls -A "${home}")" ]] || state=WROTE
  [[ "$(snapshot "${store}")" == "${store_before}" ]] || state=STORE-CHANGED
  rm -rf "${dir}" "${home}"
  echo "${rc} ${state}"
}

# The fixture project for check 2: one domain with the generator and a
# store of its own, the framework tree copied (the bash tree must resolve
# inside the project), the toolchain linked entry by entry, and kustomize
# replaced by a stub that logs a build and fails it (other calls go to the
# pinned kustomize).
proj="${work}/proj"
mkdir -p "${proj}/clusters/d.dev/secrets" "${proj}/.bin" "${proj}/.kustomize/secrets.lok8s.dev/v1/secret" "${work}/lo-home"
cp -R "${ROOT}/.lok8s" "${proj}/.lok8s"
for entry in "${ROOT}"/.bin/*; do ln -s "${entry}" "${proj}/.bin/${entry##*/}"; done
rm -f "${proj}/.bin/kustomize"
cat > "${proj}/.bin/kustomize" <<EOF
#!/usr/bin/env bash
if [[ "\${1:-}" == build ]]; then
  echo "kustomize \$*" >> "${work}/kustomize.log"
  exit 1
fi
exec "${ROOT}/.bin/kustomize" "\$@"
EOF
chmod +x "${proj}/.bin/kustomize"
printf 'kind: Lo\nmetadata:\n  name: d\n' > "${proj}/clusters/d.dev/cluster.lok8s.yaml"
printf 'resources:\n  - cm.yaml\ngenerators:\n  - secret.yaml\n' > "${proj}/clusters/d.dev/kustomization.yaml"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: demo\n' > "${proj}/clusters/d.dev/cm.yaml"
cp "${spec}" "${proj}/clusters/d.dev/secret.yaml"
printf 'store-canary\n' > "${proj}/clusters/d.dev/secrets/Secret.app.demo.PASSWD"
proj_store_before="$(snapshot "${proj}/clusters/d.dev/secrets")"

# lo_keys_only <plugin> <impl>: check 2, one run. Installs a wrapper that
# logs each call and runs <plugin>, selects the implementation and runs
# `lo build --keys-only`. Prints "<rc> <stdout bytes> <kustomize builds>
# <store>". The plugin calls are in ${work}/plugin.log, the stderr in
# ${work}/lo.err.
lo_keys_only() {
  local plugin="${1}" impl="${2}" rc=0 st=unchanged
  cat > "${proj}/.kustomize/secrets.lok8s.dev/v1/secret/Secret" <<EOF
#!/usr/bin/env bash
echo "plugin \$*" >> "${work}/plugin.log"
exec "${plugin}" "\$@"
EOF
  chmod +x "${proj}/.kustomize/secrets.lok8s.dev/v1/secret/Secret"
  printf 'apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: skew\nspec:\n  implementation:\n    default: %s\n' "${impl}" \
    > "${proj}/lok8s.yaml"
  : > "${work}/plugin.log"
  : > "${work}/kustomize.log"
  (cd "${proj}" && env -i PATH="${proj}/.bin:/usr/bin:/bin" HOME="${work}/lo-home" TMPDIR="${work}" LC_ALL=C \
    KUBECONFIG="${work}/kubeconfig.absent" LO_RENDER=exec \
    "${LO_BIN}" build --domain d.dev --keys-only < /dev/null > "${work}/lo.out" 2> "${work}/lo.err") || rc=$?
  [[ "$(snapshot "${proj}/clusters/d.dev/secrets")" == "${proj_store_before}" ]] || st=CHANGED
  echo "${rc} $(wc -c < "${work}/lo.out") $(wc -l < "${work}/kustomize.log") ${st}"
}

# keys_only_call <bin> <out>: check 3, the keys-only call itself.
keys_only_call() {
  local rc=0
  (cd "${work}" && env -i PATH=/usr/bin:/bin HOME="${work}" \
    LOK8S_SECRETS_DISABLE=1 LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 PATH_SECRETS="${store}" \
    "${1}" "${spec}" > "${2}" 2> "${2}.err") || rc=$?
  echo "${rc}"
}

# check_refused <tag> <side> <rc> <out bytes> <builds> <store>: check 2's
# verdict, with the plugin calls and the stderr of that run. It sets
# verdict; it is never run in a command substitution, so a fail ends the
# script.
check_refused() {
  local tag="${1}" side="${2}" rc="${3}" out="${4}" builds="${5}" st="${6}" calls
  calls="$(paste -sd, "${work}/plugin.log")"
  if [[ "${rc}" == 0 ]] || ! grep -qF "${refusal}" "${work}/lo.err"; then
    fail "${tag}: lo (${side}) did not refuse the plugin: rc ${rc}: $(cat "${work}/lo.err")"
  fi
  [[ "${out}" == 0 ]] || fail "${tag}: lo (${side}) printed something on stdout"
  [[ "${calls}" == "plugin --capabilities" ]] || fail "${tag}: lo (${side}) ran the plugin as: ${calls:-<never>}"
  [[ "${builds}" == 0 ]] || fail "${tag}: lo (${side}) started the kustomize build"
  [[ "${st}" == unchanged ]] || fail "${tag}: lo (${side}) changed the domain's store"
  verdict="${rc} refused ${calls}"
}

refusal="does not have the keys-only run"
row='%-8s | %-24s | %-24s | %-36s | %-36s | %s\n'
# shellcheck disable=SC2059 # the format is the row layout
printf "${row}" "plugin" "probe: rc state stdout" "probe with a store" "lo build --keys-only (Go, exec)" "lo build --keys-only (bash)" "keys-only call: rc stdout store script"
for tag in "${tags[@]}"; do
  git -C "${ROOT}" rev-parse --verify --quiet "refs/tags/${tag}^{commit}" >/dev/null \
    || fail "tag ${tag} is not in this clone (git fetch --tags)"
  src="${work}/src-${tag}"
  mkdir -p "${src}"
  git -C "${ROOT}" archive "${tag}" kustomize | tar -x -C "${src}"
  bin="${work}/Secret-${tag}"
  build_plugin "${src}" "${bin}" || fail "${tag}: the plugin does not build"

  # 1. The probe, without and with a store.
  read -r p1rc p1state < <(probe "${bin}" - "${work}/probe1.out")
  read -r p2rc p2state < <(probe "${bin}" "${store}" "${work}/probe2.out")
  for o in "${work}/probe1.out" "${work}/probe2.out"; do
    ! grep -qx 'keys-only' "${o}" || fail "${tag}: the probe printed keys-only"
  done
  [[ "${p1state}" == clean && "${p2state}" == clean ]] \
    || fail "${tag}: the probe left a file or changed the store (${p1state}, ${p2state})"
  p1="${p1rc} ${p1state} $(paste -sd, "${work}/probe1.out")"
  p2="${p2rc} ${p2state} $(paste -sd, "${work}/probe2.out")"

  # 2. lo refuses, in both implementations.
  read -r rc out builds st < <(lo_keys_only "${bin}" go)
  check_refused "${tag}" go "${rc}" "${out}" "${builds}" "${st}"
  g="${verdict}"
  read -r rc out builds st < <(lo_keys_only "${bin}" bash)
  check_refused "${tag}" bash "${rc}" "${out}" "${builds}" "${st}"
  b="${verdict}"

  # 3. The keys-only call, for a plugin with the store-free switch.
  k="not made: no store-free switch"
  if git -C "${ROOT}" grep -q LOK8S_SECRETS_DISABLE "${tag}" -- kustomize/; then
    out="${work}/out-${tag}"
    k_rc="$(keys_only_call "${bin}" "${out}")"
    k_st=unchanged; [[ "$(snapshot "${store}")" == "${store_before}" ]] || k_st=CHANGED
    k_m=absent; [[ ! -e "${marker}" ]] || k_m=PRESENT
    [[ "${k_rc}" == 0 ]] || fail "${tag}: the keys-only call exits ${k_rc}: $(cat "${out}.err")"
    [[ ! -s "${out}" ]] || fail "${tag}: the keys-only call printed something"
    [[ "${k_st}" == unchanged && "${k_m}" == absent ]] || fail "${tag}: the keys-only call changed the store or ran the script"
    k="${k_rc} $(wc -c < "${out}") ${k_st} ${k_m}"
  fi
  # shellcheck disable=SC2059
  printf "${row}" "${tag}" "${p1}" "${p2}" "${g}" "${b}" "${k}"
done

# The contrast: the working tree's plugin.
bin="${work}/Secret-tree"
build_plugin "${ROOT}" "${bin}" || fail "the working tree's plugin does not build"
read -r p1rc p1state < <(probe "${bin}" - "${work}/probe1.out")
grep -qx 'keys-only' "${work}/probe1.out" && [[ "${p1rc}" == 0 && "${p1state}" == clean ]] \
  || fail "tree: the probe did not print keys-only, or it left a file (rc ${p1rc}, ${p1state})"
for impl in go bash; do
  read -r rc _ builds _ < <(lo_keys_only "${bin}" "${impl}")
  calls="$(paste -sd, "${work}/plugin.log")"
  # The stub fails the kustomize build: lo got past the probe.
  [[ "${calls}" == "plugin --capabilities" && "${builds}" == 1 ]] \
    || fail "tree: lo (${impl}) did not start the render after the probe (plugin calls ${calls}, builds ${builds}, rc ${rc})"
  ! grep -qF "${refusal}" "${work}/lo.err" || fail "tree: lo (${impl}) refused the plugin"
done
out="${work}/out-tree"
rc="$(keys_only_call "${bin}" "${out}")"
[[ "${rc}" == 0 ]] || fail "tree: the keys-only call exits ${rc}: $(cat "${out}.err")"
for key in BASH ENV_REQUIRED PASSWD; do
  grep -qxF "  ${key}: \"\"" "${out}" || fail "tree: the key ${key} with an empty value is missing"
done
grep -qF 'secrets.lok8s.dev/keys-only: "true"' "${out}" || fail "tree: the keys-only annotation is missing"
[[ "$(snapshot "${store}")" == "${store_before}" && ! -e "${marker}" ]] || fail "tree: the store changed or the script ran"
# shellcheck disable=SC2059
printf "${row}" "tree" "${p1rc} ${p1state} $(paste -sd, "${work}/probe1.out")" "-" "render started" "render started" "each key empty"
echo
echo "proof: lo refused all ${#tags[@]} released plugins after the probe alone; the probe left no file and changed no store; the tree's plugin passed"
