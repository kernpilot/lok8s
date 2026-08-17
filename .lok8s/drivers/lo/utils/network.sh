# shellcheck shell=bash
# network.sh — Docker network lifecycle for Lo clusters

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
    docker network create -d=bridge --subnet "${subnet}" \
      -o "com.docker.network.bridge.name=${network}" \
      -o "com.docker.network.bridge.enable_ip_masquerade=true" \
      -o "com.docker.network.bridge.enable_icc=true" \
      -o "com.docker.network.bridge.host_binding_ipv4=0.0.0.0" \
      "${network}"
  fi
}

# lo::registry_dynamic_range <cidr> — the upper half of <cidr> as its own
# CIDR (10.125.200.0/24 → 10.125.200.128/25). Passed as --ip-range so
# Docker's DYNAMIC allocation (kind nodes attaching to the shared network)
# never hands out the low addresses the registries claim statically —
# without it a node attached before the registries start squats their IPs
# ("Address already in use" on every restart).
lo::registry_dynamic_range() {
  local cidr="${1}"
  local base="${cidr%/*}" prefix="${cidr#*/}"
  (( prefix >= 1 && prefix <= 30 )) || return 1
  echo "$(ip::add "${base}" $(( 1 << (31 - prefix) )))/$(( prefix + 1 ))"
}

lo::registry_network() {
  local network
  network=$(registry::network_name)
  local subnet
  subnet=$(registry::network_cidr)

  if docker network inspect "${network}" &>/dev/null; then
    local current_subnet
    current_subnet=$(docker network inspect "${network}" \
      --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}' 2>/dev/null || true)
    if [[ "${current_subnet}" != "${subnet}" ]]; then
      echo "error: registry network '${network}' exists with subnet ${current_subnet}, expected ${subnet}" >&2
      echo "error: run 'lo registry clean --shared' to recreate, or adjust spec.registries.network.subnet" >&2
      return 1
    fi
    # A pre-ip-range network stays usable: lo::registries evicts any node
    # squatting a registry IP and re-attaches it afterwards, which converges
    # to the same layout the reserved range enforces up front.
    return 0
  fi

  local ip_range_args=()
  local dynamic_range
  if dynamic_range=$(lo::registry_dynamic_range "${subnet}"); then
    ip_range_args=(--ip-range "${dynamic_range}")
  fi

  docker network create -d=bridge --subnet "${subnet}" \
    "${ip_range_args[@]}" \
    -o "com.docker.network.bridge.enable_ip_masquerade=true" \
    -o "com.docker.network.bridge.enable_icc=true" \
    --label "lok8s.registry=shared" \
    "${network}"
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

# lo::heal_node_ips <cluster-name> [kubeconfig]
# Repair kind nodes that registered their InternalIP on the WRONG docker network.
#
# Shared-registry clusters are dual-homed: kind creates the node on the cluster
# network ($KIND_EXPERIMENTAL_DOCKER_NETWORK) and
# lo::connect_nodes_to_registry_network attaches a second NIC on the registry
# network. Docker names endpoints by attach order, so when a registry endpoint is
# recreated it can come back as eth0 — and kind's entrypoint, which derives
# --node-ip from the first interface it finds, then pins kubelet to the REGISTRY
# address on the next container restart. That address is dynamically allocated,
# so it also drifts, leaving the node registered on an IP that answers nowhere.
#
# The failure is silent and brutal: the node stays Ready (kubelet dials OUT
# fine), but nothing can reach INTO it — apiserver→kubelet and every cross-node
# pod route break, so any pod on that node becomes unreachable. Observed
# 2026-08-17 on kubehz-dev: cert-manager-webhook lived there, so every
# webhook-validated apply failed and `lo up` died on `networking` + `cnpg-plugin`
# after burning all 6 of bootstrap's CRD/webhook retries — retries that can never
# win, because this is not the transient race they exist for.
#
# So heal it here instead: kubelet's --node-ip must match the node's address on
# the CLUSTER network. Idempotent and silent when everything already agrees.
lo::heal_node_ips() {
  local cluster_name="${1}" kubeconfig="${2:-}"
  local network="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-}"
  [[ -n "${network}" ]] || return 0

  local node want have healed=()
  for node in $(kind get nodes --name "${cluster_name}" 2>/dev/null); do
    # The node's address on the CLUSTER network — the only correct --node-ip.
    want=$(docker inspect "${node}" \
      --format "{{with index .NetworkSettings.Networks \"${network}\"}}{{.IPAddress}}{{end}}" 2>/dev/null || true)
    [[ -n "${want}" ]] || continue

    have=$(docker exec "${node}" cat /var/lib/kubelet/kubeadm-flags.env 2>/dev/null \
      | sed -n 's/.*--node-ip=\([^ "]*\).*/\1/p')
    [[ -n "${have}" ]] || continue
    [[ "${have}" == "${want}" ]] && continue

    # Dual-stack (--node-ip=v4,v6) is a deliberate config, not the drift this
    # heals — a naive rewrite would silently drop the v6 half. Warn, don't touch.
    if [[ "${have}" == *,* ]]; then
      warn "lo: ${node} has a dual-stack --node-ip=${have} — not healing (expected ${want} on ${network})"
      continue
    fi

    warn "lo: ${node} registered --node-ip=${have} (wrong network) — repointing to ${want} on ${network}"
    docker exec "${node}" bash -c "
      sed -i 's|--node-ip=${have}|--node-ip=${want}|' /var/lib/kubelet/kubeadm-flags.env
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
