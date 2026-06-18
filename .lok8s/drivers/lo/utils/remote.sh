# shellcheck shell=bash
# remote.sh — Remote VM provisioning + CI mode

# Wait for SSH, cloud-init, and Docker on a freshly provisioned VM.
# Sets: LOK8S_REMOTE_IP, LOK8S_REMOTE_USER, DOCKER_HOST
lo::provision_remote() {
  local domain="$1" cluster_yaml="$2"
  local work_dir="${PATH_CLUSTERS}/${domain}/.provider"
  mkdir -p "${work_dir}"

  provider::provision "${PROVIDER_CONFIG_FILE}" "${work_dir}"

  local provider_output
  provider_output=$(provider::output "${PROVIDER_CONFIG_FILE}")

  local remote_ip remote_user
  remote_ip=$(echo "${provider_output}" | jq -r '.nodes[0].public_ip // empty')
  remote_user=$(echo "${provider_output}" | jq -r '.nodes[0].ssh_user // "root"')

  if [[ -z "${remote_ip}" ]]; then
    warn "provider loaded but no nodes in output — running kind locally"
    return 0
  fi

  # Wait for SSH
  debug "waiting for SSH on ${remote_ip}..."
  local _attempts=0 _ssh_ok=0
  while (( _attempts < 30 )); do
    if ssh -o ConnectTimeout=5 -o BatchMode=yes "${remote_user}@${remote_ip}" true 2>/dev/null; then
      _ssh_ok=1
      debug "SSH ready on ${remote_ip} (after $(( _attempts * 2 ))s)"
      break
    fi
    _attempts=$(( _attempts + 1 ))
    sleep 2
  done
  if (( ! _ssh_ok )); then
    error "SSH not reachable on ${remote_ip} after 60s"
    return 1
  fi

  # Wait for cloud-init
  debug "waiting for cloud-init to finish on ${remote_ip}..."
  local _ci_done=0
  _attempts=0
  while (( _attempts < 90 )); do
    if ssh "${remote_user}@${remote_ip}" 'test -f /var/lib/cloud/instance/boot-finished' 2>/dev/null; then
      _ci_done=1
      debug "cloud-init finished on ${remote_ip} (after $(( _attempts * 3 ))s)"
      break
    fi
    _attempts=$(( _attempts + 1 ))
    sleep 3
  done
  if (( ! _ci_done )); then
    warn "cloud-init did not finish within 270s — proceeding anyway"
  fi

  # Wait for Docker
  debug "waiting for Docker on ${remote_ip}..."
  local _docker_ok=0
  _attempts=0
  while (( _attempts < 60 )); do
    if ssh "${remote_user}@${remote_ip}" 'command -v docker && docker info' &>/dev/null; then
      _docker_ok=1
      debug "Docker ready on ${remote_ip} (after $(( _attempts * 3 ))s)"
      break
    fi
    _attempts=$(( _attempts + 1 ))
    sleep 3
  done
  if (( ! _docker_ok )); then
    error "Docker not available on ${remote_ip} after 180s. Check cloud-init logs: ssh ${remote_user}@${remote_ip} cat /var/log/cloud-init-output.log"
    return 1
  fi

  export LOK8S_REMOTE_IP="${remote_ip}"
  export LOK8S_REMOTE_USER="${remote_user}"

  export DOCKER_HOST="ssh://${remote_user}@${remote_ip}"
  debug "remote Docker: DOCKER_HOST=${DOCKER_HOST}"

  # Verify Docker is reachable
  local _dh_ok=0
  for (( _attempts=0; _attempts < 10; _attempts++ )); do
    if docker info &>/dev/null; then
      _dh_ok=1
      debug "DOCKER_HOST verified (attempt ${_attempts})"
      break
    fi
    sleep 3
  done
  if (( ! _dh_ok )); then
    error "Docker not reachable via DOCKER_HOST=${DOCKER_HOST}"
    return 1
  fi
}

