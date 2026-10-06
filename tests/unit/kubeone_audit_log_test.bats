#!/usr/bin/env bats
# kubeone_audit_log_test.bats — spec.auditLog → features.staticAuditLog in the
# frozen bash twin (kubeone::_inject_audit_log, run through
# kubeone::generate_config). The Go twin is pinned by
# internal/driver/kubeone/auditlog_test.go; hack/parity-orchestrate.sh diffs
# the two. Uses the real yq / envsubst provided by `argsh test`.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"

  import() { :; }
  export -f import

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  if ! command -v envsubst &>/dev/null; then skip "envsubst not available"; fi

  provider::detect() { echo "hetzner"; }
  export -f provider::detect
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"

  CY="${PATH_CLUSTERS}/ko.cloud/cluster.lok8s.yaml"
  POLICY="${PATH_CLUSTERS}/ko.cloud/audit-policy.yaml"
  OUT="${PATH_CLUSTERS}/ko.cloud/.kubeone"
  M="${OUT}/kubeone.yaml"
  mkdir -p "${PATH_CLUSTERS}/ko.cloud"
  cat >"${POLICY}" <<'YAML'
apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: Metadata
    resources:
      - group: ""
        resources: ["secrets"]
  - level: None
YAML
}

teardown() {
  teardown_tmpdir
}

# _spec [extra-spec-lines] — the cluster spec; extra lines are appended under
# spec: (two-space indent, as written by the caller).
_spec() {
  cat >"${CY}" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: ko-test
spec:
  kubernetes:
    version: "v1.35.5"
  provider:
    name: hetzner
YAML
  if [[ -n "${1:-}" ]]; then printf '%s\n' "${1}" >>"${CY}"; fi
}

