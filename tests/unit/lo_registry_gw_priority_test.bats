#!/usr/bin/env bats
# lo_registry_gw_priority_test.bats — the default route of a kind node on a
# shared-registry cluster.
#
# Why this exists
# ---------------
# kind creates the node on the project network; lo::connect_nodes_to_registry_network
# then attaches the shared registry network. A container on two networks gets
# its default route from the network with the highest gateway priority, and
# between equal priorities Docker takes the network whose name sorts first.
# Without a priority, a project network named after "lok8s-registries" ("ops")
# lost the default route to the registry network (live on lanisce/ops,
# 2026-10-12: `default via 10.125.200.128 dev eth1` on both nodes; Cilium's init
# container hung on 10.96.0.1:443). Go twin: internal/driver/lo/gwpriority_test.go.
#
# The fake docker below simulates what the fix depends on: the version, the
# endpoints of each node in attach order (eth0, eth1, …) with their priorities
# and gateways, Docker's choice of the default route, and the in-node `ip`
# reads and the `ip route replace` that overrides that choice. Like moby,
# `docker network inspect` reports the gateway of a network created without
# --gateway only from Docker 29 on; the endpoint in `docker inspect <node>`
# always carries it. State, per node:
#   eps/<node>       "<network> <priority> <dev>" lines, in attach order
#   override/<node>  the default route `ip route replace` set
#
# docker and kind are shell functions here. A failing docker and kind stub
# sits first on PATH as well, so a call that escapes the functions (an exec,
# `command docker`) fails the test instead of reaching a live daemon.

setup() {
  load "../test_helper"
  setup_tmpdir
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/ip.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/config-registry.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/network.sh"
  unset _LO_DOCKER_GW_PRIORITY

  FAKE="${BATS_TEST_TMPDIR}/fake"
  LOG="${FAKE}/log"
  mkdir -p "${FAKE}/eps" "${FAKE}/override" "${FAKE}/gw"
  : > "${LOG}"
  NODES="c-control-plane c-worker"
  REFUSE_GW_PRIORITY=0
  NO_ENDPOINT_GATEWAY=0
  FAIL_EXEC=()

  mkdir -p "${FAKE}/stubbin"
  local tool
  for tool in docker kind; do
    printf '#!/usr/bin/env bash\necho "real %s reached from a unit test: $*" >&2\nexit 97\n' "${tool}" > "${FAKE}/stubbin/${tool}"
    chmod +x "${FAKE}/stubbin/${tool}"
  done
  export PATH="${FAKE}/stubbin:${PATH}"
}

teardown() { teardown_tmpdir; }

