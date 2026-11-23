# shellcheck shell=bash disable=SC2016
# SC2016: single-quoted $ here is jq/yq/awk program text or a remote-script literal
# expose.sh — Nginx reverse proxy for remote cluster access
#
# nginx.conf serves HTTPS only, so the proxy needs a TLS certificate and key.
# Without both files, lo::expose does not start the proxy: it warns and
# returns 0. Each docker step that sets up the proxy (the run, each copy and
# the reload) must succeed. A failed step removes the proxy container and
# stops lo::expose with an error that names the step and the container, and
# lo::expose does not print "cluster exposed". The callers run lo::expose
# under `|| return 1`, which turns errexit off in the function, so each step
# checks its own status. lo::expose sets LO_EXPOSE_STARTED to true only when
# the proxy runs (lo::remote_ci reads it). Go twin:
# internal/driver/lo/expose.go.

# lo::expose_step_failed reports a failed docker step of lo::expose: the
# step, its exit status and the next action (Go: exposeStepFailed). First it
# removes the proxy container (the status of that removal does not count): a
# container that a failed step left would restart always with a config that
# is not complete.
lo::expose_step_failed() {
  local rc="${1}" step="${2}" proxy_name="${3}"
  docker rm -f "${proxy_name}" >/dev/null 2>&1 || true
  error "expose: ${step} failed (exit status ${rc}), so the proxy does not run. Fix the cause in the docker message above, then run the command again."
}

lo::expose() {
  local cluster_name="${1}" cluster_yaml="${2}"
  # shellcheck disable=SC2034  # read by lo::remote_ci (remote.sh)
  LO_EXPOSE_STARTED=false
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

  # The TLS certificate and key of the proxy. The driver does not make them:
  # put a tls.crt and a tls.key here (for example from a cert: Secret, or
  # with mkcert). nginx.conf serves HTTPS only, so without both files the
  # proxy does not start.
  local cert_path="${PATH_BASE}/.secrets/tls/tls.crt"
  local key_path="${PATH_BASE}/.secrets/tls/tls.key"

  local nginx_template="${PATH_LOK8S}/drivers/lo/cluster/expose/nginx.conf"
  if [[ ! -f "${nginx_template}" ]]; then
    error "expose: nginx template not found at ${nginx_template}"
    return 1
  fi

  # Remove the proxy of an earlier run first, also when this run starts no
  # proxy, so that no stale proxy stays.
  docker rm -f "${proxy_name}" 2>/dev/null || true

  if [[ ! -f "${cert_path}" ]] || [[ ! -f "${key_path}" ]]; then
    warn "expose: no TLS certificate and key at ${cert_path} and ${key_path}, so the proxy is not started. Add both files, or set spec.remote.expose: false."
    return 0
  fi

  # Render nginx config to a local temp file
  local nginx_conf_file
  nginx_conf_file=$(mktemp /tmp/lok8s-nginx.XXXXXX.conf)
  LOK8S_EXPOSE_DOMAIN="${domain}" LOK8S_EXPOSE_BACKEND_IP="${backend_ip}" \
    envsubst '${LOK8S_EXPOSE_DOMAIN} ${LOK8S_EXPOSE_BACKEND_IP}' \
    < "${nginx_template}" > "${nginx_conf_file}"

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
    lo::expose_step_failed "$?" "docker run of the proxy container ${proxy_name}" "${proxy_name}"
    rm -f "${nginx_conf_file}"
    return 1
  }

  # Copy the rendered config and the TLS files into the running container.
  # The ssl_certificate and ssl_certificate_key paths of nginx.conf are the
  # targets of the two TLS copies.
  local rc=0
  docker cp "${nginx_conf_file}" "${proxy_name}:/etc/nginx/nginx.conf" || rc=$?
  rm -f "${nginx_conf_file}"
  if (( rc != 0 )); then
    lo::expose_step_failed "${rc}" "docker cp of the nginx config to ${proxy_name}:/etc/nginx/nginx.conf" "${proxy_name}"
    return 1
  fi
  docker cp "${cert_path}" "${proxy_name}:/tls.crt" || {
    lo::expose_step_failed "$?" "docker cp of the TLS certificate to ${proxy_name}:/tls.crt" "${proxy_name}"
    return 1
  }
  docker cp "${key_path}" "${proxy_name}:/tls.key" || {
    lo::expose_step_failed "$?" "docker cp of the TLS key to ${proxy_name}:/tls.key" "${proxy_name}"
    return 1
  }

  # Reload nginx with the new config
  docker exec "${proxy_name}" nginx -s reload || {
    lo::expose_step_failed "$?" "docker exec ${proxy_name} nginx -s reload" "${proxy_name}"
    return 1
  }

  local access_ip="${LOK8S_REMOTE_IP:-localhost}"
  debug "expose: nginx proxy ${proxy_name} running on ${access_ip}:443 → ${backend_ip}"
  # shellcheck disable=SC2034  # read by lo::remote_ci (remote.sh)
  LO_EXPOSE_STARTED=true
  echo ":: cluster exposed at https://*.${domain} (via ${access_ip}:443)"
}