@test "generate_config merges spec.auditLog into features.staticAuditLog" {
  _spec '  oidc:
    issuer: https://id.kubehz.dev
    clientID: kubectl
  auditLog:
    policy: audit-policy.yaml
    maxAge: 30
    maxBackup: 10
    maxSize: 100'
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_success
  run yq -r '.features.staticAuditLog.enable' "${M}"
  assert_output "true"
  run yq -r '.features.staticAuditLog.config.policyFilePath' "${M}"
  assert_output "${POLICY}"
  [[ "${output}" == /* ]]
  # The limits land as integers (KubeOne's fields are int), not as text.
  run yq -r '.features.staticAuditLog.config | [.logMaxAge, .logMaxBackup, .logMaxSize] | map(tag + "=" + tostring) | join(" ")' "${M}"
  assert_output "!!int=30 !!int=10 !!int=100"
  # KubeOne's default log path applies.
  run yq -r '.features.staticAuditLog.config | has("logPath")' "${M}"
  assert_output "false"
  # The sibling features survive the merge.
  run yq -r '.features.encryptionProviders.enable' "${M}"
  assert_output "true"
  run yq -r '.features.openidConnect.enable' "${M}"
  assert_output "true"
}

@test "generate_config omits the limits the spec does not set" {
  # maxAge without a value reads as null: unset, like an absent field.
  _spec '  auditLog:
    policy: audit-policy.yaml
    maxAge:
    maxBackup: 7'
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_success
  run yq -r '.features.staticAuditLog.config | keys | join(" ")' "${M}"
  assert_output "policyFilePath logMaxBackup"
}

@test "kubeone::_abs_path cleans a path the way Go's filepath.Abs does" {
  run kubeone::_abs_path "/a//b/./c/../d/"
  assert_output "/a/b/d"
  run kubeone::_abs_path "/../x"
  assert_output "/x"
  run kubeone::_abs_path "/"
  assert_output "/"
  cd "${BATS_TEST_TMPDIR}"
  run kubeone::_abs_path "x/../y"
  assert_output "${BATS_TEST_TMPDIR}/y"
}

@test "generate_config resolves the policy against the cluster dir and cleans the path" {
  mkdir -p "${PATH_CLUSTERS}/shared"
  cp "${POLICY}" "${PATH_CLUSTERS}/shared/audit-policy.yaml"
  _spec '  auditLog:
    policy: ./sub/../../shared/audit-policy.yaml'
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_success
  run yq -r '.features.staticAuditLog.config.policyFilePath' "${M}"
  assert_output "${PATH_CLUSTERS}/shared/audit-policy.yaml"
}

@test "generate_config leaves the manifest without staticAuditLog when spec.auditLog is absent or null" {
  _spec
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_success
  run yq -r '.features | has("staticAuditLog")' "${M}"
  assert_output "false"
  _spec '  auditLog:'
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_success
  run yq -r '.features | has("staticAuditLog")' "${M}"
  assert_output "false"
}

# _refused <spec-lines> <expected error text> — generate_config must fail with
# the text, and the refused block must not reach the manifest.
_refused() {
  _spec "${1}"
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_failure
  assert_output --partial "${2}"
  run yq -r '.features | has("staticAuditLog")' "${M}"
  assert_output "false"
}

@test "generate_config refuses a spec.auditLog that is not a mapping" {
  _refused '  auditLog: true' \
    "audit log: spec.auditLog in ${CY} is not a mapping. Set spec.auditLog.policy to the path of an audit Policy file."
}

@test "generate_config refuses an unknown spec.auditLog field" {
  _refused '  auditLog:
    policy: audit-policy.yaml
    maxBackups: 3' \
    "audit log: spec.auditLog.maxBackups in ${CY} is not a known field. Use policy, maxAge, maxBackup or maxSize."
}

@test "generate_config refuses a missing, empty or non-text policy" {
  _refused '  auditLog:
    maxAge: 3' \
    "audit log: spec.auditLog.policy in ${CY} is not set. Set it to the path of an audit Policy file, relative to the cluster file."
  _refused '  auditLog:
    policy: ""' \
    "audit log: spec.auditLog.policy in ${CY} is not set."
  _refused '  auditLog:
    policy: [a.yaml]' \
    "audit log: spec.auditLog.policy in ${CY} is not a file path (found !!seq). Set it to the path of an audit Policy file, relative to the cluster file."
}

@test "generate_config refuses a limit that is not a whole number from 1 to 999999999" {
  local v want
  for v in "0|!!int '0'" "-5|!!int '-5'" "\"10\"|!!str '10'" "1.5|!!float '1.5'" "{days: 3}|!!map" "1000000000|!!int '1000000000'"; do
    want="audit log: spec.auditLog.maxAge in ${CY} must be a whole number from 1 to 999999999 (found ${v#*|}). Fix the value, or remove the field to use the KubeOne default."
    _refused "  auditLog:
    policy: audit-policy.yaml
    maxAge: ${v%%|*}" "${want}"
  done
}

@test "generate_config refuses a policy file that does not exist" {
  rm -f "${POLICY}"
  _refused '  auditLog:
    policy: audit-policy.yaml' \
    "audit log: policy file not found: ${POLICY} (spec.auditLog.policy: audit-policy.yaml). Create the file, or fix spec.auditLog.policy in ${CY}."
  mkdir -p "${POLICY}"
  _refused '  auditLog:
    policy: audit-policy.yaml' \
    "audit log: policy file not found: ${POLICY}"
}

@test "generate_config refuses a policy file that is not an audit.k8s.io/v1 Policy" {
  local content
  local want="audit log: ${POLICY} is not an audit Policy. The file must be YAML with apiVersion audit.k8s.io/v1 and kind Policy. Fix the file, or set spec.auditLog.policy to another file."
  local -a contents=(
    $'apiVersion: audit.k8s.io/v1\nkind: Event'
    $'apiVersion: audit.k8s.io/v1beta1\nkind: Policy'
    ''
    $'- apiVersion: audit.k8s.io/v1\n  kind: Policy'
    'apiVersion: ['
    $'apiVersion: audit.k8s.io/v1\nkind: Policy\n---\nrules: ['
  )
  for content in "${contents[@]}"; do
    printf '%s\n' "${content}" >"${POLICY}"
    _refused '  auditLog:
    policy: audit-policy.yaml' "${want}"
  done
}
