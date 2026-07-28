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