# _world <project> <docker-version> [shared-member] — the registry map, both
# gateways, and the nodes kind created on the project network (eth0, priority
# 0). shared-member is the "shared" member of the map: '"shared":true,' (the
# default), '"shared":false,' or '' (the key omitted).
_world() {
  PROJECT="${1}"
  DOCKER_VERSION="${2}"
  local shared='"shared":true,'
  (( $# < 3 )) || shared="${3}"
  export LOK8S_REGISTRY_JSON="${BATS_TEST_TMPDIR}/.registries.json"
  printf '{%s"tls":false,"port":80,"network":{"name":"lok8s-registries","cidr":"10.125.200.0/24"},"project_network":"%s","registries":[]}\n' \
    "${shared}" "${PROJECT}" > "${LOK8S_REGISTRY_JSON}"
  # The shared network's gateway is the first address of its reserved
  # dynamic range, as Docker assigns it.
  echo "10.125.200.128" > "${FAKE}/gw/lok8s-registries"
  echo "10.125.201.192" > "${FAKE}/gw/${PROJECT}"
  local n
  for n in ${NODES}; do echo "${PROJECT} 0 eth0" > "${FAKE}/eps/${n}"; done
}

# _attach <node> <network> <priority> — an endpoint; Docker programs the
# gateway again on a connect.
_attach() {
  local dev
  dev="eth$(wc -l < "${FAKE}/eps/${1}")"
  echo "${2} ${3} ${dev}" >> "${FAKE}/eps/${1}"
  rm -f "${FAKE}/override/${1}"
}

# _restart <node> — a node restart: Docker programs the default route again,
# and the route that `ip route replace` set is gone.
_restart() { rm -f "${FAKE}/override/${1}"; }

# _network_reports_gateway — moby's behaviour for a network created with
# --subnet and no --gateway: the gateway is in `docker network inspect` from
# Docker 29 on, and empty before.
_network_reports_gateway() {
  local -a v
  read -r -a v <<<"${DOCKER_VERSION}"
  (( ${#v[@]} == 2 )) && (( ${v[1]%%.*} >= 29 ))
}

# _default_route <node> — what `ip -4 route show default` prints: the override,
# else Docker's choice (the highest priority, then the name that sorts first).
_default_route() {
  if [[ -f "${FAKE}/override/${1}" ]]; then cat "${FAKE}/override/${1}"; return; fi
  local net prio dev
  read -r net prio dev < <(LC_ALL=C sort -k2,2nr -k1,1 "${FAKE}/eps/${1}")
  echo "default via $(cat "${FAKE}/gw/${net}") dev ${dev}"
}

kind() {
  echo "kind $*" >> "${LOG}"
  [[ "${1:-} ${2:-}" == "get nodes" ]] && tr ' ' '\n' <<<"${NODES}"
  return 0
}

docker() {
  echo "docker $*" >> "${LOG}"
  case "${1:-} ${2:-}" in
    "version --format")
      [[ -n "${DOCKER_VERSION}" ]] || return 1
      echo "${DOCKER_VERSION}" ;;
    "network inspect")
      local fmt="${!#}" n
      if [[ "${fmt}" == *".Gateway"* ]]; then
        if _network_reports_gateway; then printf '%s' "$(cat "${FAKE}/gw/${3}" 2>/dev/null)"; fi
        echo " "
      elif [[ "${fmt}" == *".Containers"* ]]; then
        for n in ${NODES}; do
          if grep -q "^${3} " "${FAKE}/eps/${n}"; then printf '%s ' "${n}"; fi
        done
        echo
      fi ;;
    "network connect")
      shift 2
      local prio=0
      if [[ "${1}" == "--gw-priority" ]]; then
        (( ! REFUSE_GW_PRIORITY )) || { echo "unknown flag: --gw-priority" >&2; return 125; }
        prio="${2}"; shift 2
      fi
      _attach "${2}" "${1}" "${prio}" ;;
    "inspect "*)
      # {{with index .NetworkSettings.Networks "<net>"}}{{.Gateway}}{{end}}
      [[ "${3:-}" == "--format" && "${4:-}" == *'"}}{{.Gateway}}{{end}}' ]] || { echo "unexpected docker inspect: $*" >&2; return 99; }
      local want="${4#*Networks \"}" net prio dev
      want="${want%%\"*}"
      while read -r net prio dev; do
        if [[ "${net}" == "${want}" ]] && (( ! NO_ENDPOINT_GATEWAY )); then cat "${FAKE}/gw/${net}"; fi
      done < "${FAKE}/eps/${2}"
      echo ;;
    "exec "*)
      local node="${2}"; shift 2
      local cmd="${*}" net prio dev p
      for p in "${FAIL_EXEC[@]}"; do
        [[ "${cmd}" != "${p}"* ]] || return 1
      done
      case "${cmd}" in
        "ip -4 route show default") echo "$(_default_route "${node}") " ;;
        "ip -o -4 route get "*)
          while read -r net prio dev; do
            if [[ "$(cat "${FAKE}/gw/${net}")" == "${cmd#ip -o -4 route get }" ]]; then
              echo "${cmd#ip -o -4 route get } dev ${dev} src 10.0.0.9 uid 0 \\    cache "
            fi
          done < "${FAKE}/eps/${node}" ;;
        "ip route replace default "*) echo "${cmd#ip route replace }" > "${FAKE}/override/${node}" ;;
        *) echo "unexpected docker exec: ${cmd}" >&2; return 99 ;;
      esac ;;
  esac
}

