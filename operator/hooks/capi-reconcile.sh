#!/usr/bin/env bash
# shell-operator hook: reconcile Capi CRDs
# Watches Capi custom resources, detects the CAPI provider,
# generates CAPI resources from templates, and applies them
# to the management cluster.
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
  - apiVersion: cluster.lok8s.dev/v1beta1
    kind: Capi
    executeHookOnEvent: ["Added", "Modified"]
    jqFilter: ".spec"
EOF
}

# Detect CAPI provider from the CR spec JSON.
capi::detect_provider_from_spec() {
  local spec="$1"
  if echo "${spec}" | jq -e '.hcloud' &>/dev/null; then
    echo "hetzner"
  elif echo "${spec}" | jq -e '.aws' &>/dev/null; then
    echo "aws"
  else
    echo "error: no known CAPI provider found in Capi spec" >&2
    return 1
  fi
}

# Generate CAPI resources from templates using CR spec values.
# Templates are copied into /hooks/capi-templates/ in the container.
capi::generate_from_spec() {
  local spec="$1" provider="$2" name="$3"
  local tmpl_dir="${HOOK_DIR}/capi-templates"

  if [[ ! -d "${tmpl_dir}" ]]; then
    echo "error: CAPI template directory not found: ${tmpl_dir}" >&2
    return 1
  fi

  # Extract variables from spec
  export CLUSTER_NAME="${name}"
  export CLUSTER_NAMESPACE
  CLUSTER_NAMESPACE=$(echo "${spec}" | jq -r '.cluster.namespace // "default"')
  export CLUSTER_DOMAIN
  CLUSTER_DOMAIN=$(echo "${spec}" | jq -r '.cluster.domain')
  export K8S_VERSION
  K8S_VERSION=$(echo "${spec}" | jq -r '.kubernetes.version // "v1.31.10"')
  export CP_REPLICAS
  CP_REPLICAS=$(echo "${spec}" | jq -r '.controlPlane.replicas // 1')
  export CREDENTIAL_SECRET_NAME
  CREDENTIAL_SECRET_NAME=$(echo "${spec}" | jq -r ".credentials.secretName // \"${name}-credentials\"")

  # Provider-specific variables
  case "${provider}" in
    hetzner)
      export INFRA_API_VERSION="infrastructure.cluster.x-k8s.io/v1beta1"
      export INFRA_CLUSTER_KIND="HetznerCluster"
      export INFRA_MACHINE_TEMPLATE_KIND="HCloudMachineTemplate"
      export HCLOUD_REGION
      HCLOUD_REGION=$(echo "${spec}" | jq -r '.hcloud.region')
      export HCLOUD_SSH_KEY_NAME
      HCLOUD_SSH_KEY_NAME=$(echo "${spec}" | jq -r '.hcloud.sshKeyName')
      ;;
    aws)
      export INFRA_API_VERSION="infrastructure.cluster.x-k8s.io/v1beta2"
      export INFRA_CLUSTER_KIND="AWSCluster"
      export INFRA_MACHINE_TEMPLATE_KIND="AWSMachineTemplate"
      export AWS_REGION
      AWS_REGION=$(echo "${spec}" | jq -r '.aws.region')
      ;;
    *)
      echo "error: unsupported CAPI provider: ${provider}" >&2
      return 1
      ;;
  esac

  # Render core templates
  local first=1
  for tmpl in "${tmpl_dir}"/core/*.yaml; do
    [[ -f "${tmpl}" ]] || continue
    if (( first )); then
      first=0
    else
      echo "---"
    fi
    envsubst < "${tmpl}"
  done

  # Render provider templates
  local provider_dir="${tmpl_dir}/providers/${provider}"
  if [[ -d "${provider_dir}" ]]; then
    for tmpl in "${provider_dir}"/*.yaml; do
      [[ -f "${tmpl}" ]] || continue
      echo "---"
      envsubst < "${tmpl}"
    done
  fi

  # Render worker pool machine deployments
  local workers
  workers=$(echo "${spec}" | jq -r '.workers // empty')
  if [[ -n "${workers}" && "${workers}" != "null" ]]; then
    local pool
    while IFS= read -r pool; do
      [[ -n "${pool}" ]] || continue
      export POOL_NAME="${pool}"
      export POOL_REPLICAS POOL_TYPE
      # Pass the pool name as a jq --arg — never interpolate a tenant-controlled
      # key into the jq PROGRAM string (it could rewrite the filter). The CRD's
      # workers propertyNames pattern also constrains it at admission.
      POOL_REPLICAS=$(echo "${spec}" | jq -r --arg p "${pool}" '.workers[$p].replicas // 1')
      POOL_TYPE=$(echo "${spec}" | jq -r --arg p "${pool}" '.workers[$p].type')
      echo "---"
      envsubst < "${tmpl_dir}/core/machine-deployment.yaml"
      if [[ -f "${provider_dir}/hcloud-machine-template.yaml" ]]; then
        echo "---"
        envsubst < "${provider_dir}/hcloud-machine-template.yaml"
      fi
    done < <(echo "${spec}" | jq -r '.workers | keys[]')
  fi
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

    local name namespace spec
    name=$(jq -r ".[${i}].object.metadata.name" "${context_file}")
    namespace=$(jq -r ".[${i}].object.metadata.namespace // \"default\"" "${context_file}")
    spec=$(jq -r ".[${i}].filterResult" "${context_file}")

    echo "info: reconciling Capi ${namespace}/${name}" >&2

    # Update status to Provisioning
    kubectl patch capi "${name}" -n "${namespace}" \
      --type merge \
      --subresource status \
      -p '{"status":{"phase":"Provisioning","ready":false}}' 2>/dev/null || true

    # Detect provider
    local provider
    if ! provider=$(capi::detect_provider_from_spec "${spec}"); then
      kubectl patch capi "${name}" -n "${namespace}" \
        --type merge \
        --subresource status \
        -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"UnknownProvider","message":"No known CAPI provider found in spec"}]}}' 2>/dev/null || true
      continue
    fi

    # Record detected provider in status
    kubectl patch capi "${name}" -n "${namespace}" \
      --type merge \
      --subresource status \
      -p "{\"status\":{\"provider\":\"${provider}\"}}" 2>/dev/null || true

    # Generate CAPI resources from templates
    local resources
    if ! resources=$(capi::generate_from_spec "${spec}" "${provider}" "${name}"); then
      kubectl patch capi "${name}" -n "${namespace}" \
        --type merge \
        --subresource status \
        -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"GenerationFailed","message":"Failed to generate CAPI resources from templates"}]}}' 2>/dev/null || true
      continue
    fi

    # Apply generated CAPI resources to the cluster
    if echo "${resources}" | kubectl apply -f - 2>&1; then
      echo "info: CAPI resources applied for ${name}" >&2
      # Status will be updated by capi-status-sync when CAPI Cluster becomes ready
    else
      kubectl patch capi "${name}" -n "${namespace}" \
        --type merge \
        --subresource status \
        -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"ApplyFailed","message":"Failed to apply CAPI resources"}]}}' 2>/dev/null || true
      echo "error: failed to apply CAPI resources for ${name}" >&2
    fi
  done
}

if [[ "${1:-}" == "--config" ]]; then
  hook::config
else
  hook::trigger
fi
