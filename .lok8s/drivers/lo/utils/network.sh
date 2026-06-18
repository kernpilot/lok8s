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
    return 0
  fi

  docker network create -d=bridge --subnet "${subnet}" \
    -o "com.docker.network.bridge.enable_ip_masquerade=true" \
    -o "com.docker.network.bridge.enable_icc=true" \
    --label "lok8s.registry=shared" \
    "${network}"
}

lo::connect_nodes_to_registry_network() {
  local cluster_name="$1"
  local registry_network
  registry_network=$(registry::network_name)

  for node in $(kind get nodes --name "${cluster_name}" 2>/dev/null); do
    docker network connect "${registry_network}" "${node}" 2>/dev/null || true
  done
}