# _assert_project_default — every node's default route goes through the
# project network (eth0, the project network's gateway).
_assert_project_default() {
  local n got want
  want="default via $(cat "${FAKE}/gw/${PROJECT}") dev eth0"
  for n in ${NODES}; do
    got=$(_default_route "${n}")
    [[ "${got}" == "${want}" ]] || {
      echo "${n}: the default route is '${got}', want '${want}' (the project network ${PROJECT})" >&2
      return 1
    }
  done
}

@test "simulation: Docker gives the default route to lok8s-registries for a project name after it" {
  # The bug, reproduced on the fake (no lok8s code).
  _world ops "29.6.2 29.6.2"
  _attach c-control-plane lok8s-registries 0
  [ "$(_default_route c-control-plane)" == "default via 10.125.200.128 dev eth1" ]
  _world kubehz "29.6.2 29.6.2"
  _attach c-control-plane lok8s-registries 0
  [ "$(_default_route c-control-plane)" == "default via 10.125.201.192 dev eth0" ]
}

@test "connect: Docker 28+ attaches the registry network with --gw-priority -1" {
  local project
  for project in ops zz-gwtest kubehz; do
    _world "${project}" "29.6.2 29.6.2"
    : > "${LOG}"
    unset _LO_DOCKER_GW_PRIORITY

    lo::connect_nodes_to_registry_network c
    run grep '^docker network connect' "${LOG}"
    assert_output "docker network connect --gw-priority -1 lok8s-registries c-control-plane
docker network connect --gw-priority -1 lok8s-registries c-worker"
    ! grep -q '^docker exec' "${LOG}" || {
      echo "${project}: the route was read or changed although Docker applies the priority:" >&2
      grep '^docker exec' "${LOG}" >&2; return 1
    }
    [ "$(grep -c '^docker version' "${LOG}")" -eq 1 ] || { echo "docker version asked more than once" >&2; return 1; }
    _assert_project_default
  done
}

@test "connect: Docker 27 connects plainly, then moves the default route to the project network" {
  _world ops "27.5.1 27.5.1"

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q -- '--gw-priority' "${LOG}" || { echo "--gw-priority used on Docker 27" >&2; cat "${LOG}" >&2; return 1; }
  grep -qx 'docker exec c-control-plane ip route replace default via 10.125.201.192 dev eth0' "${LOG}"
  grep -qx 'docker exec c-worker ip route replace default via 10.125.201.192 dev eth0' "${LOG}"
  _assert_project_default
  assert_output --partial "c-worker: the default route did not go through ops — moved it there (via 10.125.201.192 dev eth0)"
}

@test "connect: Docker 27 leaves a correct route alone (kubehz sorts first)" {
  _world kubehz "27.5.1 27.5.1"

  run lo::connect_nodes_to_registry_network c
  assert_success
  assert_output ""
  ! grep -q 'ip route replace' "${LOG}" || { echo "a correct route was replaced" >&2; return 1; }
  grep -qx 'docker exec c-control-plane ip -4 route show default' "${LOG}"
  _assert_project_default
}

@test "connect: a client that refuses --gw-priority still attaches the node and repairs the route" {
  _world ops "29.6.2 29.6.2"
  REFUSE_GW_PRIORITY=1

  lo::connect_nodes_to_registry_network c
  grep -qx 'docker network connect lok8s-registries c-worker' "${LOG}"
  _assert_project_default
}

@test "connect: a node an older lok8s attached without a priority is repaired, not connected again" {
  _world ops "29.6.2 29.6.2"
  local n
  for n in ${NODES}; do _attach "${n}" lok8s-registries 0; done

  lo::connect_nodes_to_registry_network c
  ! grep -q '^docker network connect' "${LOG}" || { echo "an attached node was connected again" >&2; return 1; }
  _assert_project_default
}

