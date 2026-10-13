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
#   - an image tag (any OCI tag, with an optional @sha256 digest; aliases and
#     merge keys resolved) → that image on every node;
#   - a shape that cannot be read, or a value that is not an image tag → an
#     error BEFORE any docker, kind or provider call, never "unset".
# The reader cases are shared with the Go twin: tests/fixtures/kind-node-version/.

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
# The shared cases: tests/fixtures/kind-node-version/<verdict>--<case>.yaml, the
# expected answer in <case>.want ("unset", "tag <tag>" or "error <message>").
# The Go twin (TestNodeVersionFixtures) reads the same files, and
# TestNodeVersionMatchesBash runs this reader on each of them.

# _verdict <spec file> — the reader's answer in the .want format.
_verdict() {
  local out
  if out=$(spec::kind_node_version "${1}"); then
    if [[ -n "${out}" ]]; then printf 'tag %s\n' "${out}"; else echo unset; fi
  else
    printf 'error %s\n' "${out}"
  fi
}

@test "spec::kind_node_version: each shared fixture gets its expected answer" {
  local f want got n=0 bad=0
  for f in "${_PROJECT_ROOT}"/tests/fixtures/kind-node-version/*.yaml; do
    want=$(<"${f%.yaml}.want")
    got=$(_verdict "${f}" 2>"${BATS_TEST_TMPDIR}/stderr")
    if [[ "${got}" != "${want}" ]]; then
      printf '%s:\n  got  %s\n  want %s\n' "${f##*/}" "${got}" "${want}" >&2
      bad=$(( bad + 1 ))
    fi
    if [[ -s "${BATS_TEST_TMPDIR}/stderr" ]]; then
      printf '%s: the reader wrote to stderr:\n' "${f##*/}" >&2
      cat "${BATS_TEST_TMPDIR}/stderr" >&2
      bad=$(( bad + 1 ))
    fi
    n=$(( n + 1 ))
  done
  (( n >= 50 )) || { echo "only ${n} fixtures found" >&2; return 1; }
  (( bad == 0 ))
}

@test "spec::kind_node_version: a file that does not parse is an error with the reason, never unset" {
  local spec="${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  printf 'kind: Lo\nspec: [\n' > "${spec}"
  run spec::kind_node_version "${spec}"
  assert_failure
  assert_output --regexp '^spec\.kubernetes\.version cannot be read: .+\. Correct the spec file\.$'
}

# _yq_stub <body> — a yq on PATH that runs <body> (bash) and stays in front
# of the real one for the rest of the test.
_yq_stub() {
  mkdir -p "${BATS_TEST_TMPDIR}/stub"
  printf '#!/usr/bin/env bash\n%s\n' "${1}" > "${BATS_TEST_TMPDIR}/stub/yq"
  chmod +x "${BATS_TEST_TMPDIR}/stub/yq"
  export PATH="${BATS_TEST_TMPDIR}/stub:${PATH}"
}

@test "spec::kind_node_version: a yq without --yaml-fix-merge-anchor-to-spec names the yq version to install" {
  _spec "version: v1.31.4"
  _yq_stub 'echo "Error: unknown flag: --yaml-fix-merge-anchor-to-spec" >&2; exit 1'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.47.1 or later (b install installs the pinned yq)."
}

@test "spec::kind_node_version: a read past the time limit is named, without timeout(1)" {
  _spec "version: v1.31.4"
  # The time limit is pure bash (macOS has no GNU timeout): a timeout(1)
  # call fails the test.
  timeout() { echo "timeout(1) was called" >&2; return 99; }
  export -f timeout
  _yq_stub 'exec sleep 5'
  _SPEC_KVER_TIMEOUT=1
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq did not finish in 1 s. Simplify the spec file."
}

@test "spec::kind_node_version: yq runs under the data memory limit (ulimit -d)" {
  _spec "version: v1.31.4"
  _yq_stub "ulimit -d > '${BATS_TEST_TMPDIR}/limit'; exit 1"
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  [ "$(cat "${BATS_TEST_TMPDIR}/limit")" = "262144" ] || {
    echo "yq ran with ulimit -d $(cat "${BATS_TEST_TMPDIR}/limit"), want 262144" >&2; return 1
  }
}

@test "spec::kind_node_version: a yq that runs out of memory is named" {
  _spec "version: v1.31.4"
  _yq_stub 'echo "fatal error: runtime: cannot allocate memory" >&2; exit 2'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq stopped at the memory limit of 256 MiB, or a signal stopped it. Simplify the spec file."
}

@test "spec::kind_node_version: a yq stopped by SIGSEGV is named" {
  _spec "version: v1.31.4"
  _yq_stub 'ulimit -c 0; kill -SEGV $$'
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output "spec.kubernetes.version cannot be read: yq stopped at the memory limit of 256 MiB, or a signal stopped it. Simplify the spec file."
}

@test "spec::_kver_yq: an interrupt removes the temporary files" {
  _spec "version: v1.31.4"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  # The yq stub stops the read it runs in (its parent) with SIGTERM, as an
  # interrupt of `lo` would.
  _yq_stub 'kill -TERM "${PPID}"; exec sleep 1'
  run bash -c 'import() { :; }; source "$1/domain.sh"; source "$1/spec.sh"; spec::_kver_yq "$2" -r .kind' \
    bash "${_PROJECT_ROOT}/.lok8s/utils" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  [ "${status}" -eq 143 ] || { echo "status ${status}, want 143 (SIGTERM)" >&2; return 1; }
  run ls -A "${TMPDIR}"
  assert_output ""
}

