# shellcheck shell=bash disable=SC2034
# render.sh — Kind config rendering (YAML/TOML generation)

lo::render_kind_config() {
  local cluster_name="$1" k8s_version="$2" network="$3" cluster_yaml="${4:-}"

  local nodes_yaml
  nodes_yaml=$(lo::render_nodes "${k8s_version}" "${cluster_yaml}")

  cat <<EOF
# Rendered kind config for ${cluster_name}
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
name: ${cluster_name}
networking:
  disableDefaultCNI: true
  podSubnet: "${LO_DEFAULT_POD_CIDR}"
  serviceSubnet: "${LO_DEFAULT_SVC_CIDR}"
${nodes_yaml}
containerdConfigPatches:
  - |-
    [plugins."io.containerd.grpc.v1.cri".registry]
      config_path = "/etc/containerd/certs.d"
    [plugins."io.containerd.grpc.v1.cri".containerd]
      max_concurrent_downloads = ${LOK8S_MAX_CONCURRENT_DOWNLOADS:-3}
EOF
}

lo::render_certs_d_mount() {
  local host_path="$1"
  cat <<EOF
      - hostPath: ${host_path}
        containerPath: /etc/containerd/certs.d
        readOnly: true
EOF
}

# Render the nodes section.
# Usage: lo::render_nodes <k8s_version> <cluster_yaml>
lo::render_nodes() {
  local k8s_version="$1"
  local cluster_yaml="${2:-}"
  local result="nodes:"

  local domain="${DOMAIN_NAME:-${LO_DEFAULT_DOMAIN}}"
  local certs_d_host="${PATH_CLUSTERS}/${domain}/.containerd/certs.d"

  local i
  for (( i = 0; i < LOK8S_CP_COUNT; i++ )); do
    result+="
  - role: control-plane
    image: \"kindest/node:${k8s_version}\""
    if (( i == 0 )); then
      result+="
    kubeadmConfigPatches:
      - |
        kind: InitConfiguration
        nodeRegistration:
          kubeletExtraArgs:
            node-labels: \"ingress-ready=true\""
      # spec.oidc: append the ClusterConfiguration patch so kubeadm gives the
      # apiserver static pod --authentication-config + the file as an
      # extraVolume. Guarded by oidc::enabled — no spec.oidc ⇒ nothing appended
      # ⇒ this list is byte-identical to today.
      if oidc::enabled; then
        result+="$(lo::render_oidc_cp_patch)"
      fi
      if [[ "${LOK8S_HOST_PORTS:-false}" == "true" ]]; then
        result+="
    extraPortMappings:
      - containerPort: 80
        hostPort: 80
        protocol: TCP
      - containerPort: 443
        hostPort: 443
        protocol: TCP
      - containerPort: 8080
        hostPort: 8080
        protocol: TCP"
      fi
      result+="
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
$(lo::render_certs_d_mount "${certs_d_host}")"
      # spec.oidc: bind-mount the host auth-config file onto node 0 so kubeadm's
      # extraVolume (in the ClusterConfiguration patch above) can surface it
      # inside the apiserver pod. Guarded by oidc::enabled — no spec.oidc ⇒ no
      # extra mount appended.
      if oidc::enabled; then
        result+="
$(lo::render_oidc_mount "$(lo::oidc_auth_config_host_path "${domain}")")"
      fi
      # Append user-defined extraMounts from spec.nodes.extraMounts[]
      if (( LOK8S_EXTRA_MOUNTS_COUNT > 0 )) && [[ -n "${cluster_yaml}" ]]; then
        local m
        for (( m = 0; m < LOK8S_EXTRA_MOUNTS_COUNT; m++ )); do
          local em_host em_container em_readonly
          em_host=$(yq -r ".spec.nodes.extraMounts[${m}].hostPath" "${cluster_yaml}")
          em_container=$(yq -r ".spec.nodes.extraMounts[${m}].containerPath" "${cluster_yaml}")
          em_readonly=$(yq -r ".spec.nodes.extraMounts[${m}].readOnly // false" "${cluster_yaml}")
          result+="
      - hostPath: ${em_host}
        containerPath: ${em_container}"
          if [[ "${em_readonly}" == "true" ]]; then
            result+="
        readOnly: true"
          fi
        done
      fi
    else
      result+="
    extraMounts:
$(lo::render_certs_d_mount "${certs_d_host}")"
    fi
  done

  for (( i = 0; i < LOK8S_WORKER_COUNT; i++ )); do
    result+="
  - role: worker
    image: \"kindest/node:${k8s_version}\"
    extraMounts:
$(lo::render_certs_d_mount "${certs_d_host}")"
  done

  echo "${result}"
}

