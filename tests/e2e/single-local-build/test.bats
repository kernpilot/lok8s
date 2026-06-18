#!/usr/bin/env bats
# E2E: single-local-build — verify the full Tilt build → push →
# deploy roundtrip for a single locally-built service.
#
# Validates:
#   1. lo env services discovers the single service from services.yaml.
#   2. lo env kustomization writes per-target artifacts and a
#      top-level kustomization that references them.
#   3. tilt ci builds the app image, pushes to lok8s.local, applies
#      the deployment, and the pod reaches Running state.

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  e2e::require_tools docker kind kustomize yq tilt dig
  e2e::require_dns 127.lok8s.dev
  e2e::init "${BATS_TEST_DIRNAME}" 127.lok8s.dev
  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 127.lok8s.dev
  e2e::destroy
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 127.lok8s.dev
}

@test "lo env services returns the single service" {
  run e2e::lo env services
  assert_success
  assert_output --partial "app:"
}

@test "lo env kustomization writes per-target artifacts" {
  run e2e::lo env kustomization
  assert_success
  assert [ -f "${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/kustomization.yaml" ]
  assert [ -f "${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/app/artifacts.yaml" ]
  # build:true → no image swap; the manifest's lok8s.local/app stays
  # untouched (no `newName:` rewrite in the kustomization).
  e2e::assert_kustomization_missing 'newName:'
  # No build:false services → cache queue should be empty.
  e2e::assert_queue_empty
}

@test "tilt ci builds the app and the pod reaches Running" {
  run e2e::tilt_ci
  assert_success
  # The framework Tiltfile emits its own progress markers; the key
  # success signal is "SUCCESS. All workloads are healthy." at the
  # end, which tilt ci only prints when every k8s_resource reaches
  # Ready state.
  assert_output --partial "Building image"
  assert_output --partial "SUCCESS. All workloads are healthy."

  # Verify the pod is actually Running in-cluster.
  run kubectl --kubeconfig "${KUBECONFIG}" get pod -l app=app \
    -o jsonpath='{.items[0].status.phase}'
  assert_success
  assert_output --partial "Running"
}
