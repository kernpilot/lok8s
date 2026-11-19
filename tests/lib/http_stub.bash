# shellcheck shell=bash
# http_stub.bash — run the real curl against a local server, with a config
# file of the user in place.
#
#   http_stub_start [cert key]  start tests/lib/http_stub.py (https with a
#                               cert and a key): STUB_PORT, STUB_LOG, STUB_PID
#   http_stub_stop              stop it (the teardown of the test file
#                               kills STUB_PID too)
#   curlrc_dir VAR              a private dir for VAR (CURL_HOME,
#                               XDG_CONFIG_HOME or HOME) with the config file
#                               curl reads there: RC_DIR, RC_TRACE, RC_OUT
#   curlrc_env VAR              in the subshell that runs the code under
#                               test: VAR names RC_DIR, the other two name
#                               no config file
#   curlrc_control VAR URL      the positive control: curl WITHOUT -q, in
#                               the environment of curlrc_env VAR, reads the
#                               file of curlrc_dir VAR (the trace appears)
#   curl_reads_xdg              0 when this curl looks in XDG_CONFIG_HOME
#                               (curl 7.73 and later)
#   stub_cert [SAN]             a throwaway self-signed certificate for
#                               SAN (default IP:127.0.0.1) in the test dir
#                               (openssl): STUB_CERT, STUB_KEY. Another call
#                               overwrites them: copy the paths first
#   keylog_control [URL]        the control of a key log test: curl with
#                               SSLKEYLOGFILE, to URL (default /control of
#                               the https stub; the path must be /control,
#                               the URL can go through a tunnel of the
#                               stub). 0: the request is in the stub log and
#                               curl wrote a key log. 1: the request is in
#                               the stub log and curl wrote no key log (its
#                               TLS backend ignores SSLKEYLOGFILE): a test
#                               that checks the key log cannot fail, so it
#                               must skip. 2: the request is not in the stub
#                               log (no connect, a TLS failure): the test
#                               fails, because a skip would hide it
#   tls_spy_on DIR              a curl function for the code under test:
#                               for each call it writes a record file in DIR
#                               (tls_spy_record), then runs the real curl.
#                               Needs utils/http.sh: the names are those of
#                               its list _HTTP_TLS_LOG_VARS
#   tls_spy_record FILE         write the record of this process to FILE:
#                               the line "record", each name of the TLS list
#                               (TLS_SPY_NAMES) that is in the exported
#                               environment, and "control" when
#                               TLS_ENV_CONTROL is exported. Names only,
#                               never a value: no credential of the host
#                               gets to the disk
#
# Unless -q is its first argument, curl reads one config file of the user:
# $CURL_HOME/.curlrc, else $XDG_CONFIG_HOME/curlrc (no dot), else
# $HOME/.curlrc. The file of curlrc_dir has a trace-ascii line (RC_TRACE:
# each header and each answer would be logged there) and an output line
# (RC_OUT: the answer of the first URL would be written there).

http_stub_start() {
  local pf="${BATS_TEST_TMPDIR}/stub.port"
  STUB_LOG="${BATS_TEST_TMPDIR}/stub.log"
  : > "${STUB_LOG}"
  rm -f -- "${pf}"
  # 3>&-: bats waits for each process that holds its fd 3.
  python3 "${_PROJECT_ROOT}/tests/lib/http_stub.py" "${pf}" "${STUB_LOG}" "$@" >/dev/null 2>&1 3>&- &
  STUB_PID=$!
  for _ in $(seq 1 200); do
    [[ -s "${pf}" ]] && break
    command sleep 0.05
  done
  [[ -s "${pf}" ]] || { echo "http_stub: no port after 10 seconds" >&2; return 1; }
  # shellcheck disable=SC2034 # read by the tests that source this file
  STUB_PORT=$(<"${pf}")
}

