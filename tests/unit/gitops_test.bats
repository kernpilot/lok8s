#!/usr/bin/env bats
# gitops_test.bats — unit tests for .lok8s/libs/gitops
#
# The gitops library is currently a stub while the post-refactor
# redesign lands (see libs/gitops header comment). These tests just
# verify the stub behaves: deferred commands emit a clear error,
# bootstrap is a no-op.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/gitops"
}

teardown() {
  teardown_tmpdir
}

@test "gitops::flux is deferred and returns error" {
  run gitops::flux "test.lok8s.dev"
  assert_failure
  assert_output --partial "deferred"
}

@test "gitops::argo is deferred and returns error" {
  run gitops::argo "test.lok8s.dev"
  assert_failure
  assert_output --partial "deferred"
}

@test "gitops::bootstrap is a no-op stub" {
  run gitops::bootstrap "test.lok8s.dev" "flux"
  assert_success
}
