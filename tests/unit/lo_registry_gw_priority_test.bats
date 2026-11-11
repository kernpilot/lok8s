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
  # LB: kind's HA load balancer ("" = none). `kind get nodes` lists it after
  # the nodes; its role label is external-load-balancer and its image has no
  # `ip`, so every docker exec in it fails.
  LB=""
  # FOREIGN: other members of lok8s-registries (registries, another cluster's
  # nodes); STOPPED: nodes with State.Running false; GONE: nodes that
  # `kind get nodes` lists and docker inspect does not know.
  FOREIGN=""
  STOPPED=""
  GONE=""
  # MEMBERS_READ: when set, the member read of lok8s-registries prints this
  # partial list and fails.
  MEMBERS_READ=""
  REFUSE_GW_PRIORITY=0
  NO_ENDPOINT_GATEWAY=0
  NO_ROUTE_DEV=0
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
# gateways, and the containers kind created on the project network (eth0,
# priority 0). shared-member is the "shared" member of the map:
# '"shared":true,' (the default), '"shared":false,' or '' (the key omitted).
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
  rm -f "${FAKE}/eps/"* "${FAKE}/override/"*
  local n
  for n in $(_containers); do echo "${PROJECT} 0 eth0" > "${FAKE}/eps/${n}"; done
}

# _world_ha <project> <docker-version> — an HA cluster: three control-plane
# nodes, a worker and kind's load balancer.
_world_ha() {
  NODES="c-control-plane c-control-plane2 c-control-plane3 c-worker"
  LB="c-external-load-balancer"
  _world "${@}"
}

# _containers — the cluster's containers as `kind get nodes` lists them.
_containers() { echo "${NODES}${LB:+ ${LB}}"; }

# _in <word> <list> — true when word is in the space-separated list.
_in() { [[ " ${2} " == *" ${1} "* ]]; }

# _attach <node> <network> <priority> — an endpoint; Docker programs the
# gateway again on a connect.
_attach() {
  local dev
  dev="eth$(wc -l < "${FAKE}/eps/${1}")"
  echo "${2} ${3} ${dev}" >> "${FAKE}/eps/${1}"
  rm -f "${FAKE}/override/${1}"
}

# _attached <node> <network>
_attached() { [[ -f "${FAKE}/eps/${1}" ]] && grep -q "^${2} " "${FAKE}/eps/${1}"; }

# _restart <node> — a node restart: Docker programs the default route again,
# and the route that `ip route replace` set is gone.
_restart() { rm -f "${FAKE}/override/${1}"; }

# _daemon_major — the major version of the daemon in DOCKER_VERSION, 0 when
# it does not parse.
_daemon_major() {
  local -a v
  read -r -a v <<<"${DOCKER_VERSION}"
  local m="${v[1]:-0}"
  m="${m%%.*}"
  [[ "${m}" =~ ^[0-9]+$ ]] || m=0
  echo "${m}"
}

# _network_reports_gateway — moby's behaviour for a network created with
# --subnet and no --gateway: the gateway is in `docker network inspect` from
# Docker 29 on, and empty before.
_network_reports_gateway() { (( $(_daemon_major) >= 29 )); }

# _default_route <node> — what `ip -4 route show default` prints: the override,
# else Docker's choice (the highest priority, then the name that sorts first).
_default_route() {
  if [[ -f "${FAKE}/override/${1}" ]]; then cat "${FAKE}/override/${1}"; return; fi
  local net prio dev
  read -r net prio dev < <(LC_ALL=C sort -k2,2nr -k1,1 "${FAKE}/eps/${1}")
  echo "default via $(cat "${FAKE}/gw/${net}") dev ${dev}"
}

