# shellcheck shell=bash disable=SC2034
# config.sh — Lo driver config readers, validators, and spec-env export
#
# Exported interface (set after lo::read_config):
#   KIND_EXPERIMENTAL_DOCKER_NETWORK  — docker bridge name
#   LOK8S_NETWORK_CIDR                — project /24 subnet
#   LOK8S_NETWORK_SUBNET              — alias for CIDR
#   LOK8S_NETWORK_BASE_IP             — /24 base (e.g. 10.125.130.0)
#   LOK8S_REGISTRY_IP_BUILD / _CACHE / _IO_DOCKER / _IO_QUAY / ...
#   (mirror names are uppercased with hyphens → underscores)
#   LOK8S_REGISTRY_SHARED             — "true"|"false"
#   LOK8S_REGISTRY_NETWORK            — shared registry docker network name
#   LOK8S_REGISTRY_NETWORK_CIDR       — shared registry subnet
#   LOK8S_REGISTRY_NETWORK_SUBNET     — alias
#   LOK8S_CP_COUNT, LOK8S_WORKER_COUNT, LOK8S_HOST_PORTS
#   LOK8S_EXTRA_MOUNTS_COUNT, LOK8S_MAX_CONCURRENT_DOWNLOADS
#   LOK8S_LB_POOL                     — MetalLB IP range
#   LOK8S_REMOTE_MODE, LOK8S_REMOTE_EXPOSE, LOK8S_REMOTE_SYNC_*
#   LOK8S_REMOTE_TILT

# ── Validation ────────────────────────────────────────────

