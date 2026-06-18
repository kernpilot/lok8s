#!/usr/bin/env bash
# shell-operator hook: sync CAPI Cluster status to lok8s Capi CRs
# Watches cluster.x-k8s.io/v1beta1 Cluster resources with the
# lok8s.dev/managed label and mirrors their status back to the
# corresponding lok8s Capi CR. Triggers GitOps bootstrap or
# direct target deployment when a cluster becomes Provisioned.
set -euo pipefail

HOOK_DIR="$(cd "$(dirname "$0")" && pwd)"
export PATH_BASE="${HOOK_DIR}"

# Shim argsh 'import' for plain bash — libs use 'import libs/...' which is
# an argsh builtin. In the operator container (plain bash), all libs are
# already sourced by the glob below, so import is a no-op.
import() { :; }

for lib in "${HOOK_DIR}"/lib/*; do
  # shellcheck source=/dev/null
  [[ -f "${lib}" ]] && source "${lib}"
done

hook::config() {
  cat <<'EOF'
configVersion: v1
kubernetes:
  - apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    executeHookOnEvent: ["Modified"]
    jqFilter: ".status"
    labelSelector:
      matchLabels:
        lok8s.dev/managed: "true"
EOF
}

hook::trigger() {
  local context_file="${BINDING_CONTEXT_PATH}"
  local count
  count=$(jq -r 'length' "${context_file}")

  for (( i=0; i<count; i++ )); do
    local event_type
    event_type=$(jq -r ".[${i}].type // \"Event\"" "${context_file}")

    if [[ "${event_type}" == "Synchronization" ]]; then
      continue
    fi

    local name namespace status phase
    name=$(jq -r ".[${i}].object.metadata.name" "${context_file}")
    namespace=$(jq -r ".[${i}].object.metadata.namespace // \"default\"" "${context_file}")
    status=$(jq -r ".[${i}].filterResult" "${context_file}")
    phase=$(echo "${status}" | jq -r '.phase // "Unknown"')

    echo "info: syncing CAPI Cluster status for ${namespace}/${name}: phase=${phase}" >&2

    # Map CAPI phase to lok8s phase
    local lok8s_phase
    case "${phase}" in
      Provisioned) lok8s_phase="Provisioned" ;;
      Provisioning|Pending) lok8s_phase="Provisioning" ;;
      Failed|Deleting) lok8s_phase="Failed" ;;
      *) lok8s_phase="Provisioning" ;;
    esac

    local cp_ready
    cp_ready=$(echo "${status}" | jq -r '.controlPlaneReady // false')

    # Build status patch
    local status_patch
    status_patch=$(jq -n \
      --arg phase "${lok8s_phase}" \
      --argjson ready "${cp_ready}" \
      '{status: {phase: $phase, ready: $ready}}')

    # Add controlPlaneEndpoint if available
    local cp_host cp_port
    cp_host=$(echo "${status}" | jq -r '.controlPlaneEndpoint.host // empty')
    cp_port=$(echo "${status}" | jq -r '.controlPlaneEndpoint.port // empty')
    if [[ -n "${cp_host}" && -n "${cp_port}" ]]; then
      status_patch=$(echo "${status_patch}" | jq \
        --arg host "${cp_host}" \
        --argjson port "${cp_port}" \
        '.status.controlPlaneEndpoint = {host: $host, port: $port}')
    fi

    # Update the lok8s Capi CR status
    kubectl patch capi "${name}" -n "${namespace}" \
      --type merge \
      --subresource status \
      -p "${status_patch}" 2>/dev/null || {
      echo "warn: could not patch Capi CR ${name} status (CR may not exist)" >&2
      continue
    }

    # When cluster becomes Provisioned, trigger post-provision actions
    if [[ "${phase}" == "Provisioned" ]]; then
      echo "info: Capi cluster ${name} is Provisioned, running post-provision" >&2

      # Extract kubeconfig for the work cluster
      local kubeconfig_secret="${name}-kubeconfig"
      local kc
      if kc=$(clusterctl get kubeconfig "${name}" -n "${namespace}" 2>/dev/null) && [[ -n "${kc}" ]]; then
        # Pipe the work-cluster kubeconfig straight into the Secret — never write
        # full cluster-admin creds to a predictable, world-readable /tmp path.
        printf '%s' "${kc}" | kubectl create secret generic "${kubeconfig_secret}" \
          -n "${namespace}" \
          --from-file=value=/dev/stdin \
          --dry-run=client -o yaml | kubectl apply -f - 2>/dev/null || true

        # Update Capi CR with kubeconfig reference
        kubectl patch capi "${name}" -n "${namespace}" \
          --type merge \
          --subresource status \
          -p "{\"status\":{\"kubeconfig\":{\"secretRef\":\"${kubeconfig_secret}\"}}}" 2>/dev/null || true
      fi

      # Check if GitOps is configured
      local gitops_provider domain
      gitops_provider=$(kubectl get capi "${name}" -n "${namespace}" \
        -o jsonpath='{.spec.gitops.provider}' 2>/dev/null || true)
      domain=$(kubectl get capi "${name}" -n "${namespace}" \
        -o jsonpath='{.spec.cluster.domain}' 2>/dev/null || true)

      if [[ -n "${gitops_provider}" && -n "${domain}" ]]; then
        echo "info: bootstrapping GitOps (${gitops_provider}) for ${domain}" >&2
        if declare -f gitops::bootstrap &>/dev/null; then
          gitops::bootstrap "${domain}" "${gitops_provider}" || {
            echo "warn: GitOps bootstrap failed for ${domain}" >&2
          }
          # Update GitOps status
          kubectl patch capi "${name}" -n "${namespace}" \
            --type merge \
            --subresource status \
            -p "{\"status\":{\"gitops\":{\"provider\":\"${gitops_provider}\",\"status\":\"Bootstrapped\"}}}" 2>/dev/null || true
        fi
      elif [[ -n "${domain}" ]]; then
        # Direct deploy mode (no GitOps)
        echo "info: no GitOps configured, direct deploy for ${domain}" >&2
        if declare -f deploy::apply &>/dev/null; then
          deploy::apply "${domain}" || {
            echo "warn: direct deploy failed for ${domain}" >&2
          }
        fi
      fi

      # Update conditions
      kubectl patch capi "${name}" -n "${namespace}" \
        --type merge \
        --subresource status \
        -p '{"status":{"conditions":[{"type":"InfrastructureReady","status":"True"},{"type":"ControlPlaneReady","status":"True"}]}}' 2>/dev/null || true
    fi
  done
}

if [[ "${1:-}" == "--config" ]]; then
  hook::config
else
  hook::trigger
fi
