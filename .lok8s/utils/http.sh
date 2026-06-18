# shellcheck shell=bash
# http.sh — shared HTTP utilities

# Validate that a URL uses HTTPS.
# Usage: http::require_https <url> [label]
# Returns 1 if URL does not use https:// scheme.
http::require_https() {
  local url="$1" label="${2:-URL}"

  if [[ "${url}" != https://* ]]; then
    error "${label} must use HTTPS: ${url}"
    error "Plain HTTP is not allowed for security reasons"
    return 1
  fi
  return 0
}
