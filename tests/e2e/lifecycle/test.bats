#!/usr/bin/env bats
# E2E: lifecycle — slot 134. The cluster lifecycle, twice around, and the
# confirmation prompts on a real terminal.
#
# What it proves:
#   1. `lo provision` builds the cluster, the project docker network and
#      one registry set.
#   2. A second `lo provision` is idempotent: the same cluster, the same
#      registry containers, no second set. `--force-recreate` too.
#   3. `lo down` removes the cluster and the registry containers and
#      KEEPS the data volumes and the project network, and the machine
#      carries nothing else of the scenario's afterwards.
#   4. A second `lo provision` onto the same project reuses that network
#      and those volumes.
#   5. `lo destroy` removes the volumes as well.
#   6. The confirmation gate (v0.5.0): on a terminal `lo down` lists what
#      it removes and asks. `n` removes nothing, `y` removes it, `--yes`
#      does not ask, and a run whose stdin is a pipe is unchanged — it
#      never asks and proceeds. The answers are TYPED INTO A PTY
#      (e2e::pty_run); piping an answer into the command's own stdin is
#      what turns the prompt off, which is the failure mode being pinned
#      down here.
#
# `lo provision` is the up step, not `lo up`: `lo up` adds Tilt, which
# the e2e-lo-up job already proves end to end and which would leave a
# background process this scenario would then have to reason about. The
# cluster half is what the lifecycle is about.
#
# The prompt is Go-only (internal/cli/confirm.go — the frozen bash tree
# never asked), so the prompt tests skip on the bash and mixed legs and
# say so. Every other assertion runs identically on every leg, and each
# leg must reach the same end state.

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  # init first: it puts the project's own toolchain on PATH, so the tool
  # check below reports on the binaries the run will actually use.
  e2e::init "${BATS_TEST_DIRNAME}" 134.lok8s.dev
  e2e::require_tools docker kind
  e2e::require_dns 134.lok8s.dev
  e2e::require_binary
  e2e::banner
  e2e::snapshot_world
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 134.lok8s.dev
  e2e::final_teardown
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 134.lok8s.dev
}

# _ids <filter> — the container IDs behind an anchored name filter, one
# per line. A recreated container gets a new ID, so this is what tells
# "unchanged" from "rebuilt and happens to be running".
_ids() {
  docker ps -a --filter "name=$1" --format '{{.ID}} {{.Names}}' | sort
}

@test "lo provision brings up the cluster, the network and one registry set" {
  e2e::provision

  run kind get clusters
  assert_success
  assert_line "${LOK8S_CLUSTER_NAME}"

  run docker network inspect "${E2E_NETWORK}" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'
  assert_success
  assert_output "10.125.134.0/24"

  e2e::assert_registry_containers up build cache io-docker io-quay io-k8s io-ghcr
}

@test "lo status reports Running and lo registry status lists the set" {
  run e2e::lo status --domain "${DOMAIN_NAME}"
  assert_success
  assert_line "Running"
  assert_output --partial "${LOK8S_CLUSTER_NAME}-control-plane"

  # On the mixed leg this command is the routed one: the same output,
  # produced by the frozen bash tree, against the same live cluster.
  run e2e::lo registry status --domain "${DOMAIN_NAME}"
  assert_success
  assert_output --partial "${E2E_NETWORK}-registry-build"
  assert_output --partial "${E2E_NETWORK}-registry-cache"
}

@test "a second lo provision changes nothing: same cluster, same containers" {
  local before_clusters before_ids
  before_clusters="$(kind get clusters | sort)"
  before_ids="$(_ids "^${E2E_NETWORK}-registry-")"

  e2e::provision

  assert_equal "$(kind get clusters | sort)" "${before_clusters}"
  assert_equal "$(_ids "^${E2E_NETWORK}-registry-")" "${before_ids}"

  # One set, not two: the count is the assertion a "reused the mirrors"
  # claim needs. Six registries — build, cache and the four io-* mirrors.
  run bash -c "docker ps --filter 'name=^${E2E_NETWORK}-registry-' --format '{{.Names}}' | wc -l"
  assert_output "6"
}

@test "lo provision --force-recreate changes nothing either" {
  local before_clusters before_ids
  before_clusters="$(kind get clusters | sort)"
  before_ids="$(_ids "^${E2E_NETWORK}-registry-")"

  # --force-recreate is about objects an apply cannot patch (an immutable
  # field, a stuck finalizer). With `bootstrap: []` there is nothing to
  # apply, so the cluster and the registry set must come through it
  # untouched — the flag is not a "rebuild the cluster" switch.
  run e2e::lo provision --domain "${DOMAIN_NAME}" --force-recreate
  assert_success

  assert_equal "$(kind get clusters | sort)" "${before_clusters}"
  assert_equal "$(_ids "^${E2E_NETWORK}-registry-")" "${before_ids}"
}

