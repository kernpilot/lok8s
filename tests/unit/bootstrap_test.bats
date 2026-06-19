#!/usr/bin/env bats
# bootstrap_test.bats — unit tests for .lok8s/libs/bootstrap
#
# Covers the values precedence chain for bootstrap addons:
#   base (values.yaml)
#     < driver  (values.${kind}.yaml)
#       < provider (values.${provider_name}.yaml)
#         < inline  (spec.bootstrap: [name: {overrides}])
#
# Exercises bootstrap::apply with a fake framework addon and stubbed
# kustomize/kubectl/envsubst so we only assert on the merged values file
# that gets staged into the temp build dir.

setup() {
  load "../test_helper"
  setup_tmpdir

  # bootstrap is an argsh script with `import` — stub it out.
  import() { :; }
  export -f import

  # Make template::envsubst_whitelist return something harmless.
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"

  # Fake addon dir: .lok8s/addons/testcni/
  ADDON_DIR="${PATH_LOK8S}/addons/testcni"
  mkdir -p "${ADDON_DIR}"

  # Minimal chart.yaml so the "framework addon with chart.yaml" branch
  # in bootstrap::apply is taken (that's the branch doing values merging).
  cat > "${ADDON_DIR}/chart.yaml" <<'YAML'
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: testcni
valueFiles:
  - values.yaml
YAML

  # Layered values files. Keys are designed so we can tell which layer "won":
  #   - only_base          → only in base
  #   - only_driver        → only in driver
  #   - only_provider      → only in provider
  #   - shared_all         → set in base, driver, provider, and inline
  #   - nested.*           → tests deep merge
  cat > "${ADDON_DIR}/values.yaml" <<'YAML'
only_base: "base"
shared_all: "base"
nested:
  from_base: true
  overridden: "base"
YAML

  cat > "${ADDON_DIR}/values.lo.yaml" <<'YAML'
only_driver: "driver"
shared_all: "driver"
nested:
  from_driver: true
  overridden: "driver"
YAML

  cat > "${ADDON_DIR}/values.hetzner.yaml" <<'YAML'
only_provider: "provider"
shared_all: "provider"
nested:
  from_provider: true
  overridden: "provider"
YAML

  # Cluster spec under PATH_CLUSTERS
  CLUSTER_YAML="${PATH_CLUSTERS}/test.lok8s.dev/cluster.lok8s.yaml"
  mkdir -p "$(dirname "${CLUSTER_YAML}")"

  # Fake kubeconfig so the [[ -f kubeconfig ]] guard passes.
  KUBECONFIG_FILE="${PATH_BASE}/.kubeconfig/e2e-test.yaml"
  mkdir -p "$(dirname "${KUBECONFIG_FILE}")"
  : > "${KUBECONFIG_FILE}"

  # Intercept kustomize & kubectl & envsubst so no real tools run.
  # kustomize captures the build dir it was invoked with so tests can
  # inspect the merged values file after bootstrap::apply returns.
  export CAPTURED_BUILD_DIR_FILE="${BATS_TEST_TMPDIR}/captured_build_dir"
  : > "${CAPTURED_BUILD_DIR_FILE}"

  kustomize() {
    # Walk args to find the build dir (last positional after flags).
    local arg build_dir=""
    for arg in "$@"; do
      case "${arg}" in
        --enable-alpha-plugins|build) ;;
        -*) ;;
        *) build_dir="${arg}" ;;
      esac
    done
    [[ -z "${build_dir}" ]] || echo "${build_dir}" >> "${CAPTURED_BUILD_DIR_FILE}"
    # Copy the merged values file out so the tmp build dir's cleanup
    # doesn't delete it before the test can assert on it.
    if [[ -n "${build_dir}" && -f "${build_dir}/values.merged.yaml" ]]; then
      cp "${build_dir}/values.merged.yaml" "${BATS_TEST_TMPDIR}/last_merged.yaml"
    fi
    echo "---"
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: testcni"
  }
  export -f kustomize

  envsubst() { cat; }
  export -f envsubst

  kubectl() { return 0; }
  export -f kubectl

  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
}

teardown() {
  teardown_tmpdir
}

# --- Helpers ------------------------------------------------------------------

# Writes a cluster spec with kind=Lo, provider=hetzner, and the given
# bootstrap entries. Usage: write_cluster_spec "testcni" "testcni: {shared_all: inline}"
write_cluster_spec() {
  local entries=""
  local e
  for e in "$@"; do
    entries+="  - ${e}
"
  done
  cat > "${CLUSTER_YAML}" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap:
${entries}
YAML
}

# --- Tests --------------------------------------------------------------------

