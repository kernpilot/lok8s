# shellcheck shell=bash
# expose.sh — Nginx reverse proxy for remote cluster access

lo::expose() {
  local cluster_name="$1" cluster_yaml="$2"
  local domain
  domain=$(yq -r '.spec.cluster.domain' "${cluster_yaml}")

  local proxy_name="${cluster_name}-proxy"
  local network="${KIND_EXPERIMENTAL_DOCKER_NETWORK}"

  local backend_ip
  if [[ -n "${LOK8S_LB_POOL:-}" ]]; then
    backend_ip="${LOK8S_LB_POOL%%-*}"
  else
    backend_ip="${LOK8S_NETWORK_BASE_IP:-127.0.0.1}"
  fi

  # TLS cert paths (written by lo::mkcert)
  local cert_path="${PATH_BASE}/.secrets/tls/tls.crt"
  local key_path="${PATH_BASE}/.secrets/tls/tls.key"

  local nginx_template="${PATH_LOK8S}/drivers/lo/cluster/expose/nginx.conf"
  if [[ ! -f "${nginx_template}" ]]; then
    error "expose: nginx template not found at ${nginx_template}"
    return 1
  fi

  # Render nginx config to a local temp file
  local nginx_conf_file
  nginx_conf_file=$(mktemp /tmp/lok8s-nginx.XXXXXX.conf)
  LOK8S_EXPOSE_DOMAIN="${domain}" LOK8S_EXPOSE_BACKEND_IP="${backend_ip}" \
    envsubst '${LOK8S_EXPOSE_DOMAIN} ${LOK8S_EXPOSE_BACKEND_IP}' \
    < "${nginx_template}" > "${nginx_conf_file}"

  docker rm -f "${proxy_name}" 2>/dev/null || true

  # Start container with default nginx config, then overwrite it.
  # docker cp streams file content over the Docker connection (works
  # with DOCKER_HOST=ssh:// where -v mounts can't reach local paths).
  local -a docker_args=(
    run -d --restart=always
    --name "${proxy_name}"
    --network "${network}"
    -p 80:80 -p 443:443
  )
  docker "${docker_args[@]}" nginx:alpine

  # Copy rendered config + optional TLS certs into the running container
  docker cp "${nginx_conf_file}" "${proxy_name}:/etc/nginx/nginx.conf"
  rm -f "${nginx_conf_file}"

  if [[ -f "${cert_path}" ]] && [[ -f "${key_path}" ]]; then
    docker cp "${cert_path}" "${proxy_name}:/tls.crt"
    docker cp "${key_path}" "${proxy_name}:/tls.key"
  else
    warn "expose: TLS certs not found at ${cert_path} — proxy will run without TLS"
  fi

  # Reload nginx with the new config
  docker exec "${proxy_name}" nginx -s reload

  local access_ip="${LOK8S_REMOTE_IP:-localhost}"
  debug "expose: nginx proxy ${proxy_name} running on ${access_ip}:443 → ${backend_ip}"
  echo ":: cluster exposed at https://*.${domain} (via ${access_ip}:443)"
}
