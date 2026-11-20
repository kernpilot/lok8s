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
# --request-timeout=5s, for every driver result (issue #194), and gets an
# empty stdin. A probe with no HTTP answer skips both reads. A probe that
# fails with an HTTP answer (401, 403, 404, 500) does not. Each call runs
# under timeout(1) when it is on PATH, and a call that does not end stops
# at the bound. Go twins: TestStatusKubectlReadsCarryRequestTimeout and the
# tests in internal/cli/cmd_status_bound_test.go.

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
# "fail", every kubectl call fails, with kubectl's line for a connection
# that is refused (an API server that does not answer). With "reads-fail",
# the probe passes and the two reads fail. With "probe-err", the probe
# prints KUBECTL_PROBE_ERR on stderr and fails, and the reads are as in the
# default. Else the probe passes, the node read prints a table, and the
# inventory read fails (no ClusterInventory), so jq never runs. With
# KUBECTL_STDIN set, the kubectl stand-in also logs the first line of its
# stdin ("stdin=empty" for none).
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
    if [[ -n "${KUBECTL_STDIN:-}" ]]; then
      local line
      IFS= read -r line || line=empty
      echo "stdin=${line}" >> "${KUBECTL_LOG}"
    fi
    if [[ "${KUBECTL_MODE}" == fail ]]; then
      echo "The connection to the server 10.0.0.1:6443 was refused - did you specify the right host or port?" >&2
      return 1
    fi
    if [[ "${2:-}" == "--raw" ]]; then
      [[ "${KUBECTL_MODE}" == probe-err ]] || return 0
      printf '%s\n' "${KUBECTL_PROBE_ERR}" >&2
      return 1
    fi
    [[ "${KUBECTL_MODE}" != reads-fail ]] || return 1
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

# A probe with no HTTP answer: kubectl's lines for a connection that is
# refused, a DNS error, a TLS error and the request timeout (Go twin:
# statusProbeAnswers).
@test "status::all skips both reads and prints (not reachable) when the /readyz probe gets no HTTP answer" {
  local err
  for err in \
    'The connection to the server 10.0.0.1:6443 was refused - did you specify the right host or port?' \
    'Unable to connect to the server: dial tcp: lookup api.x.dev on 127.0.0.53:53: no such host' \
    'Unable to connect to the server: tls: failed to verify certificate: x509: certificate signed by unknown authority' \
    'Unable to connect to the server: net/http: request canceled (Client.Timeout exceeded while awaiting headers)'; do
    status_all_stand_ins Running probe-err
    KUBECTL_PROBE_ERR="${err}"
    run status::all x.dev
    assert_success
    assert_output "$(_status_report Running '  (not reachable)')"
    run cat "${KUBECTL_LOG}"
    assert_output "$(_status_probe)"
  done
}

# A probe that fails with an HTTP answer: a 401, a 403 (a cluster that
# removed the system:public-info-viewer grant; also after a warning line),
# a 404 (an older API server, or a proxy) and a 500 (an API server that is
# not ready). The API server answers, so both reads run, and the report
# shows the node table. The stand-in prints kubectl's line for each answer
# (Go twin: statusProbeAnswers).
@test "status::all runs both reads when the /readyz probe gets an HTTP answer (401, 403, 404, 500)" {
  local err
  for err in \
    'error: You must be logged in to the server (Unauthorized)' \
    'Error from server (Forbidden): forbidden: User "system:anonymous" cannot get path "/readyz"' \
    $'W1120 04:12:00.000000   12345 warnings.go:70] a warning\nError from server (Forbidden): forbidden: User "system:anonymous" cannot get path "/readyz"' \
    'Error from server (NotFound): the server could not find the requested resource' \
    'Error from server (InternalError): an error on the server ("[-]etcd failed: reason withheld\nreadyz check failed") has prevented the request from succeeding'; do
    status_all_stand_ins Running probe-err
    KUBECTL_PROBE_ERR="${err}"
    run status::all x.dev
    assert_success
    assert_output "$(_status_report Running 'NAME   STATUS' 'x-control-plane   Ready')"
    run cat "${KUBECTL_LOG}"
    assert_output "$(_status_reads)"
  done
}

# Each kubectl call gets an empty stdin (</dev/null), with timeout on PATH
# and without it: timeout runs kubectl in a process group that is not the
# foreground group of a terminal, so a credential plugin that reads the
# terminal would stop until the bound. The stdin of status::all is a file
# with lines in it, as a terminal with typed input (Go twin:
# TestStatusKubectlCallsGetAnEmptyStdin).
@test "status::all gives each kubectl call an empty stdin, with and without timeout on PATH" {
  local typed="${BATS_TEST_TMPDIR}/typed"
  printf 'typed\ntyped\ntyped\n' > "${typed}"
  status_all_stand_ins Running
  KUBECTL_STDIN=1
  run status::all x.dev < "${typed}"
  assert_success
  assert_output "$(_status_report Running 'NAME   STATUS' 'x-control-plane   Ready')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(printf '%s\n' \
    'timeout -k 2 10' 'get --raw /readyz --request-timeout=5s' 'stdin=empty' \
    'timeout -k 2 10' 'get nodes -o wide --request-timeout=5s' 'stdin=empty' \
    'timeout -k 2 10' 'get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s' 'stdin=empty')"

  status_all_stand_ins Running
  KUBECTL_STDIN=1
  unset -f timeout
  mkdir -p "${BATS_TEST_TMPDIR}/empty"
  PATH="${BATS_TEST_TMPDIR}/empty" run status::all x.dev < "${typed}"
  assert_success
  assert_output "$(_status_report Running 'NAME   STATUS' 'x-control-plane   Ready')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(printf '%s\n' \
    'get --raw /readyz --request-timeout=5s' 'stdin=empty' \
    'get nodes -o wide --request-timeout=5s' 'stdin=empty' \
    'get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s' 'stdin=empty')"
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
