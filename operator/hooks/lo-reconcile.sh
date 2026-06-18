#!/usr/bin/env bash
# shell-operator hook: full lifecycle for Lo CRDs.
# Creation, idempotent convergence, kubeconfig publication, drift
# detection on a schedule, and finalizer-guarded teardown — using the
# same driver contract as the lo CLI.
set -euo pipefail

HOOK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=runtime.sh
source "${HOOK_DIR}/runtime.sh"

# The Lo driver contract + its helper utils (present in the image;
# absent when sourced from the repo by tests, which stub driver::*)
for _f in "${HOOK_DIR}"/drivers/lo/utils/*.sh "${HOOK_DIR}"/drivers/lo/libs/* \
  "${HOOK_DIR}"/drivers/lo/main; do
  # shellcheck source=/dev/null
  [[ -f "${_f}" ]] && source "${_f}"
done
unset _f

FINALIZER="lok8s.dev/lo-teardown"

hook::config() {
  cat <<'EOF'
configVersion: v1
kubernetes:
  - apiVersion: cluster.lok8s.dev/v1beta1
    kind: Lo
    executeHookOnEvent: ["Added", "Modified"]
    executeHookOnSynchronization: true
    jqFilter: "{spec: .spec, metadata: {name: .metadata.name, namespace: .metadata.namespace, deletionTimestamp: .metadata.deletionTimestamp, finalizers: .metadata.finalizers}}"
schedule:
  - name: lo-drift
    crontab: "*/3 * * * *"
EOF
}

# Write the CR as a cluster spec where the driver contract expects it:
# $PATH_CLUSTERS/<domain>/cluster.lok8s.yaml
lo_hook::materialize_spec() {
  local domain="$1" object_json="$2"
  mkdir -p "${PATH_CLUSTERS}/${domain}"
  echo "${object_json}" | yq -P '.' > "${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
}

lo_hook::ensure_finalizer() {
  local name="$1" namespace="$2" finalizers_json="$3"
  if echo "${finalizers_json}" | jq -e --arg f "${FINALIZER}" 'index($f) != null' >/dev/null 2>&1; then
    return 0
  fi
  kubectl patch lo "${name}" -n "${namespace}" --type json \
    -p "[{\"op\":\"add\",\"path\":\"/metadata/finalizers/-\",\"value\":\"${FINALIZER}\"}]" 2>/dev/null ||
    kubectl patch lo "${name}" -n "${namespace}" --type json \
      -p "[{\"op\":\"add\",\"path\":\"/metadata/finalizers\",\"value\":[\"${FINALIZER}\"]}]" ||
    echo "warn: failed to add finalizer to Lo ${namespace}/${name}" >&2
}

lo_hook::remove_finalizer() {
  local name="$1" namespace="$2"
  local remaining
  remaining=$(kubectl get lo "${name}" -n "${namespace}" \
    -o jsonpath='{.metadata.finalizers}' 2>/dev/null |
    jq -c --arg f "${FINALIZER}" 'map(select(. != $f))' 2>/dev/null || echo '[]')
  kubectl patch lo "${name}" -n "${namespace}" --type merge \
    -p "{\"metadata\":{\"finalizers\":${remaining}}}" ||
    echo "warn: failed to remove finalizer from Lo ${namespace}/${name}" >&2
}

# Publish the cluster's kubeconfig as Secret <name>-kubeconfig and
# reference it from status.
lo_hook::publish_kubeconfig() {
  local name="$1" namespace="$2" domain="$3"
  local cluster_name kubeconfig_path
  cluster_name=$(yq -r '.metadata.name' "${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml")
  kubeconfig_path="${PATH_BASE}/.kubeconfig/${cluster_name}.yaml"

  [[ -f "${kubeconfig_path}" ]] || driver::kubeconfig "${domain}" >/dev/null || return 1

  kubectl create secret generic "${name}-kubeconfig" \
    -n "${namespace}" \
    --from-file=value="${kubeconfig_path}" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  hook::patch_status lo "${name}" "${namespace}" \
    "{\"status\":{\"kubeconfig\":{\"secretRef\":\"${name}-kubeconfig\"}}}"
}

lo_hook::teardown() {
  local name="$1" namespace="$2" domain="$3"

  hook::patch_status lo "${name}" "${namespace}" \
    '{"status":{"phase":"Terminating","ready":false}}'

  if driver::destroy "${domain}"; then
    kubectl delete secret "${name}-kubeconfig" -n "${namespace}" \
      --ignore-not-found=true >/dev/null 2>&1 || true
    rm -rf "${PATH_CLUSTERS:?}/${domain}"
    lo_hook::remove_finalizer "${name}" "${namespace}"
    echo "info: Lo ${namespace}/${name} torn down" >&2
  else
    # keep the finalizer: the schedule binding retries
    hook::patch_status lo "${name}" "${namespace}" \
      '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"DestroyFailed","message":"driver::destroy failed; will retry"}]}}'
    echo "error: Lo ${namespace}/${name} teardown failed (will retry)" >&2
  fi
}

