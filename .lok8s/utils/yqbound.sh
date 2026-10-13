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
  # On an interrupt: stop yq and the watchdog, then the EXIT trap removes tmp.
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 130' INT
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 131' QUIT
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 143' TERM
  mkfifo "${tmp}/tick" 2>/dev/null || exit 4
  (
    ulimit -d "${kib}" 2>/dev/null
    exec yq "${@}" "${file}" >"${tmp}/out" 2>"${tmp}/err"
  ) &
  pid=$!
  # The watchdog's stdout and stderr go to /dev/null: if the body dies (a
  # SIGKILL), the watchdog must not hold the caller's $(…) pipe open until
  # the time limit.
  (
    read -r -t "${seconds}" <>"${tmp}/tick" || :
    : >"${tmp}/timeout"
    kill -TERM "${pid}" 2>/dev/null
  ) >/dev/null 2>&1 &
  watchdog=$!
  rc=0
  wait "${pid}" 2>/dev/null || rc=$?
  # `|| :` on both: the watchdog may be gone already, and a caller under
  # `set -e` must not stop here after a successful yq.
  kill -TERM "${watchdog}" 2>/dev/null || :
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
