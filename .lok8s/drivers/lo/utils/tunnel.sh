# shellcheck shell=bash
# tunnel.sh — SSH tunnel + kubeconfig rewrite for remote API access

# Open an SSH tunnel for the k8s API and rewrite kubeconfig to localhost.
# Usage: lo::kubeconfig_tunnel <kubeconfig_path> <remote_user> <remote_ip>
lo::kubeconfig_tunnel() {
  local kubeconfig_path="$1" remote_user="$2" remote_ip="$3"

  local current_server
  current_server=$(yq -r '.clusters[0].cluster.server' "${kubeconfig_path}")
  local port="${current_server##*:}"
  local remote_host="${current_server#https://}"
  remote_host="${remote_host%:*}"

  ssh -fN \
    -o ServerAliveInterval=15 \
    -L "${port}:${remote_host}:${port}" \
    "${remote_user}@${remote_ip}" 2>/dev/null || {
      warn "SSH port-forward for API failed — kubeconfig may not be reachable locally"
    }

  yq -i ".clusters[0].cluster.server = \"https://127.0.0.1:${port}\"" "${kubeconfig_path}"
  debug "API tunnel: localhost:${port} → ${remote_host}:${port}"
}
