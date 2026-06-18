# shellcheck shell=bash
# template.sh — YAML template rendering via envsubst
# Exports variables from a cluster spec YAML and renders template files.

import utils/verbose

# Render a template file by exporting vars from a cluster spec YAML.
# Usage: template::render <template_file> <cluster_yaml>
# Outputs rendered YAML to stdout.
template::render() {
  local template="$1" cluster_yaml="$2"

  [[ -f "${template}" ]] || { error "Template not found: ${template}"; return 1; }
  [[ -f "${cluster_yaml}" ]] || { error "Cluster spec not found: ${cluster_yaml}"; return 1; }

  # Run in subshell so exports don't leak to caller's scope
  (
    export CLUSTER_NAME CLUSTER_NAMESPACE CLUSTER_DOMAIN K8S_VERSION
    CLUSTER_NAME=$(yq -r '.metadata.name' "${cluster_yaml}")
    CLUSTER_NAMESPACE=$(yq -r '.spec.cluster.namespace // "default"' "${cluster_yaml}")
    CLUSTER_DOMAIN=$(yq -r '.spec.cluster.domain' "${cluster_yaml}")
    K8S_VERSION=$(yq -r '.spec.kubernetes.version' "${cluster_yaml}")

    envsubst < "${template}"
  )
}

# Render all template files in a directory, concatenated with --- separators.
# Usage: template::render_dir <dir> <cluster_yaml>
template::render_dir() {
  local dir="$1" cluster_yaml="$2"
  local first=1

  [[ -d "${dir}" ]] || { error "Template directory not found: ${dir}"; return 1; }

  for tmpl in "${dir}"/*.yaml; do
    [[ -f "${tmpl}" ]] || continue
    if (( first )); then
      first=0
    else
      echo "---"
    fi
    template::render "${tmpl}" "${cluster_yaml}"
  done
}

# Build an envsubst whitelist string from current LOK8S_SPEC_* and LOK8S_USER_* vars.
# Usage: template::envsubst_whitelist
# Outputs: string like "${LOK8S_SPEC_FOO} ${LOK8S_USER_BAR} ..."
template::envsubst_whitelist() {
  env | awk -F= '/^LOK8S_(SPEC|USER)_/ {printf "${%s} ", $1}'
}
