# shellcheck shell=bash
# yqbound.sh — one yq call with a time limit and a memory cap, in pure bash.
#
# The time limit always holds: a watchdog stops yq after the given seconds,
# with no GNU timeout(1) (macOS has none by default). The watchdog waits in
# `read -t` on a FIFO that the call keeps open read-write (tickfd). When yq
# ends first, the call writes one line to the FIFO, and the watchdog ends
# without a signal: a line stays in the FIFO until the watchdog reads it,
# so the watchdog cannot miss it. A TERM to the watchdog could be lost:
# bash 5.2 can drop a signal that a subshell gets before it resets the
# call's traps, and the watchdog then waited the full time limit and sent
# its TERM to the pid of a yq that had ended. No sleep process is left
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

# yqbound::_stop <pid> <watchdog> <tickfd> — the stop of yqbound::run on
# INT, QUIT or TERM: TERM to yq and a wait for it, then the line on <tickfd>
# that ends the watchdog, and a wait for the watchdog. All three arguments
# are required (an empty one for a job that does not exist). The watchdog
# gets no signal; without a <tickfd> (a caller error) it gets TERM. It stays
# armed until yq ended: if yq does not take the TERM (bash can drop a signal
# that a subshell gets as it starts), the watchdog stops yq at the time
# limit, and the stop does not wait for ever. A job that ended gets no
# signal (yqbound::_running_job): after the wait of yqbound::run, the pid of
# yq can be another process's. The trap sets INT, QUIT and TERM to do
# nothing before the stop, and a signal that comes during the stop ends a
# wait early: so the stop waits again while the job runs. It runs in a trap,
# where set -e applies again: each command that can fail ends in `|| :`.
yqbound::_stop() {
  local pid="${1}" watchdog="${2}" tickfd="${3}"
  ! yqbound::_running_job "${pid}" || kill -TERM "${pid}" 2>/dev/null || :
  while yqbound::_running_job "${pid}"; do wait "${pid}" 2>/dev/null || :; done
  if [[ -n "${tickfd}" ]]; then
    { printf '\n' >&"${tickfd}"; } 2>/dev/null || :
  else
    # No FIFO: a caller error. A TERM, so that the stop does not wait until
    # the time limit of the watchdog.
    ! yqbound::_running_job "${watchdog}" || kill -TERM "${watchdog}" 2>/dev/null || :
  fi
  while yqbound::_running_job "${watchdog}"; do wait "${watchdog}" 2>/dev/null || :; done
  return 0
}

