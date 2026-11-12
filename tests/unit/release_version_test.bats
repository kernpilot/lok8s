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
  WORKFLOW="${BATS_TEST_DIRNAME}/../../.github/workflows/release.yml"
  T="$(mktemp -d)"
  mkdir -p "${T}/hack" "${T}/internal/assets/lok8s" "${T}/.lok8s"
  cp "${SCRIPT}" "${T}/hack/check-release-version.sh"
}

teardown() {
  rm -rf "${T}"
}

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

@test "a name that is not a release tag is refused" {
  versions 0.14.1 0.14.1
  for t in 0.14.1 v0.14 main v0.14.1/x; do
    run bash "${T}/hack/check-release-version.sh" "$t"
    [ "$status" -eq 1 ]
    [[ "$output" == *"is not a release tag"* ]]
  done
}

@test "the release workflow runs the guard on the tag before goreleaser" {
  guard="$(grep -n 'bash hack/check-release-version.sh "${GITHUB_REF_NAME}"' "${WORKFLOW}" | cut -d: -f1)"
  build="$(grep -n 'uses: goreleaser/goreleaser-action@' "${WORKFLOW}" | cut -d: -f1)"
  [ -n "$guard" ]
  [ -n "$build" ]
  [ "$guard" -lt "$build" ]
}

@test "the repo's own VERSION files agree" {
  [ "$(cat "${BATS_TEST_DIRNAME}/../../internal/assets/lok8s/VERSION")" = "$(cat "${BATS_TEST_DIRNAME}/../../.lok8s/VERSION")" ]
}
