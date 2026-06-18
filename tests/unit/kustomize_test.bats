#!/usr/bin/env bats
# kustomize_test.bats — unit tests for .lok8s/libs/kustomize
#
# Covers kustomize::_sources, which resolves the kustomize plugin SOURCE dirs:
# the lok8s framework plugins (shipped with the framework) plus the project's
# own kustomize/ when present. Regression guard for the friction where
# `lo kustomize build` only looked in the project dir, so a fresh project
# (without its own Go source) could not build the framework secrets plugin.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}/project"
  # PATH_LOK8S is <repo>/.lok8s; the framework kustomize source is its sibling.
  export PATH_LOK8S="${BATS_TEST_TMPDIR}/lok8s/.lok8s"
  mkdir -p "${PATH_BASE}" "${PATH_LOK8S}"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kustomize"
}

@test "kustomize::_sources returns the lok8s framework kustomize dir" {
  mkdir -p "${BATS_TEST_TMPDIR}/lok8s/kustomize"
  run kustomize::_sources
  [ "$status" -eq 0 ]
  [[ "$output" == *"/lok8s/kustomize"* ]]
}

@test "kustomize::_sources omits the framework dir when it is absent" {
  run kustomize::_sources
  [[ "$output" != *"/lok8s/kustomize"* ]]
}

@test "kustomize::_sources also returns the project kustomize/ when present" {
  mkdir -p "${BATS_TEST_TMPDIR}/lok8s/kustomize" "${PATH_BASE}/kustomize"
  run kustomize::_sources
  [[ "$output" == *"/lok8s/kustomize"* ]]
  [[ "$output" == *"${PATH_BASE}/kustomize"* ]]
}

@test "kustomize::_sources omits the project dir when it is absent" {
  mkdir -p "${BATS_TEST_TMPDIR}/lok8s/kustomize"
  run kustomize::_sources
  [[ "$output" != *"${PATH_BASE}/kustomize"* ]]
}