@test "spec::kind_node_version: yq stderr stays out of the shape checks" {
  _spec "version: v1.31.4"
  local real
  real=$(command -v yq)
  _yq_stub "echo 'level=WARN msg=\"a warning\"' >&2; exec '${real}' \"\$@\""
  run spec::kind_node_version "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
  assert_success
  assert_output "v1.31.4"
}

# The `local LC_ALL=C` of spec::_is_image_tag only matters in a locale where
# bash matches 'é' with [A-Za-z] (en_US.UTF-8; not C.UTF-8). The Go twin runs
# every fixture in that locale (TestNodeVersionMatchesBash) and fails under
# CI without it; here the test skips when the locale is missing.
@test "spec::kind_node_version: the shared fixtures in the en_US.UTF-8 locale" {
  locale -a 2>/dev/null | grep -qiE '^en_US\.utf-?8$' || skip "no en_US.UTF-8 locale"
  local f want got bad=0
  for f in "${_PROJECT_ROOT}"/tests/fixtures/kind-node-version/error--non-ascii.yaml \
           "${_PROJECT_ROOT}"/tests/fixtures/kind-node-version/tag--plain.yaml; do
    want=$(<"${f%.yaml}.want")
    got=$(LC_ALL=en_US.UTF-8 _verdict "${f}")
    [[ "${got}" == "${want}" ]] || { printf '%s: got %s\n' "${f##*/}" "${got}" >&2; bad=1; }
  done
  (( bad == 0 ))
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
    # The image check below reads the captured config: it must be the real
    # cluster config (one control plane, one worker), not an empty capture.
    grep '^kind create' "${CALLS}" | grep -q -- ' --config ' || {
      echo "'${line}': kind create carries no --config:" >&2; grep '^kind create' "${CALLS}" >&2; return 1
    }
    if ! grep -q '^kind: Cluster$' "${BATS_TEST_TMPDIR}/kind-config" \
      || [ "$(grep -c '^  - role: ' "${BATS_TEST_TMPDIR}/kind-config")" -ne 2 ]; then
      echo "'${line}': the captured kind config is not the cluster config:" >&2
      cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    fi
    if grep -q 'image:' "${BATS_TEST_TMPDIR}/kind-config"; then
      echo "'${line}': the kind config names an image:" >&2; cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    fi
  done
}

@test "lo provision: a given version → that image on every node" {
  local line want
  # <version line, \n for a line break>;<the tag of the node image>
  while IFS=";" read -r line want; do
    _spec "${line//\\n/$'\n'}"
    _load
    rm -f "${BATS_TEST_TMPDIR}/kind-config"

    local rc=0
    driver::provision test.dev || rc=$?
    [ "${rc}" -eq 0 ] || { echo "'${line}': provision failed (rc ${rc})" >&2; return 1; }
    [ "$(grep -c "^    image: \"kindest/node:${want}\"$" "${BATS_TEST_TMPDIR}/kind-config")" -eq 2 ] || {
      echo "'${line}': want kindest/node:${want} on both nodes:" >&2; cat "${BATS_TEST_TMPDIR}/kind-config" >&2; return 1
    }
  done <<VERSIONS
version: v1.35.0;v1.35.0
version: v1.31.12${DIGEST};v1.31.12${DIGEST}
version: latest;latest
<<: {version: v1.31.4};v1.31.4
version: |\n      v1.31.4;v1.31.4
VERSIONS
}

@test "lo provision: a version that cannot be read or is not a tag fails before any network, container or VM" {
  local name want
  for name in error--image-reference error--version-is-a-map error--kubernetes-is-a-list error--merge-of-a-scalar error--merge-cycle-under-kubernetes error--nul-byte; do
    cp "${_PROJECT_ROOT}/tests/fixtures/kind-node-version/${name}.yaml" "${PATH_CLUSTERS}/test.dev/cluster.lok8s.yaml"
    want=$(<"${_PROJECT_ROOT}/tests/fixtures/kind-node-version/${name}.want")
    want="[error] ${want#error }"
    _load
    export PROVIDER_NAME="hetzner"
    provider::provision() { echo "provider::provision" >> "${CALLS}"; }
    lo::provision_remote() { echo "lo::provision_remote" >> "${CALLS}"; }
    : > "${CALLS}"

    run driver::provision test.dev
    [ "${status}" -ne 0 ] || { echo "${name}: provision accepted it" >&2; return 1; }
    grep -qxF -- "${want}" <<<"${output}" || {
      echo "${name}: want the line '${want}':" >&2; echo "${output}" >&2; return 1
    }
    if grep -qE '^\[error\] *$' <<<"${output}"; then
      echo "${name}: an empty [error] line:" >&2; echo "${output}" >&2; return 1
    fi
    [ ! -s "${CALLS}" ] || {
      echo "${name}: steps ran before the version was rejected:" >&2; cat "${CALLS}" >&2; return 1
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
