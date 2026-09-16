#!/usr/bin/env bats
# E2E: no-services — verify lok8s() handles the empty-services path.
#
# Validates:
#   1. Provision spins up a kind cluster with cilium bootstrap.
#   2. lo env kustomization runs the services-loop-skip path without
#      crashing.
#   3. The auto-generated artifacts/kustomization.yaml references the
#      single composed domain artifact (../artifacts.yaml).

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  # init first: it puts the project's own toolchain on PATH, so the tool
  # check below reports on the binaries the run will actually use.
  e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
  e2e::require_tools docker kind kustomize yq tilt dig
  e2e::require_dns 126.lok8s.dev
  e2e::require_binary
  e2e::banner
  e2e::snapshot_world
  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
  e2e::final_teardown
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
  # Domain-based build: the overlay kustomization references the single
  # composed domain artifact one level up (../artifacts.yaml).
  e2e::assert_kustomization_has '\.\./artifacts\.yaml'
  e2e::assert_kustomization_missing '^images:'
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
