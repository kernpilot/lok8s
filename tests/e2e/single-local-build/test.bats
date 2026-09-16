#!/usr/bin/env bats
# E2E: single-local-build — verify the full Tilt build → push →
# deploy roundtrip for a single locally-built service.
#
# Validates:
#   1. lo env services discovers the single service from services.yaml.
#   2. lo build composes the domain into one artifacts.yaml and lo env
#      kustomization writes an overlay that references it.
#   3. tilt ci builds the app image, pushes to lok8s.local, applies
#      the deployment, and the pod reaches Running state.

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  # init first: it puts the project's own toolchain on PATH, so the tool
  # check below reports on the binaries the run will actually use.
  e2e::init "${BATS_TEST_DIRNAME}" 127.lok8s.dev
  e2e::require_tools docker kind kustomize yq tilt dig
  e2e::require_dns 127.lok8s.dev
  e2e::require_binary
  e2e::banner
  e2e::assert_provenance
  e2e::snapshot_world
  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 127.lok8s.dev
  e2e::final_teardown
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

@test "lo env kustomization composes one domain artifact + overlay" {
  run e2e::lo env kustomization
  assert_success
  # Domain-based build: ONE composed artifact at the domain root, plus the
  # env overlay that wraps it with the image swaps.
  assert [ -f "${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts.yaml" ]
  assert [ -f "${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/kustomization.yaml" ]
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

@test "lo down and lo destroy leave the machine clean" {
  # The teardown is an assertion, not a best-effort sweep: the cluster,
  # its node containers and the registry containers go, the data volumes
  # and the project network stay, and `lo destroy` then takes the volumes
  # too. Anything the scenario owns that survives is removed here and
  # reported as a failure.
  run e2e::down
  assert_success
  e2e::assert_torn_down down

  # Stand it back up before destroying. `lo destroy` on a cluster that is
  # already down removes nothing and would pass however broken its own
  # cluster deletion is — the destroy assertion has to act on a live one.
  e2e::provision
  run kind get clusters
  assert_line "${LOK8S_CLUSTER_NAME}"

  run e2e::destroy
  assert_success
  e2e::assert_torn_down destroy
}
