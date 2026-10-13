#!/usr/bin/env bats
# lo_node_version_test.bats — spec.kubernetes.version → the kind node image.
#
# Why this exists
# ---------------
# A Lo spec without spec.kubernetes.version (the minimal form in the spec
# reference) made `lo provision` run kind with `image: "kindest/node:null"`:
# the driver read the version with a raw `yq -r`, which prints `null` for a
# missing key (and `~` for a tilde null). kind failed with "manifest for
# kindest/node:null not found" — AFTER the driver had created the docker
# network and the registry containers.
#
# The contract, pinned here and in the Go twin
# (internal/driver/lo/nodeimage_test.go):
#   - unset (missing, null, ~, "", "null") → no image line in the kind config,
#     so kind uses the default node image of its own release;
#   - a valid version → that image on every node;
#   - a malformed version → an error BEFORE any docker, kind or provider call.

setup() {
  load "../test_helper"
  setup_tmpdir
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_BASE="${BATS_TEST_TMPDIR}/base"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export KIND_EXPERIMENTAL_DOCKER_NETWORK="lok8s-test"
  export DOMAIN_NAME="test.dev"
  mkdir -p "${PATH_CLUSTERS}/test.dev" "${PATH_BASE}/.kubeconfig"
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  CALLS="${BATS_TEST_TMPDIR}/calls"
  : > "${CALLS}"
}

teardown() { teardown_tmpdir; }

DIGEST="@sha256:0f5cc49c5e73c0c2bb6e2df56e7df189240d83cf94edfa30946482eb08ec57d2"
HINT="Use the form vMAJOR.MINOR.PATCH, with an optional @sha256:<digest>, or remove the key to use the default node image of kind."

# _spec <version line> — a Lo spec; an empty argument leaves the key out.
_spec() {
  {
    printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: lotest\nspec:\n  runtime: kind\n'
    if [[ -n "${1}" ]]; then printf '  kubernetes:\n    %s\n' "${1}"; fi
  } > "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
}

# _load — the driver with every collaborator before and after the kind config
# stubbed to SUCCEED and to log its call. The kind config render is REAL.
# `kind create` copies its --config file to ${BATS_TEST_TMPDIR}/kind-config.
_load() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  :args() { domain="test.dev"; }
  export LOK8S_CP_COUNT=1 LOK8S_WORKER_COUNT=1 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID

  lo::read_config()      { echo "lo::read_config" >> "${CALLS}"; }
  lo::validate_ips()     { echo "lo::validate_ips" >> "${CALLS}"; }
  lo::network()          { echo "lo::network" >> "${CALLS}"; }
  lo::registry_network() { echo "lo::registry_network" >> "${CALLS}"; }
  registry::is_shared()  { return 1; }
  lo::registries_tls_cert()  { echo "lo::registries_tls_cert" >> "${CALLS}"; }
  lo::registries()           { echo "lo::registries" >> "${CALLS}"; }
  lo::write_certs_d()        { echo "lo::write_certs_d" >> "${CALLS}"; }
  lo::write_oidc_auth_config() { :; }
  lo::registries_tls_nudge() { :; }
  _lo_extract_kubeconfig()   { echo "${PATH_BASE}/.kubeconfig/lotest.yaml"; }
  lo::coredns()              { :; }
  lo::apply_local_registry_hosting() { :; }
  lo::registry_configmap()   { :; }
  kapply::run() { local phase="${1}" fn="${2}"; shift 2; "${fn}" "${@}"; }
  docker() { echo "docker $*" >> "${CALLS}"; }
  kind() {
    echo "kind $*" >> "${CALLS}"
    if [[ "${1:-} ${2:-}" == "create cluster" ]]; then
      local prev="" arg
      for arg in "${@}"; do
        [[ "${prev}" == "--config" ]] && cat "${arg}" > "${BATS_TEST_TMPDIR}/kind-config"
        prev="${arg}"
      done
    fi
    return 0
  }
}

# ── spec::kind_node_version ──────────────────────────────────────────────────

@test "spec::kind_node_version: an unset version prints nothing and succeeds" {
  local line out
  for line in "" "version: null" "version: ~" "version:" 'version: ""' 'version: "null"'; do
    _spec "${line}"
    out=$(spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml") || {
      echo "'${line}' was refused: ${out}" >&2; return 1
    }
    [ -z "${out}" ] || { echo "'${line}' printed '${out}', want nothing" >&2; return 1; }
  done
}

