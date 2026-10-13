#!/usr/bin/env bats
# yqbound_test.bats — yqbound::run (.lok8s/utils/yqbound.sh): one yq call with a
# time limit that always holds (pure bash, no timeout(1)) and a data-memory
# cap (ulimit -d, best effort), and an exit status per outcome: 0 success,
# 1 a yq error of its own, 2 the time limit, 3 the memory cap or a signal,
# 4 no temporary directory.

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

@test "yqbound::run: a signal exit (SIGSEGV) and a Go allocation failure map to the memory status" {
  # No core file: a core dump holds the environment of the process (tokens
  # included), and it lands in the working directory. The stub turns core
  # dumps off and dies in the test directory, which the check below reads.
  _yq 'ulimit -c 0; cd "'"${BATS_TEST_TMPDIR}"'" && kill -SEGV $$'
  _run_bound 5
  [ "${rc}" -eq 3 ] || { echo "SIGSEGV: rc ${rc}" >&2; return 1; }
  if compgen -G "${BATS_TEST_TMPDIR}/core*" >/dev/null; then
    echo "the SIGSEGV stub left a core file" >&2; return 1
  fi
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

@test "yqbound::run: TERM stops yq and the watchdog and removes the temporary directory" {
  _yq 'echo $$ > "'"${BATS_TEST_TMPDIR}"'/yq.pid"; exec sleep 30'
  yqbound::run 20 262144 "${BATS_TEST_TMPDIR}/in.yaml" '.a' > /dev/null 2>&1 &
  local bound=$! i
  for (( i = 0; i < 50; i++ )); do [ -s "${BATS_TEST_TMPDIR}/yq.pid" ] && break; sleep 0.1; done
  [ -s "${BATS_TEST_TMPDIR}/yq.pid" ]
  # TERM to the body of yqbound::run (the subshell with the traps): the
  # parent of the subshell that exec'd yq.
  local body
  body="$(ps -o ppid= -p "$(cat "${BATS_TEST_TMPDIR}/yq.pid")" | tr -d ' ')"
  [ -n "${body}" ]
  kill -TERM "${body}"
  local rc=0
  wait "${bound}" || rc=$?
  [ "${rc}" -eq 143 ] || { echo "rc ${rc}" >&2; return 1; }
  if kill -0 "$(cat "${BATS_TEST_TMPDIR}/yq.pid")" 2>/dev/null; then
    echo "yq still runs" >&2; return 1
  fi
  _clean
}
