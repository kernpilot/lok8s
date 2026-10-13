#!/usr/bin/env bats
# yqbound_test.bats — yqbound::run (.lok8s/utils/yqbound.sh): one yq call with a
# time limit that always holds (pure bash, no timeout(1)) and a data-memory
# cap (ulimit -d, best effort), and an exit status per outcome: 0 success,
# 1 a yq error of its own, 2 the time limit, 3 the memory cap, 4 no
# temporary directory or FIFO, 5 an unknown flag (an old yq), 64+N the
# signal N.

setup() {
  load "../test_helper"
  setup_tmpdir
  source "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh"
  # Stubs come first on PATH. `timeout` is there too and fails as a missing
  # command would, after it logs the call: the helper must never need it
  # (macOS has no GNU timeout by default).
  STUBS="${BATS_TEST_TMPDIR}/stub-bin"
  mkdir -p "${STUBS}"
  printf '#!/usr/bin/env bash\necho "timeout $*" >> "%s"\nexit 127\n' "${BATS_TEST_TMPDIR}/timeout.log" > "${STUBS}/timeout"
  chmod +x "${STUBS}/timeout"
  PATH="${STUBS}:${PATH}"
  export TMPDIR="${BATS_TEST_TMPDIR}/tmp"
  mkdir -p "${TMPDIR}"
  printf 'a: 1\n' > "${BATS_TEST_TMPDIR}/in.yaml"
}

teardown() { teardown_tmpdir; }

# _yq <script-body> — the yq stub.
_yq() {
  printf '#!/usr/bin/env bash\n%s\n' "${1}" > "${STUBS}/yq"
  chmod +x "${STUBS}/yq"
}

# _run_bound <seconds> — yqbound::run on the test file; sets rc, and the
# stdout / stderr files.
_run_bound() {
  rc=0
  yqbound::run "${1}" 262144 "${BATS_TEST_TMPDIR}/in.yaml" -r '.a' \
    > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
}

# _clean — no timeout(1) call and no temporary directory left behind.
_clean() {
  [ ! -s "${BATS_TEST_TMPDIR}/timeout.log" ] || { echo "timeout(1) was called:" >&2; cat "${BATS_TEST_TMPDIR}/timeout.log" >&2; return 1; }
  [ -z "$(ls -A "${TMPDIR}")" ] || { echo "left in TMPDIR:" >&2; ls -A "${TMPDIR}" >&2; return 1; }
}

