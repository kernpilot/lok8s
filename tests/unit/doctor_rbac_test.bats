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
# on PATH (the check runs it as a process, not as a shell function). No real
# kubectl runs and no cluster is reached. Only this file knows more answers:
#   hang <s> <json file>          the read ends after <s> seconds
#   tree <s> <json file> <term|ignore>
#                                 the stand-in starts a child that ignores
#                                 TERM (as a credential plugin can), and ends
#                                 after <s> seconds; with "ignore" it ignores
#                                 TERM too
#   early <json file>             the stand-in starts the same child, prints
#                                 the list and exits 0 at once: the child
#                                 stays in the process group of the read
#   int <s> <json file>           the same tree, with "ignore": after 1 s the
#                                 stand-in sends SIGINT to the subshell of the
#                                 reads, as a Ctrl-C does
#   termok <json file>            the read waits; on a TERM it prints the
#                                 list and exits 0
# The stand-in and its child write their pids to STUB_PID and CHILD_PID, and
# the stand-in writes its process group to STUB_PGID. The stand-in writes the
# clock (in microseconds) when it starts to STUB_START, and when it sends the
# SIGINT to INT_SENT.

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
  export STUB_PID="${BATS_TEST_TMPDIR}/kubectl.pid" CHILD_PID="${BATS_TEST_TMPDIR}/child.pid"
  export STUB_PGID="${BATS_TEST_TMPDIR}/kubectl.pgid"
  export STUB_START="${BATS_TEST_TMPDIR}/kubectl.start" INT_SENT="${BATS_TEST_TMPDIR}/int.sent"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"

  # The stand-in kubectl: records its argv, answers from the case directory.
  # It gets _stat_field of test_helper (a script does not load that file).
  STUB="${BATS_TEST_TMPDIR}/stub"
  mkdir -p "${STUB}"
  {
    echo '#!/usr/bin/env bash'
    declare -f _stat_field
    cat <<'STUB'
printf '%s\n' "$*" >> "${CALLS}"
res="${4:-}"
if [[ ! -f "${CASE}/${res}" ]]; then
  echo "unexpected read ${res}"
  exit 0
fi
read -r kind a b < "${CASE}/${res}"
# _tree <term|ignore>: the pid files, and a child that ignores TERM (its
# exec'd sleep inherits that). fd 3 (of bats) stays closed in each sleep.
_tree() {
  echo "${EPOCHREALTIME//[!0-9]/}" > "${STUB_START}"
  echo "$$" > "${STUB_PID}"
  _stat_field "$$" 3 > "${STUB_PGID}"
  [[ "${1}" == term ]] || trap '' TERM
  bash -c 'trap "" TERM; echo "$$" > "${CHILD_PID}"; exec sleep 30' 3>&- &
  # The child has written its pid before the stand-in goes on.
  while [[ ! -s "${CHILD_PID}" ]]; do sleep 0.01; done
}
case "${kind}" in
  json) cat "${FIX}/json/${a}" ;;
  fail) printf '%s\n' "${b}" >&2; exit "${a}" ;;
  hang) sleep "${a}" 3>&-; cat "${FIX}/json/${b}" ;;
  tree)
    read -r b mode <<< "${b}"
    _tree "${mode}"
    sleep "${a}" 3>&-
    cat "${FIX}/json/${b}" ;;
  early)
    _tree term
    cat "${FIX}/json/${a}" ;;
  int)
    _tree ignore
    sleep 1 3>&-
    # The parent of the parent is the subshell of the reads: the check runs
    # the read under a leader, a coprocess of that subshell.
    echo "${EPOCHREALTIME//[!0-9]/}" > "${INT_SENT}"
    kill -INT "$(_stat_field "${PPID}" 2)"
    sleep "${a}" 3>&-
    cat "${FIX}/json/${b}" ;;
  termok)
    # wait returns at once for a trapped signal: the trap then runs.
    trap 'cat "${FIX}/json/${a}"; exit 0' TERM
    sleep 30 3>&- &
    wait ;;
esac
STUB
  } > "${STUB}/kubectl"
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

# _early_case — the narrowed case, with a first read that ends at once and
# leaves a child in its process group (the early answer). Both reads give
# valid lists, so the lines are the ones of the narrowed case.
_early_case() {
  local dir="${BATS_TEST_TMPDIR}/case-early"
  mkdir -p "${dir}"
  cp "${FIX}/cases/narrowed/cluster.lok8s.yaml" "${FIX}/cases/narrowed/clusterroles" "${FIX}/cases/narrowed/want" "${dir}/"
  echo "early crb-narrowed.json" > "${dir}/clusterrolebindings"
  _setup_case "${dir}"
}

NO_ANSWER="  ! RBAC migration leftovers not checked: kubectl did not answer within 30 s (the cluster API, or a credential plugin that waits for a login)"

# _alive <pid> — 0 when the process runs (a zombie does not count): /proc
# where it exists (the CI bats container has no ps), else kill -0.
_alive() {
  if [[ -d /proc/self ]]; then
    local stat
    stat="$(cat "/proc/${1}/stat" 2>/dev/null)" || return 1
    [[ -n "${stat}" ]] || return 1
    stat="${stat##*) }"
    [[ "${stat:0:1}" != Z ]]
  else
    kill -0 "${1}" 2>/dev/null
  fi
}

