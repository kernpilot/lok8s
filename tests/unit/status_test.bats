#!/usr/bin/env bats
# status_test.bats — unit tests for .lok8s/libs/status
#
# Covers status::tilt, which must (a) only run for `lo` (kind) clusters and
# (b) report liveness from lok8s's own PID file rather than the bogus
# `tilt status` subcommand (`lo status` on a Kkp domain ran `tilt status` →
# `unknown command "status" for "tilt"`).
#
# Also covers the two kubectl reads of status::all (status::nodes,
# status::inventory): each ends with --request-timeout=5s, for every driver
# result (issue #194). Go twin: TestStatusKubectlReadsCarryRequestTimeout.

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

# status_all_stand_ins <driver word> [fail]: stand-ins for the functions
# that status::all calls outside libs/status, and a kubectl that writes its
# arguments to KUBECTL_LOG. All are functions of this test: nothing goes on
# PATH, and no real kubectl runs. KUBECONFIG names a file, so status::all
# runs both kubectl reads. With "fail", every kubectl call fails (an API
# server that does not answer). Else the node read prints a table and the
# inventory read fails (no ClusterInventory), so jq never runs.
status_all_stand_ins() {
  STATUS_WORD="${1}"
  KUBECTL_FAIL="${2:-}"
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
  kubectl() {
    echo "$*" >> "${KUBECTL_LOG}"
    [[ -z "${KUBECTL_FAIL}" ]] || return 1
    if [[ "${2:-}" == "nodes" ]]; then
      printf 'NAME   STATUS\nx-control-plane   Ready\n'
      return 0
    fi
    return 1
  }
}

# The two reads, in order, as the stand-in kubectl writes them.
_status_reads() {
  printf '%s\n' \
    'get nodes -o wide --request-timeout=5s' \
    'get clusterinventories.lok8s.dev cluster -o json --request-timeout=5s'
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
# gone cluster still gets both reads. The request timeout bounds them.
@test "status::all bounds both kubectl reads with --request-timeout=5s when the driver says NotFound" {
  status_all_stand_ins NotFound fail
  run status::all x.dev
  assert_success
  assert_output "$(_status_report NotFound '  (not reachable)')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_status_reads)"
}

@test "status::all prints (not reachable) when kubectl fails on a running cluster" {
  status_all_stand_ins Running fail
  run status::all x.dev
  assert_success
  assert_output "$(_status_report Running '  (not reachable)')"
  run cat "${KUBECTL_LOG}"
  assert_output "$(_status_reads)"
}
