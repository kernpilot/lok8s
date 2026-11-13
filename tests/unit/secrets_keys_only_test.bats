#!/usr/bin/env bats
# secrets_keys_only_test.bats — the Secret plugin's keys-only setting
# (LOK8S_INTERNAL_SECRETS_KEYS_ONLY) is internal to lo. A value the caller
# exported never reaches a child of lo: the entry pins it to 0 at the start
# (Go: cmd/lo/main.go). The render pins of build::artifacts and
# addons::render are tested in build_test.bats and bootstrap_test.bats.

setup() {
  load "../test_helper"
  setup_tmpdir
}

teardown() {
  teardown_tmpdir
}

# The real `lo` against a throwaway project dir, with stub tools first on
# PATH. Each stub logs the keys-only value it inherited. The caller's
# environment carries the setting as 1 (and the store-free switch as 1).
_run_lo_stubbed() {
  local proj="${BATS_TEST_TMPDIR}/proj" stub="${BATS_TEST_TMPDIR}/stub" tool
  mkdir -p "${proj}/clusters" "${stub}"
  for tool in kubectl kind tilt docker; do
    cat > "${stub}/${tool}" <<'EOF'
#!/bin/sh
printf '%s keys-only=%s\n' "$(basename "$0")" "${LOK8S_INTERNAL_SECRETS_KEYS_ONLY-<unset>}" >> "${STUB_LOG}"
echo "v1.2.3"
EOF
    chmod +x "${stub}/${tool}"
  done
  export STUB_LOG="${BATS_TEST_TMPDIR}/stub.log"
  : > "${STUB_LOG}"
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${stub}:${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    LOK8S_INTERNAL_SECRETS_KEYS_ONLY=1 \
    LOK8S_SECRETS_DISABLE=1 \
    "${_PROJECT_ROOT}/.lok8s/lo" "${@}"
}

# `lo version` starts the toolchain with no render overlay of its own: what
# the stubs see is what the entry left in the environment.
@test "the lo entry pins the keys-only setting to 0 for every child" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  _run_lo_stubbed version </dev/null
  assert_success
  run cat "${STUB_LOG}"
  [ "${#lines[@]}" -eq 4 ] || { echo "want 4 stub lines, got: ${output}"; return 1; }
  assert_line "kubectl keys-only=0"
  assert_line "kind keys-only=0"
  assert_line "tilt keys-only=0"
  assert_line "docker keys-only=0"
}