@test "on a terminal lo down lists the cluster and the registries, and n removes nothing" {
  e2e::require_go_impl "the confirmation prompt"

  e2e::pty_run n down --domain "${DOMAIN_NAME}"
  assert_equal "${E2E_PTY_RC}" "1"

  # The plan names the concrete objects, then asks.
  assert [ -n "$(grep -F 'lo down removes:' <<<"${E2E_PTY_OUTPUT}")" ]
  assert [ -n "$(grep -F "kind cluster" <<<"${E2E_PTY_OUTPUT}")" ]
  assert [ -n "$(grep -F "${LOK8S_CLUSTER_NAME}" <<<"${E2E_PTY_OUTPUT}")" ]
  assert [ -n "$(grep -F "${E2E_NETWORK}-registry-build" <<<"${E2E_PTY_OUTPUT}")" ]
  assert [ -n "$(grep -F 'Continue? [y/N]' <<<"${E2E_PTY_OUTPUT}")" ]
  assert [ -n "$(grep -F 'aborted: nothing removed' <<<"${E2E_PTY_OUTPUT}")" ]

  # "nothing removed" is a claim about the machine, not about the text.
  run kind get clusters
  assert_line "${LOK8S_CLUSTER_NAME}"
  e2e::assert_registry_containers up build cache
}

@test "lo down removes the cluster and the registry containers, volumes kept" {
  local volumes_before
  volumes_before="$(docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}' | sort)"
  assert [ -n "${volumes_before}" ]

  if [[ "${E2E_LO_IMPL}" == "go" ]]; then
    # Same command, driven through the terminal so the `y` answer is
    # the thing that removes the cluster. Off a terminal (the other
    # legs, and CI) there is no prompt and the command just runs; the
    # end state asserted below is the same either way.
    e2e::pty_run y down --domain "${DOMAIN_NAME}"
    assert_equal "${E2E_PTY_RC}" "0"
    assert [ -n "$(grep -F 'Continue? [y/N]' <<<"${E2E_PTY_OUTPUT}")" ]
  else
    run e2e::down
    assert_success
  fi

  e2e::assert_torn_down down

  # The data volumes are the documented keep: `lo up` reuses them, so a
  # cache survives a down/up cycle. `lo destroy` is what drops them.
  assert_equal "$(docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}' | sort)" "${volumes_before}"

  run e2e::lo status --domain "${DOMAIN_NAME}"
  assert_success
  assert_line "NotFound"
}

@test "lo provision after down reuses the project network and the volumes" {
  local net_id volumes_before
  net_id="$(docker network inspect "${E2E_NETWORK}" --format '{{.Id}}')"
  volumes_before="$(docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}' | sort)"

  e2e::provision

  run kind get clusters
  assert_line "${LOK8S_CLUSTER_NAME}"
  assert_equal "$(docker network inspect "${E2E_NETWORK}" --format '{{.Id}}')" "${net_id}"
  assert_equal "$(docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}' | sort)" "${volumes_before}"
  e2e::assert_registry_containers up build cache
}

@test "--yes removes the cluster on a terminal without asking" {
  if [[ "${E2E_LO_IMPL}" == "go" ]]; then
    e2e::pty_run "" down --domain "${DOMAIN_NAME}" --yes
    assert_equal "${E2E_PTY_RC}" "0"
    # --yes is the only skip. Nothing is printed and nothing is read.
    assert [ -z "$(grep -F 'Continue? [y/N]' <<<"${E2E_PTY_OUTPUT}")" ]
    assert [ -z "$(grep -F 'lo down removes:' <<<"${E2E_PTY_OUTPUT}")" ]
  else
    run e2e::down
    assert_success
  fi
  e2e::assert_torn_down down
}

@test "a piped run is unchanged: lo destroy asks nothing and drops the volumes" {
  # Stand the cluster back up first. `lo destroy` on a cluster that is
  # already down removes nothing, and would pass however broken its own
  # cluster deletion is — the destroy assertion has to act on a live one.
  e2e::provision
  run kind get clusters
  assert_line "${LOK8S_CLUSTER_NAME}"
  assert [ -n "$(docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}')" ]

  # stdin is a pipe, so it is not a terminal and the gate does not fire:
  # the command runs to completion with no question. This is what every
  # script, CI job and parity harness sees, and it must stay that way.
  run bash -c "printf '' | '${E2E_LO_BIN}' destroy --domain '${DOMAIN_NAME}' --force 2>&1"
  assert_success
  refute_output --partial "Continue? [y/N]"
  refute_output --partial "lo destroy removes:"

  e2e::assert_torn_down destroy
  assert_equal "$(docker volume ls --filter "name=^${E2E_NETWORK}-registry-" --format '{{.Name}}')" ""
}