http_stub_stop() {
  [[ -z "${STUB_PID:-}" ]] || kill "${STUB_PID}" 2>/dev/null || :
  STUB_PID=""
}

curlrc_dir() {
  local name=.curlrc
  [[ "${1}" != XDG_CONFIG_HOME ]] || name=curlrc
  RC_DIR="${BATS_TEST_TMPDIR}/curlrc-${1}"
  RC_TRACE="${RC_DIR}/trace"
  RC_OUT="${RC_DIR}/out"
  mkdir -p "${RC_DIR}" "${BATS_TEST_TMPDIR}/curlrc-none"
  printf 'trace-ascii = "%s"\noutput = "%s"\n' "${RC_TRACE}" "${RC_OUT}" > "${RC_DIR}/${name}"
}

curlrc_env() {
  unset CURL_HOME XDG_CONFIG_HOME
  export HOME="${BATS_TEST_TMPDIR}/curlrc-none"
  export "${1}=${RC_DIR}"
}

curlrc_control() {
  ( curlrc_env "${1}" && command curl -sS "${2}" ) >/dev/null 2>&1 || :
  [[ -s "${RC_TRACE}" ]]
}

curl_reads_xdg() {
  local v major minor
  v=$(command curl -q --version 2>/dev/null) || return 1
  v=${v#curl }
  v=${v%% *}
  major=${v%%.*}
  minor=${v#*.}
  minor=${minor%%.*}
  [[ "${major}" =~ ^[0-9]+$ && "${minor}" =~ ^[0-9]+$ ]] || return 1
  (( major > 7 || (major == 7 && minor >= 73) ))
}

stub_cert() {
  STUB_CERT="${BATS_TEST_TMPDIR}/stub.crt"
  STUB_KEY="${BATS_TEST_TMPDIR}/stub.key"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=127.0.0.1 \
    -addext "subjectAltName=${1:-IP:127.0.0.1}" -keyout "${STUB_KEY}" -out "${STUB_CERT}" 2>/dev/null
}

keylog_control() {
  local log="${BATS_TEST_TMPDIR}/control.keys" before after
  rm -f -- "${log}"
  # The stub writes the log line before it answers.
  before=$(grep -c '^GET /control none$' "${STUB_LOG}") || :
  SSLKEYLOGFILE="${log}" command curl -q -sS -o /dev/null "${1:-https://127.0.0.1:${STUB_PORT}/control}" 2>/dev/null || :
  after=$(grep -c '^GET /control none$' "${STUB_LOG}") || :
  if [[ "${after}" != "$((before + 1))" ]]; then
    echo "keylog_control: the control request did not get to the https stub, so the test cannot tell a skip from a fault" >&2
    return 2
  fi
  [[ -s "${log}" ]]
}

tls_spy_on() {
  TLS_SPY_DIR="${1}"
  TLS_SPY_NAMES="${_HTTP_TLS_LOG_VARS[*]:-}"
  export TLS_SPY_DIR TLS_SPY_NAMES
  [[ -n "${TLS_SPY_NAMES}" ]] || { echo "tls_spy_on: no TLS list: source utils/http.sh first" >&2; return 1; }
  mkdir -p "${TLS_SPY_DIR}"
  curl() {
    tls_spy_record "$(command mktemp "${TLS_SPY_DIR}/env.XXXXXX")"
    command curl "$@"
  }
  export -f curl tls_spy_record
}

tls_spy_record() {
  local name
  local -a names exported
  read -ra names <<<"${TLS_SPY_NAMES:-}"
  mapfile -t exported < <(compgen -e)
  {
    echo record
    (( ${#names[@]} > 0 )) || echo "no-list"
    for name in "${exported[@]}"; do
      [[ " ${TLS_SPY_NAMES:-} " != *" ${name} "* ]] || echo "${name}"
    done
    [[ " ${exported[*]} " != *" TLS_ENV_CONTROL "* ]] || echo control
  } > "${1}"
}
