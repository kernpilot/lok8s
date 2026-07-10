#!/usr/bin/env bats
# cli_dispatch_test.bats — pins the `:usage` → `"${usage[@]}"` dispatch contract.
#
# Regression guard for the inert-dispatch class: `lo image` and `lo gitops`
# once called :usage without the follow-up "${usage[@]}" — every subcommand
# parsed, did nothing, and exited 0 (argsh-conformance review, fixed 2026-07-10).
# The static check makes the whole class impossible to reintroduce; the live
# tests pin the two commands' real behavior through the actual CLI.

setup() {
  load "../test_helper"
  setup_tmpdir
}

teardown() {
  teardown_tmpdir
}

# Shared runner: the real `lo` against a throwaway project dir (same pattern
# as init_test.bats) — no cluster, no docker, help/stub paths only.
_run_lo() {
  local proj="${BATS_TEST_TMPDIR}/proj"
  mkdir -p "${proj}/clusters"
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    PATH="${_PROJECT_ROOT}/.bin:${_PROJECT_ROOT}/.lok8s:${PATH}" \
    "${_PROJECT_ROOT}/.lok8s/lo" "${@}"
}

# Static invariant: in every argsh script, each `:usage` call is paired with a
# `"${usage[@]}"` dispatch in the same file. A hub that parses but never
# dispatches is silently inert — exactly the lo image / lo gitops bug.
@test "every :usage call is paired with a usage-array dispatch" {
  local f calls dispatches failed=0
  while IFS= read -r f; do
    calls=$(grep -cE '^\s*:usage ' "${f}") || true
    (( calls > 0 )) || continue
    dispatches=$(grep -cE '^\s*"\$\{usage\[@\]\}"' "${f}") || true
    if (( dispatches < calls )); then
      echo "inert dispatch: ${f#"${_PROJECT_ROOT}/"} has ${calls} :usage call(s) but only ${dispatches} \"\${usage[@]}\" dispatch(es)" >&2
      failed=1
    fi
  done < <(
    grep -rl '^#!/usr/bin/env argsh' "${_PROJECT_ROOT}/.lok8s"
    echo "${_PROJECT_ROOT}/.lok8s/lo"
  )
  [ "${failed}" -eq 0 ]
}

@test "lo gitops flux dispatches and reports the deferred stub (exit 1)" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  _run_lo gitops flux
  [ "$status" -eq 1 ]
  [[ "$output" == *"deferred"* ]]
}

@test "lo image --help dispatches and lists its subcommands (exit 0)" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  _run_lo image --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"cache"* ]]
  [[ "$output" == *"clean"* ]]
}

@test "sourcing lo does not execute main (entrypoint guard)" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] || skip "argsh binary not available"
  local proj="${BATS_TEST_TMPDIR}/proj"
  mkdir -p "${proj}/clusters"
  run env -C "${proj}" \
    PATH_BASE="${proj}" \
    PATH_BIN="${_PROJECT_ROOT}/.bin" \
    PATH_LOK8S="${_PROJECT_ROOT}/.lok8s" \
    PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s" \
    PATH_CLUSTERS="${proj}/clusters" \
    bash -c "source '${_PROJECT_ROOT}/.lok8s/lo'"
  [ "$status" -eq 0 ]
  [[ "$output" != *"Usage:"* ]]
}