@test "spec::kind_node_version: a valid version prints the tag" {
  local v out
  for v in v1.35.0 v1.31.4 "v1.31.12${DIGEST}" v1.36.0-rc.1; do
    _spec "version: ${v}"
    out=$(spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml")
    [ "${out}" == "${v}" ] || { echo "'${v}' printed '${out}'" >&2; return 1; }
  done
}

@test "spec::kind_node_version: a malformed version prints the problem and fails" {
  local line want rc out
  while IFS='|' read -r line want; do
    _spec "${line}"
    rc=0
    out=$(spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml") || rc=$?
    [ "${rc}" -eq 1 ] || { echo "'${line}' was accepted (rc ${rc}, out '${out}')" >&2; return 1; }
    [ "${out}" == "${want} ${HINT}" ] || { echo "'${line}': got '${out}', want '${want} ${HINT}'" >&2; return 1; }
  done <<EOF
version: "1.31.0"|spec.kubernetes.version '1.31.0' is not a kind node version.
version: "1.31"|spec.kubernetes.version '1.31' is not a kind node version.
version: 1.31|spec.kubernetes.version '1.31' is not a kind node version.
version: false|spec.kubernetes.version 'false' is not a kind node version.
version: latest|spec.kubernetes.version 'latest' is not a kind node version.
version: v1.35.0@sha256:abc|spec.kubernetes.version 'v1.35.0@sha256:abc' is not a kind node version.
version: {a: b}|spec.kubernetes.version is a map, not a kind node version.
version: [v1.35.0]|spec.kubernetes.version is a list, not a kind node version.
EOF
}

# ── lo::render_nodes ─────────────────────────────────────────────────────────

@test "lo::render_nodes: an unset version names no image on any node" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  export LOK8S_CP_COUNT=3 LOK8S_WORKER_COUNT=2 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  local v out
  for v in "" null; do
    out=$(lo::render_nodes "${v}" "")
    [ "$(grep -c 'role: ' <<<"${out}")" -eq 5 ] || { echo "want 5 nodes:" >&2; echo "${out}" >&2; return 1; }
    if grep -q 'image:' <<<"${out}"; then
      echo "version '${v}' still names an image:" >&2; echo "${out}" >&2; return 1
    fi
  done
}

@test "lo::render_nodes: a version names its image on every node" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  export LOK8S_CP_COUNT=3 LOK8S_WORKER_COUNT=2 LOK8S_HOST_PORTS=false LOK8S_EXTRA_MOUNTS_COUNT=0
  local out
  out=$(lo::render_nodes "v1.35.0" "")
  [ "$(grep -c '^    image: "kindest/node:v1.35.0"$' <<<"${out}")" -eq 5 ] || {
    echo "want the image on all 5 nodes:" >&2; echo "${out}" >&2; return 1
  }
}

# ── driver::provision ────────────────────────────────────────────────────────

@test "lo provision: no version → kind create gets no --image and a config without an image" {
  local line
  for line in "" "version: null" "version: ~" 'version: "null"' 'version: ""'; do
    _spec "${line}"
    _load
    rm -f "${BATS_TEST_TMPDIR}/kind-config"
    : > "${CALLS}"

    local rc=0
    driver::provision test.dev || rc=$?
    [ "${rc}" -eq 0 ] || { echo "'${line}': provision failed (rc ${rc})" >&2; return 1; }
    grep -q '^kind create cluster ' "${CALLS}" || { echo "'${line}': kind create never ran" >&2; return 1; }
    if grep '^kind create' "${CALLS}" | grep -q -- '--image'; then
      echo "'${line}': kind create carries --image:" >&2; grep '^kind create' "${CALLS}" >&2; return 1
    fi
    [ -s "${BATS_TEST_TMPDIR}/kind-config" ] || { echo "'${line}': no kind config captured" >&2; return 1; }
    if grep -q 'image:' "${BATS_TEST_TMPDIR}/kind-config"; then
      echo "'${line}': the kind config names an image:" >&2; cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    fi
  done
}

@test "lo provision: a given version → that image on every node" {
  local v
  for v in v1.35.0 "v1.31.12${DIGEST}"; do
    _spec "version: ${v}"
    _load
    rm -f "${BATS_TEST_TMPDIR}/kind-config"

    local rc=0
    driver::provision test.dev || rc=$?
    [ "${rc}" -eq 0 ] || { echo "'${v}': provision failed (rc ${rc})" >&2; return 1; }
    [ "$(grep -c "^    image: \"kindest/node:${v}\"$" "${BATS_TEST_TMPDIR}/kind-config")" -eq 2 ] || {
      echo "'${v}': want the image on both nodes:" >&2; cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    }
  done
}

@test "lo provision: a malformed version fails before any network, container or VM" {
  local line
  for line in 'version: "1.31"' 'version: 1.35.0' 'version: {minor: 31}'; do
    _spec "${line}"
    _load
    export PROVIDER_NAME="hetzner"
    provider::provision() { echo "provider::provision" >> "${CALLS}"; }
    lo::provision_remote() { echo "lo::provision_remote" >> "${CALLS}"; }
    : > "${CALLS}"

    run driver::provision test.dev
    [ "${status}" -ne 0 ] || { echo "'${line}': provision accepted it" >&2; return 1; }
    [[ "${output}" == *"spec.kubernetes.version"*"not a kind node version. ${HINT}"* ]] || {
      echo "'${line}': the problem is not reported:" >&2; echo "${output}" >&2; return 1
    }
    [ ! -s "${CALLS}" ] || {
      echo "'${line}': steps ran before the version was rejected:" >&2; cat "${CALLS}" >&2; return 1
    }
    unset PROVIDER_NAME
  done
}

@test "lo::export_spec_envs: the literal string null exports an empty version" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  oidc::load_spec() { :; }
  local line want
  while IFS='|' read -r line want; do
    _spec "${line}"
    lo::export_spec_envs "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml" >/dev/null 2>&1 || true
    [ "${LOK8S_SPEC_KUBERNETES_VERSION}" == "${want}" ] || {
      echo "'${line}': LOK8S_SPEC_KUBERNETES_VERSION='${LOK8S_SPEC_KUBERNETES_VERSION}', want '${want}'" >&2; return 1
    }
  done <<'EOF'
version: "null"|
version: ~|
|
version: v1.35.0|v1.35.0
EOF
}
