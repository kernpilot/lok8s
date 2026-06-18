# shellcheck shell=bash
# config-registry.sh — Registry config: JSON generation + query helpers
#
# Generates .registries.json for the active cluster, then provides
# helpers to iterate and query it. No global associative arrays.
#
# JSON schema:
#   {
#     "shared": true,
#     "tls": false,
#     "port": 80,
#     "network": { "name": "lok8s-registries", "cidr": "10.125.200.0/24" },
#     "project_network": "local",
#     "registries": [
#       { "name": "build", "ip": "10.125.125.101", "url": "", "domain": "", "host": "lok8s.local", "type": "framework" },
#       ...
#     ]
#   }
#
# tls:  when true, registries serve HTTPS with a mkcert-signed cert
#       (.secrets/tls/registries/) and clients trust them via the mkcert
#       root CA — no `insecure-registries` daemon config required.
# port: listen/connect port. 80 in plain-HTTP mode, 443 in TLS mode (so a
#       bare-IP `docker push` reaches the registry on the HTTPS default port).

# Static mappings — baked into each registry entry at generation time

# Path to the generated JSON. Set by registry::config_generate.
LOK8S_REGISTRY_JSON=""

# ── Generate ──────────────────────────────────────────────

# Generate .registries.json for a cluster.
# Sets: LOK8S_REGISTRY_JSON (path to the file)
# Exports: LOK8S_REGISTRY_SHARED, LOK8S_REGISTRY_NETWORK, LOK8S_REGISTRY_NETWORK_CIDR
#          LOK8S_REGISTRY_IP_BUILD / _CACHE (framework) and
#          LOK8S_REGISTRY_IP_<MIRROR_NAME> (mirrors — uppercased,
#          hyphens converted to underscores, e.g. io-docker →
#          LOK8S_REGISTRY_IP_IO_DOCKER)
registry::config_generate() {
  local cluster_yaml="$1"
  local domain_dir
  domain_dir=$(dirname "${cluster_yaml}")

  local project_subnet="${LOK8S_NETWORK_BASE_IP:-}"
  [[ -n "${project_subnet}" ]] || {
    echo "error: lo::read_network_config must run before registry::config_generate" >&2
    return 1
  }

  # Read shared settings from spec
  local shared_enabled
  shared_enabled=$(yq -r '.spec.registries.shared.enabled' "${cluster_yaml}")
  [[ "${shared_enabled}" != "null" && -n "${shared_enabled}" ]] || shared_enabled="true"

  # TLS mode (default false for back-compat). When true, registries serve
  # HTTPS on :443 with a mkcert cert; clients trust them via the mkcert CA.
  local tls_enabled
  tls_enabled=$(yq -r '.spec.registries.tls' "${cluster_yaml}")
  case "${tls_enabled}" in
    true)  tls_enabled="true" ;;
    false|null|"") tls_enabled="false" ;;
    *)
      echo "error: spec.registries.tls must be true or false, got '${tls_enabled}'" >&2
      return 1
      ;;
  esac

  # Listen/connect port is TLS-mode-dependent (see defaults.sh).
  local reg_port="${LO_REGISTRY_PORT}"
  [[ "${tls_enabled}" == "true" ]] && reg_port="${LO_REGISTRY_PORT_TLS}"

  local net_name net_cidr
  net_name=$(yq -r ".spec.registries.shared.network.name // \"${LO_SHARED_REGISTRY_NETWORK}\"" "${cluster_yaml}")
  net_cidr=$(yq -r ".spec.registries.shared.network.cidr // \"${LO_SHARED_REGISTRY_CIDR}\"" "${cluster_yaml}")

  local shared_base="${net_cidr%/*}"
  local project_network="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-lok8s}"

  # Build the registries array as JSON via jq
  local registries="[]"

  # Framework-private registries (always on project subnet)
  local build_ip cache_ip
  build_ip=$(ip::add "${project_subnet}" "${LO_REGISTRY_OFFSET_BUILD}")
  cache_ip=$(ip::add "${project_subnet}" "${LO_REGISTRY_OFFSET_CACHE}")

  registries=$(jq -n \
    --arg build_ip "${build_ip}" \
    --arg cache_ip "${cache_ip}" \
    '[
      { name: "build", ip: $build_ip, url: "", domain: "", host: "lok8s.local", type: "framework" },
      { name: "cache", ip: $cache_ip, url: "", domain: "", host: "lok8s.cache", type: "framework" }
    ]')

  # Collect mirrors (user-defined or defaults)
  local mirror_count
  mirror_count=$(yq -r '.spec.registries.mirrors | length // 0' "${cluster_yaml}")

  local -a m_names=() m_urls=()
  if (( mirror_count == 0 )); then
    m_names=("io-docker" "io-quay" "io-k8s" "io-ghcr")
    m_urls=("https://registry-1.docker.io" "https://quay.io" "https://registry.k8s.io" "https://ghcr.io")
  else
    local i
    for (( i = 0; i < mirror_count; i++ )); do
      local _n _u
      _n=$(yq -r ".spec.registries.mirrors[${i}].name" "${cluster_yaml}")
      _u=$(yq -r ".spec.registries.mirrors[${i}].url // \"\"" "${cluster_yaml}")

      lo::validate_mirror_name "${_n}" || return 1
      if [[ "${_n}" == "build" || "${_n}" == "cache" ]]; then
        echo "error: spec.registries.mirrors: '${_n}' is reserved for the framework" >&2
        return 1
      fi
      [[ -n "${_u}" ]] || {
        echo "error: spec.registries.mirrors[${i}] (${_n}): url is required" >&2
        return 1
      }

      m_names+=("${_n}")
      m_urls+=("${_u}")
    done
  fi

  # Allocate IPs and add mirror entries
  local idx
  for (( idx = 0; idx < ${#m_names[@]}; idx++ )); do
    local name="${m_names[${idx}]}"
    local url="${m_urls[${idx}]}"
    local mirror_ip

    if [[ "${shared_enabled}" == "true" ]]; then
      mirror_ip=$(ip::add "${shared_base}" $(( idx + 2 )))
    else
      mirror_ip=$(ip::add "${project_subnet}" $(( LO_REGISTRY_OFFSET_CACHE + idx + 1 )))
    fi

    # Resolve the containerd-facing domain. For standard mirrors the
    # upstream domain differs from the registry API hostname
    # (e.g. docker.io pulls go to registry-1.docker.io).
    local domain=""
    case "${name}" in
      io-docker) domain="docker.io" ;;
      io-quay)   domain="quay.io" ;;
      io-k8s)    domain="registry.k8s.io" ;;
      io-ghcr)   domain="ghcr.io" ;;
      *)
        domain="${url#https://}"
        domain="${domain#http://}"
        domain="${domain%%/*}"
        ;;
    esac

    registries=$(echo "${registries}" | jq \
      --arg name "${name}" \
      --arg ip "${mirror_ip}" \
      --arg url "${url}" \
      --arg domain "${domain}" \
      '. + [{ name: $name, ip: $ip, url: $url, domain: $domain, host: "", type: "mirror" }]')
  done

  # Write the full JSON
  LOK8S_REGISTRY_JSON="${domain_dir}/.registries.json"
  jq -n \
    --argjson shared "$(jq -n --arg v "${shared_enabled}" 'if $v == "true" then true else false end')" \
    --argjson tls "$(jq -n --arg v "${tls_enabled}" 'if $v == "true" then true else false end')" \
    --argjson port "${reg_port}" \
    --arg net_name "${net_name}" \
    --arg net_cidr "${net_cidr}" \
    --arg project_network "${project_network}" \
    --argjson registries "${registries}" \
    '{
      shared: $shared,
      tls: $tls,
      port: $port,
      network: { name: $net_name, cidr: $net_cidr },
      project_network: $project_network,
      registries: $registries
    }' > "${LOK8S_REGISTRY_JSON}"

  # Export scalars for external consumers (libs/image, Tilt, tests)
  export LOK8S_REGISTRY_JSON
  export LOK8S_REGISTRY_SHARED="${shared_enabled}"
  export LOK8S_REGISTRY_TLS="${tls_enabled}"
  export LOK8S_REGISTRY_PORT="${reg_port}"
  export LOK8S_REGISTRY_NETWORK="${net_name}"
  export LOK8S_REGISTRY_NETWORK_CIDR="${net_cidr}"
  export LOK8S_REGISTRY_IP_BUILD="${build_ip}"
  export LOK8S_REGISTRY_IP_CACHE="${cache_ip}"

  # Mirror IP exports (read back from JSON — single jq call)
  local _exports
  _exports=$(jq -r '.registries[] | select(.type == "mirror") |
    "export LOK8S_REGISTRY_IP_" + (.name | gsub("-";"_") | ascii_upcase) + "=" + .ip' \
    "${LOK8S_REGISTRY_JSON}")
  eval "${_exports}"
}

