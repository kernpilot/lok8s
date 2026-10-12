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
  _refused "${spec}" "audit log: ${p} has an anchor or an alias (&name, *name). ${plain} Write the value out in full."
  printf -- '--- null\n---\napiVersion: audit.k8s.io/v1\nkind: Policy\nrules: [{level: None}]\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} has a value on a document marker line (--- or ...). ${plain} Move the value to the next line."
  printf -- '---\n---\napiVersion: audit.k8s.io/v1\nkind: Policy\nrules: [{level: None}]\n' >"${POLICY}"
  _refused "${spec}" "audit log: ${p} starts with an empty YAML document. The apiserver reads only the first document and does not start. Remove the extra --- line before the policy."
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

# _fake_yq <dir> <script body>: a yq on PATH in front of the real one.
_fake_yq() {
  local dir="${1}" real; real=$(command -v yq)
  mkdir -p "${dir}"
  printf '#!/usr/bin/env bash\nREAL_YQ=%q\n%s\n' "${real}" "${2}" > "${dir}/yq"
  chmod +x "${dir}/yq"
}

@test "a yq that cannot run the check is a yq failure, not an invalid file" {
  # An older or broken yq must never turn a valid policy into "is not valid
  # YAML". This yq parses, but fails on one read of the check.
  _fake_yq "${BATS_TEST_TMPDIR}/fakeyq" 'for a in "$@"; do [[ "${a}" == *"anchor"* ]] && { echo "Error: unknown operator: anchor" >&2; exit 1; }; done
exec "${REAL_YQ}" "$@"'
  run env PATH="${BATS_TEST_TMPDIR}/fakeyq:${PATH}" bash -c 'import() { :; }; source "$1"; kubeone::_audit_policy_verdict "$2"' bash "${PATH_LOK8S}/drivers/kubeone/config" "${POLICY}"
  assert_output "yqfailed: Error: unknown operator: anchor"

  _spec '  auditLog:
    policy: audit-policy.yaml'
  local saved_path="${PATH}"
  PATH="${BATS_TEST_TMPDIR}/fakeyq:${PATH}"
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  PATH="${saved_path}"
  assert_failure
  assert_output --partial "audit log: yq failed while it checked ${POLICY}: Error: unknown operator: anchor. Make sure that yq is a working v4 release (b install), then run lo provision again."
  refute_output --partial "is not valid YAML"
}

@test "a yq that does not run at all is a yq failure, not an invalid file" {
  _fake_yq "${BATS_TEST_TMPDIR}/brokenyq" 'echo "yq: cannot execute binary file" >&2; exit 126'
  run env PATH="${BATS_TEST_TMPDIR}/brokenyq:${PATH}" bash -c 'import() { :; }; source "$1"; kubeone::_audit_policy_verdict "$2"' bash "${PATH_LOK8S}/drivers/kubeone/config" "${POLICY}"
  assert_output "yqfailed: yq: cannot execute binary file"
}

