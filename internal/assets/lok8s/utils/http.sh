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
# the config line `proto = "-all"`, and returns 1. That line turns off every
# protocol, so curl stops before it connects (curl 8.20 refuses the line,
# older versions refuse the URL) instead of sending the request without the
# credential. A --proto after the -K would turn a protocol on again: the
# callers put theirs first. Callers check each credential first with
# http::credential_ok, which names the variable.
# The Go twins are curlConfigQuote and curlConfigData (internal/driver/kkp).
# Usage: http::curl_config <option> <value> [<option> <value> ...]
http::curl_config() {
  local LC_ALL=C
  # opt, value and out hold the credential line. +x: a local takes the
  # export flag of an exported variable of the same name (no child runs
  # here today). +i: under shopt localvar_inherit (BASHOPTS in the
  # environment can turn it on), a local takes the attributes and the value
  # of a variable of the same name in a caller scope, and an integer one
  # (declare -i) would print the credential in an arithmetic error. A
  # readonly name (also a readonly nameref) makes the local fail: then the
  # function refuses as for a control character, with the line curl
  # refuses.
  local +x +i opt value out || {
    error "http::curl_config: a local cannot be declared (a readonly name or a nameref of the caller)"
    printf 'proto = "-all"\n'
    return 1
  }
  out=""
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
      printf 'proto = "-all"\n'
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

# Unset the controls in the environment that make curl or its TLS library
# write session keys, or a trace with plaintext, before a call that carries
# a credential. Call it as the first command of the subshell that runs
# curl, joined to curl with &&, so that the caller keeps its environment:
#   ( http::tls_log_vars_unset && curl -q ... )
# Each curl call of the bash tree has this form, except the calls in the
# allowlist of tests/lib/curl_gate.bash (the source gate).
# The list:
#   SSLKEYLOGFILE        curl appends the TLS session keys to this file, for
#                        each backend that has a key log. The keys decrypt a
#                        captured exchange.
#   GNUTLS_KEYLOGFILE    the same key log under the GnuTLS name, which
#                        GnuTLS also reads.
#   GNUTLS_DEBUG_LEVEL   GnuTLS writes debug output on stderr; at high levels
#                        it holds key material.
#   SSLTRACE, SSLDEBUG, SSLDEBUGFILE
#                        the trace and debug output of NSS (an older
#                        NSS-backed curl, NSS built with DEBUG or TRACE). It
#                        can hold the plaintext (a bearer, a signed URL).
#                        Without SSLDEBUGFILE it goes to stderr.
# A readonly name cannot be unset. The function then unsets the others,
# prints one error line for each readonly name and returns 1, so
# `&& curl` does not run curl. The caller then gets that 1, not a status
# of curl. An empty or unset list is refused the same way (one error
# line, status 1): with no names the function would clear nothing.
# Not covered: a curl or TLS library built to write keys or a trace by
# itself (wolfSSL built with --enable-keylog-export, a debug build of a TLS
# library), and an environment that an attacker controls (who sets the
# environment can also set LD_PRELOAD).
#
# _HTTP_TLS_LOG_VARS is the one list. The tests read it, and tests compare
# it with the two copies that cannot read this file: the Go list
# (credentials.TLSLogVars) and the script of the agent CronJob
# (libs/kubehz/manifests/agent/cronjob.yaml).
_HTTP_TLS_LOG_VARS=(SSLKEYLOGFILE GNUTLS_KEYLOGFILE GNUTLS_DEBUG_LEVEL SSLTRACE SSLDEBUG SSLDEBUGFILE)

# Usage: http::tls_log_vars_unset
http::tls_log_vars_unset() {
  local _http_name _http_rc=0
  http::_tls_log_list_ok || return 1
  for _http_name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    unset -v "${_http_name}" 2>/dev/null && continue
    http::_tls_log_var_stop "${_http_name}"
    _http_rc=1
  done
  return "${_http_rc}"
}

# http::tls_log_vars_readonly: print the names of the list that are
# readonly here (one line, a space between two names). Status 1 when no
# name is readonly. The probe of each name runs in a subshell, so the
# function changes no variable.
# Usage: names=$(http::tls_log_vars_readonly)
http::tls_log_vars_readonly() {
  local _http_name
  local -a _http_found=()
  for _http_name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    ( unset -v "${_http_name}" ) 2>/dev/null || _http_found+=("${_http_name}")
  done
  (( ${#_http_found[@]} > 0 )) || return 1
  printf '%s\n' "${_http_found[*]}"
}

# http::tls_log_vars_ok: status 0 when no name of the list is readonly.
# Else the same error line as http::tls_log_vars_unset for each readonly
# name, and status 1. For a caller that sends the stderr of its curl call
# to /dev/null: the check before the call shows the cause, and the
# caller stops.
# Usage: http::tls_log_vars_ok || return 1
http::tls_log_vars_ok() {
  local _http_name _http_rc=0
  http::_tls_log_list_ok || return 1
  for _http_name in "${_HTTP_TLS_LOG_VARS[@]}"; do
    ( unset -v "${_http_name}" ) 2>/dev/null && continue
    http::_tls_log_var_stop "${_http_name}"
    _http_rc=1
  done
  return "${_http_rc}"
}

# http::tls_log_var_readonly_text <names>: the start of each message
# about a readonly name of the list, "<names> is readonly, so lo cannot
# clear it for curl". The callers add what does not run.
http::tls_log_var_readonly_text() {
  printf '%s is readonly, so lo cannot clear it for curl' "${1}"
}

# The error line for a readonly name of the list.
http::_tls_log_var_stop() {
  error "$(http::tls_log_var_readonly_text "${1}"): the request does not run"
}

# Status 0 when the list has at least one name. Else one error line and
# status 1: an empty or unset list would clear nothing, and the request
# would run with each TLS log and trace control of the environment. (The
# ${…[@]+…} form expands to nothing for an unset array under set -u too.)
http::_tls_log_list_ok() {
  set -- ${_HTTP_TLS_LOG_VARS[@]+"${_HTTP_TLS_LOG_VARS[@]}"}
  (( $# > 0 )) && return 0
  error "the list of TLS log and trace variables (_HTTP_TLS_LOG_VARS) is empty or not set: the request does not run"
  return 1
}
