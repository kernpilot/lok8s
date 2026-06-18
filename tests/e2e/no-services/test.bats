#!/usr/bin/env bats
# E2E: no-services — verify lok8s() handles the empty-services path.
#
# Validates:
#   1. Provision spins up a kind cluster with cilium bootstrap.
#   2. lo env kustomization runs the services-loop-skip path without
#      crashing.
#   3. The auto-generated artifacts/kustomization.yaml references the
#      single placeholder target.

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  e2e::require_tools docker kind kustomize yq tilt dig
  e2e::require_dns 126.lok8s.dev
  e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
  e2e::destroy
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
}

@test "cluster is running after provision and bootstrap completed" {
  # Verifies provision + framework bootstrap (cilium) ran end-to-end:
  # control plane container is up AND lo status reports Running
  # (which queries the cluster, not just docker).
  run docker ps --filter "name=${LOK8S_CLUSTER_NAME}-control-plane" --format '{{.Status}}'
  assert_success
  assert_output --partial "Up"

  run e2e::lo status --domain 126.lok8s.dev
  assert_success
  assert_output --partial "Running"
}

@test "lo env kustomization handles the no-services path" {
  run e2e::lo env kustomization
  assert_success
  e2e::assert_kustomization_has 'kind: Kustomization'
  # Per-target build: top-level kustomization references each target's
  # artifacts.yaml — no unified .artifacts.yaml.
  e2e::assert_kustomization_has 'placeholder/artifacts\.yaml'
  e2e::assert_kustomization_missing '^images:'
}