# _inspect <node> <format> — evaluates the --format of `docker inspect` for the
# pieces the driver uses. .GwPriority before Docker 28 fails the template, as
# the field does not exist there.
_inspect() {
  local node="${1}" fmt="${2}" out role=worker running=true val net prio dev
  if [[ ! -f "${FAKE}/eps/${node}" ]] || _in "${node}" "${GONE}"; then
    echo "Error: No such object: ${node}" >&2; return 1
  fi
  if [[ "${fmt}" == *".GwPriority"* ]] && (( $(_daemon_major) < 28 )); then
    echo "template: :1: can't evaluate field GwPriority" >&2; return 1
  fi
  [[ "${node}" != *-control-plane* ]] || role=control-plane
  [[ "${node}" != "${LB}" ]] || role=external-load-balancer
  ! _in "${node}" "${STOPPED}" || running=false
  local p_role='{{index .Config.Labels "io.x-k8s.kind.role"}}' p_running='{{.State.Running}}'
  out="${fmt//"${p_role}"/${role}}"
  out="${out//"${p_running}"/${running}}"
  local re='\{\{with index \.NetworkSettings\.Networks "([^"]+)"\}\}\{\{\.(Gateway|GwPriority)\}\}\{\{end\}\}'
  while [[ "${out}" =~ ${re} ]]; do
    local piece="${BASH_REMATCH[0]}" want="${BASH_REMATCH[1]}" field="${BASH_REMATCH[2]}"
    val=""
    while read -r net prio dev; do
      [[ "${net}" == "${want}" ]] || continue
      if [[ "${field}" == GwPriority ]]; then
        val="${prio}"
      elif (( ! NO_ENDPOINT_GATEWAY )) && [[ "${running}" == true ]]; then
        val="$(cat "${FAKE}/gw/${net}")"
      fi
    done < "${FAKE}/eps/${node}"
    out="${out/"${piece}"/${val}}"
  done
  [[ "${out}" != *"{{"* ]] || { echo "unexpected docker inspect format: ${fmt}" >&2; return 99; }
  echo "${out}"
}

kind() {
  echo "kind $*" >> "${LOG}"
  [[ "${1:-} ${2:-}" == "get nodes" ]] && tr ' ' '\n' <<<"$(_containers)"
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
        if [[ -n "${MEMBERS_READ}" && "${3}" == lok8s-registries ]]; then
          printf '%s' "${MEMBERS_READ}"
          echo "Error response from daemon: context deadline exceeded" >&2
          return 1
        fi
        if [[ "${3}" == lok8s-registries ]]; then
          for n in ${FOREIGN}; do printf '%s ' "${n}"; done
        fi
        for n in $(_containers); do
          if _attached "${n}" "${3}"; then printf '%s ' "${n}"; fi
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
      if _attached "${2}" "${1}"; then
        echo "Error response from daemon: endpoint with name ${2} already exists in network ${1}" >&2
        return 1
      fi
      _attach "${2}" "${1}" "${prio}" ;;
    "inspect "*)
      [[ "${3:-}" == "--format" && $# -eq 4 ]] || { echo "unexpected docker inspect: $*" >&2; return 99; }
      _inspect "${2}" "${4}" ;;
    "exec "*)
      local node="${2}"; shift 2
      local cmd="${*}" net prio dev p
      if [[ -n "${LB}" && "${node}" == "${LB}" ]]; then
        echo "OCI runtime exec failed: exec: \"ip\": executable file not found in \$PATH" >&2
        return 127
      fi
      if _in "${node}" "${STOPPED}"; then
        echo "Error response from daemon: container ${node} is not running" >&2
        return 1
      fi
      for p in "${FAIL_EXEC[@]}"; do
        if [[ "${cmd}" == "${p}"* ]]; then echo "ip: failed" >&2; return 2; fi
      done
      case "${cmd}" in
        "ip -4 route show default") echo "$(_default_route "${node}") " ;;
        "ip -o -4 route get "*)
          (( ! NO_ROUTE_DEV )) || return 0
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
      grep "^docker inspect ${n} --format " "${LOG}" | grep -qF '{{with index .NetworkSettings.Networks "ops"}}{{.Gateway}}{{end}}' || {
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
    assert_output --partial "lo: ${n}: found no IPv4 gateway for network ops (docker inspect: \"\"). lo did not examine the default route. Attach the node to ops (docker network connect ops ${n}). Then run lo up again."
  done
}

@test "repair: a default route that cannot be read gets a warning with the exit status" {
  _world ops "27.5.1 27.5.1"
  FAIL_EXEC=("ip -4 route show default")

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q 'ip route replace' "${LOG}" || { echo "the route was changed after a failed read" >&2; return 1; }
  assert_output --partial "lo: c-worker: cannot read the default route (docker exec c-worker ip -4 route show default: exit status 2). lo did not change the default route. Make sure that the node has the ip command (iproute2). Then run lo up again."
}

