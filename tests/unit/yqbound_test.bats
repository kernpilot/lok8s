#!/usr/bin/env bats
# yqbound_test.bats — yqbound::run (.lok8s/utils/yqbound.sh): one yq call with a
# time limit that always holds (pure bash, no timeout(1)) and a data-memory
# cap (ulimit -d, best effort), and an exit status per outcome: 0 success,
# 1 a yq error of its own, 2 the time limit, 3 the memory cap, 4 no
# temporary directory, FIFO or free file descriptor, 5 an unknown flag (an old yq), 6 yq
# ended but its output file is gone, 7 yq did not start or its files are
# gone, 8 no yq on PATH, 9 a yq that cannot run, 10 yq ended but its output
# could not be passed on, 64+N the signal N.

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

# bash reports a job that a signal stopped on stderr ("line N: PID
# Segmentation fault ( ulimit -d … )") at its next notify point: after a
# command, and after a DEBUG trap (bats sets one, with functrace). The
# caller writes its own line for that cause, so the report must not reach
# the caller's stderr. The watchdog delay (_YQBOUND_TEST_DELAY_WATCHDOG)
# holds the window open after the stub died: its sleep is a notify point,
# and so is each DEBUG trap. The call runs in a new bash, so the case does
# not depend on the traps of bats.
@test "yqbound::run: bash puts no report of a yq that a signal stopped on the caller's stderr" {
  local sig want trace rc bad=0
  for sig in SEGV KILL; do
    want=75
    [[ "${sig}" == KILL ]] && want=73
    _yq 'ulimit -c 0; cd "'"${BATS_TEST_TMPDIR}"'" && kill -'"${sig}"' $$'
    for trace in none debug; do
      rc=0
      bash -c 'source "$1"; _YQBOUND_TEST_DELAY_WATCHDOG=1
        [[ "$2" == none ]] || { set -T; trap ":" DEBUG; }
        yqbound::run 5 262144 "$3" -r .a' \
        bash "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${trace}" "${BATS_TEST_TMPDIR}/in.yaml" \
        > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
      if (( rc != want )); then
        echo "SIG${sig}, DEBUG trap ${trace}: status ${rc}, want ${want}" >&2; bad=1
      fi
      if [ -s "${BATS_TEST_TMPDIR}/err" ]; then
        echo "SIG${sig}, DEBUG trap ${trace}: the caller's stderr is not empty:" >&2
        cat "${BATS_TEST_TMPDIR}/err" >&2; bad=1
      fi
    done
  done
  if compgen -G "${BATS_TEST_TMPDIR}/core*" >/dev/null; then
    echo "a stub left a core file" >&2; bad=1
  fi
  (( bad == 0 ))
  _clean
}

