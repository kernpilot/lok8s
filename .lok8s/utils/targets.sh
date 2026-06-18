# shellcheck shell=bash
# targets.sh — shared target discovery
# Centralizes the for-dir-in-targets/*/ pattern used by build, deploy, env, status.

# Discover targets for a domain. Returns target names, one per line.
# Usage: targets::discover <domain> [requested_targets...]
# If requested_targets are given, only return those that exist.
targets::discover() {
  local domain="$1"; shift
  local -a requested=("$@")
  local targets_dir="${PATH_CLUSTERS}/${domain}/targets"

  [[ -d "${targets_dir}" ]] || return 0

  if (( ${#requested[@]} > 0 )); then
    local t
    for t in "${requested[@]}"; do
      [[ -d "${targets_dir}/${t}" ]] && echo "${t}"
    done
  else
    local dir
    for dir in "${targets_dir}"/*/; do
      [[ -d "${dir}" ]] || continue
      echo "$(basename "${dir}")"
    done
  fi
}
