#!/usr/bin/env bats
# test_helper_isolation_test.bats — the guard for the call of
# isolate_state_dirs when tests/test_helper.bash loads.
#
# This file does NOT call setup_tmpdir, so only the call at load sets the
# state directories. Nine test files work this way. Without that call, a
# test in them runs against the directories that the calling shell exports,
# which are the real directories on a developer machine.

setup() {
  load "../test_helper"
}

@test "loading test_helper moves the state directories under BATS_TEST_TMPDIR" {
  local name
  for name in XDG_STATE_HOME XDG_CACHE_HOME LO_REGISTRY_STATE_DIR; do
    [[ "${!name:-}" == "${BATS_TEST_TMPDIR}/"* ]] \
      || fail "${name} is not under BATS_TEST_TMPDIR: '${!name:-}'"
  done
}