@test "repair: the gateway comes from the node endpoint (the network reports none before Docker 29)" {
  # Up to Docker 28.5, `docker network inspect` reports no gateway for a
  # network created with --subnet and no --gateway, and lo::network creates the
  # project network that way. A repair that read the network only skipped every
  # node on the engines that the fallback exists for.
  local version n
  for version in "27.5.1 27.5.1" "28.5.1 28.5.1"; do
    _world ops "${version}"
    : > "${LOG}"
    unset _LO_DOCKER_GW_PRIORITY
    # 28.5 connects with --gw-priority: test the node an older lok8s attached.
    if [[ "${version}" == 28.* ]]; then
      for n in ${NODES}; do _attach "${n}" lok8s-registries 0; done
    fi
    ! _network_reports_gateway || { echo "precondition: ${version} must report no network gateway" >&2; return 1; }

    run lo::connect_nodes_to_registry_network c
    assert_success
    for n in ${NODES}; do
      grep -qxF "docker inspect ${n} --format {{with index .NetworkSettings.Networks \"ops\"}}{{.Gateway}}{{end}}" "${LOG}" || {
        echo "${version}: ${n}: the endpoint gateway was not read" >&2; cat "${LOG}" >&2; return 1
      }
      grep -qx "docker exec ${n} ip route replace default via 10.125.201.192 dev eth0" "${LOG}" || {
        echo "${version}: ${n}: the route was not moved" >&2; cat "${LOG}" >&2; return 1
      }
    done
    _assert_project_default
    refute_output --partial "found no"
  done
}

@test "repair: no gateway on the endpoint warns per node, reads nothing, and does not fail" {
  _world ops "27.5.1 27.5.1"
  NO_ENDPOINT_GATEWAY=1

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q '^docker exec' "${LOG}" || { echo "the route was read or changed without a gateway:" >&2; grep '^docker exec' "${LOG}" >&2; return 1; }
  local n
  for n in ${NODES}; do
    assert_output --partial "lo: ${n}: found no IPv4 gateway for network ops (docker inspect: \"\"). lo did not examine the default route. Examine it: docker exec ${n} ip -4 route show default"
  done
}

@test "repair: a default route that cannot be read gets a warning" {
  _world ops "27.5.1 27.5.1"
  FAIL_EXEC=("ip -4 route show default")

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q 'ip route replace' "${LOG}" || { echo "the route was changed after a failed read" >&2; return 1; }
  assert_output --partial "lo: c-worker: cannot read the default route (docker exec c-worker ip -4 route show default failed). Make sure that the node runs, then run lo up again."
}

@test "repair: no interface to the gateway gets a warning" {
  _world ops "27.5.1 27.5.1"
  FAIL_EXEC=("ip -o -4 route get")

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q 'ip route replace' "${LOG}" || { echo "the route was changed after a failed read" >&2; return 1; }
  assert_output --partial 'lo: c-worker: found no interface to gateway 10.125.201.192 of network ops (ip -o -4 route get 10.125.201.192: ""). lo did not change the default route. Examine it: docker exec c-worker ip -4 route show default'
}

@test "reconcile: shared off or omitted, an attached node is repaired and nothing is connected" {
  # shared.enabled defaulted to true until 2026-08. A cluster created then
  # keeps its registry attachment after the default changed, and its registry
  # map now reads "not shared". The route repair runs for every attached node
  # (lo::heal_node_ips gates on membership the same way). Only the worker is
  # attached: the control plane must stay off the network.
  local shared
  for shared in '"shared":false,' ''; do
    _world ops "28.5.1 28.5.1" "${shared}"
    : > "${LOG}"
    unset _LO_DOCKER_GW_PRIORITY
    _attach c-worker lok8s-registries 0

    run lo::reconcile_registry_network_nodes c
    assert_success
    ! grep -q '^docker network connect' "${LOG}" || { echo "shared='${shared}': a cluster that is not shared was connected" >&2; cat "${LOG}" >&2; return 1; }
    ! grep -q '^docker exec c-control-plane' "${LOG}" || { echo "shared='${shared}': the route of a node that is not attached was read" >&2; return 1; }
    _assert_project_default
    assert_output --partial "c-worker: the default route did not go through ops"
  done
}