# yqbound::_cleanup <owner> <dir> — the EXIT trap of yqbound::run: remove
# <dir>, the call's temporary directory, but only in the call's own process
# (<owner>, its BASHPID). A job of the call (the watchdog) that gets TERM
# before it can take the signal can run the call's EXIT trap as it ends
# (bash 5.2 does). Then it removed the directory while the call still
# needed it, and a yq that had ended well gave no output. spec.sh has its
# own copy (spec::_kver_cleanup).
yqbound::_cleanup() {
  [[ "${BASHPID}" != "${1}" ]] || rm -rf "${2}"
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
#   4    no temporary directory or FIFO in ${TMPDIR:-/tmp}, or no free file
#        descriptor (yq did not run).
#   5    yq does not know a flag that it got (an old yq).
#   6    yq ended well, but its output file is gone (a process removed it
#        from ${TMPDIR:-/tmp}): nothing to stdout.
#   7    yq did not start, or its files are gone: yq's stderr file is
#        missing.
#   8    no yq on PATH (the shell's exit status 127).
#   9    a yq on PATH that cannot run: no execute permission, or not a
#        program for this system (the shell's exit status 126).
#   10   yq ended well, but its output could not be passed on: the read of
#        its output file, or the write to stdout, failed. Part of the output
#        can be on stdout.
#   64+N the signal N stopped yq (SIGSEGV: 75, SIGKILL: 73).
#   130, 131, 143: yqbound::run itself got INT, QUIT or TERM.
# For 2 to 10 and 64+N, yq's stderr (a usage text or a Go runtime dump) is
# dropped; the caller writes the message.
yqbound::run() (
  seconds="${1}" kib="${2}" file="${3}"
  shift 3
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/lo-yq.XXXXXX" 2>/dev/null) || exit 4
  owner="${BASHPID}"
  trap 'yqbound::_cleanup "${owner}" "${tmp}"' EXIT
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
  # A job that a signal stopped (yq on SIGSEGV or SIGKILL) gets a report
  # from bash on stderr ("line N: PID Segmentation fault ( ulimit -d … )"),
  # at the next notify point (after a DEBUG trap; bash 4.3: a command too) until a
  # `wait` collects the job. The caller writes its own line for that cause,
  # so from the start of yq to the wait for the watchdog, stderr goes to
  # /dev/null (a set -x trace of these lines too). errfd keeps the caller's
  # stderr until then, and each job closes it: a job that kept a copy would
  # hold a caller's $(…) pipe open. Not a `{ …; } 2>/dev/null` group: bash
  # keeps the caller's stderr in a copy that the watchdog would get.
  exec {errfd}>&2 2>/dev/null || exit 4
  # tickfd: the FIFO, read-write, so that neither its open nor a write
  # blocks, and the watchdog's read sees no end of file.
  exec {tickfd}<>"${tmp}/tick" || exit 4
  (
    ulimit -d "${kib}" 2>/dev/null
    exec yq "${@}" "${file}" >"${tmp}/out" 2>"${tmp}/err"
  ) {errfd}>&- {tickfd}>&- &
  [[ -z "${_YQBOUND_TEST_DELAY_YQ}" ]] || sleep "${_YQBOUND_TEST_DELAY_YQ}"
  pid=$!
  # The watchdog: a line on tickfd (yq ended) ends it; at the time limit it
  # stops yq. A failed read stops yq too. Its stdout and stderr go to
  # /dev/null: if the body dies (a SIGKILL), the watchdog must not hold the
  # caller's $(…) pipe open until the time limit. A yq that ends just at
  # the time limit can still get the watchdog's TERM by its pid after bash
  # reaped it.
  (
    read -r -t "${seconds}" -u "${tickfd}" && exit 0
    : >"${tmp}/timeout"
    kill -TERM "${pid}" 2>/dev/null
  ) >/dev/null 2>&1 {errfd}>&- &
  [[ -z "${_YQBOUND_TEST_DELAY_WATCHDOG}" ]] || sleep "${_YQBOUND_TEST_DELAY_WATCHDOG}"
  watchdog=$!
  trap 'trap : INT QUIT TERM; yqbound::_stop "${pid}" "${watchdog}" "${tickfd}"; exit 130' INT
  trap 'trap : INT QUIT TERM; yqbound::_stop "${pid}" "${watchdog}" "${tickfd}"; exit 131' QUIT
  trap 'trap : INT QUIT TERM; yqbound::_stop "${pid}" "${watchdog}" "${tickfd}"; exit 143' TERM
  [[ -z "${sig}" ]] || kill -"${sig}" "${BASHPID}"
  rc=0
  wait "${pid}" 2>/dev/null || rc=$?
  # The line that ends the watchdog. `|| :`: a caller under `set -e` must
  # not stop here after a successful yq.
  { printf '\n' >&"${tickfd}"; } 2>/dev/null || :
  wait "${watchdog}" 2>/dev/null || :
  exec 2>&"${errfd}" {errfd}>&- {tickfd}>&-
  # yq ended well: its output is the answer. A missing output file is no
  # answer (6): an empty stdout would read as an empty value. A failed read
  # of the file, or a failed write to stdout, is no answer either (10).
  if (( rc == 0 )); then
    [[ -f "${tmp}/out" ]] || exit 6
    cat "${tmp}/out" 2>/dev/null || exit 10
    # yq's notes, if any: a lost stderr file costs only the notes.
    cat "${tmp}/err" >&2 2>/dev/null || :
    exit 0
  fi
  [[ ! -e "${tmp}/timeout" ]] || exit 2
  (( rc <= 128 )) || exit $(( rc - 128 + 64 ))
  # 127 and 126 are the shell's own: `exec yq` found no yq on PATH (8), or
  # one that it cannot run (9). Not 1: a caller reads 1 as yq's own verdict
  # on the file.
  (( rc != 127 )) || exit 8
  (( rc != 126 )) || exit 9
  # No stderr file of yq: its redirect failed (yq did not start), or the
  # files are gone (7). Not 1: a caller reads 1 as yq's own verdict on the
  # file.
  [[ -f "${tmp}/err" ]] || exit 7
  if grep -qE 'out of memory|cannot allocate memory|stack overflow|goroutine stack exceeds' "${tmp}/err"; then
    exit 3
  fi
  if grep -q 'unknown flag: ' "${tmp}/err"; then
    exit 5
  fi
  cat "${tmp}/err" >&2
  exit 1
)