@test "repair: a failed route get shows its exit status, an empty one shows the empty answer" {
  _world ops "27.5.1 27.5.1"
  FAIL_EXEC=("ip -o -4 route get")

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q 'ip route replace' "${LOG}" || { echo "the route was changed after a failed read" >&2; return 1; }
  assert_output --partial "lo: c-worker: found no interface to gateway 10.125.201.192 of network ops (ip -o -4 route get 10.125.201.192: exit status 2). lo did not change the default route. Make sure that the node has an address in the subnet of ops (docker exec c-worker ip -4 addr show). Then run lo up again."

  _world ops "27.5.1 27.5.1"
  : > "${LOG}"
  unset _LO_DOCKER_GW_PRIORITY
  FAIL_EXEC=()
  NO_ROUTE_DEV=1
  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q 'ip route replace' "${LOG}" || { echo "the route was changed without an interface" >&2; return 1; }
  assert_output --partial 'lo: c-worker: found no interface to gateway 10.125.201.192 of network ops (ip -o -4 route get 10.125.201.192: ""). lo did not change the default route. Make sure that the node has an address in the subnet of ops (docker exec c-worker ip -4 addr show). Then run lo up again.'
}

@test "repair: a failed move shows its exit status and the command to move it by hand" {
  _world ops "27.5.1 27.5.1"
  FAIL_EXEC=("ip route replace")

  run lo::connect_nodes_to_registry_network c
  assert_success
  assert_output --partial "lo: c-worker: could not move the default route to ops (ip route replace default via 10.125.201.192 dev eth0: exit status 2). Move the route by hand (docker exec c-worker ip route replace default via 10.125.201.192 dev eth0). Then run lo up again."
}

# _assert_only_cluster_touched — no call names a FOREIGN member, and no docker
# exec reaches the load balancer or a container outside NODES.
_assert_only_cluster_touched() {
  local f node
  for f in ${FOREIGN}; do
    ! grep -q " ${f}" "${LOG}" || { echo "a foreign member was touched:" >&2; grep " ${f}" "${LOG}" >&2; return 1; }
  done
  while read -r _ _ node _; do
    _in "${node}" "${NODES}" || { echo "docker exec outside the cluster's nodes: ${node}" >&2; return 1; }
  done < <(grep '^docker exec ' "${LOG}")
}

@test "HA: the load balancer is attached but never read, foreign members are never touched" {
  # `kind get nodes` lists kind's HA load balancer too. Its envoy image has no
  # `ip`: a docker exec in it fails, and a repair that tried printed a warning
  # on every lo up. It is attached like a node (the connect did that before the
  # gateway priority existed), but its route is never read.
  local -a cases=(
    "ops|29.6.2 29.6.2|-1|no|no"
    "kubehz|27.5.1 27.5.1|0|yes|no"
    "ops|27.5.1 27.5.1|0|yes|yes"
    "ops|29.6.2 29.6.2|none|no|no"
    "ops|27.5.1 27.5.1|none|yes|yes"
  )
  local c project version attached want_exec want_moves n label
  for c in "${cases[@]}"; do
    IFS='|' read -r project version attached want_exec want_moves <<<"${c}"
    _world_ha "${project}" "${version}"
    FOREIGN="other-control-plane lok8s-registry-io-docker"
    : > "${LOG}"
    unset _LO_DOCKER_GW_PRIORITY
    if [[ "${attached}" != none ]]; then
      for n in $(_containers); do _attach "${n}" lok8s-registries "${attached}"; done
    fi
    label="${project} ${version} attached=${attached}"

    run lo::connect_nodes_to_registry_network c
    assert_success
    _assert_only_cluster_touched || { echo "${label}" >&2; return 1; }
    _assert_project_default || { echo "${label}" >&2; return 1; }
    _attached "${LB}" lok8s-registries || { echo "${label}: the load balancer is not attached" >&2; return 1; }
    if grep -q '^docker exec ' "${LOG}"; then n=yes; else n=no; fi
    [[ "${n}" == "${want_exec}" ]] || { echo "${label}: docker exec called: ${n}, want ${want_exec}" >&2; cat "${LOG}" >&2; return 1; }
    [[ "${output}" != *"${LB}"* ]] || { echo "${label}: a warning names the load balancer: ${output}" >&2; return 1; }
    if [[ "${want_moves}" == yes ]]; then
      [ "$(grep -c 'moved it there' <<<"${output}")" -eq 4 ] || { echo "${label}: want a move per node: ${output}" >&2; return 1; }
    else
      [[ -z "${output}" ]] || { echo "${label}: output on a healthy cluster: ${output}" >&2; return 1; }
    fi
  done
}