# _group_alive <pgid> — 0 when a process of the process group runs (a zombie
# does not count): the pgrp field of each /proc/<pid>/stat where /proc
# exists, else kill -0 of the group.
_group_alive() {
  if [[ -d /proc/self ]]; then
    local f stat state pgrp
    for f in /proc/[0-9]*/stat; do
      { IFS= read -r stat < "${f}"; } 2>/dev/null || continue
      # After the command name: state, ppid, pgrp.
      read -r state _ pgrp _ <<< "${stat##*) }"
      if [[ "${pgrp}" == "${1}" && "${state}" != Z ]]; then return 0; fi
    done
    return 1
  fi
  kill -0 -- "-${1}" 2>/dev/null
}

# _assert_tree_gone <clock file> <max ms> — the stand-in and its child no
# longer run, and no other process of the stand-in's process group, at once
# after the check returned (the check waits for them). The stand-in is not
# the leader of that group: the check runs it under a leader that holds the
# group. The check returned less than <max ms> after the clock in the file
# (in microseconds, EPOCHREALTIME of bash 5).
_assert_tree_gone() {
  local now="${EPOCHREALTIME//[!0-9]/}" start took pid pgid
  start="$(cat "${1}")"
  [[ "${start}" =~ ^[0-9]+$ ]] || fail "no clock in ${1}"
  took=$(( (now - start) / 1000 ))
  for pid in "$(cat "${STUB_PID}")" "$(cat "${CHILD_PID}")"; do
    [[ -n "${pid}" ]] || fail "a pid file is empty"
    ! _alive "${pid}" || fail "process ${pid} still runs after the check"
  done
  pgid="$(cat "${STUB_PGID}")"
  [[ "${pgid}" =~ ^[0-9]+$ ]] || fail "no process group in ${STUB_PGID}"
  [[ "${pgid}" != "$(cat "${STUB_PID}")" ]] || fail "the stand-in leads its process group"
  ! _group_alive "${pgid}" || fail "a process of the group of the read still runs after the check"
  (( took < ${2} )) || fail "the check took ${took} ms, want less than ${2} ms"
}

# _log_kill — the check's kill(1) calls go to KILL_LOG, each with the state
# of the target group's leader at the call: "<args>|<state> <parent>
# <group> <shell>" from /proc (the shell is the process that calls kill).
# Then the builtin kill runs. The stand-ins are scripts, so they do not see
# this function.
_log_kill() {
  export KILL_LOG="${BATS_TEST_TMPDIR}/kill.log"
  kill() {
    local t="${*: -1}"
    t="${t#-}"
    printf '%s|%s %s %s %s\n' "$*" "$(_stat_field "${t}" 1)" "$(_stat_field "${t}" 2)" "$(_stat_field "${t}" 3)" "${BASHPID}" >> "${KILL_LOG}"
    builtin kill "$@"
  }
}

# _assert_group_signals <min> — KILL_LOG has at least <min> calls, and each
# one sends TERM, CONT or KILL to the process group of the stand-in
# (never to one pid), while the leader of that group runs (not a zombie)
# as a child of the shell that sends the signal.
_assert_group_signals() {
  local pgid line args st n=0
  pgid="$(cat "${STUB_PGID}")"
  [[ -s "${KILL_LOG}" ]] || fail "no kill call"
  while IFS= read -r line; do
    n=$(( n + 1 ))
    args="${line%%|*}" st="${line#*|}"
    [[ "${args}" =~ ^-(TERM|CONT|KILL)\ --\ -${pgid}$ ]] || fail "a kill call that is not a signal to the group -${pgid}: ${args}"
    read -r -a st <<< "${st}"
    [[ "${st[0]}" != Z && -n "${st[0]}" ]] || fail "a signal after the leader ended: ${line}"
    [[ "${st[1]}" == "${st[3]}" && "${st[2]}" == "${pgid}" ]] || fail "a signal while the leader is not a child of the shell that leads the group: ${line}"
  done < "${KILL_LOG}"
  (( n >= ${1} )) || fail "${n} kill calls, want at least ${1}"
}

