#!/usr/bin/env bats
# hooks_test.bats — unit tests for .lok8s/libs/hooks (the `lo hooks` command).
# Covers the pure logic: the label-selector → yq translation (incl. injection
# rejection — security critical, it builds a yq expression) and the artifact
# label-filter. The kubectl verbs (recreate/restart/apply) need a live cluster
# and are exercised by the Tilt hooks: integration, not here.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  # Stub argsh builtins so the lib sources without an argsh runtime.
  import() { :; }; export -f import
  :usage() { :; }; export -f :usage
  :args() { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/hooks"
}

@test "_yq_filter: multi-label selector → AND-joined select()" {
  run hooks::_yq_filter 'lok8s.dev/type=seed,lok8s.dev/name=zitadel'
  assert_success
  assert_output 'select((.metadata.labels."lok8s.dev/type" == "seed") and (.metadata.labels."lok8s.dev/name" == "zitadel"))'
}

@test "_yq_filter: single label" {
  run hooks::_yq_filter 'app=kubehz-auth'
  assert_success
  assert_output 'select((.metadata.labels."app" == "kubehz-auth"))'
}

@test "_yq_filter: rejects shell/yq injection in the value" {
  run hooks::_yq_filter 'a=b;rm -rf /'
  assert_failure
  assert_output --partial 'invalid selector clause'
}

@test "_yq_filter: rejects a clause without '='" {
  run hooks::_yq_filter 'noequalshere'
  assert_failure
  assert_output --partial 'must be key=value'
}

@test "_yq_filter: rejects an empty selector" {
  run hooks::_yq_filter ''
  assert_failure
  assert_output --partial 'required'
}

@test "_select: returns only label-matching objects from the rendered artifacts" {
  mkdir -p "${PATH_CLUSTERS}/d.dev/artifacts/zitadel"
  cat > "${PATH_CLUSTERS}/d.dev/artifacts/zitadel/artifacts.yaml" <<'YAML'
apiVersion: batch/v1
kind: Job
metadata:
  name: zitadel-provision
  labels:
    lok8s.dev/role: seed
    lok8s.dev/name: zitadel
---
apiVersion: batch/v1
kind: Job
metadata:
  name: zitadel-setup
  labels:
    lok8s.dev/name: zitadel
YAML
  run hooks::_select d.dev 'lok8s.dev/role=seed'
  assert_success
  assert_output --partial 'zitadel-provision'
  refute_output --partial 'zitadel-setup'
}

@test "_select: empty when nothing matches" {
  mkdir -p "${PATH_CLUSTERS}/d.dev/artifacts/zitadel"
  printf 'kind: Job\nmetadata:\n  name: x\n  labels: {lok8s.dev/name: other}\n' \
    > "${PATH_CLUSTERS}/d.dev/artifacts/zitadel/artifacts.yaml"
  run hooks::_select d.dev 'lok8s.dev/role=seed'
  assert_success
  refute_output --partial 'name: x'
}
