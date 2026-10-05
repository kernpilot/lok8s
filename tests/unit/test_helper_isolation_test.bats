#!/usr/bin/env bats
# test_helper_isolation_test.bats — the guards for the call of
# isolate_state_dirs when tests/test_helper.bash loads.
#
# This file does NOT call setup_tmpdir, so only the call at load sets the
# state directories, as in every test file that does not call setup_tmpdir.
# Without that call, a test in such a file runs against the directories that
# the calling shell exports, which are the real directories on a developer
# machine.

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

# bats loads a file with `if ! source <file>`, where errexit is off. The
# child shell below loads the helper the same way, without BATS_TEST_TMPDIR.
@test "loading test_helper without BATS_TEST_TMPDIR fails" {
  run env -u BATS_TEST_TMPDIR bash -c 'if ! source "$1"; then exit 3; fi' _ "${_TESTS_DIR}/test_helper.bash"
  [[ "${status}" -eq 3 ]] || fail "the load did not fail (status ${status}): ${output}"
  [[ "${output}" == *"BATS_TEST_TMPDIR is not set"* ]] || fail "no reason in the output: ${output}"
}
