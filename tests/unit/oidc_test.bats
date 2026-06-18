#!/usr/bin/env bats
# oidc_test.bats — unit tests for .lok8s/utils/oidc.sh and the lo-driver OIDC
# render wiring (.lok8s/drivers/lo/utils/render.sh).

setup() {
  load "../test_helper"
  setup_tmpdir

  # Stub import (we're not running under argsh here).
  import() { :; }
  export -f import

  # Source the unit under test. verbose.sh is already sourced by test_helper.
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"

  # A fully-populated spec.oidc env baseline; individual tests unset/override.
  export LOK8S_SPEC_OIDC_ISSUER="https://id.kubehz.dev"
  export LOK8S_SPEC_OIDC_CLIENTID="kubectl-cli"
  export LOK8S_SPEC_OIDC_USERNAMECLAIM="email"
  export LOK8S_SPEC_OIDC_USERNAMEPREFIX="oidc:"
  export LOK8S_SPEC_OIDC_GROUPSCLAIM="groups"
  export LOK8S_SPEC_OIDC_GROUPSPREFIX="oidc:"
  unset LOK8S_SPEC_OIDC_CABUNDLE 2>/dev/null || true
}

teardown() {
  teardown_tmpdir
}

# --- oidc::enabled ---

@test "oidc::enabled is true when issuer + clientID are set" {
  run oidc::enabled
  assert_success
}

@test "oidc::enabled is false when issuer is missing" {
  unset LOK8S_SPEC_OIDC_ISSUER
  run oidc::enabled
  assert_failure
}

@test "oidc::enabled is false when clientID is missing" {
  unset LOK8S_SPEC_OIDC_CLIENTID
  run oidc::enabled
  assert_failure
}

@test "oidc::enabled is false when both are unset (no spec.oidc)" {
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID
  run oidc::enabled
  assert_failure
}

# --- oidc::render_auth_config: schema ---

@test "render_auth_config emits the stable v1 apiVersion + kind" {
  run oidc::render_auth_config
  assert_success
  assert_output --partial "apiVersion: apiserver.config.k8s.io/v1"
  assert_output --partial "kind: AuthenticationConfiguration"
}

@test "render_auth_config emits the issuer URL" {
  run oidc::render_auth_config
  assert_success
  assert_output --partial 'url: "https://id.kubehz.dev"'
}

@test "render_auth_config emits the clientID as the single audience" {
  run oidc::render_auth_config
  assert_success
  assert_output --partial "audiences:"
  assert_output --partial '- "kubectl-cli"'
}

@test "render_auth_config maps username claim + prefix" {
  run oidc::render_auth_config
  assert_success
  assert_output --partial "username:"
  assert_output --partial 'claim: "email"'
  assert_output --partial 'prefix: "oidc:"'
}

@test "render_auth_config maps groups claim + prefix" {
  export LOK8S_SPEC_OIDC_GROUPSCLAIM="roles"
  export LOK8S_SPEC_OIDC_GROUPSPREFIX="grp:"
  run oidc::render_auth_config
  assert_success
  assert_output --partial "groups:"
  assert_output --partial 'claim: "roles"'
  assert_output --partial 'prefix: "grp:"'
}

@test "render_auth_config applies defaults for unset claim fields" {
  unset LOK8S_SPEC_OIDC_USERNAMECLAIM LOK8S_SPEC_OIDC_USERNAMEPREFIX
  unset LOK8S_SPEC_OIDC_GROUPSCLAIM LOK8S_SPEC_OIDC_GROUPSPREFIX
  run oidc::render_auth_config
  assert_success
  # username default claim "sub", default prefix "oidc:"; groups default "groups".
  assert_output --partial 'claim: "sub"'
  assert_output --partial 'claim: "groups"'
  assert_output --partial 'prefix: "oidc:"'
}

@test "render_auth_config preserves a literal '-' username prefix (no-prefix semantics)" {
  export LOK8S_SPEC_OIDC_USERNAMEPREFIX="-"
  run oidc::render_auth_config
  assert_success
  assert_output --partial 'prefix: "-"'
}

# --- oidc::render_auth_config: caBundle ---

@test "render_auth_config includes certificateAuthority when caBundle is set" {
  export LOK8S_SPEC_OIDC_CABUNDLE=$'-----BEGIN CERTIFICATE-----\nMIIBdev\n-----END CERTIFICATE-----'
  run oidc::render_auth_config
  assert_success
  assert_output --partial "certificateAuthority: |"
  assert_output --partial "-----BEGIN CERTIFICATE-----"
  assert_output --partial "MIIBdev"
}

@test "render_auth_config OMITS certificateAuthority when caBundle is unset" {
  unset LOK8S_SPEC_OIDC_CABUNDLE
  run oidc::render_auth_config
  assert_success
  refute_output --partial "certificateAuthority"
}

# --- oidc::render_auth_config: failure / validation ---

@test "render_auth_config returns non-zero and emits nothing when issuer empty" {
  unset LOK8S_SPEC_OIDC_ISSUER
  run oidc::render_auth_config
  assert_failure
  [ -z "${output}" ]
}