@test "reconcile: shared off and no node attached, nothing is connected, read or changed" {
  _world ops "29.6.2 29.6.2" '"shared":false,'

  run lo::reconcile_registry_network_nodes c
  assert_success
  assert_output ""
  ! grep -qE '^docker (network connect|exec|inspect)|^kind get nodes' "${LOG}" || {
    echo "a call on a cluster that is not shared and not attached:" >&2; cat "${LOG}" >&2; return 1
  }
}

@test "reconcile: shared on goes through the connect" {
  _world ops "29.6.2 29.6.2"

  run lo::reconcile_registry_network_nodes c
  assert_success
  [ "$(grep -c '^docker network connect --gw-priority -1 lok8s-registries ' "${LOG}")" -eq 2 ]
  _assert_project_default
}

@test "fallback: after a node restart, the next lo up moves the route back" {
  # The documented contract of the fallback: Docker programs the default route
  # again when a node restarts, so the move is gone. The next `lo up` (a new
  # process) finds the node attached and moves the route back.
  _world ops "27.5.1 27.5.1"
  lo::connect_nodes_to_registry_network c 2>/dev/null
  _assert_project_default

  local n
  for n in ${NODES}; do _restart "${n}"; done
  [ "$(_default_route c-worker)" == "default via 10.125.200.128 dev eth1" ] || {
    echo "simulation: after the restart the route is '$(_default_route c-worker)', want the registry network" >&2; return 1
  }

  unset _LO_DOCKER_GW_PRIORITY
  run lo::connect_nodes_to_registry_network c
  assert_success
  [ "$(grep -c '^docker network connect' "${LOG}")" -eq 2 ] || { echo "want one connect per node over both runs" >&2; cat "${LOG}" >&2; return 1; }
  for n in ${NODES}; do
    [ "$(grep -c "^docker exec ${n} ip route replace" "${LOG}")" -eq 2 ] || { echo "${n}: want 2 route moves (one per lo up)" >&2; cat "${LOG}" >&2; return 1; }
  done
  _assert_project_default
  assert_output --partial "c-worker: the default route did not go through ops — moved it there"
}

@test "lo::gw_priority_supported: client and daemon at Docker 28 or later" {
  local v
  for v in "29.6.2 29.6.2" "28.0.0 28.0.0" "28.0.0-rc.1 28.0.0-rc.1"; do
    lo::gw_priority_supported "${v}" || { echo "'${v}' refused" >&2; return 1; }
  done
  for v in "27.5.1 27.5.1" "27.5.1 29.6.2" "29.6.2 27.5.1" "29.6.2" "" "5.2.0 5.2.0" "dev dev"; do
    ! lo::gw_priority_supported "${v}" || { echo "'${v}' accepted" >&2; return 1; }
  done
}

@test "lo::default_route_repair: the device and the verdict" {
  local gw="10.125.201.192" get="10.125.201.192 dev eth0 src 10.125.201.193 uid 0 \\    cache "
  run lo::default_route_repair "default via 10.125.200.128 dev eth1 " "${get}" "${gw}"
  assert_success; assert_output "eth0"
  run lo::default_route_repair "default via ${gw} dev eth0 " "${get}" "${gw}"
  assert_failure
  run lo::default_route_repair "" "${get}" "${gw}"
  assert_success; assert_output "eth0"
  run lo::default_route_repair "default via ${gw} dev eth1" "${get}" "${gw}"
  assert_success; assert_output "eth0"
  run lo::default_route_repair "default via ${gw} dev eth0"$'\n'"default via 10.125.200.128 dev eth1" "${get}" "${gw}"
  assert_failure
  run lo::default_route_repair "default via 10.125.200.128 dev eth1" "" "${gw}"
  assert_failure 2
}
