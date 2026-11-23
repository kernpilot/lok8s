#!/usr/bin/env bats
# test_helper_isolation_test.bats — the guards for the call of
# isolate_state_dirs when tests/test_helper.bash loads, and for
# isolate_secrets_home, the HOME of a test that can reach a key.
#
# Only one test calls setup_tmpdir; the others see the state that the call
# at load sets, as in every test file that does not call setup_tmpdir.
# Without the call at load, a test in such a file runs against the directories
# that the calling shell exports, which are the real directories on a developer
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

# isolate_secrets_home moves HOME and XDG_CONFIG_HOME under the temp dir and
# unsets every LOK8S_* variable, the sops identity variables and
# PATH_SECRETS. The values below name no real file.
@test "isolate_secrets_home moves HOME and unsets the key variables" {
  local home xdg left v
  setup_tmpdir
  export LOK8S_SSH_KEY=/somewhere/.ssh/id_ed25519 LOK8S_SPEC_FILE=/somewhere/spec.yaml
  export SOPS_AGE_KEY=x SOPS_AGE_KEY_FILE=/somewhere/keys.txt SOPS_AGE_KEY_CMD=x
  export SOPS_AGE_SSH_PRIVATE_KEY_FILE=/somewhere/id PATH_SECRETS=/somewhere/store
  isolate_secrets_home || fail "isolate_secrets_home failed"
  home=${HOME}
  xdg=${XDG_CONFIG_HOME}
  left="$(compgen -v LOK8S_ || true)"
  for v in SOPS_AGE_KEY SOPS_AGE_KEY_FILE SOPS_AGE_KEY_CMD SOPS_AGE_SSH_PRIVATE_KEY_FILE PATH_SECRETS; do
    [[ -z "${!v+set}" ]] || left+=" ${v}"
  done
  [[ -d "${home}" ]] || fail "HOME ${home} was not made"
  teardown_tmpdir
  [[ "${home}" == "${BATS_TEST_TMPDIR}/home" ]] || fail "HOME is '${home}'"
  [[ "${xdg}" == "${home}/.config" ]] || fail "XDG_CONFIG_HOME is '${xdg}'"
  [[ -z "${left// /}" ]] || fail "still set: ${left}"
}

# The sentinel, with a HOME of the test as the one bats started with: it
# names the variable (not its value) of a key path under that HOME, and a
# HOME that is that HOME. A path under BATS_TEST_TMPDIR is no leak, also when
# the temp dir is under that HOME.
@test "assert_no_real_home names a key path or a HOME under the HOME that bats started with" {
  local real=/nonexistent-home-of-the-guard-test
  setup_tmpdir
  _LOK8S_REAL_HOME=${real}
  isolate_secrets_home || fail "isolate_secrets_home failed"
  run assert_no_real_home
  [[ "${status}" -eq 0 ]] || fail "after isolate_secrets_home: ${output}"

  export LOK8S_SSH_KEY="${real}/.ssh/id_ed25519"
  run assert_no_real_home
  [[ "${status}" -eq 1 && "${output}" == *"LOK8S_SSH_KEY names a path under the HOME"* ]] \
    || fail "LOK8S_SSH_KEY: status ${status}: ${output}"
  [[ "${output}" != *"${real}"* ]] || fail "the guard printed the value: ${output}"
  unset LOK8S_SSH_KEY

  HOME=${real} run assert_no_real_home
  [[ "${status}" -eq 1 && "${output}" == *"HOME names a path under the HOME"* ]] \
    || fail "HOME: status ${status}: ${output}"

  _LOK8S_REAL_HOME=${BATS_TEST_TMPDIR%/*}
  run assert_no_real_home
  [[ "${status}" -eq 0 ]] || fail "a path under BATS_TEST_TMPDIR: ${output}"
  teardown_tmpdir
}

# Every test file that sources the secrets library or carries the tag of
# the real sops calls isolate_secrets_home. A pattern that finds too few
# files fails, so that the guard cannot pass over an empty set.
@test "every test file that reaches the secrets library or the real sops isolates HOME" {
  local f n=0 missing=()
  for f in "${_TESTS_DIR}"/unit/*.bats; do
    [[ "${f##*/}" != test_helper_isolation_test.bats ]] || continue
    grep -qE 'libs/secrets"|test_tags=real-sops' "${f}" || continue
    n=$((n + 1))
    grep -q 'isolate_secrets_home' "${f}" || missing+=("${f##*/}")
  done
  (( n >= 6 )) || fail "only ${n} files matched: the pattern no longer finds the secrets tests"
  (( ${#missing[@]} == 0 )) || fail "no isolate_secrets_home: ${missing[*]}"
}
