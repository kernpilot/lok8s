#!/usr/bin/env bats
# loup_verify_test.bats — unit tests for install/lo-up's post-step verification
#
# lo-up used to trust `b`'s exit code. `b` has shipped more than one silent
# rc-0 no-op (fentas/b#180, #184), each of which left a fresh machine with an
# "installed" env containing nothing — the failure only surfaced much later as
# a missing binary. These tests pin the two guards that check the artefacts.

setup() {
  load "../test_helper"
  setup_tmpdir

  # lo-up is an argsh script; source only the pieces under test. `:args` and
  # the argsh runtime are not needed because main() is never called.
  _say()  { printf '%s\n' "${*}" >&2; }
  _info() { _say "• ${*}"; }
  _ok()   { _say "✓ ${*}"; }
  _warn() { _say "! ${*}"; }
  _die()  { _say "✗ ${*}"; exit 1; }

  # The functions read these from main()'s scope.
  LOUP_REPO="github.com/kernpilot/lok8s"
  profile="local"
  git_ref="main"
  PATH_BIN="${BATS_TEST_TMPDIR}/.bin"
  mkdir -p "${PATH_BIN}"

  # Pull in the two functions without executing the script.
  eval "$(sed -n '/^loup::verify_registered()/,/^}/p;/^loup::verify_materialised()/,/^}/p' \
    "${_PROJECT_ROOT}/install/lo-up")"

  cd "${BATS_TEST_TMPDIR}" || return 1
}

# --- loup::verify_registered ---

@test "verify_registered fails when b.yaml was never created" {
  run loup::verify_registered
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"does not exist"* ]]
}

@test "verify_registered fails when b.yaml has no lok8s env (the rc-0 no-op)" {
  mkdir -p .bin
  printf 'binaries: {}\n' > .bin/b.yaml
  run loup::verify_registered
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"registered no lok8s env"* ]]
  # The message must tell the user how to retry.
  [[ "${output}" == *"b env add github.com/kernpilot/lok8s#local"* ]]
}

@test "verify_registered passes once the env is recorded" {
  mkdir -p .bin
  printf 'envs:\n  git@github.com:kernpilot/lok8s#local: {}\n' > .bin/b.yaml
  run loup::verify_registered
  [ "${status}" -eq 0 ]
}

# --- loup::verify_materialised ---

# A core binary already on PATH satisfies the check by design (users may have
# kubectl/jq system-wide), so the missing-binary cases must run with a PATH
# that holds nothing but coreutils.
bare_path() {
  PATH="/usr/bin:/bin" "${@}"
}

# NOTE: /usr/bin commonly HAS jq and yq, so bare_path does not hide them. Only
# argsh is reliably absent from a system PATH — use it for missing-binary cases.

materialise() {
  mkdir -p .lok8s
  printf '#!/bin/sh\n' > .lok8s/lo
  local bin
  for bin in argsh kubectl jq yq; do
    printf '#!/bin/sh\n' > "${PATH_BIN}/${bin}"
    chmod +x "${PATH_BIN}/${bin}"
  done
}

@test "verify_materialised fails when the framework was not vendored" {
  materialise
  rm -rf .lok8s
  run loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"vendored framework"* ]]
}

@test "verify_materialised fails when the lo entrypoint is missing" {
  materialise
  rm -f .lok8s/lo
  run loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *".lok8s/lo"* ]]
}

@test "verify_materialised fails when a core binary is missing" {
  materialise
  rm -f "${PATH_BIN}/argsh"
  run bare_path loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"argsh"* ]]
}

@test "verify_materialised reports every missing piece at once" {
  materialise
  rm -rf .lok8s
  # argsh, not jq/yq: those are commonly installed system-wide, which the
  # ambient-PATH fallback would (correctly) accept even under bare_path.
  rm -f "${PATH_BIN}/argsh"
  run bare_path loup::verify_materialised
  [ "${status}" -eq 1 ]
  [[ "${output}" == *"vendored framework"* ]]
  [[ "${output}" == *".lok8s/lo"* ]]
  [[ "${output}" == *"argsh"* ]]
}

@test "verify_materialised passes on a complete env" {
  materialise
  run loup::verify_materialised
  [ "${status}" -eq 0 ]
}

@test "verify_materialised accepts a core binary already on PATH" {
  materialise
  rm -f "${PATH_BIN}/kubectl"
  local stub="${BATS_TEST_TMPDIR}/onpath"
  mkdir -p "${stub}"
  printf '#!/bin/sh\n' > "${stub}/kubectl"
  chmod +x "${stub}/kubectl"
  PATH="${stub}:${PATH}" run loup::verify_materialised
  [ "${status}" -eq 0 ]
}
