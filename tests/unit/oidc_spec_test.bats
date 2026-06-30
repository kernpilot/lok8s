#!/usr/bin/env bats
# oidc_spec_test.bats — verify BOTH drivers read spec.oidc into LOK8S_SPEC_OIDC_*
# (deliverable 1) and that the kubeone driver renders the controlPlaneComponents
# apiServer.flags block only when oidc is set (deliverable 4). Uses the real yq /
# envsubst provided by `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import
  # verbose.sh already loaded by test_helper.

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
}

teardown() {
  teardown_tmpdir
}

# --- lo driver: lo::export_spec_envs reads spec.oidc ---

_lo_export_for() {
  local fixture="$1"
  # config.sh references LO_DEFAULT_* + registry helpers; source defaults and
  # stub the registry/LB lookups export_spec_envs touches.
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/defaults.sh"
  registry::get() { echo ""; }
  registry::config_generate() { :; }
  export -f registry::get registry::config_generate
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/config.sh"
  lo::export_spec_envs "${fixture}"
}

@test "lo::export_spec_envs reads all spec.oidc fields from the fixture" {
  _lo_export_for "${FIXTURES_DIR}/lo-cluster-oidc.lok8s.yaml"
  [ "${LOK8S_SPEC_OIDC_ISSUER}" = "https://id.kubehz.dev" ]
  [ "${LOK8S_SPEC_OIDC_CLIENTID}" = "kubectl-cli" ]
  [ "${LOK8S_SPEC_OIDC_USERNAMECLAIM}" = "email" ]
  [ "${LOK8S_SPEC_OIDC_USERNAMEPREFIX}" = "oidc:" ]
  [ "${LOK8S_SPEC_OIDC_GROUPSCLAIM}" = "groups" ]
  [ "${LOK8S_SPEC_OIDC_GROUPSPREFIX}" = "oidc:" ]
  [[ "${LOK8S_SPEC_OIDC_CABUNDLE}" == *"BEGIN CERTIFICATE"* ]]
}

@test "lo::export_spec_envs leaves oidc vars empty + applies claim defaults for a non-oidc spec" {
  _lo_export_for "${FIXTURES_DIR}/lo-cluster.lok8s.yaml"
  [ -z "${LOK8S_SPEC_OIDC_ISSUER}" ]
  [ -z "${LOK8S_SPEC_OIDC_CLIENTID}" ]
  [ -z "${LOK8S_SPEC_OIDC_CABUNDLE}" ]
  # Claim fields still take their documented defaults (harmless; gated by enabled).
  [ "${LOK8S_SPEC_OIDC_USERNAMECLAIM}" = "sub" ]
  [ "${LOK8S_SPEC_OIDC_GROUPSCLAIM}" = "groups" ]
}

# --- kubeone driver: extract_vars + render_oidc_components ---

# Build a minimal kubeone cluster spec on the fly (the fixtures dir has no
# kubeone fixture, and extract_vars only needs a few fields here).
_kubeone_spec() {
  local with_oidc="$1" path="${BATS_TEST_TMPDIR}/kubeone-cluster.yaml"
  cat >"${path}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: ko-test
spec:
  kubernetes:
    version: "v1.35.5"
  provider:
    hetzner: {}
YAML
  if [[ "${with_oidc}" == "oidc" ]]; then
    cat >>"${path}" <<'YAML'
  oidc:
    issuer: https://id.kubehz.dev
    clientID: kubectl-cli
YAML
  fi
  echo "${path}"
}

_load_kubeone_config() {
  # extract_vars calls provider::detect, credentials helpers, etc.; stub the
  # ones the OIDC path doesn't need so the file sources cleanly.
  provider::detect() { echo "hetzner"; }
  export -f provider::detect
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
}

@test "kubeone::extract_vars reads spec.oidc into LOK8S_SPEC_OIDC_*" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec oidc)
  kubeone::extract_vars "${spec}"
  [ "${LOK8S_SPEC_OIDC_ISSUER}" = "https://id.kubehz.dev" ]
  [ "${LOK8S_SPEC_OIDC_CLIENTID}" = "kubectl-cli" ]
  run oidc::enabled
  assert_success
}

@test "kubeone::_inject_oidc merges features.openidConnect when oidc set" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec oidc)
  kubeone::extract_vars "${spec}"
  local m="${BATS_TEST_TMPDIR}/m.yaml"
  printf 'features:\n  encryptionProviders:\n    enable: true\n' > "${m}"
  run kubeone::_inject_oidc "${m}"
  assert_success
  run yq -r '.features.openidConnect.enable' "${m}"
  assert_output "true"
  run yq -r '.features.openidConnect.config.usernameClaim' "${m}"
  assert_output "sub"
  run yq -r '.features.encryptionProviders.enable' "${m}"
  assert_output "true"
}

@test "kubeone::_inject_oidc is a no-op without spec.oidc (back-compat)" {
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec none)
  kubeone::extract_vars "${spec}"
  local m="${BATS_TEST_TMPDIR}/m2.yaml"
  printf 'features:\n  encryptionProviders:\n    enable: true\n' > "${m}"
  run kubeone::_inject_oidc "${m}"
  assert_success
  run yq -r '.features | has("openidConnect")' "${m}"
  assert_output "false"
}

@test "kubeone::generate_config wires features.openidConnect for an oidc spec" {
  if ! command -v envsubst &>/dev/null; then skip "envsubst not available"; fi
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec oidc)
  local out="${BATS_TEST_TMPDIR}/gen"
  kubeone::generate_config "${spec}" "hetzner" "${out}"
  [ -f "${out}/kubeone.yaml" ]
  run yq -r '.features.openidConnect.enable' "${out}/kubeone.yaml"
  assert_output "true"
  run yq -r '.versions.kubernetes' "${out}/kubeone.yaml"
  assert_output "v1.35.5"
}

@test "kubeone::generate_config omits features.openidConnect for a non-oidc spec" {
  if ! command -v envsubst &>/dev/null; then skip "envsubst not available"; fi
  _load_kubeone_config
  local spec; spec=$(_kubeone_spec none)
  local out="${BATS_TEST_TMPDIR}/gen2"
  kubeone::generate_config "${spec}" "hetzner" "${out}"
  [ -f "${out}/kubeone.yaml" ]
  run yq -r '.features | has("openidConnect")' "${out}/kubeone.yaml"
  assert_output "false"
}