# Containerd reads the registry CA from this fixed path inside every kind
# node. The whole certs.d tree is bind-mounted, so a copy of mkcert's
# rootCA placed at ${certs_d}/.ca/rootCA.pem on the host appears here.
LO_CERTS_D_CA_PATH="/etc/containerd/certs.d/.ca/rootCA.pem"

lo::write_certs_d() {
  local domain="${DOMAIN_NAME:-${LO_DEFAULT_DOMAIN}}"
  local certs_d="${PATH_CLUSTERS}/${domain}/.containerd/certs.d"

  # Refresh the certs.d tree IN PLACE — do NOT `rm -rf` the directory itself.
  # A kind node bind-mounts this dir; removing it gives it a new inode while the
  # running node's mount still points at the deleted one, so the node sees an
  # EMPTY certs.d → containerd falls back to HTTPS:443 (the registry serves
  # HTTP:80) → ImagePullBackOff on every re-`lo up`. Clearing the contents
  # (mindepth 1) drops stale host entries while keeping the dir's inode, so an
  # existing node mount stays valid.
  mkdir -p "${certs_d}"
  find "${certs_d}" -mindepth 1 -delete 2>/dev/null || true

  # TLS mode: containerd connects over HTTPS and verifies the registry
  # cert against the local dev root CA. Copy the CA into the certs.d tree so
  # each hosts.toml can reference it via the bind mount. Plain mode keeps
  # the HTTP + skip_verify behavior (no CA needed).
  local tls=0
  local scheme="http"
  if registry::is_tls; then
    tls=1
    scheme="https"
    # Resolve the shared dev CA the way mkcert (and the cert: generator) do
    # (binary-free): $CAROOT, else the XDG/OS data dir + /mkcert. The registry
    # cert is signed by this CA; `lo trust` (mkcert -install) installs it.
    local caroot="${CAROOT:-${XDG_DATA_HOME:-${HOME}/.local/share}/mkcert}"
    local ca_src="${caroot}/rootCA.pem"
    if [[ -f "${ca_src}" ]]; then
      mkdir -p "${certs_d}/.ca"
      cp "${ca_src}" "${certs_d}/.ca/rootCA.pem"
    else
      echo "warning: registry TLS enabled but the local dev CA was not found at" >&2
      echo "         ${ca_src}; containerd pulls will fail cert verification. Run 'lo trust'." >&2
    fi
  fi

  # shellcheck disable=SC2329  # invoked indirectly via `registry::each` below
  _lo_write_certs_d_entry() {
    local name="$1" ip="$2" url="$3" reg_domain="$4" host="$5" type="$6"
    [[ -n "${ip}" ]] || return 0

    local hostname=""
    local capabilities='["pull", "resolve"]'

    if [[ -n "${host}" ]]; then
      # Framework registry (build/cache) — use canonical hostname, allow push
      hostname="${host}"
      capabilities='["pull", "resolve", "push"]'
    elif [[ -n "${reg_domain}" ]]; then
      # Mirror with known upstream domain
      hostname="${reg_domain}"
    elif [[ -n "${url}" ]]; then
      # Fallback: derive hostname from URL
      hostname="${url#https://}"
      hostname="${hostname#http://}"
      hostname="${hostname%%/*}"
      if [[ ! "${hostname}" =~ ^[a-zA-Z0-9._-]+$ ]]; then
        echo "warning: skipping mirror '${name}' — unsafe hostname '${hostname}'" >&2
        return 0
      fi
    else
      return 0
    fi

    # Emit the per-host trust line: a CA reference in TLS mode, or
    # skip_verify for plain HTTP. The server URL omits the port — in
    # both modes containerd uses the scheme's default (80 / 443), which
    # matches the registry's listen addr and the host's push target.
    local trust_line
    if (( tls )); then
      trust_line="  ca = \"${LO_CERTS_D_CA_PATH}\""
    else
      trust_line="  skip_verify = true"
    fi

    mkdir -p "${certs_d}/${hostname}"
    cat > "${certs_d}/${hostname}/hosts.toml" <<EOF
# Auto-generated by lok8s — registry mirror for ${name}
server = "${scheme}://${ip}"

[host."${scheme}://${ip}"]
  capabilities = ${capabilities}
${trust_line}
EOF

    mkdir -p "${certs_d}/${ip}"
    cat > "${certs_d}/${ip}/hosts.toml" <<EOF
# Auto-generated by lok8s — direct IP entry for ${name}
server = "${scheme}://${ip}"

[host."${scheme}://${ip}"]
  capabilities = ${capabilities}
${trust_line}
EOF
  }

  registry::each _lo_write_certs_d_entry

  debug "wrote containerd certs.d at ${certs_d} (tls=${tls})"
}