@test "repair: Docker 28+ skips a node whose registry endpoint has a negative priority" {
  # The priority comes from the same `docker inspect`; the three in-node
  # commands are skipped. A node at 0 (an older lok8s) is still repaired.
  _world ops "28.5.1 28.5.1"
  _attach c-control-plane lok8s-registries -1
  _attach c-worker lok8s-registries 0

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q '^docker exec c-control-plane' "${LOG}" || { echo "a node at priority -1 was read or changed" >&2; cat "${LOG}" >&2; return 1; }
  [ "$(grep -c '^docker exec c-worker ip route replace' "${LOG}")" -eq 1 ] || { echo "the node at priority 0 was not repaired" >&2; cat "${LOG}" >&2; return 1; }
  [ "$(grep -c '^docker inspect ' "${LOG}")" -eq 2 ] || { echo "want one docker inspect per node" >&2; cat "${LOG}" >&2; return 1; }
  _assert_project_default
  refute_output --partial c-control-plane
}

@test "repair: a node that does not run gets its own warning, no docker exec" {
  _world ops "27.5.1 27.5.1"
  local n
  for n in ${NODES}; do _attach "${n}" lok8s-registries 0; done
  STOPPED="c-worker"

  run lo::connect_nodes_to_registry_network c
  assert_success
  ! grep -q '^docker exec c-worker' "${LOG}" || { echo "docker exec in a node that does not run" >&2; return 1; }
  assert_output --partial 'lo: c-worker: the node is not running (State.Running: "false"). lo did not examine the default route. Start the node (docker start c-worker). Then run lo up again.'
  refute_output --partial "found no IPv4 gateway"
  assert_output --partial "c-control-plane: the default route did not go through ops"
}

@test "repair: a node that docker cannot inspect gets a warning with the exit status" {
  _world ops "27.5.1 27.5.1"
  local n
  for n in ${NODES}; do _attach "${n}" lok8s-registries 0; done
  GONE="c-worker"

  run lo::connect_nodes_to_registry_network c
  assert_success
  assert_output --partial "lo: c-worker: cannot inspect the node (docker inspect c-worker: exit status 1). lo did not examine the default route. Make sure that the node exists. Then run lo up again."
  ! grep -q '^docker exec c-worker' "${LOG}" || { echo "docker exec in a node that cannot be inspected" >&2; return 1; }
}

@test "members: a member read that fails with partial output is no membership" {
  # The partial list names another cluster's node and c-worker, which is NOT
  # attached. The connect still attaches every node (Docker refuses a second
  # endpoint for c-control-plane, and the repair moves its route); the
  # repair-only walk touches nothing.
  _world ops "29.6.2 29.6.2"
  FOREIGN="other-control-plane lok8s-registry-io-docker"
  _attach c-control-plane lok8s-registries 0
  MEMBERS_READ="other-control-plane c-worker "

  run lo::connect_nodes_to_registry_network c
  assert_success
  local n
  for n in ${NODES}; do
    _attached "${n}" lok8s-registries || { echo "${n} is not attached" >&2; cat "${LOG}" >&2; return 1; }
  done
  [ "$(grep -c '^lok8s-registries ' "${FAKE}/eps/c-control-plane")" -eq 1 ] || { echo "a second endpoint for c-control-plane" >&2; return 1; }
  _assert_only_cluster_touched
  _assert_project_default

  _world ops "29.6.2 29.6.2" '"shared":false,'
  : > "${LOG}"
  unset _LO_DOCKER_GW_PRIORITY
  _attach c-worker lok8s-registries 0
  run lo::reconcile_registry_network_nodes c
  assert_success
  assert_output ""
  ! grep -qE '^docker (network connect|exec|inspect)|^kind get nodes' "${LOG}" || {
    echo "a call after a failed member read:" >&2; cat "${LOG}" >&2; return 1
  }
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