lo::validate_mirror_name() {
  local name="$1"
  if [[ ! "${name}" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
    echo "error: invalid mirror name '${name}': must match ^[a-z0-9][a-z0-9-]*$" >&2
    return 1
  fi
}

lo::validate_ips() {
  local subnet="$1"
  local metallb_pool="${2:-}"
  local errors=0

  local subnet_ip="${subnet%/*}"
  ip::validate_format "${subnet_ip}" || { errors=$(( errors + 1 )); }

  # Validate registry IPs from JSON (single jq call)
  local shared_cidr
  shared_cidr=$(registry::network_cidr)

  _lo_validate_registry_ip() {
    local name="$1" ip="$2" url="$3" domain="$4" host="$5" type="$6"
    local target_subnet="${subnet}"
    if registry::is_shared && [[ "${type}" == "mirror" ]]; then
      target_subnet="${shared_cidr}"
    fi
    if ! ip::validate_format "${ip}"; then
      errors=$(( errors + 1 ))
      return 0
    fi
    if ! ip::validate_in_subnet "${ip}" "${target_subnet}"; then
      echo "error: registry '${name}' IP ${ip} is outside subnet ${target_subnet}" >&2
      errors=$(( errors + 1 ))
    fi
  }
  registry::each _lo_validate_registry_ip

  if [[ -n "${metallb_pool}" ]]; then
    local pool_start="${metallb_pool%-*}"
    local pool_end="${metallb_pool#*-}"
    if ! ip::validate_format "${pool_start}"; then
      errors=$(( errors + 1 ))
    elif ! ip::validate_in_subnet "${pool_start}" "${subnet}"; then
      echo "error: MetalLB pool start ${pool_start} is outside subnet ${subnet}" >&2
      errors=$(( errors + 1 ))
    fi
    if ! ip::validate_format "${pool_end}"; then
      errors=$(( errors + 1 ))
    elif ! ip::validate_in_subnet "${pool_end}" "${subnet}"; then
      echo "error: MetalLB pool end ${pool_end} is outside subnet ${subnet}" >&2
      errors=$(( errors + 1 ))
    fi
    local start_int end_int
    start_int=$(ip::to_int "${pool_start}" 2>/dev/null) || true
    end_int=$(ip::to_int "${pool_end}" 2>/dev/null) || true
    if [[ -n "${start_int}" ]] && [[ -n "${end_int}" ]] && (( start_int > end_int )); then
      echo "error: MetalLB pool start ${pool_start} is greater than end ${pool_end}" >&2
      errors=$(( errors + 1 ))
    fi
  fi

  if (( errors > 0 )); then
    echo "error: ${errors} IP validation error(s). Aborting." >&2
    return 1
  fi
  return 0
}

# ── Slot helper ───────────────────────────────────────────

# Derive a numeric slot from a cluster's spec.cluster.domain.
# Returns empty string for non-*.lok8s.dev domains.
lo::slot_from_domain() {
  local cluster_yaml="${1}"
  local domain
  domain=$(yq -r '.spec.cluster.domain // ""' "${cluster_yaml}")
  [[ -n "${domain}" ]] || { echo ""; return 0; }

  if [[ "${domain}" == "${LO_DEFAULT_DOMAIN}" ]]; then
    echo "${LO_DEFAULT_SLOT}"
    return 0
  elif [[ "${domain}" =~ ^([0-9]+)\.lok8s\.dev$ ]]; then
    local slot="${BASH_REMATCH[1]}"
    if (( slot >= 2 && slot <= 199 )); then
      echo "${slot}"
      return 0
    fi
  fi
  echo ""
}

# ── Config readers ────────────────────────────────────────

lo::read_network_config() {
  local cluster_yaml="$1"

  local net_name net_cidr
  net_name=$(yq -r '.spec.network.name // ""' "${cluster_yaml}")
  net_cidr=$(yq -r '.spec.network.cidr // ""' "${cluster_yaml}")

  if [[ -z "${net_name}" ]] || [[ -z "${net_cidr}" ]]; then
    local slot
    slot=$(lo::slot_from_domain "${cluster_yaml}")
    if [[ -n "${slot}" ]]; then
      if [[ -z "${net_name}" ]]; then
        net_name=$(yq -r '.metadata.name // ""' "${cluster_yaml}")
      fi
      if [[ -z "${net_cidr}" ]]; then
        net_cidr="10.125.${slot}.0/24"
      fi
    fi
  fi

  [[ -n "${net_name}" ]] || { echo "error: spec.network.name is required (no default — metadata.name was also empty)" >&2; return 1; }
  [[ -n "${net_cidr}" ]] || { echo "error: spec.network.cidr is required (e.g. \"10.125.50.0/24\" for slot 50; defaults only apply to *.lok8s.dev domains)" >&2; return 1; }

  LOK8S_NETWORK_BASE_IP="${net_cidr%/*}"

  export KIND_EXPERIMENTAL_DOCKER_NETWORK="${net_name}"
  export LOK8S_NETWORK_CIDR="${net_cidr}"
  export LOK8S_NETWORK_SUBNET="${net_cidr}"
  export LOK8S_NETWORK_BASE_IP

  registry::config_generate "${cluster_yaml}"
}

lo::read_node_config() {
  local cluster_yaml="$1"

  local _default_host_ports="false"
  local _slot
  _slot=$(lo::slot_from_domain "${cluster_yaml}")
  if [[ "${_slot}" == "${LO_DEFAULT_SLOT}" ]]; then
    _default_host_ports="true"
  fi

  local has_nodes
  has_nodes=$(yq -r '.spec.nodes // ""' "${cluster_yaml}")

  if [[ -n "${has_nodes}" ]]; then
    LOK8S_CP_COUNT=$(yq -r '.spec.nodes.controlPlane // 1' "${cluster_yaml}")
    LOK8S_WORKER_COUNT=$(yq -r '.spec.nodes.workers // 0' "${cluster_yaml}")
    local _hp
    _hp=$(yq -r '.spec.nodes.hostPorts' "${cluster_yaml}")
    if [[ "${_hp}" == "null" || -z "${_hp}" ]]; then
      LOK8S_HOST_PORTS="${_default_host_ports}"
    else
      LOK8S_HOST_PORTS="${_hp}"
    fi
  else
    LOK8S_CP_COUNT=1
    LOK8S_WORKER_COUNT=0
    LOK8S_HOST_PORTS="${_default_host_ports}"
  fi

  LOK8S_EXTRA_MOUNTS_COUNT=$(yq -r '.spec.nodes.extraMounts | length // 0' "${cluster_yaml}")

  local _mcd
  _mcd=$(yq -r '.spec.nodes.maxConcurrentDownloads' "${cluster_yaml}")
  if [[ "${_mcd}" == "null" || -z "${_mcd}" ]]; then
    LOK8S_MAX_CONCURRENT_DOWNLOADS=3
  else
    if ! [[ "${_mcd}" =~ ^[1-9][0-9]*$ ]]; then
      echo "error: spec.nodes.maxConcurrentDownloads must be a positive integer, got '${_mcd}'" >&2
      return 1
    fi
    LOK8S_MAX_CONCURRENT_DOWNLOADS="${_mcd}"
  fi

  export LOK8S_CP_COUNT LOK8S_WORKER_COUNT LOK8S_HOST_PORTS LOK8S_EXTRA_MOUNTS_COUNT LOK8S_MAX_CONCURRENT_DOWNLOADS
}

lo::read_lb_config() {
  local cluster_yaml="$1"

  local has_lb
  has_lb=$(yq -r '.spec.loadBalancer // ""' "${cluster_yaml}")

  if [[ -n "${has_lb}" ]]; then
    LOK8S_LB_POOL=$(yq -r '.spec.loadBalancer.pool // ""' "${cluster_yaml}")
  else
    LOK8S_LB_POOL=""
  fi

  if [[ -z "${LOK8S_LB_POOL}" ]]; then
    local _slot
    _slot=$(lo::slot_from_domain "${cluster_yaml}")
    if [[ -n "${_slot}" ]]; then
      LOK8S_LB_POOL="10.125.${_slot}.125-10.125.${_slot}.150"
    fi
  fi

  export LOK8S_LB_POOL
}

lo::read_remote_config() {
  local cluster_yaml="$1"

  LOK8S_REMOTE_MODE=$(yq -r '.spec.remote.mode // "docker"' "${cluster_yaml}")

  local _expose
  _expose=$(yq -r '.spec.remote.expose' "${cluster_yaml}")
  if [[ "${_expose}" == "null" || -z "${_expose}" ]]; then
    if [[ -n "${PROVIDER_NAME:-}" ]]; then
      LOK8S_REMOTE_EXPOSE="true"
    else
      LOK8S_REMOTE_EXPOSE="false"
    fi
  else
    LOK8S_REMOTE_EXPOSE="${_expose}"
  fi

  LOK8S_REMOTE_SYNC_PATH=$(yq -r '.spec.remote.sync.path // "."' "${cluster_yaml}")
  LOK8S_REMOTE_SYNC_DEST=$(yq -r '.spec.remote.sync.dest // "/workspace"' "${cluster_yaml}")

  local -a _default_exclude=(".git" "node_modules" ".secrets" ".kubeconfig" "clusters/.active")
  local _exclude_json
  _exclude_json=$(yq -r '.spec.remote.sync.exclude // "null"' "${cluster_yaml}")
  if [[ "${_exclude_json}" == "null" ]]; then
    LOK8S_REMOTE_SYNC_EXCLUDE=("${_default_exclude[@]}")
  else
    mapfile -t LOK8S_REMOTE_SYNC_EXCLUDE < <(yq -r '.spec.remote.sync.exclude[]?' "${cluster_yaml}")
  fi

  LOK8S_REMOTE_TILT=$(yq -r '.spec.remote.tilt // "true"' "${cluster_yaml}")

  export LOK8S_REMOTE_MODE LOK8S_REMOTE_EXPOSE LOK8S_REMOTE_SYNC_PATH LOK8S_REMOTE_SYNC_DEST LOK8S_REMOTE_TILT
}

# lo::read_config — read all config sections in the correct order.
lo::read_config() {
  local cluster_yaml="$1"
  lo::read_network_config "${cluster_yaml}"   # also calls read_registry_config
  lo::read_node_config "${cluster_yaml}"
  lo::read_lb_config "${cluster_yaml}"
}

# ── Spec env export ───────────────────────────────────────

lo::export_spec_envs() {
  local cluster_yaml="$1"

  LOK8S_SPEC_CLUSTER_NAME=$(yq -r '.metadata.name // ""' "${cluster_yaml}")
  LOK8S_SPEC_CLUSTER_DOMAIN=$(yq -r '.spec.cluster.domain // ""' "${cluster_yaml}")
  LOK8S_SPEC_CLUSTER_NAMESPACE=$(yq -r '.spec.cluster.namespace // "default"' "${cluster_yaml}")
  LOK8S_SPEC_KUBERNETES_VERSION=$(yq -r '.spec.kubernetes.version // ""' "${cluster_yaml}")
  LOK8S_SPEC_DNS_DOMAINFILTER=$(yq -r '.spec.dns.domainFilter // ""' "${cluster_yaml}")
  export LOK8S_SPEC_CLUSTER_NAME LOK8S_SPEC_CLUSTER_DOMAIN
  export LOK8S_SPEC_CLUSTER_NAMESPACE LOK8S_SPEC_KUBERNETES_VERSION
  export LOK8S_SPEC_DNS_DOMAINFILTER

  # spec.oidc — apiserver StructuredAuthenticationConfiguration inputs (consumed
  # by .lok8s/utils/oidc.sh → oidc::render_auth_config and the kind render).
  # Absent spec.oidc ⇒ ISSUER/CLIENTID empty ⇒ oidc::enabled false ⇒ NO apiserver
  # OIDC wiring (strict back-compat: the rendered kind config is unchanged).
  # Defaults mirror the schema doc in utils/oidc.sh.
  LOK8S_SPEC_OIDC_ISSUER=$(yq -r '.spec.oidc.issuer // ""' "${cluster_yaml}")
  LOK8S_SPEC_OIDC_CLIENTID=$(yq -r '.spec.oidc.clientID // ""' "${cluster_yaml}")
  LOK8S_SPEC_OIDC_USERNAMECLAIM=$(yq -r '.spec.oidc.usernameClaim // "sub"' "${cluster_yaml}")
  LOK8S_SPEC_OIDC_USERNAMEPREFIX=$(yq -r '.spec.oidc.usernamePrefix // "oidc:"' "${cluster_yaml}")
  LOK8S_SPEC_OIDC_GROUPSCLAIM=$(yq -r '.spec.oidc.groupsClaim // "groups"' "${cluster_yaml}")
  LOK8S_SPEC_OIDC_GROUPSPREFIX=$(yq -r '.spec.oidc.groupsPrefix // "oidc:"' "${cluster_yaml}")
  LOK8S_SPEC_OIDC_CABUNDLE=$(yq -r '.spec.oidc.caBundle // ""' "${cluster_yaml}")
  export LOK8S_SPEC_OIDC_ISSUER LOK8S_SPEC_OIDC_CLIENTID
  export LOK8S_SPEC_OIDC_USERNAMECLAIM LOK8S_SPEC_OIDC_USERNAMEPREFIX
  export LOK8S_SPEC_OIDC_GROUPSCLAIM LOK8S_SPEC_OIDC_GROUPSPREFIX
  export LOK8S_SPEC_OIDC_CABUNDLE

  export LOK8S_SPEC_NETWORK_NAME="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-}"
  export LOK8S_SPEC_NETWORK_SUBNET="${LOK8S_NETWORK_SUBNET:-}"
  export LOK8S_SPEC_NETWORK_BASE_IP="${LOK8S_NETWORK_BASE_IP:-}"

  LOK8S_SPEC_REGISTRY_PREFIX=$(yq -r '.spec.registries.prefix // "lok8s.local"' "${cluster_yaml}")
  LOK8S_SPEC_REGISTRY_BUILD_HOST=$(registry::get build host)
  LOK8S_SPEC_REGISTRY_CACHE_HOST=$(registry::get cache host)
  export LOK8S_SPEC_REGISTRY_PREFIX LOK8S_SPEC_REGISTRY_BUILD_HOST LOK8S_SPEC_REGISTRY_CACHE_HOST
  export LOK8S_SPEC_REGISTRY_BUILD_IP="${LOK8S_REGISTRY_IP_BUILD:-}"
  export LOK8S_SPEC_REGISTRY_CACHE_IP="${LOK8S_REGISTRY_IP_CACHE:-}"

  export LOK8S_SPEC_LOADBALANCER_POOL="${LOK8S_LB_POOL:-}"
  if [[ -n "${LOK8S_LB_POOL:-}" && "${LOK8S_LB_POOL}" == *-* ]]; then
    export LOK8S_SPEC_LOADBALANCER_POOL_START="${LOK8S_LB_POOL%-*}"
    export LOK8S_SPEC_LOADBALANCER_POOL_END="${LOK8S_LB_POOL#*-}"
  else
    export LOK8S_SPEC_LOADBALANCER_POOL_START=""
    export LOK8S_SPEC_LOADBALANCER_POOL_END=""
  fi

  export LOK8S_SPEC_KIND_PODSUBNET="${LO_DEFAULT_POD_CIDR}"
  export LOK8S_SPEC_KIND_SERVICESUBNET="${LO_DEFAULT_SVC_CIDR}"
}
