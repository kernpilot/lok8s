# shellcheck shell=bash
# network.sh — Docker network lifecycle for Lo clusters

import ^utils/ip

lo::network() {
  local network="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-}"
  local subnet="${LOK8S_NETWORK_SUBNET:-}"
  [[ -n "${network}" ]] || { echo "error: KIND_EXPERIMENTAL_DOCKER_NETWORK not set (call lo::read_network_config first)" >&2; return 1; }
  [[ -n "${subnet}" ]]  || { echo "error: LOK8S_NETWORK_SUBNET not set (call lo::read_network_config first)" >&2; return 1; }

  if docker network inspect "${network}" &>/dev/null; then
    local current_subnet
    current_subnet=$(docker network inspect "${network}" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null || true)
    if [[ "${current_subnet}" != "${subnet}" ]]; then
      docker network rm -f "${network}" 2>/dev/null || true
    fi
  fi

  if ! docker network inspect "${network}" &>/dev/null; then
    # Reserve the upper quarter for Docker's dynamic allocation (kind nodes),
    # same idea as the shared network's reservation: everything static on the
    # project net — build/cache at .101/.102, non-shared mirrors at .103+, the
    # MetalLB pool at .125-.150 — lives BELOW it, so a node can never squat a
    # registry address or collide with an LB IP after a reboot. Legacy
    # networks (no range) keep working; a squat there fails loudly with the
    # holder named (see lo::registries).
    local ip_range_args=()
    local dynamic_range
    if dynamic_range=$(lo::network_dynamic_range "${subnet}"); then
      ip_range_args=(--ip-range "${dynamic_range}")
    fi
    docker network create -d=bridge --subnet "${subnet}" \
      "${ip_range_args[@]}" \
      -o "com.docker.network.bridge.name=$(lo::network_bridge_name "${network}")" \
      -o "com.docker.network.bridge.enable_ip_masquerade=true" \
      -o "com.docker.network.bridge.enable_icc=true" \
      -o "com.docker.network.bridge.host_binding_ipv4=0.0.0.0" \
      "${network}"
  fi
}

# lo::network_bridge_name <network> — the host bridge interface for a
# project network. A name of at most 15 bytes (IFNAMSIZ-1: IFNAMSIZ is 16
# and counts the NUL terminator) is used as it is, so every existing
# bridge keeps its name. A longer one becomes "lo-<12 hex of its sha256>":
# Docker hands the option to the kernel unchecked, and the kernel answers
# a 16-byte name with "numerical result out of range". Same rule as the
# Go driver's bridgeName: bytes, not characters, and the hash of the raw
# bytes without a newline.
lo::network_bridge_name() {
  local network="${1}"
  local -i bytes
  bytes="$(printf '%s' "${network}" | wc -c)"
  if (( bytes <= 15 )); then
    echo "${network}"
    return 0
  fi
  # coreutils on Linux, shasum on macOS (the kubehz lib does the same).
  local sum
  sum="$(printf '%s' "${network}" | sha256sum 2>/dev/null | cut -c1-12)"
  [[ -n "${sum}" ]] || sum="$(printf '%s' "${network}" | shasum -a 256 2>/dev/null | cut -c1-12)"
  if [[ -z "${sum}" ]]; then
    echo "error: lo::network_bridge_name needs sha256sum or shasum for the bridge name of '${network}'" >&2
    return 1
  fi
  echo "lo-${sum}"
}

# lo::network_dynamic_range <cidr> — the upper QUARTER of <cidr> as its own
# CIDR (10.125.125.0/24 → 10.125.125.192/26). The project net needs a smaller
# dynamic pool than the shared net's upper half: its static tenants reach
# higher (registries .101+, the default MetalLB pool up to .150), and ~60
# dynamic addresses is far beyond any kind cluster's node count.
lo::network_dynamic_range() {
  local cidr="${1}"
  local base="${cidr%/*}" prefix="${cidr#*/}" start
  (( prefix >= 1 && prefix <= 28 )) || return 1
  start=$(ip::add "${base}" $(( 3 << (30 - prefix) ))) || return 1
  echo "${start}/$(( prefix + 2 ))"
}

