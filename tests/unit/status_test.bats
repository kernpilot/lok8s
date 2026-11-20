#!/usr/bin/env bats
# status_test.bats — unit tests for .lok8s/libs/status
#
# Covers status::tilt, which must (a) only run for `lo` (kind) clusters and
# (b) report liveness from lok8s's own PID file rather than the bogus
# `tilt status` subcommand (`lo status` on a Kkp domain ran `tilt status` →
# `unknown command "status" for "tilt"`).
#
# Also covers the kubectl calls of status::all: the /readyz probe, then the
# two reads (status::nodes, status::inventory). Each ends with
# --request-timeout=5s, for every driver result (issue #194). A failed
# probe skips both reads. Each call runs under timeout(1) when it is on
# PATH, and a call that does not end stops at the bound. Go twins:
# TestStatusKubectlReadsCarryRequestTimeout and the tests in
# internal/cli/cmd_status_bound_test.go.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/status"

  PIDFILE="${PATH_BASE}/.tilt.pid"
}

teardown() { teardown_tmpdir; }

@test "status::tilt is silent for non-lo kinds" {
  for k in kubeone capi kkp "" ; do
    run status::tilt "${k}"
    [ "$status" -eq 0 ]
    [ -z "$output" ]
  done
}

@test "status::tilt reports not running for lo with no pidfile" {
  run status::tilt lo
  [ "$status" -eq 0 ]
  [[ "$output" == *"Tilt"* ]]
  [[ "$output" == *"(not running)"* ]]
}

@test "status::tilt reports not running for lo with a stale (dead) pid" {
  # A pid that is virtually certain not to exist.
  echo "2147480000" > "${PIDFILE}"
  run status::tilt lo
  [[ "$output" == *"(not running)"* ]]
}

@test "status::tilt reports running for lo with a live pid" {
  # $$ is this test process — guaranteed alive.
  echo "$$" > "${PIDFILE}"
  run status::tilt lo
  [[ "$output" == *"running (pid $$)"* ]]
  [[ "$output" != *"(not running)"* ]]
}

@test "status::tilt handles an empty pidfile gracefully" {
  : > "${PIDFILE}"
  run status::tilt lo
  [ "$status" -eq 0 ]
  [[ "$output" == *"(not running)"* ]]
}

# status_all_stand_ins <driver word> [fail|reads-fail]: stand-ins for the
# functions that status::all calls outside libs/status, a kubectl that
# writes its arguments to KUBECTL_LOG, and a timeout that writes its
# options there and then runs the rest of its arguments. All are functions
# of this test: nothing goes on PATH, and no real kubectl or timeout runs.
# KUBECONFIG names a file, so status::all runs the /readyz probe. With
# "fail", every kubectl call fails (an API server that does not answer).
# With "reads-fail", the probe passes and the two reads fail. Else the probe
# passes, the node read prints a table, and the inventory read fails (no
# ClusterInventory), so jq never runs.
status_all_stand_ins() {
  STATUS_WORD="${1}"
  KUBECTL_MODE="${2:-}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}/x.dev"
  KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${KUBECTL_LOG}"
  export KUBECONFIG="${BATS_TEST_TMPDIR}/x.yaml"
  echo "apiVersion: v1" > "${KUBECONFIG}"
  provision::dispatch_status() { echo "${STATUS_WORD}"; }
  domain::spec_driver() { echo "kubeone"; }
  targets::discover() { :; }
  jq() { echo "stand-in: jq must not run" >&2; return 1; }
  timeout() {
    echo "timeout ${1} ${2} ${3}" >> "${KUBECTL_LOG}"
    shift 3
    "$@"
  }
  kubectl() {
    echo "$*" >> "${KUBECTL_LOG}"
    [[ "${KUBECTL_MODE}" != fail ]] || return 1
    [[ "${2:-}" != "--raw" ]] || return 0
    [[ -z "${KUBECTL_MODE}" ]] || return 1
    if [[ "${2:-}" == "nodes" ]]; then
      printf 'NAME   STATUS\nx-control-plane   Ready\n'
      return 0
    fi
    return 1
  }
}

# The /readyz probe, as the stand-ins write it: timeout's options, then
# the kubectl arguments.
_status_probe() {
  printf '%s\n' \
    'timeout -k 2 10' 'get --raw /readyz --request-timeout=5s'
}

# The probe and the two reads, in order, as the stand-ins write them.
_status_reads() {
  _status_probe
  printf '%s\n' \
    'timeout -k 2 10' 'get nodes -o wide --request-timeout=5s' \
    'timeout -k 2 10' 'get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s'
}

