#!/usr/bin/env bats
# doctor_rbac_test.bats — the RBAC migration check of `lo doctor`
# (doctor::_rbac_check; issues #311 and #315).
#
# The cases are shared with the Go twin (internal/cli/cmd_doctor_rbac_test.go)
# under tests/fixtures/doctor-rbac/cases/<case>/:
#   cluster.lok8s.yaml   the domain's spec (absent: the domain has none)
#   deploy.lok8s.yaml    the domain's deploy spec (absent: none)
#   kubeconfig-name      the name of the kubeconfig file (absent: rbac-test)
#   no-kubeconfig        present: the domain has no kubeconfig
#   clusterrolebindings  the answer of that read: `json <file in json/>`, or
#   clusterroles         `fail <rc> <stderr>`. Absent: the read must not run.
#   want                 the exact output of the check; @BASE@ stands for the
#                        project directory (the kubeconfig in a command)
# Every failed read carries the word CANARY in its stderr (or its stdout): the
# check must never print it. kubectl is a stand-in script of this test, first
# on PATH (the check runs it through timeout(1), which cannot run a shell
# function). No real kubectl runs and no cluster is reached. Only this file
# knows two more answers: `hang <s> <json file>` (the read ends after <s>
# seconds) and `int <s> <json file>` (the stand-in sends SIGINT to the
# subshell of the reads, as a Ctrl-C does).

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

  export FIX="${_PROJECT_ROOT}/tests/fixtures/doctor-rbac"
  export PATH_BASE="${BATS_TEST_TMPDIR}/project"
  export PATH_CLUSTERS="${PATH_BASE}/clusters"
  # As in a project (and the Go twin's test): the tree under PATH_BASE. The
  # check reads no file of it.
  export PATH_LOK8S="${PATH_BASE}/.lok8s"
  unset LOK8S_CLUSTER_NAME
  export CALLS="${BATS_TEST_TMPDIR}/kubectl.calls"
  export STUB_PID="${BATS_TEST_TMPDIR}/kubectl.pid"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"

  # The stand-in kubectl: records its argv, answers from the case directory.
  STUB="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${STUB}"
  cat > "${STUB}/kubectl" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${CALLS}"
res="${4:-}"
if [[ ! -f "${CASE}/${res}" ]]; then
  echo "unexpected read ${res}"
  exit 0
fi
read -r kind a b < "${CASE}/${res}"
case "${kind}" in
  json) cat "${FIX}/json/${a}" ;;
  fail) printf '%s\n' "${b}" >&2; exit "${a}" ;;
  hang) sleep "${a}" 3>&-; cat "${FIX}/json/${b}" ;;
  int)
    echo "$$" > "${STUB_PID}"
    sleep 1 3>&-
    # The subshell of the reads is INT_HOPS parents up: 1 is the parent,
    # 2 the parent of timeout(1). _ppid (test_helper, exported by the test)
    # reads /proc where it exists, so the case needs no ps.
    target="${PPID}"
    if (( INT_HOPS > 1 )); then target="$(_ppid "${target}")"; fi
    kill -INT "${target}"
    sleep "${a}" 3>&-
    cat "${FIX}/json/${b}" ;;
esac
STUB
  chmod +x "${STUB}/kubectl"
  export PATH="${STUB}:${PATH}"
  [[ "$(command -v kubectl)" == "${STUB}/kubectl" ]] || fail "the stand-in kubectl is not first on PATH"
}

teardown() { teardown_tmpdir; }

# _case <name> — run the check for the case and compare the output and the
# reads with what the case says.
_case() {
  _setup_case "${FIX}/cases/${1}"
  run doctor::_rbac_check rbac.example
  _assert_case
}

# _setup_case <dir> — the domain of the case under PATH_BASE.
_setup_case() {
  export CASE="${1}"
  [[ -f "${CASE}/want" ]] || fail "no case ${CASE}"
  KC_NAME=rbac-test
  if [[ -f "${CASE}/kubeconfig-name" ]]; then KC_NAME="$(cat "${CASE}/kubeconfig-name")"; fi
  mkdir -p "${PATH_CLUSTERS}/rbac.example"
  local f
  for f in cluster.lok8s.yaml deploy.lok8s.yaml; do
    if [[ -f "${CASE}/${f}" ]]; then cp "${CASE}/${f}" "${PATH_CLUSTERS}/rbac.example/${f}"; fi
  done
  if [[ ! -f "${CASE}/no-kubeconfig" ]]; then
    mkdir -p "${PATH_BASE}/.kubeconfig"
    printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/${KC_NAME}.yaml"
  fi
}

