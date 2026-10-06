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
# the text, and leave no kubeone.yaml and no temp file behind (the render
# writes a temp file and renames it only on success).
_refused() {
  _spec "${1}"
  rm -rf "${OUT}"
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_failure
  assert_output --partial "${2}"
  [ ! -e "${M}" ] || { echo "a refused render left ${M}" >&2; return 1; }
  local left; left=$(find "${OUT}" -name '.kubeone.yaml.*' 2>/dev/null)
  [ -z "${left}" ] || { echo "a refused render left temp files: ${left}" >&2; return 1; }
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

@test "generate_config names the reason when the policy file is refused" {
  local p="${POLICY}"
  local plain="lok8s reads a policy file only as plain YAML."
  local spec='  auditLog:
    policy: audit-policy.yaml'
  printf 'apiVersion: audit.k8s.io/v1\nkind: Event\nrules: [{level: None}]\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} is not an audit Policy. The file must have apiVersion audit.k8s.io/v1 and kind Policy. Fix the file, or set spec.auditLog.policy to another file."
  printf 'apiVersion: audit.k8s.io/v1\nkind: Policy\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} has no rules. The apiserver does not start with a policy that has no rules. Add at least one rule to the rules list."
  printf 'apiVersion: [\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} is not valid YAML. Fix the file, or set spec.auditLog.policy to another file."
  printf '%%YAML 1.2\n---\napiVersion: audit.k8s.io/v1\nkind: Policy\nrules: [{level: None}]\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} has a YAML directive (a line that starts with %). ${plain} Remove the directive."
  printf 'kind: Policy\napiVersion: audit.k8s.io/v1\nkind: Policy\nrules: [{level: None}]\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} has a duplicate key. ${plain} Keep each key once."
  printf 'apiVersion: audit.k8s.io/v1\nkind: Policy\nrules:\n  - &r {level: None}\n  - *r\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} has an alias (*name). ${plain} Write the value out in full."
  printf '<<: {apiVersion: audit.k8s.io/v1, kind: Policy}\nrules: [{level: None}]\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} has a merge key (<<). ${plain} Write the keys out in full."
}

@test "generate_config refuses an unreadable policy file with a read error" {
  [[ "$(id -u)" != 0 ]] || skip "root reads a file without read permission"
  chmod 000 "${POLICY}"
  _refused '  auditLog:
    policy: audit-policy.yaml' \
    "audit log: cannot read the policy file ${POLICY}. Make it readable for the user that runs lo."
  chmod 600 "${POLICY}"
}

@test "kubeone::_audit_policy_verdict gives every shared fixture its verdict" {
  # tests/fixtures/audit-policy/<verdict>--<case>.yaml: the same files drive
  # the Go test (TestAuditPolicyVerdictFixtures) and hack/parity-orchestrate.sh.
  local f want got n=0
  local -a wrong=()
  for f in "${FIXTURES_DIR}"/audit-policy/*--*.yaml; do
    want="${f##*/}"; want="${want%%--*}"
    got=$(kubeone::_audit_policy_verdict "${f}")
    [[ "${got}" == "${want}" ]] || wrong+=("${f##*/}: got ${got}")
    n=$(( n + 1 ))
  done
  (( n >= 20 )) || { echo "only ${n} fixtures found" >&2; return 1; }
  (( ${#wrong[@]} == 0 )) || { printf '%s\n' "${wrong[@]}" >&2; return 1; }
}

@test "a refused render keeps the manifest of the last good render" {
  _spec '  auditLog:
    policy: audit-policy.yaml'
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_success
  cp "${M}" "${BATS_TEST_TMPDIR}/good.yaml"
  _spec '  auditLog:
    policy: audit-policy.yaml
    maxAge: 0'
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_failure
  cmp -s "${M}" "${BATS_TEST_TMPDIR}/good.yaml" || { echo "the last good manifest changed" >&2; return 1; }
  [ -z "$(find "${OUT}" -name '.kubeone.yaml.*')" ]
}

@test "driver::provision checks spec.auditLog before the infrastructure step" {
  # The real check, wired into the real driver::provision: a typo must fail
  # before provider::provision reconciles any server.
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"
  :args() { domain="ko.cloud"; }
  kubehz::read_config() { export LOK8S_KUBEHZ_HOSTING="self-hosted"; return 0; }
  local trace="${BATS_TEST_TMPDIR}/trace"; : > "${trace}"
  provider::provision() { echo provision >> "${trace}"; return 0; }
  export PROVIDER_CONFIG_FILE="${BATS_TEST_TMPDIR}/provider.json" PROVIDER_NAME=hetzner
  _spec '  auditLog:
    policy: audit-policy.yaml
    maxBackups: 3'
  local rc=0
  driver::provision ko.cloud 2>"${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -ne 0 ]
  grep -q "spec.auditLog.maxBackups" "${BATS_TEST_TMPDIR}/err"
  [ ! -s "${trace}" ] || { echo "provider::provision ran before the spec.auditLog check" >&2; return 1; }
}
