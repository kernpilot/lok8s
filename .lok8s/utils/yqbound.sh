# shellcheck shell=bash
# yqbound.sh — one yq call with a time limit and a memory cap, in pure bash.
#
# The time limit always holds: a watchdog stops yq after the given seconds,
# with no GNU timeout(1) (macOS has none by default). The watchdog waits in
# `read -t` on a FIFO that it opens read-write itself: no writer ever comes,
# so the read returns at the time limit, and no sleep process is left
# behind. A FIFO, not a read-write redirect onto a process substitution:
# macOS cannot reopen the read end of a pipe through /dev/fd (EACCES, or EOF
# at once), so that wait returned at once.
# The memory cap is `ulimit -d` (data memory), best effort: it holds where
# the OS enforces it (Linux); elsewhere the time limit is the only bound.
# `ulimit -v` is not used: Go reserves large virtual address ranges, so a
# virtual-memory cap can stop a Go program such as yq from starting.
#
# Pattern of spec::_kver_yq (PR #269): a subshell body, yq in the background,
# the watchdog, INT/QUIT/TERM traps that stop both, and a temporary
# directory removed on exit.

# Test only (tests/unit/yqbound_test.bats): a delay, in seconds, in
# yqbound::run between the start of yq (_YQ) or of the watchdog (_WATCHDOG)
# and the line that keeps its pid, so that a test can send a signal in that
# window. Empty in a normal run. Shell variables, set here, so the
# environment cannot set them.
_YQBOUND_TEST_DELAY_YQ=""
_YQBOUND_TEST_DELAY_WATCHDOG=""

# yqbound::_running_job <pid> — true while <pid> is a running job of the
# calling shell. The shell reaps a job that ends (in its SIGCHLD handler),
# and after that the pid can be another process's. So yqbound::run sends a
# signal to a job of its own only after this test. spec.sh has its own copy
# (spec::_running_job): it does not import this file.
yqbound::_running_job() {
  local job
  for job in $(jobs -rp); do
    [[ "${job}" != "${1}" ]] || return 0
  done
  return 1
}

# yqbound::_stop <pid> <watchdog> — the stop of yqbound::run on INT, QUIT or
# TERM: TERM to the watchdog and to yq, then a wait for both. A job that
# ended gets no signal (yqbound::_running_job): after the wait of
# yqbound::run, the pid of yq can be another process's. The trap sets INT,
# QUIT and TERM to do nothing before the stop, and a signal that comes
# during the stop ends a wait early: so the stop waits again while the job
# runs. It runs in a trap, where set -e applies again: each command that
# can fail ends in `|| :`.
yqbound::_stop() {
  local pid="${1}" watchdog="${2}"
  ! yqbound::_running_job "${watchdog}" || kill -TERM "${watchdog}" 2>/dev/null || :
  ! yqbound::_running_job "${pid}" || kill -TERM "${pid}" 2>/dev/null || :
  while yqbound::_running_job "${pid}"; do wait "${pid}" 2>/dev/null || :; done
  while yqbound::_running_job "${watchdog}"; do wait "${watchdog}" 2>/dev/null || :; done
  return 0
}

# yqbound::run <seconds> <kib> <file> <yq argument>... — run
# `yq <yq argument>... <file>` with the time limit <seconds> and the data
# memory cap <kib> KiB. Exit status:
#   0    yq succeeded: its stdout goes to stdout byte for byte, its stderr
#        (if any) to stderr.
#   1    yq failed on its own (for example an unparsable file): its stderr
#        goes to stderr as is, nothing to stdout.
#   2    the time limit stopped yq.
#   3    the memory cap stopped yq: a Go allocation failure or stack limit
#        on its stderr.
#   4    no temporary directory or FIFO in ${TMPDIR:-/tmp} (yq did not run).
#   5    yq does not know a flag that it got (an old yq).
#   64+N the signal N stopped yq (SIGSEGV: 75, SIGKILL: 73).
#   130, 131, 143: yqbound::run itself got INT, QUIT or TERM.
# For 2 to 5 and 64+N, yq's stderr (a usage text or a Go runtime dump) is
# dropped; the caller writes the message.
yqbound::run() (
  seconds="${1}" kib="${2}" file="${3}"
  shift 3
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/lo-yq.XXXXXX" 2>/dev/null) || exit 4
  trap 'rm -rf "${tmp}"' EXIT
  # On an interrupt: stop yq and the watchdog (yqbound::_stop), then the
  # EXIT trap removes tmp. Until the pids of yq and of the watchdog are
  # kept, INT, QUIT and TERM are only noted: a stop between a start and its
  # `$!` would not know that pid and would leave yq or the watchdog
  # running. Then the stop traps come, and a noted signal is sent again, so
  # it stops the call as a later one does. A stop first sets the three
  # signals to do nothing: a second signal would start a second stop inside
  # the first (bash runs its trap between the commands of the first).
  sig=""
  trap 'sig="${sig:-INT}"' INT
  trap 'sig="${sig:-QUIT}"' QUIT
  trap 'sig="${sig:-TERM}"' TERM
  mkfifo "${tmp}/tick" 2>/dev/null || exit 4
  (
    ulimit -d "${kib}" 2>/dev/null
    exec yq "${@}" "${file}" >"${tmp}/out" 2>"${tmp}/err"
  ) &
  [[ -z "${_YQBOUND_TEST_DELAY_YQ}" ]] || sleep "${_YQBOUND_TEST_DELAY_YQ}"
  pid=$!
  # The watchdog's stdout and stderr go to /dev/null: if the body dies (a
  # SIGKILL), the watchdog must not hold the caller's $(…) pipe open until
  # the time limit.
  (
    read -r -t "${seconds}" <>"${tmp}/tick" || :
    : >"${tmp}/timeout"
    kill -TERM "${pid}" 2>/dev/null
  ) >/dev/null 2>&1 &
  [[ -z "${_YQBOUND_TEST_DELAY_WATCHDOG}" ]] || sleep "${_YQBOUND_TEST_DELAY_WATCHDOG}"
  watchdog=$!
  trap 'trap : INT QUIT TERM; yqbound::_stop "${pid}" "${watchdog}"; exit 130' INT
  trap 'trap : INT QUIT TERM; yqbound::_stop "${pid}" "${watchdog}"; exit 131' QUIT
  trap 'trap : INT QUIT TERM; yqbound::_stop "${pid}" "${watchdog}"; exit 143' TERM
  [[ -z "${sig}" ]] || kill -"${sig}" "${BASHPID}"
  rc=0
  wait "${pid}" 2>/dev/null || rc=$?
  # `|| :` on both: the watchdog may be gone already, and a caller under
  # `set -e` must not stop here after a successful yq.
  ! yqbound::_running_job "${watchdog}" || kill -TERM "${watchdog}" 2>/dev/null || :
  wait "${watchdog}" 2>/dev/null || :
  if (( rc == 0 )); then
    cat "${tmp}/out"
    cat "${tmp}/err" >&2
    exit 0
  fi
  [[ ! -e "${tmp}/timeout" ]] || exit 2
  (( rc <= 128 )) || exit $(( rc - 128 + 64 ))
  if grep -qE 'out of memory|cannot allocate memory|stack overflow|goroutine stack exceeds' "${tmp}/err"; then
    exit 3
  fi
  if grep -q 'unknown flag: ' "${tmp}/err"; then
    exit 5
  fi
  cat "${tmp}/err" >&2
  exit 1
)