# ── Query helpers ─────────────────────────────────────────

# Iterate all registries. Calls a callback with tab-separated fields.
# Usage: registry::each <callback>
# Callback receives: name ip url domain host type
# Single jq fork for the entire iteration.
registry::each() {
  local callback="$1"
  local json="${LOK8S_REGISTRY_JSON:-}"
  [[ -f "${json}" ]] || { echo "error: .registries.json not found (run registry::config_generate first)" >&2; return 1; }

  # locals: bash dynamic scoping — without these, `read` clobbers the CALLER's
  # name/ip/url/domain/host/type (e.g. driver::provision's `domain` went empty
  # after lo::validate_ips → registry::each, breaking every later step).
  local name ip url domain host type
  while IFS=$'\t' read -r name ip url domain host type; do
    # Restore empty strings from sentinel
    [[ "${url}" != "-" ]] || url=""
    [[ "${domain}" != "-" ]] || domain=""
    [[ "${host}" != "-" ]] || host=""
    "${callback}" "${name}" "${ip}" "${url}" "${domain}" "${host}" "${type}"
  done < <(jq -r '.registries[] | [.name, .ip, (if .url == "" then "-" else .url end), (if .domain == "" then "-" else .domain end), (if .host == "" then "-" else .host end), .type] | @tsv' "${json}")
}

