# shellcheck shell=bash
# template.sh — YAML template rendering via envsubst
# Exports variables from a cluster spec YAML and renders template files.

import ^utils/verbose

# Render a template file by exporting vars from a cluster spec YAML.
# Usage: template::render <template_file> <cluster_yaml>
# Outputs rendered YAML to stdout.
template::render() {
  local template="${1}" cluster_yaml="${2}"

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
  local dir="${1}" cluster_yaml="${2}"
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

# ── envsubst flavor shim ─────────────────────────────────────────────────────
# Everywhere lok8s renders manifests it restricts substitution to an explicit
# variable list, so arbitrary `$…` in user content survives untouched. GNU
# gettext envsubst takes that list as one SHELL-FORMAT positional arg
# ('${A} ${B}'); renvsubst — the envsubst `b` curates (.bin/b.yaml alias) —
# REJECTS positional args ("ERROR: Unknown flag", exit 1, which under pipefail
# fails the whole build) and expects --variable/--prefix filters instead.
# template::envsubst takes the GNU SHELL-FORMAT string and speaks whichever
# dialect the `envsubst` on PATH understands. Both flavors substitute a
# listed-but-unset var with the empty string, so behavior is identical.
# Restricted call sites go through this shim; a bare `envsubst < file`
# (substitute-everything) works in both flavors and needs no shim.

# Flavor is per-process stable — detect once, cache.
declare -g _TEMPLATE_ENVSUBST_FLAVOR=""

# Usage: template::envsubst_flavor  → echoes "gnu" or "renvsubst"
template::envsubst_flavor() {
  if [[ -z "${_TEMPLATE_ENVSUBST_FLAVOR}" ]]; then
    if envsubst --version 2>/dev/null | grep -q "GNU gettext"; then
      _TEMPLATE_ENVSUBST_FLAVOR="gnu"
    else
      _TEMPLATE_ENVSUBST_FLAVOR="renvsubst"
    fi
  fi
  echo "${_TEMPLATE_ENVSUBST_FLAVOR}"
}

# Substitute stdin→stdout restricted to the vars named in a GNU SHELL-FORMAT
# string. An empty/whitespace-only list replaces NOTHING (GNU semantics) —
# critical under renvsubst, whose filterless default substitutes EVERYTHING.
# Usage: … | template::envsubst '${VAR_A} ${VAR_B} '
template::envsubst() {
  local shell_format="${1:-}"
  local -a vars=()
  local tok
  # Word-splitting the format string IS the parse: '${A} ${B}' → A B.
  # shellcheck disable=SC2086
  for tok in ${shell_format}; do
    tok="${tok#\$}"
    tok="${tok#\{}"
    tok="${tok%\}}"
    [[ -z "${tok}" ]] || vars+=("${tok}")
  done
  if (( ${#vars[@]} == 0 )); then
    cat
  elif [[ "$(template::envsubst_flavor)" == "gnu" ]]; then
    envsubst "${shell_format}"
  else
    local -a args=()
    local v
    for v in "${vars[@]}"; do args+=("--variable=${v}"); done
    envsubst "${args[@]}"
  fi
}