@test "kubeone::_audit_policy_verdict gives the same verdicts on a second yq release" {
  # The check uses only small reads that every yq v4 from v4.30 runs. The
  # argsh test image ships its own yq (/usr/local/bin/yq) beside the pinned
  # one; LOK8S_TEST_OLD_YQ names another (a manual run).
  local other="${LOK8S_TEST_OLD_YQ:-/usr/local/bin/yq}" pinned
  pinned=$(command -v yq)
  [[ -x "${other}" ]] || skip "no second yq (${other})"
  [[ "$(readlink -f "${other}")" != "$(readlink -f "${pinned}")" ]] || skip "the second yq is the pinned one"
  # TAP diagnostic: CI shows which yq ran.
  echo "# second yq: $("${other}" --version 2>&1)" >&3
  mkdir -p "${BATS_TEST_TMPDIR}/otheryq"
  ln -s "${other}" "${BATS_TEST_TMPDIR}/otheryq/yq"
  local f want got
  local -a wrong=()
  for f in "${FIXTURES_DIR}"/audit-policy/*--*.yaml; do
    want="${f##*/}"; want="${want%%--*}"
    got=$(PATH="${BATS_TEST_TMPDIR}/otheryq:${PATH}" bash -c 'import() { :; }; source "$1"; kubeone::_audit_policy_verdict "$2"' bash "${PATH_LOK8S}/drivers/kubeone/config" "${f}")
    [[ "${got}" == "${want}" ]] || wrong+=("${f##*/}: got ${got}")
  done
  (( ${#wrong[@]} == 0 )) || { "${other}" --version >&2; printf '%s\n' "${wrong[@]}" >&2; return 1; }
}

@test "a signal during the render leaves no temp file behind" {
  # The temp manifest can hold registry credentials. A child shell renders,
  # and a merge step sends it SIGTERM: the trap removes the temp file and
  # the signal still ends the run.
  _spec '  auditLog:
    policy: audit-policy.yaml'
  run bash -c '
    import() { :; }
    provider::detect() { echo hetzner; }
    source "${4}/utils/verbose.sh"; source "${4}/utils/spec.sh"
    source "${4}/utils/template.sh"; source "${4}/utils/oidc.sh"
    source "${4}/drivers/kubeone/config"
    kubeone::_inject_registry_auth() { kill -TERM $$; sleep 5; }
    kubeone::generate_config "${1}" hetzner "${2}"
    echo "not ended by the signal"
  ' bash "${CY}" "${OUT}" unused "${PATH_LOK8S}"
  [ "${status}" -eq 143 ] || { echo "status ${status}: ${output}" >&2; return 1; }
  refute_output --partial "not ended by the signal"
  [ -z "$(find "${OUT}" -name '.kubeone.yaml.*')" ] || { echo "temp file left: $(ls -a "${OUT}")" >&2; return 1; }
  [ ! -e "${M}" ]
}

@test "the render restores the INT, TERM and HUP traps it found" {
  _spec '  auditLog:
    policy: audit-policy.yaml'
  trap 'echo caller-int' INT
  local before; before=$(trap -p INT TERM HUP)
  # Anti-vacuity: there must be a trap to restore.
  [[ "${before}" == *caller-int* ]] || { echo "no caller trap set: '${before}'" >&2; return 1; }
  kubeone::generate_config "${CY}" hetzner "${OUT}"
  [ "$(trap -p INT TERM HUP)" = "${before}" ] || { echo "traps changed: $(trap -p INT TERM HUP)" >&2; return 1; }
  trap - INT
}

@test "kubeone::_inject_audit_log names the cause: a manifest that does not parse, or a failed write" {
  _spec '  auditLog:
    policy: audit-policy.yaml'
  local m="${BATS_TEST_TMPDIR}/m.yaml"
  printf 'features: [\n' > "${m}"
  run kubeone::_inject_audit_log "${m}" "${CY}"
  assert_failure
  assert_output --partial "audit log: lok8s cannot parse the rendered manifest in ${BATS_TEST_TMPDIR} as YAML. Check the KubeOne template (drivers/kubeone/cluster/core/kubeone.yaml), then run lo provision again."
  run kubeone::_inject_audit_log "${BATS_TEST_TMPDIR}/absent.yaml" "${CY}"
  assert_failure
  assert_output --partial "audit log: lok8s cannot read the rendered manifest in ${BATS_TEST_TMPDIR}. Check the permissions of the directory, then run lo provision again."
  # yq -i writes through a temp file and falls back to writing in place:
  # only a read-only file in a read-only directory makes it fail.
  [[ "$(id -u)" != 0 ]] || skip "root writes a read-only file"
  local ro="${BATS_TEST_TMPDIR}/ro"
  mkdir -p "${ro}"
  printf 'features: {}\n' > "${ro}/m.yaml"
  chmod 400 "${ro}/m.yaml"
  chmod 500 "${ro}"
  run kubeone::_inject_audit_log "${ro}/m.yaml" "${CY}"
  chmod 700 "${ro}"
  chmod 600 "${ro}/m.yaml"
  assert_failure
  assert_output --partial "audit log: cannot write the manifest in ${ro}. Check the free disk space and the permissions of the directory, then run lo provision again."
}

@test "a yq that is not mikefarah yq v4 is a yq failure, not an invalid file" {
  # A distro's Python yq (a jq wrapper) runs `.` but not `document_index`:
  # every valid policy would read as "not valid YAML". This fake behaves
  # the same way.
  _fake_yq "${BATS_TEST_TMPDIR}/pyyq" 'for a in "$@"; do [[ "${a}" == *document_index* ]] && { echo "jq: error: document_index/0 is not defined at <top-level>, line 1:" >&2; exit 3; }; done
echo null'
  run env PATH="${BATS_TEST_TMPDIR}/pyyq:${PATH}" bash -c 'import() { :; }; source "$1"; kubeone::_audit_policy_verdict "$2"' bash "${PATH_LOK8S}/drivers/kubeone/config" "${POLICY}"
  assert_output "yqfailed: jq: error: document_index/0 is not defined at <top-level>, line 1:"
}

@test "generate_config names a temp file it cannot create and a manifest it cannot replace" {
  _spec '  auditLog:
    policy: audit-policy.yaml'
  # kubeone.yaml is a directory: mv would put the file INTO it.
  mkdir -p "${M}/keep"
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  assert_failure
  assert_output --partial "cannot replace ${M}. Check the permissions of ${OUT}, then run lo provision again."
  [ -z "$(find "${OUT}" -name '.kubeone.yaml.*')" ] || { echo "temp file left: $(find "${OUT}" -name '.kubeone.yaml.*')" >&2; return 1; }
  rm -rf "${M}"

  [[ "$(id -u)" != 0 ]] || skip "root writes into a read-only directory"
  chmod 500 "${OUT}"
  run kubeone::generate_config "${CY}" hetzner "${OUT}"
  chmod 700 "${OUT}"
  assert_failure
  assert_output --partial "cannot create a temp file in ${OUT}. Check the free disk space and the permissions of the directory, then run lo provision again."
}