# lo::registry_dynamic_range <cidr> — the upper half of <cidr> as its own
# CIDR (10.125.200.0/24 → 10.125.200.128/25). Passed as --ip-range so
# Docker's DYNAMIC allocation (kind nodes attaching to the shared network)
# never hands out the low addresses the registries claim statically —
# without it a node attached before the registries start squats their IPs
# ("Address already in use" on every restart).
lo::registry_dynamic_range() {
  local cidr="${1}"
  local base="${cidr%/*}" prefix="${cidr#*/}" start
  (( prefix >= 1 && prefix <= 30 )) || return 1
  start=$(ip::add "${base}" $(( 1 << (31 - prefix) ))) || return 1
  echo "${start}/$(( prefix + 1 ))"
}

# lo::registry_network_create <network> <subnet> [ip-range]
# The one place the shared network is created. The reserved dynamic range is
# what makes registry-IP squatting IMPOSSIBLE — statics live below it, dynamic
# attachers (kind nodes) can only ever land inside it.
lo::registry_network_create() {
  local network="${1}" subnet="${2}" dynamic_range="${3:-}"
  local ip_range_args=()
  [[ -n "${dynamic_range}" ]] && ip_range_args=(--ip-range "${dynamic_range}")

  docker network create -d=bridge --subnet "${subnet}" \
    "${ip_range_args[@]}" \
    -o "com.docker.network.bridge.enable_ip_masquerade=true" \
    -o "com.docker.network.bridge.enable_icc=true" \
    --label "lok8s.registry=shared" \
    "${network}"
}

# lo::registry_network
# Ensure the shared registry network exists WITH the reserved dynamic range.
#
# A network created before the range reservation is force-recreated: the
# attached lok8s mirror containers are removed too, so the lo::registries
# reconcile that follows in the same run recreates them at their static
# addresses on the new network; kind nodes are force-detached and re-attach
# via lo::connect_nodes_to_registry_network (this run for the active cluster,
# the next `lo up` for any other project). No in-place migration choreography
# — the network and its mirrors are cheap, disposable state.
lo::registry_network() {
  local network
  network=$(registry::network_name)
  local subnet
  subnet=$(registry::network_cidr)

  local dynamic_range=""
  dynamic_range=$(lo::registry_dynamic_range "${subnet}") || dynamic_range=""

  if docker network inspect "${network}" &>/dev/null; then
    local current_subnet
    current_subnet=$(docker network inspect "${network}" \
      --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null || true)
    if [[ "${current_subnet}" != "${subnet}" ]]; then
      echo "error: registry network '${network}' exists with subnet ${current_subnet}, expected ${subnet}" >&2
      echo "error: run 'lo registry clean --shared' to recreate, or adjust spec.registries.shared.network.cidr" >&2
      return 1
    fi

    # THE reserved range — nothing to do. A non-empty range that differs
    # (older tooling, a hand-made network) is NOT safe: dynamic allocation
    # could still overlap the statics — recreate it like the no-range case.
    local current_range
    current_range=$(docker network inspect "${network}" \
      --format '{{range .IPAM.Config}}{{.IPRange}}{{end}}' 2>/dev/null || true)
    if [[ "${current_range}" == "${dynamic_range}" ]] || [[ -z "${dynamic_range}" ]]; then
      return 0
    fi

    # Legacy network without the reserved range: recreate it. Remove OUR
    # mirror containers first (a running mirror whose config-hash still
    # matches would otherwise reconcile as "unchanged" while detached from
    # the new network); their named volumes — the cache — survive.
    warn "lo: registry network '${network}' lacks the reserved dynamic range (has '${current_range:-none}') — recreating it with ${dynamic_range} (mirrors + nodes re-attach via the normal reconcile)"

    # Serialize the recreate across concurrent `lo` runs (the shared network
    # is host-global). Best-effort, same pattern as the registry reconcile:
    # without flock a loser can rm the WINNER's freshly-created network.
    local lockfd=0
    mkdir -p "${LO_REGISTRY_STATE_DIR}" 2>/dev/null || true
    if command -v flock >/dev/null 2>&1 && exec 8>"${LO_REGISTRY_STATE_DIR}/${network}.netlock"; then
      lockfd=1
      flock -w 60 8 || debug "registry network ${network}: lock wait timed out, proceeding unlocked"
      # The winner may have finished the recreate while we waited — re-check
      # against the SAME equality as above, not mere non-emptiness.
      current_range=$(docker network inspect "${network}" \
        --format '{{range .IPAM.Config}}{{.IPRange}}{{end}}' 2>/dev/null || true)
      if [[ "${current_range}" == "${dynamic_range}" ]]; then
        (( lockfd )) && exec 8>&-
        return 0
      fi
    fi

    # From here to the create the lock must stay HELD: releasing it between
    # the rm and the create re-opens the exact window it exists for — a
    # loser's rm retry removing the winner's freshly-created network.
    local rm_rc=0
    if docker network inspect "${network}" &>/dev/null; then
      local name
      for name in $(docker network inspect "${network}" \
        -f '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}' 2>/dev/null); do
        if [[ "${name}" == "${LO_SHARED_REGISTRY_PREFIX}"* ]]; then
          # Removed, not detached: a running mirror with a matching config-hash
          # would reconcile "unchanged" while off-network. Cache volumes survive.
          docker rm -f "${name}" >/dev/null 2>&1 || true
        else
          # `docker network rm` REFUSES a network with active endpoints (even
          # with -f, which only suppresses the not-found error — verified live)
          # — every remaining member must be detached explicitly.
          docker network disconnect -f "${network}" "${name}" >/dev/null 2>&1 || true
        fi
      done
      # A just-removed container's endpoint can lag its release (the same lag
      # the registry-start retry absorbs) — one bounded retry before giving up.
      # stderr is held back until the retry also fails: a transiently-lagging
      # first attempt is expected, not something to print.
      local attempt rm_err=""
      rm_rc=1
      for attempt in 1 2; do
        rm_err=$(docker network rm "${network}" 2>&1 >/dev/null) && { rm_rc=0; break; }
        (( attempt == 1 )) && sleep 1
      done
      (( rm_rc == 0 )) || [[ -z "${rm_err}" ]] || echo "${rm_err}" >&2
    fi
    # else: a prior run removed the network but died before recreating it —
    # fall through and create.

    local create_rc=0
    if (( rm_rc == 0 )); then
      lo::registry_network_create "${network}" "${subnet}" "${dynamic_range}" || create_rc=1
    fi
    (( lockfd )) && exec 8>&-
    (( rm_rc == 0 )) || return 1
    return "${create_rc}"
  fi

  lo::registry_network_create "${network}" "${subnet}" "${dynamic_range}"
}

