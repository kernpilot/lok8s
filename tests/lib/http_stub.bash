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
#
# Unless -q is its first argument, curl reads one config file of the user:
# $CURL_HOME/.curlrc, else $XDG_CONFIG_HOME/curlrc (no dot), else
# $HOME/.curlrc. The file of curlrc_dir has a trace-ascii line (RC_TRACE:
# each header and each answer would be logged there) and an output line
# (RC_OUT: the answer of the first URL would be written there).

http_stub_start() {
  local pf="${BATS_TEST_TMPDIR}/stub.port" i
  STUB_LOG="${BATS_TEST_TMPDIR}/stub.log"
  : > "${STUB_LOG}"
  rm -f -- "${pf}"
  # 3>&-: bats waits for each process that holds its fd 3.
  python3 "${_PROJECT_ROOT}/tests/lib/http_stub.py" "${pf}" "${STUB_LOG}" "$@" >/dev/null 2>&1 3>&- &
  STUB_PID=$!
  for i in $(seq 1 200); do
    [[ -s "${pf}" ]] && break
    command sleep 0.05
  done
  [[ -s "${pf}" ]] || { echo "http_stub: no port after 10 seconds" >&2; return 1; }
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
