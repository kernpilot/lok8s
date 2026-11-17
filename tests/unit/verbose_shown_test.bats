#!/usr/bin/env bats
# verbose_shown_test.bats — the jq def shown_name (VERBOSE_JQ_SHOWN_NAME,
# utils/verbose.sh) and verbose::shown_names: a name from an API as a
# message shows it. The Go twin, ui.Shown, reads the same cases
# (internal/ui/testdata/shown.json), so the two rules cannot drift apart.

setup() {
  load "../test_helper"
  CASES="${_PROJECT_ROOT}/internal/ui/testdata/shown.json"
}

@test "shown_name: each case of the shared table" {
  run jq 'length' "${CASES}"
  assert_success
  (( output >= 10 ))
  # Each case whose result is not the expected one, with what it gave.
  run jq -r "${VERBOSE_JQ_SHOWN_NAME}"' .[] | (.in | shown_name) as $got
    | select($got != .want) | "\(.case): \($got | @json), want \(.want | @json)"' "${CASES}"
  assert_success
  assert_output ""
}

@test "verbose::shown_names: one line out for each line in, an empty line stays empty" {
  run verbose::shown_names < <(printf '%s\n' 'ö-測試' $'a\x7fb\xc2\x85c' '\' '' $'x\xe2\x80\xaey\xe2\x80\x8bz' $'e\x1b[31mvil')
  assert_success
  assert_output "ö-測試
a?b?c
?

x?y?z
e?[31mvil"
}

@test "verbose::shown_names: a jq that fails gives its rc" {
  jq() { return 5; }
  run verbose::shown_names <<<"cp-0"
  [ "${status}" -eq 5 ]
}
