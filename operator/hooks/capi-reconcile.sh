#!/usr/bin/env bash
# shell-operator hook: reconcile Capi CRDs
# Watches Capi custom resources, detects the CAPI provider,
# generates CAPI resources from templates, and applies them
# to the management cluster.
set -euo pipefail

# BASH_SOURCE (not $0) so the file resolves correctly when sourced by tests.
HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export PATH_BASE="${HOOK_DIR}"

# Shim argsh 'import' for plain bash — libs use 'import libs/...' which is
# an argsh builtin. In the operator container (plain bash), all libs are
# already sourced by the glob below, so import is a no-op.
import() { :; }

for lib in "${HOOK_DIR}"/lib/*; do
  # shellcheck source=/dev/null
  [[ -f "${lib}" ]] && source "${lib}"
done

# Finalizer that intercepts deletion so we can tear the workload cluster down
# before the Capi CR vanishes (otherwise the applied CAPI Cluster + machines +
# infra are orphaned and keep the cloud cluster — and its bill — alive).
FINALIZER="lok8s.dev/capi-teardown"

hook::config() {
  cat <<'EOF'
configVersion: v1
kubernetes:
  - apiVersion: cluster.lok8s.dev/v1beta1
    kind: Capi
    executeHookOnEvent: ["Added", "Modified"]
    executeHookOnSynchronization: true
    jqFilter: "{spec: .spec, metadata: {name: .metadata.name, namespace: .metadata.namespace, deletionTimestamp: .metadata.deletionTimestamp, finalizers: .metadata.finalizers}}"
schedule:
  - name: capi-drift
    crontab: "*/3 * * * *"
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

capi_hook::ensure_finalizer() {
  local name="$1" namespace="$2" finalizers_json="$3"
  if echo "${finalizers_json}" | jq -e --arg f "${FINALIZER}" 'index($f) != null' >/dev/null 2>&1; then
    return 0
  fi
  kubectl patch capi "${name}" -n "${namespace}" --type json \
    -p "[{\"op\":\"add\",\"path\":\"/metadata/finalizers/-\",\"value\":\"${FINALIZER}\"}]" 2>/dev/null ||
    kubectl patch capi "${name}" -n "${namespace}" --type json \
      -p "[{\"op\":\"add\",\"path\":\"/metadata/finalizers\",\"value\":[\"${FINALIZER}\"]}]" ||
    echo "warn: failed to add finalizer to Capi ${namespace}/${name}" >&2
}

capi_hook::remove_finalizer() {
  local name="$1" namespace="$2"
  local remaining
  remaining=$(kubectl get capi "${name}" -n "${namespace}" \
    -o jsonpath='{.metadata.finalizers}' 2>/dev/null |
    jq -c --arg f "${FINALIZER}" 'map(select(. != $f))' 2>/dev/null || echo '[]')
  kubectl patch capi "${name}" -n "${namespace}" --type merge \
    -p "{\"metadata\":{\"finalizers\":${remaining}}}" ||
    echo "warn: failed to remove finalizer from Capi ${namespace}/${name}" >&2
}

# Tear down the workload cluster by deleting the CAPI Cluster object. CAPI's
# own Cluster finalizer then cascades teardown of the control plane, machine
# deployments, machines and the infrastructure (→ cloud servers/LB/network),
# using the still-present credential Secret. --wait=false: that teardown is
# async and outlives our CR, so we don't block the hook on it. We drop our
# finalizer only once the delete is accepted — a failed API call keeps the
# finalizer so the capi-drift schedule retries, rather than orphaning the
# cluster. The CAPI Cluster lives in spec.cluster.namespace (not necessarily
# the CR's namespace) and is named after the CR (CLUSTER_NAME in generate).
capi_hook::teardown() {
  local name="$1" namespace="$2" spec="$3"
  local cluster_ns
  cluster_ns=$(echo "${spec}" | jq -r '.cluster.namespace // "default"')

  kubectl patch capi "${name}" -n "${namespace}" --type merge --subresource status \
    -p '{"status":{"phase":"Terminating","ready":false}}' 2>/dev/null || true

  if kubectl delete cluster.cluster.x-k8s.io "${name}" -n "${cluster_ns}" \
    --wait=false --ignore-not-found 2>&1; then
    capi_hook::remove_finalizer "${name}" "${namespace}"
    echo "info: Capi ${namespace}/${name} torn down (deleted CAPI Cluster ${cluster_ns}/${name})" >&2
  else
    kubectl patch capi "${name}" -n "${namespace}" --type merge --subresource status \
      -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"DestroyFailed","message":"failed to delete CAPI Cluster; will retry"}]}}' 2>/dev/null || true
    echo "error: Capi ${namespace}/${name} teardown failed (will retry)" >&2
  fi
}

