# shellcheck shell=bash
# runtime.sh — argsh-runtime shims + library loading for operator hooks.
#
# The framework libraries are argsh scripts; the operator container is
# plain bash. This file provides the minimal runtime they need:
#   - import:  no-op (everything is sourced eagerly below)
#   - :args:   positional-only subset of argsh's parser — enough for the
#              driver contract (driver::provision <domain>, ...). Flag
#              specs are ignored; hooks call functions with exact
#              positionals.
#   - :usage:  hooks never invoke dispatchers; fail loudly if one runs.
#
# Sourcing order matters: utils first (error/warn/debug, ip::, ...),
# then shared libs, then the caller sources its driver pieces.
#
# State layout (kubeconfigs, rendered specs, secret cache) lives under
# LOK8S_STATE_DIR (a writable volume), laid out exactly like a lok8s
# project so the driver contract works unchanged:
#   $PATH_BASE/clusters/<domain>/cluster.lok8s.yaml
#   $PATH_BASE/.kubeconfig/<cluster>.yaml

RUNTIME_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

import() { :; }

# Positional-only :args. Reads the caller's `args` array (spec/desc
# pairs), assigns successive positional parameters to the caller's
# locals via bash dynamic scoping. Flag specs (containing '|') are
# skipped — operator code paths don't use them.
:args() {
  shift # usage description
  local _spec _name
  local -i _i
  # shellcheck disable=SC2154 # `args` is the caller's spec array (argsh contract)
  for (( _i = 0; _i < ${#args[@]}; _i += 2 )); do
    _spec="${args[_i]}"
    [[ "${_spec}" == *'|'* ]] && continue
    _name="${_spec//-/_}"
    if (( $# > 0 )); then
      printf -v "${_name}" '%s' "$1"
      shift
    fi
  done
}

:usage() {
  echo "error: argsh dispatcher invoked inside the operator runtime: $*" >&2
  return 1
}

# ── framework env ──────────────────────────────────────────
export PATH_LOK8S="${RUNTIME_DIR}"
export PATH_BASE="${LOK8S_STATE_DIR:-/var/lib/lok8s}"
export PATH_CLUSTERS="${PATH_BASE}/clusters"
export PATH_SECRETS="${PATH_BASE}/.secrets"
export KUSTOMIZE_PLUGIN_HOME="${KUSTOMIZE_PLUGIN_HOME:-/usr/local/kustomize-plugins}"
mkdir -p "${PATH_CLUSTERS}" "${PATH_SECRETS}" "${PATH_BASE}/.kubeconfig"

# ── libraries ──────────────────────────────────────────────
for _f in "${RUNTIME_DIR}"/utils/*.sh; do
  # shellcheck source=/dev/null
  [[ -f "${_f}" ]] && source "${_f}"
done
for _f in "${RUNTIME_DIR}"/lib/*; do
  # shellcheck source=/dev/null
  [[ -f "${_f}" ]] && source "${_f}"
done
unset _f

# ── helpers ────────────────────────────────────────────────

# Patch a CR's status subresource; failures are logged, not masked.
# Usage: hook::patch_status <kind> <name> <namespace> <merge-json>
hook::patch_status() {
  local kind="$1" name="$2" namespace="$3" patch="$4"
  if ! kubectl patch "${kind}" "${name}" -n "${namespace}" \
    --type merge --subresource status -p "${patch}" 2>&1; then
    echo "warn: failed to patch ${kind} ${namespace}/${name} status" >&2
  fi
}
