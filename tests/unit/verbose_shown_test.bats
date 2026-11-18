#!/usr/bin/env bats
# verbose_shown_test.bats — the jq defs unshown and shown_name
# (VERBOSE_JQ_UNSHOWN, VERBOSE_JQ_SHOWN_NAME, utils/verbose.sh) and
# verbose::shown_names: a name from an API as a message shows it. The Go
# twins, ui.Unshown and ui.Shown, read the same files
# (internal/ui/testdata/unshown.json, every code point, and
# internal/ui/testdata/shown.json, the cases), so the two rules cannot drift
# apart.

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

@test "unshown: the jq def agrees with the shared table on every code point" {
  # internal/ui/testdata/unshown.json holds the runs of ui.Unshown, the Go
  # twin (generated from it: go test ./internal/ui/ -update). The Go test
  # checks ui.Unshown against it on each code point, this test the jq def.
  local table="${_PROJECT_ROOT}/internal/ui/testdata/unshown.json"
  run jq -r '"\(.max) \(.runs | length)"' "${table}"
  assert_success
  assert_output --regexp '^1114111 [0-9]+$'
  (( ${output#* } >= 10 ))
  # Each code point from 0 to max (U+10FFFF) where the def and the table
  # differ, with what each says.
  run jq -r "${VERBOSE_JQ_UNSHOWN}"' [.runs[] | range(.first; .last + 1)] as $want
    | [range(0; .max + 1) | select(unshown)] as $got
    | (($got - $want)[] | "code point \(.): the jq def says unshown, the table says shown"),
      (($want - $got)[] | "code point \(.): the jq def says shown, the table says unshown")' "${table}"
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