# CI mode: sync repo to remote VM, run lo provision remotely, set up expose + tunnel
lo::remote_ci() {
  local domain="$1" cluster_yaml="$2"
  local remote="${LOK8S_REMOTE_USER}@${LOK8S_REMOTE_IP}"
  local dest="${LOK8S_REMOTE_SYNC_DEST}"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "${cluster_yaml}")

  debug "CI mode: syncing repo to ${remote}:${dest}"

  ssh "${remote}" "mkdir -p '${dest}'" || {
    error "failed to create ${dest} on ${remote}"
    return 1
  }

  local -a rsync_args=(-az --delete --info=progress2)
  local excl
  for excl in "${LOK8S_REMOTE_SYNC_EXCLUDE[@]}"; do
    rsync_args+=(--exclude="${excl}")
  done

  local repo_root
  repo_root=$(git -C "${PATH_BASE}" rev-parse --show-toplevel 2>/dev/null || echo "${PATH_BASE}")
  local sync_src="${LOK8S_REMOTE_SYNC_PATH}"
  if [[ "${sync_src}" != /* ]]; then
    sync_src="${repo_root}/${sync_src}"
  fi
  [[ "${sync_src}" == */ ]] || sync_src="${sync_src}/"

  rsync "${rsync_args[@]}" "${sync_src}" "${remote}:${dest}/" || {
    error "rsync failed"
    return 1
  }

  if [[ "${PATH_CLUSTERS}" != "${repo_root}/clusters" ]] && [[ -d "${PATH_CLUSTERS}" ]]; then
    rsync -az "${PATH_CLUSTERS}/" "${remote}:${dest}/clusters/" || true
  fi

  debug "repo synced to ${remote}:${dest}"

  # Run lo provision on the remote VM (without --remote)
  debug "starting lo provision on ${remote}"
  ssh "${remote}" "cd '${dest}' && \
    export DOMAIN_NAME='${domain}' && \
    export PATH_BASE='${dest}' && \
    export PATH_LOK8S='${dest}/.lok8s' && \
    export PATH_CLUSTERS='${dest}/clusters' && \
    export PATH_BIN='${dest}/.bin' && \
    export KUSTOMIZE_PLUGIN_HOME='${dest}/.kustomize' && \
    export PATH=\"${dest}/.lok8s:${dest}/.bin:\${PATH}\" && \
    .lok8s/lo provision --domain '${domain}'" || {
      error "remote lo provision failed"
      return 1
    }

  # Start Tilt if enabled
  if [[ "${LOK8S_REMOTE_TILT}" == "true" ]]; then
    debug "starting Tilt on ${remote}"
    ssh "${remote}" "cd '${dest}' && \
      export DOMAIN_NAME='${domain}' && \
      export PATH_BASE='${dest}' && \
      export PATH_LOK8S='${dest}/.lok8s' && \
      export PATH_CLUSTERS='${dest}/clusters' && \
      nohup .lok8s/lo tilt up > /tmp/lok8s-tilt.log 2>&1 &" || {
        warn "remote Tilt start failed — cluster is provisioned but Tilt isn't running"
      }
    debug "Tilt started on ${remote} (log: /tmp/lok8s-tilt.log)"
  fi

  # Expose if enabled
  if [[ "${LOK8S_REMOTE_EXPOSE}" == "true" ]]; then
    lo::read_network_config "${cluster_yaml}"
    lo::read_lb_config "${cluster_yaml}"
    DOCKER_HOST="ssh://${remote}" lo::expose "${cluster_name}" "${cluster_yaml}"
  fi

  # Set up kubeconfig + SSH tunnel
  local kubeconfig_path="${PATH_BASE}/.kubeconfig/${cluster_name}.yaml"
  if [[ ! -f "${kubeconfig_path}" ]]; then
    mkdir -p "${PATH_BASE}/.kubeconfig"
    scp "${remote}:${dest}/.kubeconfig/${cluster_name}.yaml" \
      "${kubeconfig_path}" 2>/dev/null || true
  fi

  if [[ -f "${kubeconfig_path}" ]]; then
    lo::kubeconfig_tunnel "${kubeconfig_path}" "${LOK8S_REMOTE_USER}" "${LOK8S_REMOTE_IP}"
  fi

  local access_ip="${LOK8S_REMOTE_IP}"
  echo ":: remote CI cluster ready"
  echo "   VM:         ${access_ip}"
  echo "   SSH:        ssh ${remote}"
  echo "   kubectl:    KUBECONFIG=${kubeconfig_path} kubectl get nodes"
  if [[ "${LOK8S_REMOTE_EXPOSE}" == "true" ]]; then
    echo "   URL:        https://*.${domain} (via ${access_ip}:443)"
  fi
  if [[ "${LOK8S_REMOTE_TILT}" == "true" ]]; then
    echo "   Tilt log:   ssh ${remote} tail -f /tmp/lok8s-tilt.log"
  fi
  echo "   Sync:       rsync -az ${sync_src} ${remote}:${dest}/"
}