@test "render_auth_config rejects a non-https issuer" {
  export LOK8S_SPEC_OIDC_ISSUER="http://insecure.example.com"
  run oidc::render_auth_config
  assert_failure
  assert_output --partial "https://"
}

# --- set -e safety ---

@test "render_auth_config is set -e safe with a missing/empty oidc spec" {
  # Mirror the real `lo` runtime (set -euo pipefail). render must fail CLEANLY
  # (non-zero, no crash) rather than abort the subshell on an unbound var.
  run bash -c '
    set -euo pipefail
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/oidc.sh"
    # No LOK8S_SPEC_OIDC_* exported at all.
    unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID 2>/dev/null || true
    if oidc::render_auth_config; then
      echo "UNEXPECTED_RENDER"
    else
      echo "CLEAN_FAILURE rc=$?"
    fi
    # enabled predicate must also be safe under set -e.
    if oidc::enabled; then echo "ENABLED"; else echo "DISABLED"; fi
    echo "REACHED_END"
  '
  assert_success
  assert_output --partial "CLEAN_FAILURE"
  assert_output --partial "DISABLED"
  assert_output --partial "REACHED_END"
  refute_output --partial "UNEXPECTED_RENDER"
}

@test "render_auth_config output parses as valid YAML (yq round-trip)" {
  # Only meaningful when a real yq is on PATH (argsh test provides one).
  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  export LOK8S_SPEC_OIDC_CABUNDLE=$'-----BEGIN CERTIFICATE-----\nMIIBdev\n-----END CERTIFICATE-----'
  oidc::render_auth_config > "${BATS_TEST_TMPDIR}/auth.yaml"
  run yq -r '.jwt[0].issuer.url' "${BATS_TEST_TMPDIR}/auth.yaml"
  assert_success
  assert_output "https://id.kubehz.dev"
  run yq -r '.jwt[0].issuer.audiences[0]' "${BATS_TEST_TMPDIR}/auth.yaml"
  assert_output "kubectl-cli"
  run yq -r '.jwt[0].claimMappings.username.claim' "${BATS_TEST_TMPDIR}/auth.yaml"
  assert_output "email"
}

# --- lo driver render wiring ---

# Source render.sh with the minimal deps it references. config-registry /
# registry helpers aren't needed for render_nodes beyond registry::is_tls
# (only used by write_certs_d, which we don't call here).
_load_lo_render() {
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export DOMAIN_NAME="oidc.lok8s.dev"
  LO_DEFAULT_DOMAIN="lok8s.dev"
  LO_DEFAULT_POD_CIDR="10.244.0.0/16"
  LO_DEFAULT_SVC_CIDR="10.96.0.0/12"
  LOK8S_CP_COUNT=1
  LOK8S_WORKER_COUNT=1
  LOK8S_HOST_PORTS="false"
  LOK8S_EXTRA_MOUNTS_COUNT=0
  registry::is_tls() { return 1; }
  export -f registry::is_tls
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/render.sh"
}

@test "lo::render_nodes injects the apiserver auth-config patch + mount when oidc enabled" {
  _load_lo_render
  run lo::render_nodes "kindest/node:v1.35.5"
  assert_success
  assert_output --partial "kind: ClusterConfiguration"
  assert_output --partial "apiVersion: kubeadm.k8s.io/v1beta4"
  # v1beta4: extraArgs is a LIST of {name,value}, not a map.
  assert_output --partial "- name: authentication-config"
  assert_output --partial "value: /etc/kubernetes/oidc/auth-config.yaml"
  assert_output --partial "containerPath: /etc/kubernetes/oidc/auth-config.yaml"
}

@test "lo::render_nodes is byte-identical (no oidc keys) when oidc disabled" {
  _load_lo_render
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID
  run lo::render_nodes "kindest/node:v1.35.5"
  assert_success
  refute_output --partial "ClusterConfiguration"
  refute_output --partial "authentication-config"
  refute_output --partial "/etc/kubernetes/oidc"
}

@test "lo::write_oidc_auth_config writes the file when enabled, clears it when not" {
  _load_lo_render
  # Enabled → file written with real content.
  lo::write_oidc_auth_config "oidc.lok8s.dev"
  local f="${BATS_TEST_TMPDIR}/clusters/oidc.lok8s.dev/.oidc/auth-config.yaml"
  [ -s "${f}" ]
  run grep -q "AuthenticationConfiguration" "${f}"
  assert_success

  # Disabled → existing file is truncated in place (inode kept), not removed.
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID
  lo::write_oidc_auth_config "oidc.lok8s.dev"
  [ -f "${f}" ]      # still exists (inode preserved for any live mount)
  [ ! -s "${f}" ]    # but empty
}

@test "lo::write_oidc_auth_config does nothing when no spec.oidc and no prior file" {
  _load_lo_render
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID
  run lo::write_oidc_auth_config "oidc.lok8s.dev"
  assert_success
  [ ! -e "${BATS_TEST_TMPDIR}/clusters/oidc.lok8s.dev/.oidc/auth-config.yaml" ]
}
