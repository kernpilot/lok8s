#!/usr/bin/env bats
# kubeconfig_oidc_test.bats — `lo kubeconfig --oidc` must work from a FRESH
# shell: kubeconfig::emit_oidc self-loads spec.oidc from the domain's cluster
# spec (oidc::load_spec) when no driver has exported the LOK8S_SPEC_OIDC_*
# vars. Regression for the bug where the command only worked inside a
# provision/bootstrap context and errored "no usable spec.oidc" otherwise.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import
  # verbose.sh already loaded by test_helper.

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi

  # A fresh shell carries none of the driver-exported spec vars.
  unset LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID \
    LOK8S_SPEC_OIDC_USERNAMECLAIM LOK8S_SPEC_OIDC_USERNAMEPREFIX \
    LOK8S_SPEC_OIDC_GROUPSCLAIM LOK8S_SPEC_OIDC_GROUPSPREFIX \
    LOK8S_SPEC_OIDC_CABUNDLE

  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubeconfig"

  # Minimal admin kubeconfig — emit_oidc reuses its cluster server + CA.
  _SRC="${BATS_TEST_TMPDIR}/admin.yaml"
  cat > "${_SRC}" <<'EOF'
apiVersion: v1
kind: Config
clusters:
  - name: test-oidc
    cluster:
      server: https://10.0.0.1:6443
      certificate-authority-data: ZmFrZS1jYQ==
EOF
}

teardown() {
  teardown_tmpdir
}

_write_domain() {
  # $1 = domain, $2 = fixture to copy as its cluster spec
  mkdir -p "${PATH_CLUSTERS}/${1}"
  cp "${2}" "${PATH_CLUSTERS}/${1}/cluster.lok8s.yaml"
}

@test "emit_oidc self-loads spec.oidc from the cluster spec in a fresh shell" {
  _write_domain oidc.lok8s.dev "${FIXTURES_DIR}/lo-cluster-oidc.lok8s.yaml"

  run kubeconfig::emit_oidc oidc.lok8s.dev "${_SRC}"
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"--oidc-issuer-url=https://id.kubehz.dev"* ]]
  [[ "${output}" == *"--oidc-client-id=kubectl-cli"* ]]
  [[ "${output}" == *"server: https://10.0.0.1:6443"* ]]
  [[ "${output}" == *"oidc-login"* ]]
}

@test "emit_oidc keeps driver-exported vars when already set (no clobber)" {
  _write_domain oidc.lok8s.dev "${FIXTURES_DIR}/lo-cluster-oidc.lok8s.yaml"
  export LOK8S_SPEC_OIDC_ISSUER="https://id.other.example"
  export LOK8S_SPEC_OIDC_CLIENTID="other-client"

  run kubeconfig::emit_oidc oidc.lok8s.dev "${_SRC}"
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"--oidc-issuer-url=https://id.other.example"* ]]
  [[ "${output}" == *"--oidc-client-id=other-client"* ]]
}

@test "emit_oidc fails loud for a spec without oidc" {
  _write_domain plain.lok8s.dev "${FIXTURES_DIR}/lo-cluster.lok8s.yaml"

  run kubeconfig::emit_oidc plain.lok8s.dev "${_SRC}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"no usable spec.oidc"* ]]
}

@test "emit_oidc follows a deploy domain's clusterRef to the cluster spec" {
  _write_domain oidc.lok8s.dev "${FIXTURES_DIR}/lo-cluster-oidc.lok8s.yaml"
  mkdir -p "${PATH_CLUSTERS}/deploy.lok8s.dev"
  cat > "${PATH_CLUSTERS}/deploy.lok8s.dev/deploy.lok8s.yaml" <<'EOF'
spec:
  clusterRef:
    domain: oidc.lok8s.dev
EOF
  # The REAL resolver (libs/provision), not a stub — it reads
  # .spec.clusterRef.domain, so this also pins the deploy-yaml shape.
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run kubeconfig::emit_oidc deploy.lok8s.dev "${_SRC}"
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"--oidc-client-id=kubectl-cli"* ]]
}

@test "emit_oidc rejects a plain-http issuer" {
  _write_domain http.lok8s.dev "${FIXTURES_DIR}/lo-cluster-oidc.lok8s.yaml"
  yq -i '.spec.oidc.issuer = "http://id.kubehz.dev"' \
    "${PATH_CLUSTERS}/http.lok8s.dev/cluster.lok8s.yaml"

  run kubeconfig::emit_oidc http.lok8s.dev "${_SRC}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"must be an https:// URL"* ]]
}

@test "emit_oidc fails loud on a malformed cluster spec (no generic masking)" {
  mkdir -p "${PATH_CLUSTERS}/bad.lok8s.dev"
  printf 'spec: [unclosed\n' > "${PATH_CLUSTERS}/bad.lok8s.dev/cluster.lok8s.yaml"

  run kubeconfig::emit_oidc bad.lok8s.dev "${_SRC}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"could not parse cluster spec"* ]]
  [[ "${output}" != *"no usable spec.oidc"* ]]
}

@test "load_spec accepts a comment-only (null) yaml — not a parse error" {
  mkdir -p "${PATH_CLUSTERS}/null.lok8s.dev"
  printf '# intentionally empty\n' > "${PATH_CLUSTERS}/null.lok8s.dev/cluster.lok8s.yaml"

  run kubeconfig::emit_oidc null.lok8s.dev "${_SRC}"
  [ "${status}" -ne 0 ]
  [[ "${output}" == *"no usable spec.oidc"* ]]
  [[ "${output}" != *"could not parse cluster spec"* ]]
}
