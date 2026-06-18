#!/usr/bin/env bats
# E2E: cache-mode — verify the build:false -> cache pre-pull -> swap path.
#
# Validates:
#   1. lo env kustomization queues the busybox upstream ref into
#      .cache-queue for pre-pull.
#   2. The rendered kustomization has an images: swap rewriting
#      lok8s.local/busybox to lok8s.cache/library/busybox:1.36.
#   3. tilt ci runs the full pipeline including the --pull phase
#      (populating the cache registry from Docker Hub), applies the
#      deployment, and the pod reaches Running pulling from the
#      cache registry (not upstream).

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  e2e::require_tools docker kind kustomize yq tilt dig
  e2e::require_dns 128.lok8s.dev
  e2e::init "${BATS_TEST_DIRNAME}" 128.lok8s.dev
  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 128.lok8s.dev
  e2e::destroy
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 128.lok8s.dev
}

@test "lo env kustomization queues the busybox upstream ref" {
  run e2e::lo env kustomization
  assert_success

  # The cache queue is a TSV of <svc>\t<remote-ref>\t<branch>\t<tag>.
  # For busybox at library/1.36 against docker hub, the remote ref
  # resolves to https://registry-1.docker.io/library/busybox:1.36.
  local queue="${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/.cache-queue"
  assert [ -f "${queue}" ]
  run cat "${queue}"
  assert_success
  assert_output --partial "busybox"
  assert_output --partial "library/busybox:1.36"
}

@test "lo env kustomization swaps lok8s.local/busybox to lok8s.cache" {
  run e2e::lo env kustomization
  assert_success

  # build:false services get an images: block in the top-level
  # kustomization that rewrites the canonical lok8s.local/<svc>
  # reference to lok8s.cache/<branch>/<svc>:<tag>. kind then pulls
  # from the cache registry on the project subnet, not from Docker
  # Hub directly.
  e2e::assert_kustomization_has '^images:'
  e2e::assert_kustomization_has 'name: lok8s\.local/busybox'
  e2e::assert_kustomization_has 'newName: lok8s\.cache/library/busybox'
  e2e::assert_kustomization_has 'newTag: "1\.36"'
}

@test "tilt ci pre-pulls, deploys, and the pod reaches Running" {
  run e2e::tilt_ci
  assert_success
  # The --pull phase prints a progress line per queue entry. After
  # the pre-pull completes, kustomize applies the rewritten
  # manifest and the pod pulls from lok8s.cache.
  assert_output --partial "SUCCESS. All workloads are healthy."

  # Verify the pod is actually Running in-cluster, AND that it's
  # pulling from the cache registry (not from docker.io).
  run kubectl --kubeconfig "${KUBECONFIG}" get pod -l app=busybox \
    -o jsonpath='{.items[0].status.phase}'
  assert_success
  assert_output --partial "Running"

  run kubectl --kubeconfig "${KUBECONFIG}" get pod -l app=busybox \
    -o jsonpath='{.items[0].spec.containers[0].image}'
  assert_success
  assert_output --partial "lok8s.cache/library/busybox:1.36"
}
