#!/usr/bin/env bats
# E2E: shared-registries — slots 132 and 133, sharing 10.125.201.0/24.
#
# Two domains in one project, both with spec.registries.shared.enabled.
# What it proves:
#   1. The first `lo provision` creates the shared docker network and one
#      set of mirror containers on it, and keeps build and cache
#      project-private on the project network.
#   2. The second domain REUSES that set: the same containers, not a
#      second copy, while getting its own build and cache.
#   3. `lo down` on one domain removes that domain's cluster and its
#      project registries and leaves the shared mirrors — and the other
#      cluster — alone.
#   4. `lo destroy` on each domain removes that domain's own registry
#      volumes and LEAVES the shared set: it belongs to no one project.
#   5. `lo registry clean --shared` is what removes the mirrors and the
#      shared network.
#   6. Neither teardown leaves anything of either domain behind.
#
# Naming, and why it matters here more than anywhere else: the mirror
# container prefix `lok8s-registry-` is the framework's and cannot be
# renamed, and standing the shared network up REMOVES every container
# with that prefix attached to it. On a developer machine the default
# network (`lok8s-registries`) carries another project's live mirrors. So
# the specs name their own network and give the mirrors the e2e marker,
# and every filter below is anchored on those full names.

E2E_SHARED_NET="e2e-shr-net"
E2E_MIRRORS=(lok8s-registry-e2e-docker lok8s-registry-e2e-quay)

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  # init first: it puts the project's own toolchain on PATH, so the tool
  # check below reports on the binaries the run will actually use.
  e2e::init "${BATS_TEST_DIRNAME}" 132.lok8s.dev
  e2e::require_tools docker kind
  e2e::require_dns 132.lok8s.dev
  e2e::require_dns 133.lok8s.dev
  e2e::require_binary
  e2e::banner
  e2e::snapshot_world
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  # Both domains, then what they share.
  e2e::init "${BATS_TEST_DIRNAME}" 132.lok8s.dev
  e2e::final_teardown
  e2e::init "${BATS_TEST_DIRNAME}" 133.lok8s.dev
  e2e::final_teardown
  _shared_sweep
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 132.lok8s.dev
}

# _mirror_ids — "<id> <name>" per mirror container, sorted. A recreated
# container gets a new ID, which is what tells "reused" from "rebuilt".
_mirror_ids() {
  docker ps -a --filter "name=^lok8s-registry-e2e-" --format '{{.ID}} {{.Names}}' | sort
}

# _shared_sweep — remove what the scenario shares between its domains and
# fail if anything was still there after the last teardown. The names are
# literals, not derived: this is the one place a computed name could
# reach another project's mirrors.
_shared_sweep() {
  local left m
  left="$(docker ps -a --filter "name=^lok8s-registry-e2e-" --format '{{.Names}}'
          docker network ls --filter "name=^${E2E_SHARED_NET}$" --format '{{.Name}}')"
  for m in "${E2E_MIRRORS[@]}"; do
    docker rm -f "${m}" >/dev/null 2>&1 || true
    docker volume rm -f "${m}" >/dev/null 2>&1 || true
  done
  docker network rm "${E2E_SHARED_NET}" >/dev/null 2>&1 || true
  [[ -z "${left}" ]] || fail "e2e: the shared set survived the teardown: $(tr '\n' ' ' <<<"${left}")"
}

@test "the first domain creates the shared network and one mirror set" {
  e2e::provision

  run docker network inspect "${E2E_SHARED_NET}" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'
  assert_success
  assert_output "10.125.201.0/24"

  local m
  for m in "${E2E_MIRRORS[@]}"; do
    run docker ps --filter "name=^${m}$" --format '{{.Names}}'
    assert_success
    assert_output "${m}"
    # The mirrors live on the SHARED network, not on the project one.
    run docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "${m}"
    assert_output --partial "${E2E_SHARED_NET}"
    refute_output --partial "${E2E_NETWORK}"
  done

  # build and cache stay project-private in shared mode.
  e2e::assert_registry_containers up build cache
  run docker ps -a --filter "name=^${E2E_NETWORK}-registry-e2e-" --format '{{.Names}}'
  assert_output ""
}

@test "the second domain reuses the mirror set instead of creating a second one" {
  local before
  before="$(_mirror_ids)"
  assert_equal "$(wc -l <<<"${before}")" "2"

  e2e::init "${BATS_TEST_DIRNAME}" 133.lok8s.dev
  e2e::provision

  # The same containers, by ID. A second set would show up as four rows,
  # a recreated one as two new IDs.
  assert_equal "$(_mirror_ids)" "${before}"

  # Its own build and cache on its own network.
  e2e::assert_registry_containers up build cache
  run kind get clusters
  assert_line "e2e-shr-a"
  assert_line "e2e-shr-b"
}

@test "lo down on one domain leaves the shared mirrors and the other cluster up" {
  local before
  before="$(_mirror_ids)"

  run e2e::down
  assert_success
  # A shared set is left up on purpose — the mirrors are reused by the
  # other cluster and build and cache stay warm for the next `lo up` —
  # and the command says so rather than doing it quietly.
  assert_output --partial "registries left up"
  e2e::assert_torn_down down
  e2e::assert_registry_containers up build cache

  # The shared set is untouched: same containers, still running.
  assert_equal "$(_mirror_ids)" "${before}"
  local m
  for m in "${E2E_MIRRORS[@]}"; do
    run docker ps --filter "name=^${m}$" --format '{{.Names}}'
    assert_output "${m}"
  done
  run docker network inspect "${E2E_SHARED_NET}" --format '{{.Name}}'
  assert_success

  # And so is the other cluster.
  run kind get clusters
  assert_line "e2e-shr-b"
  refute_line "e2e-shr-a"
}

@test "lo destroy removes each domain's own registry volumes" {
  local d
  for d in 132.lok8s.dev 133.lok8s.dev; do
    e2e::init "${BATS_TEST_DIRNAME}" "${d}"
    # Stand the cluster back up first. `lo destroy` on a cluster that is
    # already down removes nothing, and would pass however broken its own
    # cluster deletion is — the assertion has to act on a live one.
    e2e::provision
    run kind get clusters
    assert_line "${LOK8S_CLUSTER_NAME}"

    run e2e::destroy
    assert_success
    e2e::assert_torn_down destroy
    run docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}'
    assert_output ""
  done

  # The shared set is NOT the project's, and `lo destroy` leaves it — with
  # both clusters gone the mirrors are still up, waiting for the next
  # project. Only `lo registry clean --shared` removes them, which is the
  # next test.
  local m
  for m in "${E2E_MIRRORS[@]}"; do
    run docker ps --filter "name=^${m}$" --format '{{.Names}}'
    assert_output "${m}"
  done
  run docker network inspect "${E2E_SHARED_NET}" --format '{{.Name}}'
  assert_success
}

@test "lo registry clean --shared removes the mirrors and the shared network" {
  e2e::init "${BATS_TEST_DIRNAME}" 133.lok8s.dev

  # `--shared` reaches past this domain's own set: it removes every
  # mirror by its shared name and the shared network itself.
  run e2e::lo registry clean --shared --domain "${DOMAIN_NAME}"
  assert_success

  local m
  for m in "${E2E_MIRRORS[@]}"; do
    run docker ps -a --filter "name=^${m}$" --format '{{.Names}}'
    assert_output ""
    run docker volume ls --filter "name=^${m}$" --format '{{.Name}}'
    assert_output ""
  done
  run docker network inspect "${E2E_SHARED_NET}"
  assert_failure
}
