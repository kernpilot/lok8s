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
# also get a second line). Then the function prints nothing and returns 1.
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
    [[ "${value}" != *[[:cntrl:]]* ]] || return 1
    out+="${opt} = \"${value}\""$'\n'
  done
  printf '%s' "${out}"
}