# With no free file descriptor for errfd (ulimit -n 10: errfd takes the
# lowest free one from 10 up), the call stops before yq with 4.
@test "yqbound::run: no free file descriptor for errfd is 4, and yq does not run" {
  local rc=0
  _yq 'echo ran > "'"${BATS_TEST_TMPDIR}"'/ran"; echo 1'
  bash -c 'source "$1"; ulimit -n 10; yqbound::run 5 262144 "$2" -r .a' \
    bash "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml" \
    > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  [ "${rc}" -eq 4 ]
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  [ ! -e "${BATS_TEST_TMPDIR}/ran" ]
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

# yq ends well, but its output file is gone when the call collects it (a
# process removed it from TMPDIR). That is no answer (6): an empty stdout
# with 0 would read as an empty value.
@test "yqbound::run: a yq that ends well but whose output file is gone is 6, never an empty success" {
  _yq 'printf "1\n"; rm -f "${TMPDIR}"/lo-yq.*/out'
  _run_bound 5
  [ "${rc}" -eq 6 ] || { echo "rc ${rc}" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "stderr:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  _clean
}

# yq ends well, and only its stderr file is gone: the answer stands (0).
# The lost file costs yq's notes, and the call writes no error of its own.
@test "yqbound::run: a yq that ends well but whose stderr file is gone is 0, with no error line" {
  _yq 'printf "1\n"; rm -f "${TMPDIR}"/lo-yq.*/err'
  _run_bound 5
  [ "${rc}" -eq 0 ] || { echo "rc ${rc}" >&2; return 1; }
  [ "$(cat "${BATS_TEST_TMPDIR}/out")" == "1" ]
  [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "stderr:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  _clean
}

# yq did not end well, and its files are gone (its redirect failed, or a
# process removed them): 7, not 1, which a caller reads as yq's own verdict
# on the file (dnssearch: "no list").
@test "yqbound::run: a yq whose files are gone, and that did not end well, is 7, not a yq error of its own" {
  _yq 'rm -f "${TMPDIR}"/lo-yq.*/out "${TMPDIR}"/lo-yq.*/err; exit 1'
  _run_bound 5
  [ "${rc}" -eq 7 ] || { echo "rc ${rc}" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "stderr:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  _clean
}

# No yq on PATH: `exec yq` ends with the shell's 127. That is 8, not 1,
# which a caller reads as yq's own verdict on the file (dnssearch: "no
# list"). The shell's "not found" line is not passed on.
@test "yqbound::run: no yq on PATH is 8, not a yq error of its own" {
  local nyq
  nyq="$(_path_without_yq "${BATS_TEST_TMPDIR}/no-yq")"
  PATH="${nyq}" _run_bound 5
  [ "${rc}" -eq 8 ] || { echo "rc ${rc}: $(cat "${BATS_TEST_TMPDIR}/err")" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "stderr:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  _clean
}

# A yq on PATH that cannot run (here: no execute permission) ends with the
# shell's 126: 9, not 1.
@test "yqbound::run: a yq on PATH that cannot run is 9, not a yq error of its own" {
  local nyq
  nyq="$(_path_without_yq "${BATS_TEST_TMPDIR}/no-yq")"
  printf '#!/usr/bin/env bash\necho 1\n' > "${nyq}/yq"
  chmod 644 "${nyq}/yq"
  PATH="${nyq}" _run_bound 5
  [ "${rc}" -eq 9 ] || { echo "rc ${rc}: $(cat "${BATS_TEST_TMPDIR}/err")" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  [ ! -s "${BATS_TEST_TMPDIR}/err" ] || { echo "stderr:" >&2; cat "${BATS_TEST_TMPDIR}/err" >&2; return 1; }
  _clean
}

# yq ends well, but its output cannot be passed on: the read of the output
# file fails (no read permission). That is 10, not 6: the file is there.
@test "yqbound::run: a yq output file that cannot be read is 10, not a lost output file" {
  [[ "$(id -u)" != 0 ]] || skip "root reads a file without read permission"
  _yq 'printf "1\n"; chmod 000 "${TMPDIR}"/lo-yq.*/out'
  _run_bound 5
  [ "${rc}" -eq 10 ] || { echo "unreadable output file: rc ${rc}" >&2; return 1; }
  [ ! -s "${BATS_TEST_TMPDIR}/out" ]
  _clean
}

# yq ends well, but the write of its output to stdout fails: a reader that
# closed the pipe, and 1 MiB of output, more than a pipe holds. That is 10.
@test "yqbound::run: a yq output that cannot be written to stdout is 10" {
  local -a st
  _yq 'head -c 1048576 /dev/zero | tr "\0" a'
  yqbound::run 5 262144 "${BATS_TEST_TMPDIR}/in.yaml" -r '.a' 2>/dev/null | true
  st=("${PIPESTATUS[@]}")
  [ "${st[0]}" -eq 10 ] || { echo "closed stdout: rc ${st[0]}" >&2; return 1; }
  _clean
}

# When yq ends in time, the call ends the watchdog with a line on the FIFO,
# not with a signal. bash 5.2 can drop a TERM that a subshell gets before
# it resets the call's traps: the watchdog then waited the full time limit
# and sent its TERM to the pid of a yq that had ended. A kill() wrapper
# logs each signal of the call and of its jobs. The call runs in a new bash
# whose $0 is a mark: the watchdog is a subshell of it and has the same
# command line, so the mark finds a watchdog that runs on.
@test "yqbound::run: a call whose yq ends in time sends no signal, and no watchdog runs on" {
  local log="${BATS_TEST_TMPDIR}/kills" mark="yqbound-wake-${RANDOM}${RANDOM}" rc=0 start took
  : > "${log}"
  _yq 'printf "1\n"'
  start=$(_now_ms)
  bash -c 'log=$3; kill() { printf "%s\n" "$*" >> "${log}"; builtin kill "$@"; }; source "$1"; yqbound::run 30 262144 "$2" -r .a' \
    "${mark}" "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml" "${log}" \
    > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  took=$(( $(_now_ms) - start ))
  [ "${rc}" -eq 0 ] && [ "$(cat "${BATS_TEST_TMPDIR}/out")" == "1" ] || {
    echo "rc ${rc}: $(cat "${BATS_TEST_TMPDIR}/err")" >&2; return 1
  }
  [ ! -s "${log}" ] || { echo "the call sent signals: $(tr '\n' ' ' < "${log}")" >&2; return 1; }
  (( took < 10000 )) || { echo "the call took ${took} ms (the time limit is 30 s)" >&2; return 1; }
  _gone "${mark}" || { _kill_left "${mark}"; echo "a watchdog runs on after the call" >&2; return 1; }
  _clean
}

# A stop whose TERM yq does not take (bash can drop a signal that a
# subshell gets as it starts) still ends, at the time limit: the watchdog
# stays armed until yq ended, and then stops yq. The yq stub takes its
# first TERM as lost (a trap that only removes itself), then sends TERM to
# the call (its parent). A kill() wrapper logs each signal of the call and
# of its jobs, with the pid of the sender: the stop (in the body, the
# parent of yq) sends yq one TERM, and the watchdog sends the second at the
# time limit. The time limit is 3 s; a stop that is not done in 12 s fails
# the test.
@test "yqbound::run: a stop whose TERM yq does not take ends at the time limit" {
  local m="${BATS_TEST_TMPDIR}/m" mark="yqbound-lost-${RANDOM}${RANDOM}" log="${BATS_TEST_TMPDIR}/kills"
  local call rc=0 start took i yq body
  mkdir -p "${m}"
  : > "${log}"
  _yq "echo \$\$ > '${m}/pid'
echo \$PPID > '${m}/body'
trap 'trap - TERM' TERM
kill -TERM \"\${PPID}\"
while :; do sleep 0.1; done"
  start=$(_now_ms)
  bash -c 'log=$3; kill() { printf "%s %s\n" "${BASHPID}" "$*" >> "${log}"; builtin kill "$@"; }; source "$1"; yqbound::run 3 262144 "$2" -r .a' \
    "${mark}" "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml" "${log}" \
    > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" &
  call=$!
  for (( i = 0; i < 120; i++ )); do kill -0 "${call}" 2>/dev/null || break; sleep 0.1; done
  took=$(( $(_now_ms) - start ))
  yq=$(<"${m}/pid")
  body=$(<"${m}/body")
  if kill -0 "${call}" 2>/dev/null; then
    kill -KILL "${yq}" 2>/dev/null || :
    _kill_left "${mark}"
    wait "${call}" 2>/dev/null || :
    echo "the stop did not end in 12 s; the time limit is 3 s. Signals (sender, args): $(tr '\n' ';' < "${log}")" >&2
    return 1
  fi
  grep -qx "${body} -TERM ${yq}" "${log}" || {
    echo "the stop (pid ${body}) sent yq (${yq}) no TERM. Signals (sender, args): $(tr '\n' ';' < "${log}")" >&2; return 1
  }
  grep -vx "${body} -TERM ${yq}" "${log}" | grep -q " -TERM ${yq}$" || {
    echo "the watchdog sent yq (${yq}) no TERM at the time limit. Signals (sender, args): $(tr '\n' ';' < "${log}")" >&2; return 1
  }
  wait "${call}" || rc=$?
  [ "${rc}" -eq 143 ] || { echo "status ${rc}, want 143: $(<"${BATS_TEST_TMPDIR}/err")" >&2; return 1; }
  (( took >= 2500 )) || { echo "the call ended after ${took} ms, before the time limit: the stub did not keep yq running" >&2; return 1; }
  if kill -0 "${yq}" 2>/dev/null; then
    kill -KILL "${yq}" 2>/dev/null || :
    echo "yq (${yq}) runs on after the call" >&2; return 1
  fi
  _gone "${mark}" || { _kill_left "${mark}"; echo "a watchdog runs on after the call" >&2; return 1; }
  _clean
}

# The call removes its temporary directory in its EXIT trap, through
# yqbound::_cleanup, and only in its own process. A job of the call (the
# watchdog) that runs the call's EXIT trap as it ends must leave the
# directory to the call: bash 5.2 runs it there when the job gets TERM
# before it can take the signal, and the call then lost the output of a yq
# that had ended well.
@test "yqbound::_cleanup: the call's EXIT trap removes its directory in the call's own process only" {
  local d="${BATS_TEST_TMPDIR}/yqb" owner="${BASHPID}" pid own dir n=0
  mkdir -p "${d}"
  : > "${d}/out"
  ( yqbound::_cleanup "${owner}" "${d}" )
  [ -e "${d}/out" ] || { echo "a job of the call removed the call's directory" >&2; return 1; }
  yqbound::_cleanup "${owner}" "${d}"
  [ ! -e "${d}" ] || { echo "the call did not remove its directory" >&2; return 1; }
  # The call's EXIT trap calls it with the call's own pid and directory.
  eval "_orig_$(declare -f yqbound::_cleanup)"
  yqbound::_cleanup() {
    printf '%s %s %s\n' "${BASHPID}" "${1}" "${2}" >> "${BATS_TEST_TMPDIR}/cleanups"
    _orig_yqbound::_cleanup "$@"
  }
  : > "${BATS_TEST_TMPDIR}/cleanups"
  _yq 'printf "1\n"'
  _run_bound 5
  [ "${rc}" -eq 0 ] && [ "$(cat "${BATS_TEST_TMPDIR}/out")" == "1" ]
  while read -r pid own dir; do
    [[ "${dir}" == "${TMPDIR}/lo-yq."?????? ]] || { echo "a cleanup of ${dir}" >&2; return 1; }
    [[ "${pid}" != "${own}" ]] || n=$(( n + 1 ))
  done < "${BATS_TEST_TMPDIR}/cleanups"
  (( n == 1 )) || { echo "${n} cleanups in the call's own process, want 1" >&2; return 1; }
  _clean
}

# bash 5.2 can run the call's EXIT trap in the watchdog as the watchdog
# ends. Here a child process runs the trap string at once, when the call
# sets it, as the watchdog can. The call must keep its directory and give
# yq's output. A trap that removes the directory directly, or that reads
# the pid only when it runs, fails here.
@test "yqbound::run: a job that runs the call's EXIT trap leaves the call its directory and its output" {
  _yq 'printf "1\n"'
  trap() {
    if [[ $# -eq 2 && "${2}" == EXIT ]]; then
      ( eval "${1}" )
    fi
    # shellcheck disable=SC2064 # "$@" passes the caller's own trap string on.
    builtin trap "$@"
  }
  _run_bound 5
  unset -f trap
  [ "${rc}" -eq 0 ] || { echo "rc=${rc}: $(cat "${BATS_TEST_TMPDIR}/err")" >&2; return 1; }
  [ "$(cat "${BATS_TEST_TMPDIR}/out")" == "1" ]
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
  local rc=0 start took
  start=$(_now_ms)
  kill "-${1}" "${body}"
  wait "${bound}" || rc=$?
  took=$(( $(_now_ms) - start ))
  [ "${rc}" -eq "${2}" ] || { echo "${1}: rc ${rc}" >&2; return 1; }
  # Far below the time limit (20 s): the stop, not the watchdog, ended the
  # call.
  (( took <= 8000 )) || { echo "${1}: the call took ${took} ms after the signal; the stop did not end it" >&2; return 1; }
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

# The second case reads stderr in the $(…) too: while yq runs, the call
# keeps the caller's stderr in errfd, and no job may keep that copy.
@test "yqbound::run: a SIGKILLed body does not hold the caller's \$(…) open until the time limit" {
  local reads caller i yq body start took
  for reads in stdout stderr; do
    rm -f "${BATS_TEST_TMPDIR}/yq.pid" "${BATS_TEST_TMPDIR}/done"
    _yq 'echo $$ > "'"${BATS_TEST_TMPDIR}"'/yq.pid"; exec sleep 30'
    # `|| :`: the killed body ends the $(…) with 137, and bats runs with set -e.
    if [[ "${reads}" == stdout ]]; then
      ( out="$(yqbound::run 8 262144 "${BATS_TEST_TMPDIR}/in.yaml" '.a')" || :; : "${out}"; echo done > "${BATS_TEST_TMPDIR}/done" ) &
    else
      ( out="$(yqbound::run 8 262144 "${BATS_TEST_TMPDIR}/in.yaml" '.a' 2>&1)" || :; : "${out}"; echo done > "${BATS_TEST_TMPDIR}/done" ) &
    fi
    caller=$!
    for (( i = 0; i < 50; i++ )); do [ -s "${BATS_TEST_TMPDIR}/yq.pid" ] && break; sleep 0.1; done
    [ -s "${BATS_TEST_TMPDIR}/yq.pid" ]
    yq="$(cat "${BATS_TEST_TMPDIR}/yq.pid")"
    body="$(_ppid "${yq}")"
    [ -n "${body}" ]
    start=${SECONDS}
    kill -KILL "${body}"
    for (( i = 0; i < 60; i++ )); do [ -e "${BATS_TEST_TMPDIR}/done" ] && break; sleep 0.1; done
    took=$(( SECONDS - start ))
    kill -TERM "${yq}" 2>/dev/null || :
    wait "${caller}" 2>/dev/null || :
    [ -e "${BATS_TEST_TMPDIR}/done" ] || { echo "${reads}: the caller's \$(…) did not return within 6 s" >&2; return 1; }
    (( took < 4 )) || { echo "${reads}: the caller's \$(…) returned after ${took} s" >&2; return 1; }
  done
}

# _now_ms — the wall clock in milliseconds (bash 5 EPOCHREALTIME: no fork,
# and macOS date has no %N).
_now_ms() {
  local t="${EPOCHREALTIME/[.,]/}"
  echo $(( 10#${t} / 1000 ))
}

# _gone <ERE> — true when no process matches <ERE> (_pgrep_f of test_helper)
# within 2 s: a process that a signal stops can stay visible for a moment.
# The 2 s are a time, not a count of scans: under bats, one scan of /proc
# can take more than a second on a host with many processes, and a process
# that runs on until a time limit must still run when the check ends.
# Status 2 (with a message): _pgrep_f failed, so nothing is known.
_gone() {
  local found end=$(( $(_now_ms) + 2000 ))
  while :; do
    found=$(_pgrep_f "${1}") || { echo "_gone: _pgrep_f failed for '${1}'" >&2; return 2; }
    [[ -n "${found}" ]] || return 0
    (( $(_now_ms) < end )) || return 1
    sleep 0.1
  done
}

# _kill_left <ERE> — stop the processes that a failed test left behind.
_kill_left() {
  local p
  for p in $(_pgrep_f "${1}"); do kill "${p}" 2>/dev/null || :; done
}

# A signal that comes before yqbound::run keeps the pid of yq, or of the
# watchdog, is only noted until it keeps both. Then it stops the call as at
# any other time, so neither runs on. A forced delay of 1.5 s holds each
# window open (_YQBOUND_TEST_DELAY_YQ, _WATCHDOG; empty in a normal run).
# The yq stub sends the signal to the call (its parent): at once in the yq
# window, after 0.5 s in the watchdog window. The call runs in a new bash
# whose $0 is a mark: the watchdog is a subshell of it and has the same
# command line, so the mark finds a watchdog that runs on. The Go readers
# have no such window: they read the spec in their own process
# (internal/yqsem), with no yq.
@test "yqbound::run: a signal before it keeps the pids of yq and the watchdog stops both" {
  local window sig want rc mark stub_sleep pre dyq dwd start took bad=0
  local m="${BATS_TEST_TMPDIR}/m" log="${BATS_TEST_TMPDIR}/kills"
  mkdir -p "${m}"
  for window in yq watchdog; do
    for sig in INT QUIT TERM; do
      case "${sig}" in
        INT) want=130 ;;
        QUIT) want=131 ;;
        TERM) want=143 ;;
      esac
      mark="yqbound-window-${RANDOM}${RANDOM}"
      # A stub sleep that no other process uses, so _gone finds only it.
      stub_sleep=$(( 9000 + RANDOM % 1000 ))
      if [[ "${window}" == yq ]]; then
        dyq=1.5 dwd="" pre=""
      else
        dyq="" dwd=1.5 pre="sleep 0.5; "
      fi
      _yq "echo \$\$ > '${m}/pid'; echo \$PPID > '${m}/body'; ${pre}kill -${sig} \"\${PPID}\"; exec sleep ${stub_sleep}"
      : > "${log}"
      rc=0
      start=$(_now_ms)
      # The time limit (20 s) also ends a call whose stop does not run. It
      # is long: a watchdog that runs on must still run when the checks
      # below end (each scan of /proc can take a second or more).
      bash -c 'log=$5; kill() { printf "%s %s\n" "${BASHPID}" "$*" >> "${log}"; builtin kill "$@"; }
        source "$1"; _YQBOUND_TEST_DELAY_YQ=$3 _YQBOUND_TEST_DELAY_WATCHDOG=$4; yqbound::run 20 262144 "$2" -r .a' \
        "${mark}" "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml" "${dyq}" "${dwd}" "${log}" \
        > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
      took=$(( $(_now_ms) - start ))
      if (( rc != want )); then
        echo "${window} window, ${sig}: status ${rc}, want ${want}: $(<"${BATS_TEST_TMPDIR}/err")" >&2; bad=1
      fi
      # The watchdog gets no signal: the stop ends it with a line on the
      # FIFO. Each signal of the call (the kill() wrapper logs the sender
      # and the arguments) goes to yq, or to the body itself (a signal that
      # came before the pids were kept, sent again).
      local from ksig target
      while read -r from ksig target; do
        [[ "${target}" == "$(<"${m}/pid")" || "${target}" == "$(<"${m}/body")" ]] || {
          echo "${window} window, ${sig}: a signal to ${target}, not to yq or the body: $(tr '\n' ';' < "${log}")" >&2; bad=1
        }
      done < "${log}"
      # The floor: the call ends after the delay (the signal waits for the
      # end of the delay's sleep), so the window was open. The ceiling is
      # far below the time limit: the stop, not the watchdog, ended the call.
      if (( took < 1400 || took > 8000 )); then
        echo "${window} window, ${sig}: the call took ${took} ms, want the delay of 1500 ms and the stop" >&2; bad=1
      fi
      if ! _gone "^sleep ${stub_sleep}\$"; then
        echo "${window} window, ${sig}: yq runs on after the call" >&2; bad=1
        _kill_left "^sleep ${stub_sleep}\$"
      fi
      if ! _gone "${mark}"; then
        echo "${window} window, ${sig}: the watchdog runs on after the call" >&2; bad=1
        _kill_left "${mark}"
      fi
      if [[ -n "$(ls -A "${TMPDIR}")" ]]; then
        echo "${window} window, ${sig}: temporary files remain: $(ls -A "${TMPDIR}")" >&2; bad=1
      fi
      rm -rf "${TMPDIR:?}"/*
    done
  done
  (( bad == 0 ))
  _clean
}

# After the wait of yqbound::run, yq is reaped, and its pid can be another
# process's: a stop sends no signal to a job of the call that has ended,
# waited for or not (yqbound::_running_job). A job that runs gets TERM, and
# the stop returns after the job ends. The watchdog gets no signal: the
# stop ends it with a line on the FIFO (its third argument). Without a FIFO
# (a caller error), the stop sends the watchdog TERM. The stops run
# in a job of their own (set -m) with a time limit: a stop that does not
# return fails the test, and a KILL to the job's group leaves no process
# behind.
@test "yqbound::run: a stop sends no signal to a job of the call that has ended, nor to the watchdog" {
  local log="${BATS_TEST_TMPDIR}/kills" tick="${BATS_TEST_TMPDIR}/tick" sub i
  : > "${log}"
  set -m
  (
    kill() { printf '%s\n' "$*" >> "${log}"; builtin kill "$@"; }
    sleep 0.1 3>&- &
    waited=$!
    sleep 0.1 3>&- &
    ended=$!
    wait "${waited}"
    # The shell reaps the other one when it ends, with no wait.
    sleep 0.5
    yqbound::_stop "${waited}" "${ended}" ""
    [[ ! -s "${log}" ]] || { echo "a stop sent a signal to a job that had ended: $(<"${log}")"; exit 1; }
    sleep 5 3>&- &
    running=$!
    yqbound::_stop "${running}" "" ""
    [[ "$(<"${log}")" == "-TERM ${running}" ]] || { echo "a stop did not send TERM to the running job ${running}: $(<"${log}")"; exit 1; }
    ! builtin kill -0 "${running}" 2>/dev/null || { echo "the stop returned while the job ${running} ran"; exit 1; }
    # A watchdog: the read of yqbound::run, with a time limit of 20 s.
    mkfifo "${tick}"
    exec {fd}<>"${tick}"
    ( read -r -t 20 -u "${fd}" && exit 0; exit 9 ) 3>&- &
    watch=$!
    : > "${log}"
    yqbound::_stop "" "${watch}" "${fd}"
    [[ ! -s "${log}" ]] || { echo "a stop sent a signal to the watchdog: $(<"${log}")"; exit 1; }
    ! builtin kill -0 "${watch}" 2>/dev/null || { echo "the stop returned while the watchdog ${watch} ran"; exit 1; }
    # A stop without a FIFO (a caller error) does not wait until the time
    # limit of the watchdog: it sends the watchdog TERM.
    ( read -r -t 20 -u "${fd}" && exit 0; exit 9 ) 3>&- &
    watch=$!
    sleep 0.2
    : > "${log}"
    yqbound::_stop "" "${watch}" ""
    [[ "$(<"${log}")" == "-TERM ${watch}" ]] || { echo "a stop without a FIFO did not send TERM to the watchdog ${watch}: $(<"${log}")"; exit 1; }
    ! builtin kill -0 "${watch}" 2>/dev/null || { echo "the stop returned while the watchdog ${watch} ran"; exit 1; }
  ) 3>&- &
  sub=$!
  set +m
  for (( i = 0; i < 100; i++ )); do kill -0 "${sub}" 2>/dev/null || break; sleep 0.1; done
  if kill -0 "${sub}" 2>/dev/null; then
    kill -KILL -- -"${sub}" 2>/dev/null || true
    wait "${sub}" 2>/dev/null || true
    echo "a stop did not return in 10 s; signals from the stops: $(tr '\n' ' ' < "${log}")"
    return 1
  fi
  wait "${sub}" || return 1
}

# A second signal during a stop does nothing: the stop that runs sends yq
# one TERM and returns after yq ends. Without that, bash runs a second stop
# inside the first (between its commands), and yq gets TERM twice; or the
# second signal ends the stop's wait, and the call returns while yq runs.
# The yq stub ends 1 s after its TERM. A helper of the stub sends two TERMs
# to the call (the stub's parent), 0.3 s apart.
@test "yqbound::run: a second signal during a stop does not start a second stop" {
  local log="${BATS_TEST_TMPDIR}/kills" m="${BATS_TEST_TMPDIR}/m" rc=0 yq n
  mkdir -p "${m}"
  : > "${log}"
  _yq "echo \$\$ > '${m}/pid'
trap 'sleep 1; exit 1' TERM
( sleep 0.2; kill -TERM \"\${PPID}\"; sleep 0.3; kill -TERM \"\${PPID}\" ) &
while :; do sleep 0.1; done"
  bash -c 'log=$3; kill() { printf "%s\n" "$*" >> "${log}"; builtin kill "$@"; }; source "$1"; yqbound::run 20 262144 "$2" -r .a' \
    _ "${_PROJECT_ROOT}/.lok8s/utils/yqbound.sh" "${BATS_TEST_TMPDIR}/in.yaml" "${log}" \
    > "${BATS_TEST_TMPDIR}/out" 2> "${BATS_TEST_TMPDIR}/err" || rc=$?
  yq=$(<"${m}/pid")
  if kill -0 "${yq}" 2>/dev/null; then
    kill -KILL "${yq}" 2>/dev/null || :
    echo "the call returned while yq (${yq}) ran; signals from the call: $(tr '\n' ' ' < "${log}")" >&2
    return 1
  fi
  [ "${rc}" -eq 143 ] || { echo "status ${rc}, want 143: $(<"${BATS_TEST_TMPDIR}/err")" >&2; return 1; }
  n=$(grep -cx -- "-TERM ${yq}" "${log}" || true)
  [ "${n}" = 1 ] || { echo "yq got TERM ${n} times from the call, want once: $(tr '\n' ' ' < "${log}")" >&2; return 1; }
  _clean
}
