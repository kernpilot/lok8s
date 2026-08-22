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
      -o "com.docker.network.bridge.name=${network}" \
      -o "com.docker.network.bridge.enable_ip_masquerade=true" \
      -o "com.docker.network.bridge.enable_icc=true" \
      -o "com.docker.network.bridge.host_binding_ipv4=0.0.0.0" \
      "${network}"
  fi
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

lo::connect_nodes_to_registry_network() {
  local cluster_name="${1}"
  local registry_network
  registry_network=$(registry::network_name)

  # Skip nodes that are already attached — a re-run must not error (nor rely
  # on the 2>/dev/null to hide a real failure for the common no-op case).
  local members
  members=$(docker network inspect "${registry_network}" \
    -f '{{range .Containers}}{{.Name}} {{end}}' 2>/dev/null || true)

  local node
  for node in $(kind get nodes --name "${cluster_name}" 2>/dev/null); do
    [[ " ${members} " == *" ${node} "* ]] && continue
    docker network connect "${registry_network}" "${node}" 2>/dev/null || true
  done
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