@test "bootstrap::apply merges base < driver < provider (three-layer stack)" {
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Each layer's unique key must survive the merge (no layer dropped).
  [ "$(yq -r '.only_base' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider' "${merged}")" = "provider" ]

  # Precedence: provider wins over driver wins over base.
  [ "$(yq -r '.shared_all' "${merged}")" = "provider" ]

  # Deep merge: all nested keys preserved, overridden key follows precedence.
  [ "$(yq -r '.nested.from_base' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_driver' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_provider' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "provider" ]
}

@test "bootstrap::apply inline overrides beat provider, driver, and base values" {
  # Inline override uses the map form: "- name: {key: value}"
  write_cluster_spec "testcni: {shared_all: inline, nested: {overridden: inline, from_inline: true}}"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Inline must be highest precedence.
  [ "$(yq -r '.shared_all' "${merged}")" = "inline" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "inline" ]

  # Lower layers still contribute their unique keys.
  [ "$(yq -r '.only_base' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider' "${merged}")" = "provider" ]
  [ "$(yq -r '.nested.from_base' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_driver' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_provider' "${merged}")" = "true" ]
  [ "$(yq -r '.nested.from_inline' "${merged}")" = "true" ]
}

@test "bootstrap::apply falls back to driver precedence when no provider values" {
  # Remove provider-specific values.
  rm -f "${ADDON_DIR}/values.hetzner.yaml"
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  # Driver wins over base; provider-only keys absent.
  [ "$(yq -r '.shared_all' "${merged}")" = "driver" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "driver" ]
  [ "$(yq -r '.only_provider // "missing"' "${merged}")" = "missing" ]
}

@test "bootstrap::apply uses only base values when no driver/provider files exist" {
  rm -f "${ADDON_DIR}/values.lo.yaml" "${ADDON_DIR}/values.hetzner.yaml"
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]

  [ "$(yq -r '.shared_all' "${merged}")" = "base" ]
  [ "$(yq -r '.nested.overridden' "${merged}")" = "base" ]
  [ "$(yq -r '.only_driver // "missing"' "${merged}")" = "missing" ]
  [ "$(yq -r '.only_provider // "missing"' "${merged}")" = "missing" ]
}

@test "bootstrap::apply defaults to [cilium] when spec.bootstrap is empty" {
  # Create a minimal cilium addon stub so the default resolves without real helm.
  local cilium_dir="${PATH_LOK8S}/addons/cilium"
  mkdir -p "${cilium_dir}"
  cat > "${cilium_dir}/chart.yaml" <<'YAML'
apiVersion: khelm.mgoltzsche.github.com/v2
kind: ChartRenderer
metadata:
  name: cilium
valueFiles:
  - values.yaml
YAML
  cat > "${cilium_dir}/values.yaml" <<'YAML'
marker: "cilium-default"
YAML

  # Cluster spec with NO spec.bootstrap section.
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
YAML

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  # kustomize must have been invoked exactly once (for the default cilium addon).
  run wc -l < "${CAPTURED_BUILD_DIR_FILE}"
  assert_success
  assert_output "1"

  # The merged values file must come from the cilium addon (marker key present).
  local merged="${BATS_TEST_TMPDIR}/last_merged.yaml"
  [ -f "${merged}" ]
  [ "$(yq -r '.marker' "${merged}")" = "cilium-default" ]
}

@test "bootstrap::apply skips entirely when spec.bootstrap is an explicit empty list" {
  # `bootstrap: []` is authoritative opt-out — no cilium default.
  cat > "${CLUSTER_YAML}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Kkp
metadata:
  name: e2e-test
spec:
  provider:
    name: hetzner
  bootstrap: []
YAML

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_success

  # kustomize must NOT have been invoked.
  if [ -f "${CAPTURED_BUILD_DIR_FILE}" ]; then
    run wc -l < "${CAPTURED_BUILD_DIR_FILE}"
    assert_output "0"
  fi
}

@test "bootstrap::apply fails when kubeconfig missing" {
  write_cluster_spec "testcni"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${PATH_BASE}/.kubeconfig/does-not-exist.yaml"
  assert_failure
  assert_output --partial "kubeconfig not found"
}

@test "bootstrap::apply fails when addon directory missing" {
  write_cluster_spec "doesnotexist"

  run bootstrap::apply "test.lok8s.dev" "${CLUSTER_YAML}" "${KUBECONFIG_FILE}"
  assert_failure
  assert_output --partial "addon not found"
}

# --- per-driver default policy (bootstrap::_resolve_entries) -----------------
# The default for an ABSENT spec.bootstrap is per-driver, not one-size-fits-all:
# only `lo` (kind) ships without a CNI. KubeOne deploys its own cilium during
# apply; Capi/Kkp bring their CNI from the management cluster. (FRICTION
# 2026-06-12: the blanket [cilium] default caused stray cilium applies on
# managed clusters.)

@test "_resolve_entries: explicit non-empty list returns entries in order" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Lo
spec:
  bootstrap: [cilium, ./targets/foo, /abs/bar]
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" lo
  assert_success
  [ "${lines[0]}" = "cilium" ]
  [ "${lines[1]}" = "./targets/foo" ]
  [ "${lines[2]}" = "/abs/bar" ]
}

@test "_resolve_entries: explicit empty list opts out (lo)" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Lo
spec:
  bootstrap: []
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" lo
  assert_success
  [ -z "$output" ]
}

@test "_resolve_entries: absent bootstrap defaults to cilium for lo" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Lo
spec:
  network: {cidr: 10.0.0.0/16}
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" lo
  assert_output "cilium"
}

@test "_resolve_entries: absent bootstrap is empty for kubeone (driver owns CNI)" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: KubeOne
spec:
  network: {cidr: 10.0.0.0/16}
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" kubeone
  [ -z "$output" ]
}

@test "_resolve_entries: absent bootstrap is empty for capi and kkp" {
  cat > "${CLUSTER_YAML}" <<'YAML'
kind: Capi
spec:
  provider: {name: hetzner}
YAML
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" capi
  [ -z "$output" ]
  run bootstrap::_resolve_entries "${CLUSTER_YAML}" kkp
  [ -z "$output" ]
}
