#!/usr/bin/env bats
# doctor_rbac_test.bats — the RBAC migration check of `lo doctor`
# (doctor::_rbac_check; issues #311 and #315).
#
# The cases are shared with the Go twin (internal/cli/cmd_doctor_rbac_test.go)
# under tests/fixtures/doctor-rbac/cases/<case>/:
#   cluster.lok8s.yaml   the domain's spec (absent: the domain has none)
#   no-kubeconfig        present: the domain has no kubeconfig
#   clusterrolebindings  the answer of that read: `json <file in json/>`, or
#   clusterroles         `fail <rc> <stderr>`. Absent: the read must not run.
#   want                 the exact output of the check
# Every failed read carries the word CANARY in its stderr (or its stdout): the
# check must never print it. kubectl is a shell function of this test: no
# program on PATH runs and no cluster is reached.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }; export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  source "${_PROJECT_ROOT}/.lok8s/libs/doctor"

  if ! command -v yq &>/dev/null; then skip "yq not available"; fi
  if ! command -v jq &>/dev/null; then skip "jq not available"; fi

  FIX="${_PROJECT_ROOT}/tests/fixtures/doctor-rbac"
  export PATH_BASE="${BATS_TEST_TMPDIR}/project"
  export PATH_CLUSTERS="${PATH_BASE}/clusters"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  unset LOK8S_CLUSTER_NAME
  CALLS="${BATS_TEST_TMPDIR}/kubectl.calls"

  # The stub: records its argv, answers from the case directory.
  kubectl() {
    printf '%s\n' "$*" >> "${CALLS}"
    local res="${4:-}" kind a b
    if [[ ! -f "${CASE}/${res}" ]]; then
      echo "unexpected read ${res}"
      return 0
    fi
    read -r kind a b < "${CASE}/${res}"
    case "${kind}" in
      json) cat "${FIX}/json/${a}" ;;
      fail) printf '%s\n' "${b}" >&2; return "${a}" ;;
    esac
  }
}

teardown() { teardown_tmpdir; }

# _case <name> — run the check for the case and compare the output and the
# reads with what the case says.
_case() {
  CASE="${FIX}/cases/${1}"
  [[ -f "${CASE}/want" ]] || fail "no case ${1}"
  mkdir -p "${PATH_CLUSTERS}/rbac.example"
  if [[ -f "${CASE}/cluster.lok8s.yaml" ]]; then
    cp "${CASE}/cluster.lok8s.yaml" "${PATH_CLUSTERS}/rbac.example/cluster.lok8s.yaml"
  fi
  if [[ ! -f "${CASE}/no-kubeconfig" ]]; then
    mkdir -p "${PATH_BASE}/.kubeconfig"
    printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/rbac-test.yaml"
  fi

  run doctor::_rbac_check rbac.example
  assert_success
  assert_output "$(cat "${CASE}/want")"
  refute_output --partial CANARY

  local want_calls="" res
  for res in clusterrolebindings clusterroles; do
    if [[ -f "${CASE}/${res}" ]]; then
      want_calls+="--kubeconfig ${PATH_BASE}/.kubeconfig/rbac-test.yaml get ${res} -o json --request-timeout=10s"$'\n'
    fi
  done
  assert_equal "$(cat "${CALLS}" 2>/dev/null)" "${want_calls%$'\n'}"
}

@test "rbac: a leftover SUC binding under another name is found" { _case suc-leftover-under-another-name; }
@test "rbac: SUC leftover before the new bindings exist says to apply first" { _case suc-new-bindings-missing; }
@test "rbac: a narrowed cluster passes" { _case narrowed; }
@test "rbac: no kubeconfig skips with the skip line" { _case no-kubeconfig; }
@test "rbac: neither addon in spec.bootstrap skips" { _case addon-not-in-spec; }
@test "rbac: the SUC cluster-admin opt-in skips the SUC check" { _case suc-cluster-admin-opt-in; }
@test "rbac: the SUC opt-in still reads for redis-operator" { _case suc-opt-in-redis-checked; }
@test "rbac: no cluster spec skips" { _case no-cluster-spec; }
@test "rbac: an unreadable cluster spec skips" { _case spec-cannot-be-read; }
@test "rbac: a forbidden read prints the fixed reason" { _case read-forbidden; }
@test "rbac: an unauthorized read prints the fixed reason" { _case read-unauthorized; }
@test "rbac: an unreachable API prints the fixed reason" { _case read-unreachable; }
@test "rbac: another kubectl failure prints the fixed reason" { _case read-other; }
@test "rbac: an answer that is not a list prints the fixed reason" { _case read-not-a-list; }
@test "rbac: a failed second read prints the fixed reason" { _case read-second-fails; }
@test "rbac: redis-operator old binding, old role and aggregation labels" { _case redis-old-role-binding-and-labels; }
@test "rbac: redis-operator old role without its binding" { _case redis-old-role-alone; }
@test "rbac: redis-operator leftovers before the new bindings exist" { _case redis-new-bindings-missing; }
@test "rbac: redis-operator without pods/exec (issue #315)" { _case redis-no-exec; }
@test "rbac: the redis-operator-chart-rbac binding after its opt-out" { _case redis-chart-rbac-opt-out; }
@test "rbac: the redis-operator-chart-rbac binding with its opt-in" { _case redis-chart-rbac-opt-in; }

@test "rbac: no active domain skips" {
  run doctor::_rbac_check ""
  assert_success
  assert_output "  ℹ RBAC migration check skipped: no active domain"
}

# Every case directory has a @test above, and the Go twin reads the same
# directory: a new case runs in both suites or this fails.
@test "rbac: every fixture case has a bats test" {
  local dir name
  for dir in "${FIX}"/cases/*/; do
    name="$(basename "${dir}")"
    grep -q "_case ${name};" "${BATS_TEST_FILENAME}" || fail "case ${name} has no @test"
  done
}
