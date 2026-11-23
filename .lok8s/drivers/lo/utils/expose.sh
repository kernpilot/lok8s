# shellcheck shell=bash disable=SC2016
# SC2016: single-quoted $ here is jq/yq/awk program text or a remote-script literal
# expose.sh — Nginx reverse proxy for remote cluster access
#
# Each docker step that sets up the proxy (the run, each copy and the reload)
# must succeed. A failed step stops lo::expose with an error that names the
# step and the container, and lo::expose does not print "cluster exposed".
# The callers run lo::expose under `|| return 1`, which turns errexit off in
# the function, so each step checks its own status. Go twin:
# internal/driver/lo/expose.go.

# lo::expose_step_failed reports a failed docker step of lo::expose: the
# step, its exit status and the next action (Go: exposeStepFailed).
lo::expose_step_failed() {
  local rc="${1}" step="${2}"
  error "expose: ${step} failed (exit status ${rc}). The proxy does not serve the cluster. Fix the cause in the docker message above, then run lo up again."
}

lo::expose() {
  local cluster_name="${1}" cluster_yaml="${2}"
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

  # Optional TLS for the proxy. The driver no longer mints this cert; drop a
  # tls.crt/tls.key here (e.g. extracted from a cert: Secret, or via mkcert) to
  # serve HTTPS. Absent → the proxy runs plain HTTP (warning below).
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
  # The image is LO_EXPOSE_IMAGE (defaults.sh), pinned by tag and digest.
  local -a docker_args=(
    run -d --restart=always
    --name "${proxy_name}"
    --network "${network}"
    -p 80:80 -p 443:443
  )
  docker "${docker_args[@]}" "${LO_EXPOSE_IMAGE}" || {
    lo::expose_step_failed "$?" "docker run of the proxy container ${proxy_name}"
    rm -f "${nginx_conf_file}"
    return 1
  }

  # Copy rendered config + optional TLS certs into the running container.
  # The ssl_certificate and ssl_certificate_key paths of nginx.conf are the
  # targets of the two TLS copies.
  local rc=0
  docker cp "${nginx_conf_file}" "${proxy_name}:/etc/nginx/nginx.conf" || rc=$?
  rm -f "${nginx_conf_file}"
  if (( rc != 0 )); then
    lo::expose_step_failed "${rc}" "docker cp of the nginx config to ${proxy_name}:/etc/nginx/nginx.conf"
    return 1
  fi

  if [[ -f "${cert_path}" ]] && [[ -f "${key_path}" ]]; then
    docker cp "${cert_path}" "${proxy_name}:/tls.crt" || {
      lo::expose_step_failed "$?" "docker cp of the TLS certificate to ${proxy_name}:/tls.crt"
      return 1
    }
    docker cp "${key_path}" "${proxy_name}:/tls.key" || {
      lo::expose_step_failed "$?" "docker cp of the TLS key to ${proxy_name}:/tls.key"
      return 1
    }
  else
    warn "expose: TLS certs not found at ${cert_path} — proxy will run without TLS"
  fi

  # Reload nginx with the new config
  docker exec "${proxy_name}" nginx -s reload || {
    lo::expose_step_failed "$?" "docker exec ${proxy_name} nginx -s reload"
    return 1
  }

  local access_ip="${LOK8S_REMOTE_IP:-localhost}"
  debug "expose: nginx proxy ${proxy_name} running on ${access_ip}:443 → ${backend_ip}"
  echo ":: cluster exposed at https://*.${domain} (via ${access_ip}:443)"
}
