# shellcheck shell=bash
# argv_sentinels.bash — run code under argv spies and check that no secret
# reaches a command line.
#
# Every local user can read the argv of a process (ps, /proc/<pid>/cmdline),
# and audit tools store it. A credential must reach curl, jq or kubectl
# through a pipe, a config on stdin or a file, never as an argument. These
# helpers prove that with sentinel values:
#
#   argv_spy curl jq kubectl   put spy executables first on PATH
#   assert_argv_clean           fail when a sentinel is in a recorded argv
#
# A spy is a real executable, not a shell function, so `command curl` cannot
# go around it, and it records the argv that the kernel would show. It
# appends one line to ARGV_LOG (curl_capture.sh argv_log), then runs
# <tool>_respond when the test exports one, else the real tool. A
# <tool>_respond that calls curl_capture runs it with ARGV_LOG empty: the
# spy logged the call already.

# argv_spy <tool...>
argv_spy() {
  local dir="${BATS_TEST_TMPDIR}/spy-bin" tool real
  mkdir -p "${dir}"
  export CURL_CAPTURE_LIB="${_PROJECT_ROOT}/tests/lib/curl_capture.sh"
  : "${ARGV_LOG:=${BATS_TEST_TMPDIR}/argv.log}"
  export ARGV_LOG
  touch "${ARGV_LOG}"
  for tool in "$@"; do
    real=$(PATH="${PATH//${dir}:/}" command -v "${tool}" || true)
    cat > "${dir}/${tool}" <<SPY
#!/usr/bin/env bash
# shellcheck source=/dev/null
. "\${CURL_CAPTURE_LIB}"
argv_log ${tool} "\$@"
if declare -F ${tool}_respond >/dev/null; then
  ${tool}_respond "\$@"
  exit \$?
fi
exec "${real:-/bin/false}" "\$@"
SPY
    chmod +x "${dir}/${tool}"
  done
  case ":${PATH}:" in
    *":${dir}:"*) ;;
    *) export PATH="${dir}:${PATH}" ;;
  esac
}

# assert_argv_clean [sentinel...]: no recorded argv holds a sentinel. With
# no arguments, the sentinels are the SENTINELS array. Fails when nothing
# is in the log: an empty log proves nothing.
assert_argv_clean() {
  local -a values=("$@")
  (( ${#values[@]} )) || values=("${SENTINELS[@]}")
  if [[ ! -s "${ARGV_LOG}" ]]; then
    echo "assert_argv_clean: the argv log is empty (${ARGV_LOG})" >&2
    return 1
  fi
  local v hits=0
  for v in "${values[@]}"; do
    [[ -n "${v}" ]] || continue
    if grep -qF -- "${v}" "${ARGV_LOG}"; then
      echo "a secret is on argv: ${v}" >&2
      grep -nF -- "${v}" "${ARGV_LOG}" | head -5 >&2
      hits=$(( hits + 1 ))
    fi
  done
  (( hits == 0 ))
}
