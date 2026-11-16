#!/usr/bin/env bats
# release_header_test.bats — hack/release-header.sh, the release header that
# the release workflow passes to goreleaser's --release-header.
#
# Why this exists
# ---------------
# The hand-written notes (docs/releases/<tag>.md) are also docs pages, so
# they start with a YAML front matter block. The workflow passed the file to
# --release-header as it was, and the v0.13.0 and v0.14.1 release bodies
# started with `---`, `title: ...`, `---`. These tests feed the script a file
# with front matter and a file without it and compare the header text, and
# they check that the workflow uses the script's output.

setup() {
  load "../test_helper"
  SCRIPT="${BATS_TEST_DIRNAME}/../../hack/release-header.sh"
  RELEASES="${BATS_TEST_DIRNAME}/../../docs/releases"
  WORKFLOW="${BATS_TEST_DIRNAME}/../../.github/workflows/release.yml"
  T="$(mktemp -d)"
}

teardown() {
  rm -rf "${T}"
}

# step_block FILE NAME: the lines of the step `- name: NAME` (6-space step
# indent), up to the next step or the end of the job.
step_block() {
  awk -v n="      - name: $2" '
    $0 == n { on = 1; print; next }
    on && (/^      - / || /^[^ ]/) { exit }
    on { print }' "$1"
}

@test "a file with front matter: the header is the text after the block" {
  printf '%s\n' '---' 'title: lok8s v9.9.9' '---' '' \
    'This release fixes one thing.' '' '## Fixes' '' '- a fix' > "${T}/notes.md"
  printf '%s\n' 'This release fixes one thing.' '' '## Fixes' '' '- a fix' > "${T}/want.md"
  run bash "${SCRIPT}" "${T}/notes.md"
  assert_success
  bash "${SCRIPT}" "${T}/notes.md" > "${T}/got.md"
  cmp "${T}/want.md" "${T}/got.md" || { diff "${T}/want.md" "${T}/got.md"; return 1; }
}

@test "a file without front matter: the header is the file, byte for byte" {
  printf '%s\n' 'This release fixes one thing.' '' '---' '' 'Below a rule.' > "${T}/notes.md"
  run bash "${SCRIPT}" "${T}/notes.md"
  assert_success
  bash "${SCRIPT}" "${T}/notes.md" > "${T}/got.md"
  cmp "${T}/notes.md" "${T}/got.md" || { diff "${T}/notes.md" "${T}/got.md"; return 1; }
}

@test "a horizontal rule in the body after the front matter stays" {
  printf '%s\n' '---' 'title: x' '---' 'Intro.' '' '---' '' 'More.' > "${T}/notes.md"
  run bash "${SCRIPT}" "${T}/notes.md"
  assert_success
  assert_output "$(printf '%s\n' 'Intro.' '' '---' '' 'More.')"
}

@test "a first line --- without a closing --- is not front matter: unchanged" {
  printf '%s\n' '---' 'title: x' 'No closing line.' > "${T}/notes.md"
  bash "${SCRIPT}" "${T}/notes.md" > "${T}/got.md"
  cmp "${T}/notes.md" "${T}/got.md" || { diff "${T}/notes.md" "${T}/got.md"; return 1; }
}

@test "CRLF delimiters close the block too" {
  printf '%s\r\n' '---' 'title: x' '---' '' 'Body.' > "${T}/notes.md"
  run bash "${SCRIPT}" "${T}/notes.md"
  assert_success
  assert_output "$(printf 'Body.\r')"
}

@test "a missing notes file fails and names the file" {
  run bash "${SCRIPT}" "${T}/nope.md"
  assert_failure
  assert_output --partial "no such file: ${T}/nope.md"
}

@test "the v0.13.0 and v0.14.1 notes: the header starts with their first body line" {
  run bash "${SCRIPT}" "${RELEASES}/v0.13.0.md"
  assert_success
  assert_line --index 0 "This release fixes a Go standard library vulnerability in \`lo mcp serve\`."
  refute_line --partial "title: lok8s v0.13.0"
  run bash "${SCRIPT}" "${RELEASES}/v0.14.1.md"
  assert_success
  assert_line --index 0 "This release fixes the version that the bash tree and the operator image of"
  refute_line --partial "title: lok8s v0.14.1"
}

@test "no committed notes file gives a header that starts with front matter" {
  local f first
  for f in "${RELEASES}"/v*.md; do
    first="$(bash "${SCRIPT}" "${f}" | head -1)"
    [[ -n "${first}" ]] || { echo "empty header: ${f}"; return 1; }
    if [[ "${first}" == "---"* || "${first}" == "title:"* ]]; then
      echo "${f}: the header starts with '${first}'"
      return 1
    fi
  done
}

@test "the release workflow passes the script's output, not the notes file, to --release-header" {
  block="$(step_block "${WORKFLOW}" "Release notes for this tag")"
  [ -n "${block}" ]
  grep -qxF '            header="${RUNNER_TEMP}/release-header.md"' <<< "${block}"
  grep -qxF '            bash hack/release-header.sh "${notes}" > "${header}"' <<< "${block}"
  grep -qxF '            echo "args=--release-header ${header}" >> "${GITHUB_OUTPUT}"' <<< "${block}"
  # (a bare `! cmd` never fails a bats test: test the match and fail on it)
  if grep -qF -- '--release-header ${notes}' <<< "${block}"; then
    echo "the raw notes file still goes to --release-header: ${block}"
    return 1
  fi
}