# The gateway priority of a kind node's endpoint on the shared registry
# network. kind creates the node on the project network with the default
# priority 0. A container on two networks gets its default route from the
# network with the highest priority; between equal priorities Docker takes the
# network whose name sorts first. Without a priority, a project network whose
# name sorts after "lok8s-registries" (for example "ops") lost the default
# route to the registry network: node-to-service traffic left with the registry
# address, rp_filter dropped part of the replies, and the Cilium init container
# hung on https://10.96.0.1:443. With -1 the project network keeps the default
# route for every name. Go twin: registryGwPriority (internal/driver/lo).
LO_REGISTRY_GW_PRIORITY=-1
# The first Docker release whose `docker network connect` accepts
# --gw-priority (28.0, API 1.48).
LO_GW_PRIORITY_MIN_MAJOR=28

# lo::gw_priority_supported <versions> — true when `docker version --format
# '{{.Client.Version}} {{.Server.Version}}'` names a client AND a daemon of
# Docker 28.0 or later: the client parses the flag, the daemon applies it. A
# value that does not parse (another engine, no daemon) reads as false.
lo::gw_priority_supported() {
  local -a fields
  read -r -a fields <<<"${1}"
  (( ${#fields[@]} == 2 )) || return 1
  local v major
  for v in "${fields[@]}"; do
    major="${v%%.*}"
    [[ "${major}" =~ ^[0-9]+$ ]] || return 1
    (( 10#${major} >= LO_GW_PRIORITY_MIN_MAJOR )) || return 1
  done
}

# lo::docker_gw_priority — lo::gw_priority_supported for the docker on PATH,
# asked once per process (_LO_DOCKER_GW_PRIORITY caches the answer).
lo::docker_gw_priority() {
  if [[ -z "${_LO_DOCKER_GW_PRIORITY:-}" ]]; then
    local versions
    versions=$(docker version --format '{{.Client.Version}} {{.Server.Version}}' 2>/dev/null) || versions=""
    _LO_DOCKER_GW_PRIORITY=0
    if lo::gw_priority_supported "${versions}"; then _LO_DOCKER_GW_PRIORITY=1; fi
  fi
  (( _LO_DOCKER_GW_PRIORITY == 1 ))
}

# lo::route_field <line> <key> — the word after key in one `ip route` line
# ("dev", "via"); nothing when the line does not carry it.
lo::route_field() {
  local -a f
  read -r -a f <<<"${1}"
  local i
  for (( i = 0; i + 1 < ${#f[@]}; i++ )); do
    if [[ "${f[${i}]}" == "${2}" ]]; then
      echo "${f[$(( i + 1 ))]}"
      return 0
    fi
  done
}

# lo::default_route_repair <current> <route-get> <gw> — decides the default
# route of a node from two `ip` reads in the node: current is
# `ip -4 route show default`, route-get is `ip -o -4 route get <gw>` for the
# gateway of the project network. Prints the device that reaches gw (the
# project network's interface) and returns 0 when the default route is not
# `via <gw> dev <dev>` yet; returns 1 when it is; returns 2 when route-get
# names no device.
lo::default_route_repair() {
  local current="${1}" route_get="${2}" gw="${3}" dev first
  dev=$(lo::route_field "${route_get}" dev)
  [[ -n "${dev}" ]] || return 2
  first="${current%%$'\n'*}"
  if [[ "$(lo::route_field "${first}" via)" == "${gw}" && "$(lo::route_field "${first}" dev)" == "${dev}" ]]; then
    return 1
  fi
  echo "${dev}"
}

# lo::repair_default_route <node> <network> <registry-network> — moves the
# default route of a node back to the project network when it goes through
# another network. It is the fallback for a Docker without --gw-priority, and
# the repair for a node that an older lok8s attached without a priority. One
# `docker inspect <node>` reads the kind role label, State.Running, the gateway
# of the node's own endpoint on the project network and, on Docker 28+ only
# (lo::docker_gw_priority: an older docker fails the whole template on
# .GwPriority), the gateway priority of its registry endpoint. Docker fills
# .Gateway for every connected endpoint of a running container, but up to
# Docker 28.5 `docker network inspect` reports only a gateway that the create
# requested, and lo::network creates the project network with --subnet and no
# --gateway. Docker writes the route again when the node restarts, so on such
# a Docker the next `lo up` repeats the repair. Skipped without a docker exec:
# kind's HA load balancer (its envoy image has no `ip`; it forwards from the
# host to the control-plane nodes on the directly connected project network,
# so its default route does not matter), and on Docker 28+ a node whose
# registry endpoint has a negative gateway priority (correct by construction).
# Advisory: a node that cannot be read or changed gets a warning, never a
# failure. Go twin: repairDefaultRoute (inspectNodeRoute).
lo::repair_default_route() {
  local node="${1}" network="${2}" registry="${3}"
  local fmt out role running gw prio current route_get observed dev rc=0
  fmt='{{index .Config.Labels "io.x-k8s.kind.role"}}|{{.State.Running}}|{{with index .NetworkSettings.Networks "'"${network}"'"}}{{.Gateway}}{{end}}'
  if lo::docker_gw_priority; then
    fmt+='|{{with index .NetworkSettings.Networks "'"${registry}"'"}}{{.GwPriority}}{{end}}'
  fi
  out=$(docker inspect "${node}" --format "${fmt}" 2>/dev/null) || rc=$?
  if (( rc != 0 )); then
    warn "lo: ${node}: cannot inspect the node (docker inspect ${node}: exit status ${rc}). lo did not examine the default route. Make sure that the node exists. Then run lo up again."
    return 0
  fi
  IFS='|' read -r role running gw prio <<<"${out}"
  [[ "${role}" != external-load-balancer ]] || return 0
  if [[ "${running}" != true ]]; then
    warn "lo: ${node}: the node is not running (State.Running: \"${running}\"). lo did not examine the default route. Start the node (docker start ${node}). Then run lo up again."
    return 0
  fi
  if [[ "${prio}" =~ ^-[0-9]+$ ]] && (( 10#${prio#-} > 0 )); then
    return 0
  fi
  if ! ip::validate_format "${gw}" 2>/dev/null; then
    warn "lo: ${node}: found no IPv4 gateway for network ${network} (docker inspect: \"${gw}\"). lo did not examine the default route. Attach the node to ${network} (docker network connect ${network} ${node}). Then run lo up again."
    return 0
  fi
  current=$(docker exec "${node}" ip -4 route show default 2>/dev/null) || rc=$?
  if (( rc != 0 )); then
    warn "lo: ${node}: cannot read the default route (docker exec ${node} ip -4 route show default: exit status ${rc}). lo did not change the default route. Make sure that the node has the ip command (iproute2). Then run lo up again."
    return 0
  fi
  route_get=$(docker exec "${node}" ip -o -4 route get "${gw}" 2>/dev/null) || rc=$?
  observed="ip -o -4 route get ${gw}: \"${route_get}\""
  if (( rc != 0 )); then
    route_get=""
    observed="ip -o -4 route get ${gw}: exit status ${rc}"
    rc=0
  fi
  dev=$(lo::default_route_repair "${current}" "${route_get}" "${gw}") || rc=$?
  if (( rc == 2 )); then
    warn "lo: ${node}: found no interface to gateway ${gw} of network ${network} (${observed}). lo did not change the default route. Make sure that the node has an address in the subnet of ${network} (docker exec ${node} ip -4 addr show). Then run lo up again."
    return 0
  fi
  (( rc == 0 )) || return 0
  docker exec "${node}" ip route replace default via "${gw}" dev "${dev}" &>/dev/null || rc=$?
  if (( rc != 0 )); then
    warn "lo: ${node}: could not move the default route to ${network} (ip route replace default via ${gw} dev ${dev}: exit status ${rc}). Move the route by hand (docker exec ${node} ip route replace default via ${gw} dev ${dev}). Then run lo up again."
    return 0
  fi
  warn "lo: ${node}: the default route did not go through ${network} — moved it there (via ${gw} dev ${dev})"
}

# lo::reconcile_registry_network_nodes <cluster-name> — the step after
# `kind create`. A shared cluster attaches its nodes to the shared registry
# network (lo::connect_nodes_to_registry_network). A cluster that is not
# shared attaches nothing, but its nodes can still be on that network:
# shared.enabled defaulted to true until 2026-08, and a node keeps its
# attachment after the default changed. Those nodes get the same route
# repair. The membership read is the healer's gate too (see
# lo::nodes_on_registry_network). Advisory when not shared: a registry map
# that cannot be read is not a reason to fail the provision. Go twin:
# reconcileRegistryNetworkNodes.
lo::reconcile_registry_network_nodes() {
  local cluster_name="${1}"
  [[ -n "${LOK8S_REGISTRY_JSON:-}" && -f "${LOK8S_REGISTRY_JSON}" ]] || return 0
  if registry::is_shared; then
    lo::connect_nodes_to_registry_network "${cluster_name}"
    return
  fi
  _lo_registry_network_nodes "${cluster_name}" repair
}

# lo::connect_nodes_to_registry_network <cluster-name> — attaches the cluster's
# kind nodes to the shared registry network. The registry endpoint gets the
# gateway priority LO_REGISTRY_GW_PRIORITY, so the project network keeps the
# default route of the node. A Docker without --gw-priority (or a connect that
# refuses the flag) gets a plain connect, then lo::repair_default_route. An
# attached node goes through lo::repair_default_route too: it repairs a node
# that an older lok8s attached without a priority.
lo::connect_nodes_to_registry_network() {
  local cluster_name="${1}"
  _lo_registry_network_nodes "${cluster_name}" attach
}

# _lo_registry_network_nodes <cluster-name> <attach|repair> — walks the
# cluster's kind nodes (`kind get nodes`) against the shared registry network.
# Other members of that network (the registries, another cluster's nodes) are
# never touched. A node that is attached goes through lo::repair_default_route.
# With attach, a node that is not attached is connected (see
# lo::connect_nodes_to_registry_network); with repair, the node is left alone,
# and an empty member list ends the walk before `kind get nodes`. A member read
# that fails counts as empty, whatever it printed: a partial list is not a
# membership. kind's HA load balancer is attached like a node, as before the
# gateway priority existed; lo::repair_default_route skips it. Go twin:
# registryNetworkNodes.
_lo_registry_network_nodes() {
  local cluster_name="${1}" mode="${2}"
  local registry_network project_network repair=1
  registry_network=$(registry::network_name)
  project_network=$(registry::project_network)
  [[ -n "${project_network}" && "${project_network}" != "null" && "${project_network}" != "${registry_network}" ]] || repair=0

  # Skip nodes that are already attached — a re-run must not error (nor rely
  # on the 2>/dev/null to hide a real failure for the common no-op case).
  local members
  members=$(docker network inspect "${registry_network}" \
    -f '{{range .Containers}}{{.Name}} {{end}}' 2>/dev/null) || members=""
  if [[ "${mode}" != attach && -z "${members//[[:space:]]/}" ]]; then
    return 0
  fi

  local node
  for node in $(kind get nodes --name "${cluster_name}" 2>/dev/null); do
    if [[ " ${members} " == *" ${node} "* ]]; then
      if (( repair )); then lo::repair_default_route "${node}" "${project_network}" "${registry_network}"; fi
      continue
    fi
    [[ "${mode}" == attach ]] || continue
    if lo::docker_gw_priority &&
        docker network connect --gw-priority "${LO_REGISTRY_GW_PRIORITY}" "${registry_network}" "${node}" 2>/dev/null; then
      continue
    fi
    docker network connect "${registry_network}" "${node}" 2>/dev/null || true
    if (( repair )); then lo::repair_default_route "${node}" "${project_network}" "${registry_network}"; fi
  done
  return 0
}

# lo::nodes_on_registry_network <cluster-name>
# True when at least one of the cluster's kind nodes is attached to the shared
# registry network. The gate for lo::heal_node_ips: a node can only register on
# the wrong network if it HAS a second network — and after the shared→per-project
# default flip, an already-drifted cluster whose spec omits shared.enabled reads
# "not shared" while its nodes still carry the registry NIC from the old default.
# Gating on membership (not the spec) covers exactly that upgrade.
lo::nodes_on_registry_network() {
  local cluster_name="${1}"
  local members
  members=$(docker network inspect "$(registry::network_name)" \
    -f '{{range .Containers}}{{.Name}} {{end}}' 2>/dev/null) || return 1
  local node
  for node in $(kind get nodes --name "${cluster_name}" 2>/dev/null); do
    [[ " ${members} " == *" ${node} "* ]] && return 0
  done
  return 1
}

# lo::heal_node_ips <cluster-name> [kubeconfig]
# Repair kind nodes that registered their InternalIP on the WRONG docker network.
#
# Shared-registry clusters are dual-homed: kind creates the node on the cluster
# network ($KIND_EXPERIMENTAL_DOCKER_NETWORK) and
# lo::connect_nodes_to_registry_network attaches a second NIC on the registry
# network. kind's entrypoint derives the node address from docker DNS
# (getent ahostsv4 on the container hostname), and for a dual-homed container
# that lookup can flip to the REGISTRY network's address after an endpoint
# re-attach — the entrypoint then rewrites --node-ip and every other address
# reference to it on the next container restart. That address is dynamically
# allocated, so it also drifts, leaving the node registered on an IP that
# answers nowhere.
#
# The failure is silent and brutal: the node stays Ready (the lease heartbeat is
# a separate path from node status), but kubelet rejects every node-status
# update ("failed to validate nodeIP") and nothing can reach INTO the node —
# apiserver→kubelet and every cross-node pod route break. Observed 2026-08-17 on
# kubehz-dev: cert-manager-webhook lived there, so every webhook-validated apply
# failed and `lo up` died on `networking` + `cnpg-plugin` after burning all 6 of
# bootstrap's CRD/webhook retries — retries that can never win, because this is
# not the transient race they exist for.
#
# The repair replicates the entrypoint's OWN update mechanism, but keyed on the
# cluster-network address instead of DNS luck: sed the stale address across the
# same files_to_update set the entrypoint maintains, write /kind/old-ipv4 so the
# next boot's diff is quiet, restart kubelet. Idempotent and silent when
# everything already agrees.
lo::heal_node_ips() {
  local cluster_name="${1}" kubeconfig="${2:-}"
  local network="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-}"
  [[ -n "${network}" ]] || return 0

  local node want have observed
  local -a healed=()
  for node in $(kind get nodes --name "${cluster_name}" 2>/dev/null); do
    # The node's address on the CLUSTER network — the only correct --node-ip.
    want=$(docker inspect "${node}" \
      --format "{{with index .NetworkSettings.Networks \"${network}\"}}{{.IPAddress}}{{end}}" 2>/dev/null || true)
    [[ -n "${want}" ]] || continue

    have=$(docker exec "${node}" cat /var/lib/kubelet/kubeadm-flags.env 2>/dev/null \
      | sed -n 's/.*--node-ip=\([^ "]*\).*/\1/p')
    [[ -n "${have}" ]] || continue

    if [[ "${have}" == "${want}" ]]; then
      # The file agrees — but a prior heal may have died between the sed and
      # the kubelet restart, leaving kubelet running on the dead address while
      # the file (the only thing the drift check reads) looks repaired. The
      # Node object's InternalIP is the running truth: node status is frozen
      # at the last ACCEPTED address, so a mismatch means kubelet never
      # restarted onto the repaired flag. Restart it now.
      [[ -n "${kubeconfig}" ]] || continue
      observed=$(kubectl --kubeconfig "${kubeconfig}" get node "${node}" \
        -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
      if [[ -n "${observed}" ]] && [[ "${observed}" != "${want}" ]]; then
        warn "lo: ${node} flag already repaired but the node still reports ${observed} — restarting kubelet"
        docker exec "${node}" systemctl restart kubelet 2>/dev/null \
          || warn "lo: could not restart kubelet on ${node}"
        healed+=("${node}")
      fi
      continue
    fi

    # Dual-stack (--node-ip=v4,v6) is a deliberate config, not the drift this
    # heals — a naive rewrite would silently drop the v6 half. Warn, don't touch.
    if [[ "${have}" == *,* ]]; then
      warn "lo: ${node} has a dual-stack --node-ip=${have} — not healing (expected ${want} on ${network})"
      continue
    fi

    # Same file set kind's entrypoint updates on an address change (manifests
    # only exist on control-plane nodes — existence-guarded). \b-anchored like
    # the entrypoint's own sed, so 10.125.200.2 can't match inside .200.20.
    warn "lo: ${node} registered --node-ip=${have} (wrong network) — repointing to ${want} on ${network}"
    docker exec "${node}" bash -c "
      for f in /etc/kubernetes/manifests/etcd.yaml \
               /etc/kubernetes/manifests/kube-apiserver.yaml \
               /etc/kubernetes/manifests/kube-controller-manager.yaml \
               /etc/kubernetes/manifests/kube-scheduler.yaml \
               /etc/kubernetes/controller-manager.conf \
               /etc/kubernetes/scheduler.conf \
               /etc/kubernetes/kubelet.conf \
               /kind/kubeadm.conf \
               /var/lib/kubelet/kubeadm-flags.env; do
        [ -f \"\${f}\" ] && sed -i 's|\b${have//./\\.}\b|${want}|g' \"\${f}\"
      done
      printf '%s' '${want}' > /kind/old-ipv4
      systemctl restart kubelet
    " 2>/dev/null || { warn "lo: could not repair ${node} — see 'docker exec ${node} systemctl status kubelet'"; continue; }
    healed+=("${node}")
  done

  (( ${#healed[@]} )) || return 0

  # The CNI caches the node address too (Cilium mirrors it into CiliumNode and
  # every peer's tunnel map), so a repaired node keeps routing to the dead IP
  # until its agent re-registers. Best-effort: on a first provision there is no
  # CNI yet, and bootstrap installs a fresh one moments later either way.
  [[ -n "${kubeconfig}" ]] || return 0
  for node in "${healed[@]}"; do
    kubectl --kubeconfig "${kubeconfig}" -n kube-system delete pod \
      -l k8s-app=cilium --field-selector "spec.nodeName=${node}" 2>/dev/null || true
  done
}