# The report of status::all for x.dev: the cluster word, then the Nodes
# section lines.
_status_report() {
  local word="${1}"
  shift
  printf '%s\n' '=== Domain: x.dev ===' '' '--- Cluster ---' "${word}" '' \
    '--- Nodes ---' "$@" '' \
    '--- Targets ---' '  No targets directory' "  artifacts.yaml: not built (run 'lo build')"
}

@test "status::all reads nodes and inventory with --request-timeout=5s on a running cluster" {
  status_all_stand_ins Running
  run status::all x.dev
  assert_success
  assert_output "$(_status_report Running 'NAME   STATUS' 'x-control-plane   Ready')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_status_reads)"
}

# The driver status reaches status::all only as text on the report, so a
# gone cluster still gets the /readyz probe, with the request timeout. The
# probe fails, so the two reads do not run.
@test "status::all passes --request-timeout=5s to the /readyz probe when the driver says NotFound" {
  status_all_stand_ins NotFound fail
  run status::all x.dev
  assert_success
  assert_output "$(_status_report NotFound '  (not reachable)')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_status_probe)"
}

@test "status::all skips both reads and prints (not reachable) when the /readyz probe fails" {
  status_all_stand_ins Running fail
  run status::all x.dev
  assert_success
  assert_output "$(_status_report Running '  (not reachable)')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_status_probe)"
}

@test "status::all prints (not reachable) when the reads fail after the /readyz probe" {
  status_all_stand_ins Running reads-fail
  run status::all x.dev
  assert_success
  assert_output "$(_status_report Running '  (not reachable)')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_status_reads)"
}

# Without timeout on PATH, each call has only the request timeout, as
# before. PATH is an empty directory for the run: the stand-in kubectl is a
# function, and nothing else of status::all runs from PATH.
@test "status::all runs the kubectl calls without a bound when timeout is not on PATH" {
  status_all_stand_ins Running
  unset -f timeout
  mkdir -p "${BATS_TEST_TMPDIR}/empty"
  PATH="${BATS_TEST_TMPDIR}/empty" run status::all x.dev
  assert_success
  assert_output "$(_status_report Running 'NAME   STATUS' 'x-control-plane   Ready')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(printf '%s\n' 'get --raw /readyz --request-timeout=5s' \
    'get nodes -o wide --request-timeout=5s' \
    'get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s')"
}

# status_bound_stub <readyz|hang-probe>: a real kubectl program in a
# directory of this test, put first on PATH, with the real timeout(1)
# (skip when it is not there) and a bound of 1 s. The stub writes its
# arguments to KUBECTL_LOG. With "readyz", the /readyz probe passes and
# every other call sleeps for 10 s (a read that does not end). With
# "hang-probe", every call sleeps. The function stand-ins of kubectl and
# timeout are removed, and the test checks that kubectl is the stub, so no
# other kubectl can run.
status_bound_stub() {
  command -v timeout &>/dev/null || skip "timeout(1) not available"
  local mode="${1}" bin="${BATS_TEST_TMPDIR}/bin"
  status_all_stand_ins Running
  unset -f kubectl timeout
  mkdir -p "${bin}"
  {
    printf '#!/bin/sh\nPATH=/usr/bin:/bin\necho "$*" >> "%s"\n' "${KUBECTL_LOG}"
    [[ "${mode}" != readyz ]] || printf 'case "$*" in "get --raw /readyz "*) exit 0 ;; esac\n'
    printf 'exec sleep 10\n'
  } > "${bin}/kubectl"
  chmod +x "${bin}/kubectl"
  export PATH="${bin}:${PATH}"
  [[ "$(type -P kubectl)" == "${bin}/kubectl" ]] || { echo "kubectl is not the stub: $(type -P kubectl)" >&2; return 1; }
  status::_read_bound() { echo 1; }
}

@test "status::all stops a read that does not end at the bound" {
  status_bound_stub readyz
  local start="${SECONDS}"
  run status::all x.dev
  local took=$(( SECONDS - start ))
  assert_success
  assert_output "$(_status_report Running '  (not reachable)')"
  (( took <= 5 )) || { echo "status::all took ${took} s" >&2; return 1; }
  run cat "${KUBECTL_LOG}"
  assert_output "$(printf '%s\n' 'get --raw /readyz --request-timeout=5s' \
    'get nodes -o wide --request-timeout=5s' \
    'get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s')"
}

@test "status::all stops a /readyz probe that does not end at the bound, and skips both reads" {
  status_bound_stub hang-probe
  local start="${SECONDS}"
  run status::all x.dev
  local took=$(( SECONDS - start ))
  assert_success
  assert_output "$(_status_report Running '  (not reachable)')"
  (( took <= 4 )) || { echo "status::all took ${took} s" >&2; return 1; }
  run cat "${KUBECTL_LOG}"
  assert_output 'get --raw /readyz --request-timeout=5s'
}