lo_hook::provision() {
  local name="$1" namespace="$2" domain="$3"

  hook::patch_status lo "${name}" "${namespace}" \
    '{"status":{"phase":"Provisioning","ready":false}}'

  local cluster_yaml="${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml"
  local cluster_name
  cluster_name=$(yq -r '.metadata.name' "${cluster_yaml}")

  if driver::provision "${domain}" &&
    bootstrap::apply "${domain}" "${cluster_yaml}" "${PATH_BASE}/.kubeconfig/${cluster_name}.yaml"; then
    lo_hook::publish_kubeconfig "${name}" "${namespace}" "${domain}" || true
    hook::patch_status lo "${name}" "${namespace}" \
      '{"status":{"phase":"Provisioned","ready":true,"conditions":[{"type":"Ready","status":"True","reason":"Provisioned","message":"Cluster is ready"}]}}'
    echo "info: Lo ${namespace}/${name} provisioned" >&2
  else
    hook::patch_status lo "${name}" "${namespace}" \
      '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"ProvisionFailed","message":"Provisioning failed"}]}}'
    echo "error: Lo ${namespace}/${name} provisioning failed" >&2
  fi
}

# Converge one Lo object (full JSON) toward its spec.
lo_hook::reconcile() {
  local object_json="$1"

  local name namespace domain deletion finalizers
  name=$(echo "${object_json}" | jq -r '.metadata.name')
  namespace=$(echo "${object_json}" | jq -r '.metadata.namespace // "default"')
  domain=$(echo "${object_json}" | jq -r '.spec.cluster.domain // empty')
  deletion=$(echo "${object_json}" | jq -r '.metadata.deletionTimestamp // empty')
  finalizers=$(echo "${object_json}" | jq -c '.metadata.finalizers // []')

  if [[ -z "${domain}" ]]; then
    hook::patch_status lo "${name}" "${namespace}" \
      '{"status":{"phase":"Failed","ready":false,"conditions":[{"type":"Ready","status":"False","reason":"MissingDomain","message":"spec.cluster.domain is required"}]}}'
    return 0
  fi

  echo "info: reconciling Lo ${namespace}/${name} (${domain})" >&2
  lo_hook::materialize_spec "${domain}" "${object_json}"

  if [[ -n "${deletion}" ]]; then
    if echo "${finalizers}" | jq -e --arg f "${FINALIZER}" 'index($f) != null' >/dev/null; then
      lo_hook::teardown "${name}" "${namespace}" "${domain}"
    fi
    return 0
  fi

  lo_hook::ensure_finalizer "${name}" "${namespace}" "${finalizers}"

  local state
  state=$(driver::status "${domain}" 2>/dev/null || echo "Unknown")
  if [[ "${state}" == "Running" ]]; then
    lo_hook::publish_kubeconfig "${name}" "${namespace}" "${domain}" || true
    hook::patch_status lo "${name}" "${namespace}" \
      '{"status":{"phase":"Provisioned","ready":true,"conditions":[{"type":"Ready","status":"True","reason":"Provisioned","message":"Cluster is ready"}]}}'
    return 0
  fi

  lo_hook::provision "${name}" "${namespace}" "${domain}"
}

# Re-list every Lo CR and converge — drift detection + teardown retry.
lo_hook::reconcile_all() {
  local items count
  items=$(kubectl get lo -A -o json 2>/dev/null | jq -c '.items // []')
  count=$(echo "${items}" | jq -r 'length')
  for (( j = 0; j < count; j++ )); do
    lo_hook::reconcile "$(echo "${items}" | jq -c ".[${j}]")" ||
      echo "warn: reconcile failed for item ${j}" >&2
  done
}

hook::trigger() {
  local context_file="${BINDING_CONTEXT_PATH}"
  local count
  count=$(jq -r 'length' "${context_file}")

  for (( i = 0; i < count; i++ )); do
    local event_type
    event_type=$(jq -r ".[${i}].type // \"Event\"" "${context_file}")

    case "${event_type}" in
      Schedule|Synchronization)
        lo_hook::reconcile_all
        ;;
      *)
        lo_hook::reconcile "$(jq -c ".[${i}].object" "${context_file}")" ||
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