@test "yqbound::run: success hands over stdout byte for byte, and stderr" {
  _yq 'printf "value\n\n"; echo "a note" >&2; echo "args: $*" >&2'
  _run_bound 5
  [ "${rc}" -eq 0 ]
  [ "$(od -c < "${BATS_TEST_TMPDIR}/out")" == "$(printf 'value\n\n' | od -c)" ]
  [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "a note
args: -r .a ${BATS_TEST_TMPDIR}/in.yaml" ]
  _clean
}

@test "yqbound::run: the time limit holds without timeout(1) on PATH" {
  _yq 'exec sleep 30'
  local start=${SECONDS}
  _run_bound 1
  [ "${rc}" -eq 2 ] || { echo "rc ${rc}" >&2; return 1; }
  (( SECONDS - start < 5 )) || { echo "took $(( SECONDS - start )) s" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  _clean
}

@test "yqbound::run: yq gets the data-memory cap (ulimit -d)" {
  _yq 'echo "data memory: $(ulimit -d)"'
  _run_bound 5
  [ "${rc}" -eq 0 ]
  [ "$(cat "${BATS_TEST_TMPDIR}/out")" == "data memory: 262144" ]
  _clean
}

@test "yqbound::run: a signal exit is 64+N (SIGSEGV: 75), a Go allocation failure is the memory status" {
  # No core file: a core dump holds the environment of the process (tokens
  # included), and it lands in the working directory. The stub turns core
  # dumps off and dies in the test directory, which the check below reads.
  _yq 'ulimit -c 0; cd "'"${BATS_TEST_TMPDIR}"'" && kill -SEGV $$'
  _run_bound 5
  [ "${rc}" -eq 75 ] || { echo "SIGSEGV: rc ${rc}" >&2; return 1; }
  if compgen -G "${BATS_TEST_TMPDIR}/core*" >/dev/null; then
    echo "the SIGSEGV stub left a core file" >&2; return 1
  fi
  _yq 'kill -KILL $$'
  _run_bound 5
  [ "${rc}" -eq 73 ] || { echo "SIGKILL: rc ${rc}" >&2; return 1; }
  local message
  for message in "fatal error: runtime: out of memory" "runtime: cannot allocate memory" \
    "runtime: goroutine stack exceeds 1000000000-byte limit"; do
    _yq "echo '${message}' >&2; echo 'goroutine 1 [running]:' >&2; exit 2"
    _run_bound 5
    [ "${rc}" -eq 3 ] || { echo "${message}: rc ${rc}" >&2; return 1; }
    [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "${message}: the runtime dump reached stderr" >&2; return 1; }
  done
  _clean
}

@test "yqbound::run: an unknown flag (an old yq) is its own status, the usage text dropped" {
  _yq 'echo "Error: unknown flag: --yaml-fix-merge-anchor-to-spec" >&2; echo "Usage:" >&2; exit 1'
  _run_bound 5
  [ "${rc}" -eq 5 ] || { echo "rc ${rc}" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/err" ]
  _clean
}

@test "yqbound::run: a yq error of its own passes its stderr on" {
  _yq 'echo "Error: bad file" >&2; exit 1'
  _run_bound 5
  [ "${rc}" -eq 1 ]
  [ "$(cat "${BATS_TEST_TMPDIR}/err")" == "Error: bad file" ]
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  _clean
}

@test "yqbound::run: no temporary directory is its own status, and yq does not run" {
  _yq 'echo ran > "'"${BATS_TEST_TMPDIR}"'/ran"'
  TMPDIR="${BATS_TEST_TMPDIR}/no-such-dir" _run_bound 5
  [ "${rc}" -eq 4 ]
  [ ! -e "${BATS_TEST_TMPDIR}/ran" ]
}

@test "yqbound::run: the watchdog waits the full time limit (a slower yq is not stopped early)" {
  # The macOS failure of `read -t … <> <(:)`: the read returned at once and
  # the watchdog stopped every yq. A yq that takes 2 s under a 6 s limit must
  # succeed and take its 2 s.
  _yq 'sleep 2; echo done'
  local start=${SECONDS}
  _run_bound 6
  [ "${rc}" -eq 0 ] || { echo "rc ${rc}" >&2; return 1; }
  [ "$(cat "${BATS_TEST_TMPDIR}/out")" == "done" ]
  (( SECONDS - start >= 2 ))
  _clean
}

@test "the bash tree waits on no /dev/fd pipe (\`<> <(\` does not work on macOS)" {
  local hits
  # Code lines only: a comment may name the construct.
  hits="$(grep -rnE '^[^#]*<>[[:space:]]*<\(' "${_PROJECT_ROOT}/.lok8s" "${_PROJECT_ROOT}/internal/assets/lok8s" 2>/dev/null || true)"
  [ -z "${hits}" ] || { echo "a read-write redirect from a process substitution:" >&2; echo "${hits}" >&2; return 1; }
}

@test "yqbound::run: a successful yq under set -e outside || returns 0 with its output" {
  _yq 'echo value'
  # A fresh bash: errexit is live there (bats runs the test body in a
  # context that a `||` would switch off).
  run bash -ec 'source "$1"; yqbound::run 5 262144 "$2" -r .a; echo "after"' _ \
    "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml"
  [ "${status}" -eq 0 ] || { echo "status ${status}: ${output}" >&2; return 1; }
  [ "${output}" == "value
after" ]
  # A `kill` that fails (the watchdog is gone already) must not stop it either.
  run bash -ec 'kill() { builtin kill "$@" 2>/dev/null; return 1; }; source "$1"; yqbound::run 5 262144 "$2" -r .a; echo "after"' _ \
    "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml"
  [ "${status}" -eq 0 ] || { echo "status ${status}: ${output}" >&2; return 1; }
  [ "${output}" == "value
after" ]
  _clean
}

# _ppid <pid> / _children <pid> — from /proc where it exists (the CI bats
# container has no ps), else from ps (macOS).
_ppid() {
  if [[ -r "/proc/${1}/stat" ]]; then
    local stat state ppid
    stat="$(< "/proc/${1}/stat")" || return 1
    stat="${stat##*) }"
    read -r state ppid _ <<< "${stat}"
    echo "${ppid}"
  else
    ps -o ppid= -p "${1}" | tr -d ' '
  fi
}
_children() {
  if [[ -d /proc/self ]]; then
    local dir
    for dir in /proc/[0-9]*; do
      [[ "$(_ppid "${dir#/proc/}" 2>/dev/null)" != "${1}" ]] || echo "${dir#/proc/}"
    done
  else
    ps -A -o pid=,ppid= | awk -v p="${1}" '$2 == p { print $1 }'
  fi
}

# _interrupt <signal> <status> — send <signal> to the body of yqbound::run (the
# subshell with the traps) while yq runs: yq AND the watchdog stop, the
# temporary directory goes, the status is <status>.
_interrupt() {
  rm -f "${BATS_TEST_TMPDIR}/yq.pid"
  _yq 'echo $$ > "'"${BATS_TEST_TMPDIR}"'/yq.pid"; exec sleep 30'
  yqbound::run 20 262144 "${BATS_TEST_TMPDIR}/in.yaml" '.a' > /dev/null 2>&1 &
  local bound=$! i
  for (( i = 0; i < 50; i++ )); do [ -s "${BATS_TEST_TMPDIR}/yq.pid" ] && break; sleep 0.1; done
  [ -s "${BATS_TEST_TMPDIR}/yq.pid" ]
  local yq body watchdog
  yq="$(cat "${BATS_TEST_TMPDIR}/yq.pid")"
  body="$(_ppid "${yq}")"
  [ -n "${body}" ]
  # The watchdog: the other child of the body.
  watchdog="$(_children "${body}" | grep -vx "${yq}" | head -n 1)"
  [ -n "${watchdog}" ] || { echo "no watchdog under ${body}" >&2; return 1; }
  kill "-${1}" "${body}"
  local rc=0
  wait "${bound}" || rc=$?
  [ "${rc}" -eq "${2}" ] || { echo "${1}: rc ${rc}" >&2; return 1; }
  for (( i = 0; i < 20; i++ )); do kill -0 "${yq}" 2>/dev/null || kill -0 "${watchdog}" 2>/dev/null || break; sleep 0.1; done
  if kill -0 "${yq}" 2>/dev/null; then echo "${1}: yq still runs" >&2; return 1; fi
  if kill -0 "${watchdog}" 2>/dev/null; then echo "${1}: the watchdog still runs" >&2; return 1; fi
}

@test "yqbound::run: TERM and QUIT stop yq and the watchdog and remove the temporary directory" {
  # With /proc (Linux), as in the CI bats container: no ps on PATH (a stub
  # that fails). Without /proc (macOS), _ppid / _children need the real ps.
  if [[ -d /proc/self ]]; then
    printf '#!/usr/bin/env bash\nexit 127\n' > "${STUBS}/ps"
    chmod +x "${STUBS}/ps"
  fi
  _interrupt TERM 143
  _interrupt QUIT 131
  _clean
}

@test "yqbound::run: a SIGKILLed body does not hold the caller's \$(…) open until the time limit" {
  rm -f "${BATS_TEST_TMPDIR}/yq.pid" "${BATS_TEST_TMPDIR}/done"
  _yq 'echo $$ > "'"${BATS_TEST_TMPDIR}"'/yq.pid"; exec sleep 30'
  # `|| :`: the killed body ends the $(…) with 137, and bats runs with set -e.
  ( out="$(yqbound::run 8 262144 "${BATS_TEST_TMPDIR}/in.yaml" '.a')" || :; : "${out}"; echo done > "${BATS_TEST_TMPDIR}/done" ) &
  local caller=$! i
  for (( i = 0; i < 50; i++ )); do [ -s "${BATS_TEST_TMPDIR}/yq.pid" ] && break; sleep 0.1; done
  [ -s "${BATS_TEST_TMPDIR}/yq.pid" ]
  local yq body start
  yq="$(cat "${BATS_TEST_TMPDIR}/yq.pid")"
  body="$(_ppid "${yq}")"
  [ -n "${body}" ]
  start=${SECONDS}
  kill -KILL "${body}"
  for (( i = 0; i < 60; i++ )); do [ -e "${BATS_TEST_TMPDIR}/done" ] && break; sleep 0.1; done
  local took=$(( SECONDS - start ))
  kill -TERM "${yq}" 2>/dev/null || :
  wait "${caller}" 2>/dev/null || :
  [ -e "${BATS_TEST_TMPDIR}/done" ] || { echo "the caller's \$(…) did not return within 6 s" >&2; return 1; }
  (( took < 4 )) || { echo "the caller's \$(…) returned after ${took} s" >&2; return 1; }
}
