#!/usr/bin/env bats
# domain_spec_layout_test.bats — domain::spec_layout, the document layout of
# a spec file. One YAML document (with an optional `---` before it and an
# optional `...` after it) is the only layout that the Go readers (first
# document) and the bash readers (yq on every document) read the same way.
# The cases are shared with the Go twin (TestSpecLayoutFixtures,
# TestSpecLayoutMatchesBash): tests/fixtures/spec-layout/<verdict>--<case>.yaml
# with the expected answer, "ok" or "<line> <marker>", in <case>.want.

setup() {
  load "../test_helper"
}

@test "domain::spec_layout: each shared fixture gets its expected answer" {
  local f want got n=0 bad=0
  for f in "${_PROJECT_ROOT}"/tests/fixtures/spec-layout/*.yaml; do
    want=$(<"${f%.yaml}.want")
    if got=$(domain::spec_layout "${f}"); then got=ok; fi
    if [[ "${got}" != "${want}" ]]; then
      printf '%s: got %s, want %s\n' "${f##*/}" "${got}" "${want}" >&2
      bad=$(( bad + 1 ))
    fi
    n=$(( n + 1 ))
  done
  (( n >= 15 )) || { echo "only ${n} fixtures found" >&2; return 1; }
  (( bad == 0 ))
}

@test "domain::spec_layout: a file that cannot be read is ok (the readers report it)" {
  run domain::spec_layout "${BATS_TEST_TMPDIR}/missing.yaml"
  assert_success
  assert_output ""
}