@test "rbac: a leftover SUC binding under another name with another subject gets no delete command" { _case suc-leftover-under-another-name; }
@test "rbac: the User name of the token and the groups of the token count" { _case identity-forms; }
@test "rbac: the new role and pods/exec count through the User name of the token" { _case user-form; }
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
@test "rbac: a label that is not a string makes the list invalid" { _case read-malformed-label; }
@test "rbac: a key in another case is not the field" { _case read-key-case; }
@test "rbac: a second JSON document makes the list invalid" { _case read-two-documents; }
@test "rbac: a null item makes the list invalid" { _case read-null-item; }
@test "rbac: a leading byte-order mark makes the list invalid" { _case read-bom; }
@test "rbac: a NaN makes the list invalid" { _case read-nan; }
@test "rbac: an infinite number makes the list invalid" { _case read-infinity; }
@test "rbac: a number out of the float64 range makes the list invalid" { _case read-out-of-range; }
@test "rbac: redis-operator old binding, old role and aggregation labels" { _case redis-old-role-binding-and-labels; }
@test "rbac: redis-operator old role without its binding" { _case redis-old-role-alone; }
@test "rbac: only the exact aggregation keys are printed, never another key" { _case redis-old-role-hostile-labels; }
@test "rbac: the old role gets no delete command while bindings give it to other subjects" { _case redis-old-role-shared; }
@test "rbac: the old role with one binding of other subjects" { _case redis-old-role-one-ref; }
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
@test "rbac: the bound stops a hung read" {
  doctor::_rbac_bound() { echo 1; }
  _hang_case "hang 5 crb-narrowed.json" "${NO_ANSWER}"
  run doctor::_rbac_check rbac.example
  _assert_case
}

# The bound stops the whole process group of the read: a child that ignores
# TERM gets the KILL after the grace (2 s). The check returns only when the
# stand-in and its child are gone: after the start of the stand-in, within
# the bound (1 s) plus the grace, plus 1 s for the processes of the test.
@test "rbac: the bound stops a child of the read that ignores TERM" {
  doctor::_rbac_bound() { echo 1; }
  _hang_case "tree 30 crb-narrowed.json term" "${NO_ANSWER}"
  _log_kill
  run doctor::_rbac_check rbac.example
  unset -f kill
  _assert_case
  _assert_tree_gone "${STUB_START}" 4000
  _assert_group_signals 3
}

# A read that ends first (here with 0 and a valid list) and leaves a child
# in its process group: the child gets the stop of the group too (Go:
# stopMembers), TERM and, as it ignores TERM, KILL after the grace (2 s). The
# lines are the ones of the valid lists. The check returns after the grace,
# plus 1 s for the second read and the processes of the test.
@test "rbac: a read that ends first stops the child that it leaves in its group" {
  _early_case
  _log_kill
  run doctor::_rbac_check rbac.example
  unset -f kill
  _assert_case
  _assert_tree_gone "${STUB_START}" 4000
  _assert_group_signals 3
}

# Each signal of the stop goes to the process group of the read, never to
# one pid, and only while the leader of the group runs: the leader holds the
# id of the group until the stop is done.
@test "rbac: the bound stops a read that ignores TERM, and its child" {
  doctor::_rbac_bound() { echo 1; }
  _hang_case "tree 30 crb-narrowed.json ignore" "${NO_ANSWER}"
  _log_kill
  run doctor::_rbac_check rbac.example
  unset -f kill
  _assert_case
  _assert_tree_gone "${STUB_START}" 4000
  _assert_group_signals 3
}

# A read that the bound stopped gives the reason of the bound, also when the
# read handles the TERM and exits 0 with a valid list: the timeout mark comes
# before the exit code (Go: rbacRead checks the context first).
@test "rbac: a read that exits 0 after the stop of the bound gives the reason of the bound" {
  doctor::_rbac_bound() { echo 1; }
  _hang_case "termok crb-narrowed.json" "${NO_ANSWER}"
  run doctor::_rbac_check rbac.example
  _assert_case
}

# The command builder checks the name again (Go: rbacDeleteCommand): for a
# name that is not a DNS-1123 subdomain of at most 253 characters it prints
# nothing and fails, also when a caller did not check the name.
@test "rbac: the delete command builder refuses a name that is not a DNS-1123 subdomain" {
  local name long
  long="$(printf 'a%.0s' {1..253})"
  for name in "" "a b" 'a;rm -rf x' $'a\nb' $'a\tb' A 'a$(x)' -a a. "${long}a" $'a\e[31m' é; do
    run doctor::_rbac_delete /k clusterrolebinding "${name}"
    assert_failure
    assert_output ""
  done
  for name in a redis-operator a.b-c "${long}"; do
    run doctor::_rbac_delete /k clusterrolebinding "${name}"
    assert_success
    assert_output "kubectl --kubeconfig '/k' delete clusterrolebinding ${name}"
  done
}

# A Ctrl-C in a read: the INT trap of the reads' subshell stops the process
# group of the read (the stand-in and its child ignore TERM: the KILL after
# the grace ends them), waits for it, and removes the temp dir. The caller
# here gets no SIGINT, so it goes on and prints the fixed reason; its own
# trap stays as it was. The check returns within the grace after the
# SIGINT, plus 1 s for the processes of the test.
@test "rbac: a Ctrl-C in a read stops its process group and removes the temp dir" {
  _hang_case "int 30 crb-narrowed.json" "  ! RBAC migration leftovers not checked: kubectl failed or did not return a valid list"
  _caller() {
    trap ': the caller' INT
    doctor::_rbac_check rbac.example
    trap -p INT
  }
  run _caller
  assert_success
  assert_output "$(cat "${CASE}/want")"$'\n'"trap -- ': the caller' SIGINT"
  _assert_no_temp_dir
  _assert_tree_gone "${INT_SENT}" 3000
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