# Get a single field for a named registry.
# Usage: registry::get <name> <field>
registry::get() {
  local name="$1" field="$2"
  jq -r --arg n "${name}" --arg f "${field}" \
    '.registries[] | select(.name == $n) | .[$f] // ""' \
    "${LOK8S_REGISTRY_JSON}"
}

# Check if shared mode is enabled (reads from JSON, no globals).
registry::is_shared() {
  jq -e '.shared' "${LOK8S_REGISTRY_JSON}" > /dev/null 2>&1
}

# Check if TLS mode is enabled (reads from JSON, no globals).
registry::is_tls() {
  jq -e '.tls' "${LOK8S_REGISTRY_JSON}" > /dev/null 2>&1
}

# Get the registry listen/connect port (80 plain, 443 TLS).
registry::port() {
  jq -r '.port // 80' "${LOK8S_REGISTRY_JSON}"
}

# Build the client-facing URL for a registry IP, honoring TLS mode.
# In TLS mode the port (443) is implicit for both http(s) clients and
# containerd, so it is omitted to match how `docker push <ip>/...`
# addresses the registry. In plain mode :80 is likewise implicit.
# Usage: registry::url <ip>
registry::url() {
  local ip="$1"
  if registry::is_tls; then
    echo "https://${ip}"
  else
    echo "http://${ip}"
  fi
}

# Get the shared registry network name.
registry::network_name() {
  jq -r '.network.name' "${LOK8S_REGISTRY_JSON}"
}

# Get the shared registry network CIDR.
registry::network_cidr() {
  jq -r '.network.cidr' "${LOK8S_REGISTRY_JSON}"
}

# Get the project Docker network name.
registry::project_network() {
  jq -r '.project_network' "${LOK8S_REGISTRY_JSON}"
}

# Resolve the Docker container name + network for a registry.
# Usage: registry::container <name>
# Outputs: container_name\tnetwork_name
registry::container() {
  local name="$1"
  local json="${LOK8S_REGISTRY_JSON}"

  jq -r --arg n "${name}" '
    (.registries[] | select(.name == $n)) as $r |
    if .shared and $r.type == "mirror"
    then ["lok8s-registry-" + $n, .network.name]
    else [.project_network + "-registry-" + $n, .project_network]
    end | @tsv
  ' "${json}"
}
