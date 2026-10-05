# shellcheck shell=bash
# http.sh — shared HTTP utilities

# Validate that a URL uses HTTPS.
# Usage: http::require_https <url> [label]
# Returns 1 if URL does not use https:// scheme.
http::require_https() {
  local url="${1}" label="${2:-URL}"

  if [[ "${url}" != https://* ]]; then
    error "${label} must use HTTPS: ${url}"
    error "Plain HTTP is not allowed for security reasons"
    return 1
  fi
  return 0
}

# Print curl config lines, one `<option> = "<value>"` line for each pair.
# Pipe them into `curl -K -`, or give them as `-K <(http::curl_config …)`
# when stdin carries the request body. Thus a credential reaches curl
# through a pipe and not through argv: every local user can read argv
# (ps, /proc/<pid>/cmdline), and audit tools store it. printf is a bash
# builtin, so no process gets the value on its command line.
#
# The function quotes the value: a backslash and a double quote get a
# backslash. A data option (data, data-raw, data-binary) also gets \n, \r
# and \t for a line feed, a carriage return and a tab, which curl turns
# back into the same bytes. Any other control character, or a line break
# in another option, makes the function refuse the value: the line would
# end there, and curl would read the rest as an option (a header could
# also get a second line). Then the function prints an error on stderr and
# one config line that curl refuses (an unknown option), so curl stops
# before it connects instead of sending the request without the
# credential, and returns 1. Callers check each credential first with
# http::credential_ok, which names the variable.
# The Go twins are curlConfigQuote and curlConfigData (internal/driver/kkp).
# Usage: http::curl_config <option> <value> [<option> <value> ...]
http::curl_config() {
  local LC_ALL=C opt value out=""
  while (( $# >= 2 )); do
    opt="${1}"
    value="${2}"
    shift 2
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    case "${opt}" in
      data | data-raw | data-binary)
        value="${value//$'\n'/\\n}"
        value="${value//$'\r'/\\r}"
        value="${value//$'\t'/\\t}"
        ;;
    esac
    if [[ "${value}" == *[[:cntrl:]]* ]]; then
      error "a value for the curl option ${opt} holds a control character: lo sends no request"
      printf 'lo-refused-a-value = "%s"\n' "${opt}"
      return 1
    fi
    out+="${opt} = \"${value}\""$'\n'
  done
  printf '%s' "${out}"
}

# Succeed when no named variable holds a control character. A credential
# goes into a curl config line (http::curl_config), and a control character
# would end that line. A common cause is a CR at the end of a value from a
# file with CRLF line ends. Each refused variable gets an error with its
# name. An unset or empty variable passes: the caller decides on that.
# Usage: http::credential_ok <variable name>...
http::credential_ok() {
  local LC_ALL=C name rc=0
  for name in "${@}"; do
    if [[ "${!name:-}" == *[[:cntrl:]]* ]]; then
      error "environment variable ${name} must not contain a control character"
      echo "  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again." >&2
      rc=1
    fi
  done
  return "${rc}"
}
