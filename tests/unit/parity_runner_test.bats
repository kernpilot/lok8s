#!/usr/bin/env bats
# parity_runner_test.bats: hack/parity.sh, the runner behind the ten
# parity harnesses. A harness that exits 0 without one "ok:" line proves
# nothing (a wrong PATH, a skipped preamble), so the runner fails the run.

setup() {
  load "../test_helper"
  setup_tmpdir
  # A stand-in lo binary: parity.sh only checks that it is executable and
  # asks it for --version.
  printf '#!/usr/bin/env bash\necho "lo version 0.0.0 (stub)"\n' >"${BATS_TEST_TMPDIR}/lo"
  chmod +x "${BATS_TEST_TMPDIR}/lo"
  mkdir -p "${BATS_TEST_TMPDIR}/hack"
}

teardown() {
  teardown_tmpdir
}

# stub_harness <name> <body>: one parity-<name>.sh under the stub dir.
stub_harness() {
  printf '#!/usr/bin/env bash\n%s\n' "${2}" >"${BATS_TEST_TMPDIR}/hack/parity-${1}.sh"
}

run_parity() {
  run env PARITY_HARNESS_DIR="${BATS_TEST_TMPDIR}/hack" PARITY_HARNESSES="${1}" \
    bash "${_PROJECT_ROOT}/hack/parity.sh" "${BATS_TEST_TMPDIR}/lo"
}

@test "parity.sh: a harness with ok: lines and rc 0 passes" {
  stub_harness good 'echo "ok: one"; echo "ok: two"'
  run_parity good
  [ "${status}" -eq 0 ]
  [[ "${output}" == *"ok    parity-good "*"2 ok"* ]]
  [[ "${output}" == *"parity: all 1 harnesses passed"* ]]
}

@test "parity.sh: a harness that exits 0 with zero ok: lines fails the run" {
  stub_harness silent 'echo "nothing compared"; exit 0'
  run_parity silent
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"FAIL  parity-silent "* ]]
  [[ "${output}" == *"no ok: line"* ]]
  [[ "${output}" == *"parity: 1 harness(es) failed"* ]]
}

@test "parity.sh: a failing harness still fails with its log" {
  stub_harness bad 'echo "ok: one"; echo "FAIL: two"; exit 1'
  run_parity bad
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"FAIL  parity-bad "*"1 ok, 1 FAIL, rc 1"* ]]
  [[ "${output}" == *"FAIL: two"* ]]
}