# _assert_case — the output and the reads of the last run against the case,
# and no temp dir left.
_assert_case() {
  assert_success
  # The project directory has no single quote, so the case's quoting of the
  # kubeconfig path is the check's own.
  [[ "${PATH_BASE}" != *[\'@]* ]] || fail "the project directory ${PATH_BASE} has a quote or an @"
  local want
  want="$(cat "${CASE}/want")"
  assert_output "${want//@BASE@/"${PATH_BASE}"}"
  refute_output --partial CANARY
  local want_calls="" res
  for res in clusterrolebindings clusterroles; do
    if [[ -f "${CASE}/${res}" ]]; then
      want_calls+="--kubeconfig ${PATH_BASE}/.kubeconfig/${KC_NAME}.yaml get ${res} -o json --request-timeout=10s"$'\n'
    fi
  done
  assert_equal "$(cat "${CALLS}" 2>/dev/null)" "${want_calls%$'\n'}"
  _assert_no_temp_dir
}

_assert_no_temp_dir() {
  local left
  left="$(find "${TMPDIR}" -maxdepth 1 -name 'lo-doctor-rbac.*')"
  [[ -z "${left}" ]] || fail "temp dir left: ${left}"
}

# _path_without <tool...> — prints a PATH of one dir with a link to each
# program of PATH (the first one of a name wins, so the stand-in kubectl
# too), except the named tools.
_path_without() {
  local dir="${BATS_TEST_TMPDIR}/path-without" d t
  rm -rf "${dir}"
  mkdir -p "${dir}"
  local -a dirs=()
  IFS=: read -ra dirs <<< "${PATH}"
  for d in "${dirs[@]}"; do
    [[ -d "${d}" ]] || continue
    ln -s "${d}"/* "${dir}/" 2>/dev/null || true
  done
  for t in "$@"; do rm -f "${dir}/${t}"; done
  for t in "$@"; do [[ ! -e "${dir}/${t}" ]] || fail "${t} is still in ${dir}"; done
  if [[ -e "${dir}/kubectl" && "$(readlink "${dir}/kubectl")" != "${STUB}/kubectl" ]]; then
    fail "the kubectl of ${dir} is not the stand-in"
  fi
  echo "${dir}"
}

# _hang_case <answer of clusterrolebindings> <want> — a case dir for the
# bound and Ctrl-C tests (bats only: the Go twin tests its bound with a
# runner). The first read fails, so the second one must not run: the case
# has no clusterroles answer.
_hang_case() {
  local dir="${BATS_TEST_TMPDIR}/case-hang"
  mkdir -p "${dir}"
  cp "${FIX}/cases/narrowed/cluster.lok8s.yaml" "${dir}/"
  echo "${1}" > "${dir}/clusterrolebindings"
  printf '%s\n' "${2}" > "${dir}/want"
  _setup_case "${dir}"
}

NO_ANSWER="  ! RBAC migration leftovers not checked: kubectl did not answer within 30 s (the cluster API, or a credential plugin that waits for a login)"

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
@test "rbac: a read without a login prints the fixed reason" { _case read-not-logged-in; }
@test "rbac: an unreachable API prints the fixed reason" { _case read-unreachable; }
@test "rbac: a refused connection prints the fixed reason" { _case read-connection-refused; }
@test "rbac: kubectl's refused text prints the fixed reason" { _case read-was-refused; }
@test "rbac: an unknown host prints the fixed reason" { _case read-no-such-host; }
@test "rbac: a timeout prints the fixed reason" { _case read-timeout; }
@test "rbac: a deadline prints the fixed reason" { _case read-deadline-exceeded; }
@test "rbac: an unreachable network prints the fixed reason" { _case read-network-unreachable; }
@test "rbac: no route prints the fixed reason" { _case read-no-route; }
@test "rbac: another kubectl failure prints the fixed reason" { _case read-other; }
@test "rbac: an answer that is not a list prints the fixed reason" { _case read-not-a-list; }
@test "rbac: a failed second read prints the fixed reason" { _case read-second-fails; }
@test "rbac: a malformed field prints the fixed reason and never its value" { _case read-malformed-field; }
@test "rbac: redis-operator old binding, old role and aggregation labels" { _case redis-old-role-binding-and-labels; }
@test "rbac: redis-operator old role without its binding" { _case redis-old-role-alone; }
@test "rbac: redis-operator leftovers before the new bindings exist" { _case redis-new-bindings-missing; }
@test "rbac: redis-operator without pods/exec (issue #315)" { _case redis-no-exec; }
@test "rbac: the redis-operator-chart-rbac binding after its opt-out" { _case redis-chart-rbac-opt-out; }
@test "rbac: the redis-operator-chart-rbac binding with its opt-in" { _case redis-chart-rbac-opt-in; }
@test "rbac: the redis-operator-chart-rbac opt-in as a path does not count" { _case redis-chart-rbac-opt-in-by-path; }
@test "rbac: pods/exec through a * group" { _case exec-wildcard-group; }
@test "rbac: pods/exec through a * resource" { _case exec-wildcard-resource; }
@test "rbac: pods/exec through a * verb" { _case exec-wildcard-verb; }
@test "rbac: pods/exec through */exec" { _case exec-wildcard-subresource; }
@test "rbac: subjects null and rules null" { _case null-subjects-and-rules; }
@test "rbac: a spec without metadata.name reads the kubeconfig local" { _case spec-without-metadata-name; }
@test "rbac: a name that is not a DNS-1123 subdomain prints no name and no command" { _case unusual-names; }
@test "rbac: a deploy domain skips with the skip line" { _case deploy-domain; }
@test "rbac: a delete command quotes the kubeconfig path" { _case kubeconfig-quote; }
@test "rbac: a kubeconfig path with a control character skips" { _case kubeconfig-control-char; }

# The records step alone, without the checks of the reads before it: a jq
# error on a malformed field quotes the value. The step must fail and never
# print the error (the caller drops the records of a failed step).
@test "rbac: the records step fails on a malformed field and never prints its value" {
  run doctor::_rbac_records "${FIX}/json/crb-narrowed.json" "${FIX}/json/cr-malformed-field.json" 0
  assert_failure
  refute_output --partial CANARY
  refute_output --partial "jq:"
}

@test "rbac: no active domain prints no line (the domain section says it)" {
  run doctor::_rbac_check ""
  assert_success
  assert_output ""
}

@test "rbac: no yq says not checked and reads nothing" {
  _setup_case "${FIX}/cases/narrowed"
  local p; p="$(_path_without yq)"
  PATH="${p}" run doctor::_rbac_check rbac.example
  assert_success
  assert_output "  ! RBAC migration leftovers not checked: yq not found"
  [[ ! -e "${CALLS}" ]] || fail "a read ran: $(cat "${CALLS}")"
}

@test "rbac: no jq says not checked and reads nothing" {
  _setup_case "${FIX}/cases/narrowed"
  local p; p="$(_path_without jq)"
  PATH="${p}" run doctor::_rbac_check rbac.example
  assert_success
  assert_output "  ! RBAC migration leftovers not checked: jq not found"
  [[ ! -e "${CALLS}" ]] || fail "a read ran: $(cat "${CALLS}")"
}

@test "rbac: no kubectl says not checked" {
  _setup_case "${FIX}/cases/narrowed"
  local p; p="$(_path_without kubectl)"
  PATH="${p}" run doctor::_rbac_check rbac.example
  assert_success
  assert_output "  ! RBAC migration leftovers not checked: kubectl not found"
}

# The bound stops a read that does not end (a credential plugin that waits
# for a login). The stand-in answers a valid list after 5 s: without the
# bound, the check passes and this fails.
@test "rbac: the bound stops a hung read (timeout)" {
  command -v timeout >/dev/null 2>&1 || skip "no timeout(1)"
  doctor::_rbac_bound() { echo 1; }
  _hang_case "hang 5 crb-narrowed.json" "${NO_ANSWER}"
  run doctor::_rbac_check rbac.example
  _assert_case
}

@test "rbac: the bound stops a hung read (no timeout, the watchdog)" {
  doctor::_rbac_bound() { echo 1; }
  _hang_case "hang 5 crb-narrowed.json" "${NO_ANSWER}"
  local p; p="$(_path_without timeout)"
  PATH="${p}" run doctor::_rbac_check rbac.example
  _assert_case
}

# _ctrl_c <PATH> <hops> — a Ctrl-C in a read: the INT trap of the reads'
# subshell stops the read and removes the temp dir. The caller here gets no
# SIGINT, so it goes on and prints the fixed reason; its own trap stays as
# it was. <hops>: the parents from the stand-in up to that subshell.
_ctrl_c() {
  _hang_case "int 5 crb-narrowed.json" "  ! RBAC migration leftovers not checked: kubectl failed or did not return a valid list"
  export INT_HOPS="${2}" PS_RAN="${BATS_TEST_TMPDIR}/ps.ran"
  export -f _ppid
  # With /proc (Linux), as in the CI bats container: no ps. A ps call fails
  # here and leaves a mark. Without /proc (macOS), _ppid needs the real ps.
  if [[ -d /proc/self ]]; then
    ps() { : > "${PS_RAN}"; echo "ps ran" >&2; return 127; }
    export -f ps
  fi
  _caller() {
    trap ': the caller' INT
    doctor::_rbac_check rbac.example
    trap -p INT
  }
  PATH="${1}" run _caller
  assert_success
  assert_output "$(cat "${CASE}/want")"$'\n'"trap -- ': the caller' SIGINT"
  _assert_no_temp_dir
  [[ ! -e "${PS_RAN}" ]] || fail "ps ran"
  # The TERM of the trap ends the stand-in at once; allow 2 s for the reap.
  local pid i
  pid="$(cat "${STUB_PID}")"
  for (( i = 0; i < 20; i++ )); do
    kill -0 "${pid}" 2>/dev/null || return 0
    sleep 0.1
  done
  fail "the read still runs (pid ${pid})"
}

@test "rbac: a Ctrl-C in a read removes the temp dir and stops the read (timeout)" {
  command -v timeout >/dev/null 2>&1 || skip "no timeout(1)"
  _ctrl_c "${PATH}" 2
}

@test "rbac: a Ctrl-C in a read removes the temp dir and stops the read (no timeout)" {
  _ctrl_c "$(_path_without timeout)" 1
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
