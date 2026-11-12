#!/usr/bin/env bats
# release_version_test.bats — hack/check-release-version.sh, the release
# workflow's guard that the tag equals both VERSION files.
#
# Why this exists
# ---------------
# v0.14.0 was tagged before its version references were bumped. The binary
# reported 0.14.0, but the bash tree inside it carried VERSION 0.13.0, so a
# bash-routed `lo version` and its inventory said 0.13.0. The guard runs
# before goreleaser; these tests show it is red on each mismatch and green
# only when the tag and both files agree.

setup() {
  load "../test_helper"
  SCRIPT="${BATS_TEST_DIRNAME}/../../hack/check-release-version.sh"
  WORKFLOWS="${BATS_TEST_DIRNAME}/../../.github/workflows"
  T="$(mktemp -d)"
  mkdir -p "${T}/hack" "${T}/internal/assets/lok8s" "${T}/.lok8s"
  cp "${SCRIPT}" "${T}/hack/check-release-version.sh"
}

teardown() {
  rm -rf "${T}"
}

# step_block FILE NAME: the lines of the step `- name: NAME` (6-space step
# indent, as in both workflows), up to the next step or the end of the job.
step_block() {
  awk -v n="      - name: $2" '
    $0 == n { on = 1; print; next }
    on && (/^      - / || /^[^ ]/) { exit }
    on { print }' "$1"
}

GUARD_RUN='        run: bash hack/check-release-version.sh "${GITHUB_REF_NAME}"'

versions() { # versions <mirror> <tree>
  printf '%s\n' "$1" > "${T}/internal/assets/lok8s/VERSION"
  printf '%s\n' "$2" > "${T}/.lok8s/VERSION"
}

@test "the tag equals both VERSION files: green" {
  versions 0.14.1 0.14.1
  run bash "${T}/hack/check-release-version.sh" v0.14.1
  [ "$status" -eq 0 ]
  [[ "$output" == *"v0.14.1 equals both VERSION files"* ]]
}

@test "the v0.14.0 case: both files behind the tag: red, both named" {
  versions 0.13.0 0.13.0
  run bash "${T}/hack/check-release-version.sh" v0.14.0
  [ "$status" -eq 1 ]
  [[ "$output" == *"internal/assets/lok8s/VERSION reads '0.13.0', the tag v0.14.0 needs '0.14.0'"* ]]
  [[ "$output" == *".lok8s/VERSION reads '0.13.0', the tag v0.14.0 needs '0.14.0'"* ]]
}

@test "only the synced tree differs: red, that file named" {
  versions 0.14.1 0.14.0
  run bash "${T}/hack/check-release-version.sh" v0.14.1
  [ "$status" -eq 1 ]
  [[ "$output" == *".lok8s/VERSION reads '0.14.0'"* ]]
  [[ "$output" != *"internal/assets/lok8s/VERSION reads"* ]]
}

@test "only the embedded mirror differs: red, that file named" {
  versions 0.14.0 0.14.1
  run bash "${T}/hack/check-release-version.sh" v0.14.1
  [ "$status" -eq 1 ]
  [[ "$output" == *"internal/assets/lok8s/VERSION reads '0.14.0'"* ]]
}

@test "a missing VERSION file is red, never green" {
  versions 0.14.1 0.14.1
  rm "${T}/.lok8s/VERSION"
  run bash "${T}/hack/check-release-version.sh" v0.14.1
  [ "$status" -eq 1 ]
  [[ "$output" == *".lok8s/VERSION reads ''"* ]]
}

@test "a pre-release tag needs the same suffix in the files" {
  versions 0.15.0-rc.1 0.15.0-rc.1
  run bash "${T}/hack/check-release-version.sh" v0.15.0-rc.1
  [ "$status" -eq 0 ]
  versions 0.15.0 0.15.0
  run bash "${T}/hack/check-release-version.sh" v0.15.0-rc.1
  [ "$status" -eq 1 ]
}

@test "files with a pre-release suffix that the tag lacks: red" {
  versions 0.15.0-rc.1 0.15.0-rc.1
  run bash "${T}/hack/check-release-version.sh" v0.15.0
  [ "$status" -eq 1 ]
  [[ "$output" == *"reads '0.15.0-rc.1', the tag v0.15.0 needs '0.15.0'"* ]]
}

@test "a name that is not a release tag is refused" {
  versions 0.14.1 0.14.1
  for t in 0.14.1 v0.14 main v0.14.1/x; do
    run bash "${T}/hack/check-release-version.sh" "$t"
    [ "$status" -eq 1 ]
    [[ "$output" == *"is not a release tag"* ]]
  done
}

@test "the release workflow runs the guard on every tag, before goreleaser, and fails on it" {
  f="${WORKFLOWS}/release.yml"
  block="$(step_block "$f" "The tag equals the VERSION files")"
  [ -n "$block" ]
  grep -qxF "${GUARD_RUN}" <<< "$block"
  # (a bare `! cmd` never fails a bats test: test the match and fail on it)
  if grep -qE 'continue-on-error|^ +if:|\|\| *true' <<< "$block"; then echo "the guard step can pass on a failure: $block"; return 1; fi
  guard="$(grep -nxF "${GUARD_RUN}" "$f" | cut -d: -f1)"
  build="$(grep -n '^ *- uses: goreleaser/goreleaser-action@' "$f" | cut -d: -f1)"
  [ -n "$guard" ]
  [ -n "$build" ]
  [ "$guard" -lt "$build" ]
}

@test "the image workflow runs the guard on a tag, before the image build, and fails on it" {
  f="${WORKFLOWS}/docker.yml"
  block="$(step_block "$f" "The tag equals the VERSION files")"
  [ -n "$block" ]
  grep -qxF "${GUARD_RUN}" <<< "$block"
  grep -qxF "        if: startsWith(github.ref, 'refs/tags/')" <<< "$block"
  if grep -qE 'continue-on-error|\|\| *true' <<< "$block"; then echo "the guard step can pass on a failure: $block"; return 1; fi
  guard="$(grep -nxF "${GUARD_RUN}" "$f" | cut -d: -f1)"
  build="$(grep -n '^      - name: Build and push$' "$f" | cut -d: -f1)"
  [ -n "$guard" ]
  [ -n "$build" ]
  [ "$guard" -lt "$build" ]
}

@test "the repo's own VERSION files agree" {
  [ "$(cat "${BATS_TEST_DIRNAME}/../../internal/assets/lok8s/VERSION")" = "$(cat "${BATS_TEST_DIRNAME}/../../.lok8s/VERSION")" ]
}
