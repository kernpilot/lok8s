# shellcheck shell=sh disable=SC2034
# SC2034: the CURL_* results are for the stubs that source this file.
# curl_capture.sh — POSIX sh helpers for curl and kubectl stubs in the bats
# tests. Bash suites source this file. The agent script suites run under sh
# and put it in front of their stubs.
#
# The code under test hands credentials to curl in a config (`-K -`, or
# `-K <file>` for a process substitution or a here-document on /dev/fd/3)
# and request bodies on stdin (`--data-binary @-`), never on argv. A stub
# that reads only argv would miss them, so a stub calls curl_capture to
# read the call the way curl reads it.
#
# curl_capture <curl args...> sets:
#   CURL_URL      the last http:// or https:// argument
#   CURL_METHOD   the value of -X/--request ("" when none)
#   CURL_HEADERS  every header, one for each line: -H/--header values, then
#                 the config's `header` lines
#   CURL_USER     -u/--user, or the config's `user` line
#   CURL_BODY     -d/--data/--data-raw/--data-binary ("@-" reads stdin), or
#                 the config's data, data-raw or data-binary line
#   CURL_CONFIG   the raw config text ("" when no -K/--config)
# When ARGV_LOG names a file, curl_capture appends one line to it: "curl"
# and the arguments. The sentinel tests grep that log for secret values.
curl_capture() {
  CURL_URL=""
  CURL_METHOD=""
  CURL_HEADERS=""
  CURL_USER=""
  CURL_BODY=""
  CURL_CONFIG=""
  argv_log curl "$@"
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -H | --header)
        CURL_HEADERS="${CURL_HEADERS}${2}
"
        shift
        ;;
      -u | --user) CURL_USER="$2"; shift ;;
      -X | --request) CURL_METHOD="$2"; shift ;;
      -d | --data | --data-raw | --data-binary)
        if [ "$2" = "@-" ]; then
          CURL_BODY=$(cat; printf x)
          CURL_BODY="${CURL_BODY%x}"
        else
          CURL_BODY="$2"
        fi
        shift
        ;;
      -K | --config)
        if [ "$2" = "-" ]; then
          CURL_CONFIG=$(cat)
        else
          CURL_CONFIG=$(cat "$2")
        fi
        shift
        ;;
      http://* | https://*) CURL_URL="$1" ;;
    esac
    shift
  done
  [ -n "${CURL_CONFIG}" ] || return 0
  _cc_ifs="${IFS}"
  IFS='
'
  set -f
  for _cc_line in ${CURL_CONFIG}; do
    case "${_cc_line}" in
      *' = "'*'"') : ;;
      *) continue ;;
    esac
    _cc_opt="${_cc_line%% = \"*}"
    _cc_val="${_cc_line#*= \"}"
    _cc_val=$(curl_unquote "${_cc_val%\"}"; printf x)
    _cc_val="${_cc_val%x}"
    case "${_cc_opt}" in
      header)
        CURL_HEADERS="${CURL_HEADERS}${_cc_val}
"
        ;;
      user) CURL_USER="${_cc_val}" ;;
      data | data-raw | data-binary) CURL_BODY="${_cc_val}" ;;
    esac
  done
  set +f
  IFS="${_cc_ifs}"
}

# curl_unquote <text>: undo the quoting of a curl config value (the text
# between the quotes). \n, \r, \t and \v are the control bytes, and a
# backslash before any other character is that character.
curl_unquote() {
  _cu_in="$1"
  _cu_out=""
  while [ -n "${_cu_in}" ]; do
    _cu_rest="${_cu_in#?}"
    _cu_c="${_cu_in%"${_cu_rest}"}"
    # shellcheck disable=SC1003  # a single backslash, not an escaped quote
    if [ "${_cu_c}" = '\' ] && [ -n "${_cu_rest}" ]; then
      _cu_in="${_cu_rest}"
      _cu_rest="${_cu_in#?}"
      _cu_c="${_cu_in%"${_cu_rest}"}"
      case "${_cu_c}" in
        n) _cu_c='
' ;;
        t) _cu_c='	' ;;
        r) _cu_c=$(printf '\r') ;;
        v) _cu_c=$(printf '\v') ;;
      esac
    fi
    _cu_out="${_cu_out}${_cu_c}"
    _cu_in="${_cu_rest}"
  done
  printf '%s' "${_cu_out}"
}

# kubectl_env_file <kubectl args...>: for a call with
# --from-env-file=/dev/stdin, print the env file from stdin. Logs the call
# like curl_capture.
kubectl_env_file() {
  argv_log kubectl "$@"
  for _ke_arg in "$@"; do
    if [ "${_ke_arg}" = "--from-env-file=/dev/stdin" ]; then
      cat
      return 0
    fi
  done
}

# argv_log <tool> <args...>: one line in ARGV_LOG (when set): the tool and
# its arguments, with a line break in an argument written as \n. Builtins
# only: the spies of tests/lib/argv_sentinels.bash call this function, and
# awk or sed may be spies themselves.
argv_log() {
  [ -n "${ARGV_LOG:-}" ] || return 0
  _al_rest="$*"
  _al_out=""
  while :; do
    case "${_al_rest}" in
      *'
'*)
        _al_out="${_al_out}${_al_rest%%'
'*}\\n"
        _al_rest="${_al_rest#*'
'}"
        ;;
      *)
        _al_out="${_al_out}${_al_rest}"
        break
        ;;
    esac
  done
  printf '%s\n' "${_al_out}" >> "${ARGV_LOG}"
}
