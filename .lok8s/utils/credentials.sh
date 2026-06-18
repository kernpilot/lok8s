# shellcheck shell=bash
# credentials.sh — provider credential validation
# Centralizes environment variable checks for cloud provider credentials.

# Require specific environment variables for a provider.
# Usage: credentials::require <provider>
# Returns 0 if all required vars are set, 1 with error messages if not.
credentials::require() {
  local provider="$1"
  local -a missing=()

  case "${provider}" in
    hetzner)
      [[ -n "${HCLOUD_TOKEN:-}" ]] || missing+=("HCLOUD_TOKEN")
      ;;
    aws)
      [[ -n "${AWS_ACCESS_KEY_ID:-}" ]] || missing+=("AWS_ACCESS_KEY_ID")
      [[ -n "${AWS_SECRET_ACCESS_KEY:-}" ]] || missing+=("AWS_SECRET_ACCESS_KEY")
      ;;
    *)
      error "unknown provider '${provider}' for credential check"
      return 1
      ;;
  esac

  if (( ${#missing[@]} > 0 )); then
    local var
    for var in "${missing[@]}"; do
      error "required environment variable ${var} is not set"
    done
    return 1
  fi
  return 0
}
