#!/usr/bin/env bats
# test_helper_isolation_test.bats — the guards for the call of
# isolate_state_dirs when tests/test_helper.bash loads.
#
# Only one test calls setup_tmpdir; the others see the state that the call
# at load sets, as in every test file that does not call setup_tmpdir.
# Without the call at load, a test in such a file runs against the directories that
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

@test "loading test_helper keeps kubectl away from real clusters" {
  # A kubectl that a test did not stub must not read the developer's
  # ~/.kube/config, a kubeconfig that the calling shell exports, or the
  # in-cluster config of a CI runner pod.
  [[ "${KUBECONFIG:-}" == /dev/null ]] || fail "KUBECONFIG is '${KUBECONFIG:-}'"
  [[ -z "${KUBERNETES_SERVICE_HOST+set}" ]] || fail "KUBERNETES_SERVICE_HOST is set: '${KUBERNETES_SERVICE_HOST}'"
  [[ -z "${KUBERNETES_SERVICE_PORT+set}" ]] || fail "KUBERNETES_SERVICE_PORT is set: '${KUBERNETES_SERVICE_PORT}'"
}

@test "setup_tmpdir keeps kubectl away from real clusters" {
  local kubeconfig host port
  KUBECONFIG=/somewhere/real.yaml
  KUBERNETES_SERVICE_HOST=10.96.0.1
  KUBERNETES_SERVICE_PORT=443
  setup_tmpdir
  kubeconfig=${KUBECONFIG}
  host=${KUBERNETES_SERVICE_HOST-<unset>}
  port=${KUBERNETES_SERVICE_PORT-<unset>}
  teardown_tmpdir
  [[ "${kubeconfig}" == /dev/null ]] || fail "KUBECONFIG is '${kubeconfig}'"
  [[ "${host}" == "<unset>" ]] || fail "KUBERNETES_SERVICE_HOST is '${host}'"
  [[ "${port}" == "<unset>" ]] || fail "KUBERNETES_SERVICE_PORT is '${port}'"
}

# bats loads a file with `if ! source <file>`, where errexit is off. The
# child shell below loads the helper the same way, without BATS_TEST_TMPDIR.
@test "loading test_helper without BATS_TEST_TMPDIR fails" {
  run env -u BATS_TEST_TMPDIR bash -c 'if ! source "$1"; then exit 3; fi' _ "${_TESTS_DIR}/test_helper.bash"
  [[ "${status}" -eq 3 ]] || fail "the load did not fail (status ${status}): ${output}"
  [[ "${output}" == *"BATS_TEST_TMPDIR is not set"* ]] || fail "no reason in the output: ${output}"
}
