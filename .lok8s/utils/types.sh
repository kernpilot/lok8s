# shellcheck shell=bash
# argsh custom type validators for lok8s

# to::domain -- validate a lok8s domain name at parse time.
# Called by argsh when user provides a domain positional/flag value.
# NOT called on defaults (argsh skips type validation for pre-set locals).
#
# Validates:
#   1. Character format (alphanumeric, dots, hyphens)
#   2. Directory exists under .lok8s/
#   3. Contains cluster.lok8s.yaml or deploy.lok8s.yaml
#
# Usage in args array: 'domain:~domain' 'Description'
to::domain() {
  local value="${1}"
  local path_clusters="${PATH_CLUSTERS:-${PATH_BASE}/clusters}"

  # Resolve default: DOMAIN_NAME env → .active file
  if [[ -z "${value}" ]]; then
    value="${DOMAIN_NAME:-}"
  fi
  if [[ -z "${value}" && -f "${path_clusters}/.active" ]]; then
    value="$(cat "${path_clusters}/.active")"
  fi
  if [[ -z "${value}" ]]; then
    echo "no domain specified" >&2
    return 1
  fi

  # Character validation (prevent path traversal / injection)
  if [[ ! "${value}" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*$ ]]; then
    echo "invalid domain name: ${value}" >&2
    return 1
  fi

  local base="${path_clusters}/${value}"
  if [[ ! -d "${base}" ]]; then
    echo "domain not found: clusters/${value}/" >&2
    echo "Available domains:" >&2
    local d name
    for d in "${path_clusters}"/*/; do
      [[ -d "${d}" ]] || continue
      name=$(basename "${d}")
      [[ "${name}" == .* ]] && continue
      echo "  ${name}" >&2
    done
    return 1
  fi

  if [[ ! -f "${base}/cluster.lok8s.yaml" ]] && [[ ! -f "${base}/deploy.lok8s.yaml" ]]; then
    echo "domain '${value}' has no cluster.lok8s.yaml or deploy.lok8s.yaml" >&2
    return 1
  fi

  echo "${value}"
}
