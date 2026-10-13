# shellcheck shell=bash
# yqbound.sh — one yq call with a time limit and a memory cap, in pure bash.
#
# The time limit always holds: a watchdog stops yq after the given seconds,
# with no GNU timeout(1) (macOS has none by default). The memory cap is
# `ulimit -d` (data memory), best effort: it holds where the OS enforces it
# (Linux), and elsewhere the time limit is the only bound. `ulimit -v` is not
# used: Go reserves large virtual address ranges, so a virtual-memory cap can
# stop a Go program such as yq from starting.
#
# Pattern of spec::_kver_yq (PR #269): a subshell body, yq in the background,
# a watchdog that sleeps in `read -t` (no sleep process to leave behind),
# INT/TERM traps that stop both, and a temporary directory removed on exit.

# yqbound::run <seconds> <kib> <file> <yq argument>... — run
# `yq <yq argument>... <file>` with the time limit <seconds> and the data
# memory cap <kib> KiB. Exit status:
#   0  yq succeeded: its stdout goes to stdout byte for byte, its stderr (if
#      any) to stderr.
#   1  yq failed on its own (for example an unparsable file): its stderr goes
#      to stderr as is, nothing to stdout.
#   2  the time limit stopped yq (its stderr is dropped).
#   3  the memory cap or a signal stopped yq: a signal exit (SIGSEGV, SIGKILL,
#      …) or a Go allocation failure on stderr (its stderr, a Go runtime dump,
#      is dropped).
#   4  no temporary directory in ${TMPDIR:-/tmp} (nothing ran).
# The caller writes the message for 2, 3 and 4.
yqbound::run() (
  seconds="${1}" kib="${2}" file="${3}"
  shift 3
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/lo-yq.XXXXXX" 2>/dev/null) || exit 4
  trap 'rm -rf "${tmp}"' EXIT
  # On an interrupt: stop yq and the watchdog, then the EXIT trap removes tmp.
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 130' INT
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 143' TERM
  (
    ulimit -d "${kib}" 2>/dev/null
    exec yq "${@}" "${file}" >"${tmp}/out" 2>"${tmp}/err"
  ) &
  pid=$!
  (
    read -r -t "${seconds}" <> <(:) || :
    : >"${tmp}/timeout"
    kill -TERM "${pid}" 2>/dev/null
  ) &
  watchdog=$!
  rc=0
  wait "${pid}" 2>/dev/null || rc=$?
  kill -TERM "${watchdog}" 2>/dev/null
  wait "${watchdog}" 2>/dev/null
  if (( rc == 0 )); then
    cat "${tmp}/out"
    cat "${tmp}/err" >&2
    exit 0
  fi
  [[ ! -e "${tmp}/timeout" ]] || exit 2
  if (( rc > 128 )) || grep -qE 'out of memory|cannot allocate memory|stack overflow|goroutine stack exceeds|SIGSEGV' "${tmp}/err"; then
    exit 3
  fi
  cat "${tmp}/err" >&2
  exit 1
)