# Detect provider, render the CAPI manifests from templates, and apply them.
capi_hook::provision() {
  local name="$1" namespace="$2" spec="$3"

  kubectl patch capi "${name}" -n "${namespace}" \
    --type merge --subresource status \
    -p '{"status":{"phase":"Provisioning","ready":false}}' 2>/dev/null || true

  local provider
  if ! provider=$(capi::detect_provider_from_spec "${spec}"); then
    kubectl patch capi "${name}" -n "${namespace}" \
      --type merge --subresource status \
      -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"UnknownProvider","message":"No known CAPI provider found in spec"}]}}' 2>/dev/null || true
    return 0
  fi

  kubectl patch capi "${name}" -n "${namespace}" \
    --type merge --subresource status \
    -p "{\"status\":{\"provider\":\"${provider}\"}}" 2>/dev/null || true

  local resources
  if ! resources=$(capi::generate_from_spec "${spec}" "${provider}" "${name}"); then
    kubectl patch capi "${name}" -n "${namespace}" \
      --type merge --subresource status \
      -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"GenerationFailed","message":"Failed to generate CAPI resources from templates"}]}}' 2>/dev/null || true
    return 0
  fi

  if echo "${resources}" | kubectl apply -f - 2>&1; then
    echo "info: CAPI resources applied for ${name}" >&2
    # Status reaches Provisioned/Ready via capi-status-sync when the CAPI Cluster is up.
  else
    kubectl patch capi "${name}" -n "${namespace}" \
      --type merge --subresource status \
      -p '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"ApplyFailed","message":"Failed to apply CAPI resources"}]}}' 2>/dev/null || true
    echo "error: failed to apply CAPI resources for ${name}" >&2
  fi
}

# Converge one Capi object (full JSON) toward its spec — provision when live,
# finalizer-guarded teardown when it's being deleted.
capi_hook::reconcile() {
  local object_json="$1"
  local name namespace spec deletion finalizers
  name=$(echo "${object_json}" | jq -r '.metadata.name')
  namespace=$(echo "${object_json}" | jq -r '.metadata.namespace // "default"')
  spec=$(echo "${object_json}" | jq -c '.spec')
  deletion=$(echo "${object_json}" | jq -r '.metadata.deletionTimestamp // empty')
  finalizers=$(echo "${object_json}" | jq -c '.metadata.finalizers // []')

  echo "info: reconciling Capi ${namespace}/${name}" >&2

  if [[ -n "${deletion}" ]]; then
    if echo "${finalizers}" | jq -e --arg f "${FINALIZER}" 'index($f) != null' >/dev/null; then
      capi_hook::teardown "${name}" "${namespace}" "${spec}"
    fi
    return 0
  fi

  capi_hook::ensure_finalizer "${name}" "${namespace}" "${finalizers}"
  capi_hook::provision "${name}" "${namespace}" "${spec}"
}

# Re-list every Capi CR and converge — drift detection + teardown retry.
capi_hook::reconcile_all() {
  local items count
  items=$(kubectl get capi -A -o json 2>/dev/null | jq -c '.items // []')
  count=$(echo "${items}" | jq -r 'length')
  for (( j = 0; j < count; j++ )); do
    capi_hook::reconcile "$(echo "${items}" | jq -c ".[${j}]")" ||
      echo "warn: reconcile failed for item ${j}" >&2
  done
}

hook::trigger() {
  local context_file="${BINDING_CONTEXT_PATH}"
  local count
  count=$(jq -r 'length' "${context_file}")

  for (( i=0; i<count; i++ )); do
    local event_type
    event_type=$(jq -r ".[${i}].type // \"Event\"" "${context_file}")
    case "${event_type}" in
      Schedule|Synchronization)
        capi_hook::reconcile_all
        ;;
      *)
        capi_hook::reconcile "$(jq -c ".[${i}].object" "${context_file}")" ||
          echo "warn: reconcile failed for event ${i}" >&2
        ;;
    esac
  done
}

# Run only when executed (shell-operator); tests source this file.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  if [[ "${1:-}" == "--config" ]]; then
    hook::config
  else
    hook::trigger
  fi
fi
