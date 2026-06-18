#!/usr/bin/env bats
# status_test.bats — unit tests for .lok8s/libs/status
#
# Covers status::tilt, which must (a) only run for `lo` (kind) clusters and
# (b) report liveness from lok8s's own PID file rather than the bogus
# `tilt status` subcommand (`lo status` on a Kkp domain ran `tilt status` →
# `unknown command "status" for "tilt"`).

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
