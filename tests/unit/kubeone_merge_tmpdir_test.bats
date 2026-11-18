#!/usr/bin/env bats
# kubeone_merge_tmpdir_test.bats — a merge into the rendered KubeOne manifest
# that cannot write leaves TMPDIR as it was (drivers/kubeone/config,
# kubeone::_yq_edit). `yq -i` writes through a temp file in TMPDIR and keeps
# it when its last write fails: a copy of the manifest, and after the
# registry merge that copy holds the registry credentials. The audit-log
# merge has the same case in kubeone_audit_log_test.bats.
#
# Each case points TMPDIR at a fresh directory and requires it to be empty
# after the failed call. The credentials are fixtures. No case prints the
# content of a file: a leftover is shown by its name and mode only.
# Uses the real yq provided by `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  # A read-only file in a read-only directory makes the write fail. Root
  # writes it anyway.
  [[ "$(id -u)" != 0 ]] || skip "root writes a read-only file"

  provider::detect() { echo "hetzner"; }
  export -f provider::detect
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"

  TMPD="${BATS_TEST_TMPDIR}/tmpdir"
  RO="${BATS_TEST_TMPDIR}/ro"
  M="${RO}/m.yaml"
  mkdir -p "${TMPD}" "${RO}"
  printf 'features: {}\n' > "${M}"

  export LOK8S_SPEC_OIDC_ISSUER="https://id.example.com"
  export LOK8S_SPEC_OIDC_CLIENTID="kubectl"
  export LOK8S_SPEC_OIDC_USERNAMECLAIM="sub"
  export LOK8S_SPEC_OIDC_USERNAMEPREFIX="oidc:"
  export LOK8S_SPEC_OIDC_GROUPSCLAIM="groups"
  export LOK8S_SPEC_OIDC_GROUPSPREFIX="oidc:"
}

teardown() {
  # A failed case can stop before it gives the write access back.
  [[ ! -d "${RO:-}" ]] || chmod -R u+w "${RO}"
  teardown_tmpdir
}

# _read_only: the manifest and its directory can no longer be written.
_read_only() {
  chmod 400 "${M}"
  chmod 500 "${RO}"
}

# _writable: give the write access back (before the assertions, so that
# teardown can remove the directory).
_writable() {
  chmod 700 "${RO}"
  chmod 600 "${M}"
}

# _assert_tmpdir_empty: nothing is left in the fresh TMPDIR of the case.
_assert_tmpdir_empty() {
  local left
  left=$(find "${TMPD}" -mindepth 1 -printf '%P (mode %m)\n')
  [ -z "${left}" ] || { echo "left in TMPDIR: ${left}" >&2; return 1; }
}

# _fake_yq <dir> <script body>: a yq on PATH in front of the real one.
_fake_yq() {
  local dir="${1}" real; real=$(command -v yq)
  mkdir -p "${dir}"
  printf '#!/usr/bin/env bash\nREAL_YQ=%q\n%s\n' "${real}" "${2}" > "${dir}/yq"
  chmod +x "${dir}/yq"
}

@test "a failed write of features.openidConnect leaves TMPDIR as it was" {
  _read_only
  TMPDIR="${TMPD}" run kubeone::_inject_oidc "${M}"
  _writable
  assert_failure
  assert_output --partial "OIDC: failed to inject features.openidConnect"
  _assert_tmpdir_empty
}

@test "a failed write of the OIDC caFile leaves TMPDIR as it was" {
  export LOK8S_SPEC_OIDC_CABUNDLE="fixture-ca"
  # The features write succeeds. This yq makes the manifest and its
  # directory read-only just before the caFile write, so only that write
  # fails. The manifest is the last argument.
  _fake_yq "${BATS_TEST_TMPDIR}/fakeyq" 'for a in "$@"; do f="${a}"; done
if [[ "$*" == *caFile* ]]; then chmod 400 "${f}"; chmod 500 "${f%/*}"; fi
exec "${REAL_YQ}" "$@"'
  local saved_path="${PATH}"
  PATH="${BATS_TEST_TMPDIR}/fakeyq:${PATH}"
  TMPDIR="${TMPD}" run kubeone::_inject_oidc "${M}"
  PATH="${saved_path}"
  _writable
  assert_failure
  assert_output --partial "OIDC: failed to set caFile"
  refute_output --partial "OIDC: failed to inject features.openidConnect"
  _assert_tmpdir_empty
}

@test "a failed write of the registry credentials leaves TMPDIR as it was" {
  export DOMAIN_NAME="ko.cloud"
  local cy="${PATH_CLUSTERS}/ko.cloud/cluster.lok8s.yaml"
  local store="${PATH_CLUSTERS}/ko.cloud/secrets"
  mkdir -p "${store}"
  printf 'fixture-user' > "${store}/Secret.fixture-pull.provisioning.username"
  printf 'fixture-password' > "${store}/Secret.fixture-pull.provisioning.password"
  printf '%s\n' 'apiVersion: cluster.lok8s.dev/v1beta1' 'kind: KubeOne' 'metadata:' '  name: ko-test' \
    'spec:' '  registries:' '    registry.example.com:' '      auth:' '        secretRef:' '          name: fixture-pull' > "${cy}"

  # Anti-vacuity: on a manifest that can be written, the same merge writes
  # the fixture credentials. Checked without printing them.
  local w="${BATS_TEST_TMPDIR}/w.yaml"
  printf 'features: {}\n' > "${w}"
  TMPDIR="${TMPD}" run kubeone::_inject_registry_auth "${w}" "${cy}"
  assert_success
  [[ "$(yq -r '.containerRuntime.containerd.registries."registry.example.com".auth.username' "${w}")" == fixture-user ]] \
    || { echo "the merge did not write the username" >&2; return 1; }
  [[ "$(yq -r '.containerRuntime.containerd.registries."registry.example.com".auth.password' "${w}")" == fixture-password ]] \
    || { echo "the merge did not write the password" >&2; return 1; }
  _assert_tmpdir_empty

  _read_only
  TMPDIR="${TMPD}" run kubeone::_inject_registry_auth "${M}" "${cy}"
  _writable
  assert_failure
  assert_output --partial "registry auth: failed to inject 'registry.example.com'"
  _assert_tmpdir_empty
}