# Fixed node path the apiserver StructuredAuthenticationConfiguration is bind-
# mounted to inside CP node 0, and the path the kube-apiserver static pod reads
# it from (kubeadm extraVolume mountPath). Single source of truth for both the
# extraMount (node level) and the ClusterConfiguration patch (apiserver pod).
LO_OIDC_AUTH_CONFIG_NODE_PATH="/etc/kubernetes/oidc/auth-config.yaml"

# Host path the rendered auth-config lives at (bind-mount source for node 0).
# Usage: lo::oidc_auth_config_host_path [domain]
lo::oidc_auth_config_host_path() {
  local domain="${1:-${DOMAIN_NAME:-${LO_DEFAULT_DOMAIN}}}"
  echo "${PATH_CLUSTERS}/${domain}/.oidc/auth-config.yaml"
}

# Render spec.oidc into the host auth-config file, refreshed IN PLACE.
# No-op (and the .oidc dir is left clean) unless oidc::enabled.
#
# Like lo::write_certs_d, do NOT `rm -rf` the .oidc dir: CP node 0 bind-mounts
# the FILE inside it, and replacing the dir's inode would leave the running
# node's mount pointing at a deleted path. We truncate+rewrite the file in
# place so an existing node mount keeps seeing fresh content.
# Usage: lo::write_oidc_auth_config <domain>
lo::write_oidc_auth_config() {
  local domain="${1:-${DOMAIN_NAME:-${LO_DEFAULT_DOMAIN}}}"
  local oidc_dir="${PATH_CLUSTERS}/${domain}/.oidc"
  local auth_config="${oidc_dir}/auth-config.yaml"

  if ! oidc::enabled; then
    # spec.oidc absent/incomplete: leave nothing behind, but if a stale file
    # exists from a prior oidc-enabled run, clear its contents in place (keep
    # the inode for any live mount) so a disabled cluster doesn't keep wiring.
    [[ ! -f "${auth_config}" ]] || : > "${auth_config}"
    return 0
  fi

  mkdir -p "${oidc_dir}"
  local rendered
  if ! rendered=$(oidc::render_auth_config); then
    error "failed to render apiserver authentication config from spec.oidc"
    return 1
  fi
  # Truncate-then-write keeps the file's inode stable for a live node mount.
  printf '%s\n' "${rendered}" > "${auth_config}"
  debug "wrote apiserver authentication config at ${auth_config}"
}

# Render the CP-node-0 patches that wire the kube-apiserver to the OIDC
# StructuredAuthenticationConfiguration: a ClusterConfiguration kubeadm patch
# (apiServer.extraArgs + apiServer.extraVolumes) so the apiserver STATIC POD
# both gets the --authentication-config flag AND has the host file mounted into
# the pod. The node-level extraMount (rendered separately in lo::render_nodes)
# only gets the file onto the node filesystem; kubeadm's extraVolume is what
# surfaces it inside the apiserver pod.
#
# kubeadm API shape VERIFIED against the v1beta4 config reference (the version
# bundled with kindest/node:v1.35.x):
#   - apiVersion: kubeadm.k8s.io/v1beta4
#   - apiServer.extraArgs is a LIST of {name,value} (NOT a map) — this CHANGED
#     in v1beta4 (was map[string]string in v1beta3).
#   - apiServer.extraVolumes entries are {name,hostPath,mountPath,readOnly,pathType}.
#   https://kubernetes.io/docs/reference/config-api/kubeadm-config.v1beta4/
#
# Emitted with a leading newline so it appends cleanly under the existing
# kubeadmConfigPatches list on node 0. The host path is the bind-mount target
# INSIDE the node (LO_OIDC_AUTH_CONFIG_NODE_PATH), not the host fs path.
lo::render_oidc_cp_patch() {
  cat <<EOF

      - |
        kind: ClusterConfiguration
        apiVersion: kubeadm.k8s.io/v1beta4
        apiServer:
          extraArgs:
            - name: authentication-config
              value: ${LO_OIDC_AUTH_CONFIG_NODE_PATH}
          extraVolumes:
            - name: oidc-auth-config
              hostPath: ${LO_OIDC_AUTH_CONFIG_NODE_PATH}
              mountPath: ${LO_OIDC_AUTH_CONFIG_NODE_PATH}
              readOnly: true
              pathType: File
EOF
}

# Render the node-0 extraMount entry that binds the host auth-config file to the
# fixed node path. Usage: lo::render_oidc_mount <host_path>
lo::render_oidc_mount() {
  local host_path="$1"
  cat <<EOF
      - hostPath: ${host_path}
        containerPath: ${LO_OIDC_AUTH_CONFIG_NODE_PATH}
        readOnly: true
EOF
}
