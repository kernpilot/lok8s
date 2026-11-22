# shellcheck shell=bash
# spec.sh — the ONE reader for spec.workers, shared by every driver, for the
# kind node version of a Lo cluster (spec::kind_node_version), and the kubectl
# range of the system-upgrade-controller addon (spec::suc_kubectl, at the end).
#
# Three drivers (kkp, kubeone, capi) turn `spec.workers` into machine
# deployments. Each one used to carry its own copy of the same three facts, and
# the copies had already drifted:
#
#   - the pool-name regex `^[a-zA-Z0-9][a-zA-Z0-9-]*$`, in three places
#     (issue #132). It gates a value that reaches a yq expression, an envsubst
#     template variable and a rendered YAML field.
#   - the bracket read `.spec.workers.["<pool>"].<field>`. A dotted path parses
#     a hyphenated pool as ARITHMETIC — `.spec.workers.pool-1.type` means
#     `.spec.workers.pool - 1.type` — so it silently reads nothing. Two drivers
#     carried a comment warning about this; the third (kubeone's `ami` read) had
#     the bug.
#   - how the names are iterated. Two drivers used `while read` so a
#     whitespace-bearing name reaches the validator whole; kkp used
#     `for pool in $(yq …)`, which word-splits BEFORE validation, and each
#     fragment can pass the regex on its own.
#
# One reader owns all three. A fix here reaches every driver.

import ^utils/domain
import ^utils/verbose

# spec::pool_count <cluster_yaml> — number of pools in spec.workers (0 if none).
#
# `// {}` before `keys`, not `keys[]?` after it: mikefarah yq's `?` does not
# suppress "cannot get keys of !!null", so a spec with no spec.workers made the
# expression FAIL. The old copies hid that behind `2>/dev/null || echo 0`, which
# also hid an unreadable file.
spec::pool_count() {
  yq -r '.spec.workers // {} | keys | length' "${1}"
}

# spec::pool_names <cluster_yaml> — pool names, one per line, unvalidated.
#
# Feed it to `while IFS= read -r pool`, NEVER to `for pool in $(…)`: a name
# containing whitespace must reach spec::validate_pool_name as one word.
# Validation is the caller's next line, in the loop body, because a `return 1`
# from inside a process substitution cannot reach the caller.
spec::pool_names() {
  yq -r '.spec.workers // {} | keys | .[]' "${1}"
}

# spec::validate_pool_name <pool> — the ONE pool-name rule.
#
# The name is interpolated into a yq expression, exported as an envsubst
# template variable, and written into rendered YAML — so it is constrained to
# what a Kubernetes object name can hold anyway.
spec::validate_pool_name() {
  local pool="${1}"
  [[ "${pool}" =~ ^[a-zA-Z0-9][a-zA-Z0-9-]*$ ]] && return 0
  error "Invalid worker pool name: ${pool} (must be alphanumeric with hyphens)"
  return 1
}

# spec::pool_field <cluster_yaml> <pool> <field> [default]
#
# Read one field of one pool. Validates the name first, then reads through the
# bracket form so a hyphen cannot be parsed as subtraction.
#
# The default is applied by bash, not by yq's `//`. Two reasons: `//` needs the
# value quoted for a string and bare for a number, which is how the callers
# ended up with four spellings of the same idea; and `//` is a FALSY test in
# jq/yq semantics, so a legitimate `false` would take the default.
spec::pool_field() {
  local cluster_yaml="${1}" pool="${2}" field="${3}" default="${4-}"
  spec::validate_pool_name "${pool}" || return 1
  # The field path is a code literal at every call site, but it lands in the
  # same yq expression as the name — validate it on the same principle.
  [[ "${field}" =~ ^[a-zA-Z][a-zA-Z0-9_]*(\.[a-zA-Z][a-zA-Z0-9_]*)*$ ]] || {
    error "Invalid pool field path: ${field}"
    return 1
  }
  local value
  value=$(yq -r ".spec.workers.[\"${pool}\"].${field}" "${cluster_yaml}") || return 1
  [[ -n "${value}" && "${value}" != "null" ]] || value="${default}"
  printf '%s\n' "${value}"
}

# ── spec.kubernetes.version ──────────────────────────────────────────────────
#
# spec::kubernetes_version reads the value with the rules that every driver
# shares; spec::kind_node_version adds the kind image-tag check of a Lo
# cluster. Go twins: lodriver.KubernetesVersion and lodriver.NodeVersion
# (internal/driver/lo/nodeimage.go). The Lo driver and `lo lint` read the
# version through spec::kind_node_version, `lo audit` through
# spec::kubernetes_version. The spec export (lo::export_spec_envs) does not:
# it must not stop a command that does not use the version, so it reads the
# raw value and only maps "null" to "". The shared cases are in
# tests/fixtures/kind-node-version/.
#
# The reads are plain yq path reads under --yaml-fix-merge-anchor-to-spec:
# yq resolves the aliases and the merge keys one key at a time and expands
# nothing else, so an alias bomb or an alias cycle off the path costs nothing
# (an explode(.) of the file once cost seconds or ~1 GB). A key of the map
# wins over a merged key, and in `<<: [*a, *b]` the first map wins. Before a
# map on the path is read, the reader counts the map reads that its merge keys
# cost (spec::_kver_reads): yq reads a merged map again for each merge key
# that names it, and a merge cycle makes it recurse without end.

# _SPEC_KVER_TIMEOUT / _SPEC_KVER_MAX_KB: the limits of one yq read
# (spec::_kver_yq): seconds, and KiB of data memory (ulimit -d, where the OS
# enforces it). _SPEC_KVER_MAX_READS: the most map reads that the merge keys
# of one map on the path may cost (Go twin: maxMergeReads).
_SPEC_KVER_TIMEOUT=10
_SPEC_KVER_MAX_KB=262144
_SPEC_KVER_MAX_READS=10000

# Test only (tests/unit/lo_node_version_test.bats): a delay, in seconds, in
# spec::_kver_yq between the start of yq (_YQ) or of the watchdog
# (_WATCHDOG) and the line that keeps its pid, so that a test can send a
# signal in that window. Empty in a normal run. Shell variables, set here,
# so the environment cannot set them.
_SPEC_KVER_TEST_DELAY_YQ=""
_SPEC_KVER_TEST_DELAY_WATCHDOG=""

# spec::_running_job <pid> — true while <pid> is a running job of the
# calling shell. The shell reaps a job that ends (in its SIGCHLD handler),
# and after that the pid can be another process's. So spec::_kver_yq sends
# a signal to a job of its own only after this test. yqbound::run has its
# own copy (yqbound::_running_job): spec.sh does not import yqbound.sh.
spec::_running_job() {
  local job
  for job in $(jobs -rp); do
    [[ "${job}" != "${1}" ]] || return 0
  done
  return 1
}

# spec::_kver_stop <pid> <watchdog> <tickfd> — the stop of spec::_kver_yq on
# INT or TERM: TERM to yq and a wait for it, then the line on <tickfd> that
# ends the watchdog, and a wait for the watchdog. All three arguments are
# required (an empty one for a job that does not exist). The watchdog gets
# no signal; without a <tickfd> (a caller error) it gets TERM. It stays
# armed until yq ended: if yq does not take the TERM (bash can drop a signal
# that a subshell gets as it starts), the watchdog stops yq at the time
# limit, and the stop does not wait for ever. A job that ended gets no
# signal (spec::_running_job): after the read's wait, the pid of yq can be
# another process's. The trap sets INT and TERM to do nothing before the
# stop, and a signal that comes during the stop ends a wait early: so the
# stop waits again while the job runs. It runs in a trap, where set -e
# applies again: each command that can fail ends in `|| :`.
# yqbound.sh has its own copy (yqbound::_stop).
spec::_kver_stop() {
  local pid="${1}" watchdog="${2}" tickfd="${3}"
  ! spec::_running_job "${pid}" || kill -TERM "${pid}" 2>/dev/null || :
  while spec::_running_job "${pid}"; do wait "${pid}" 2>/dev/null || :; done
  if [[ -n "${tickfd}" ]]; then
    { printf '\n' >&"${tickfd}"; } 2>/dev/null || :
  else
    # No FIFO: a caller error. A TERM, so that the stop does not wait until
    # the time limit of the watchdog.
    ! spec::_running_job "${watchdog}" || kill -TERM "${watchdog}" 2>/dev/null || :
  fi
  while spec::_running_job "${watchdog}"; do wait "${watchdog}" 2>/dev/null || :; done
  return 0
}

# spec::_kver_cleanup <owner> <dir> — the EXIT trap of spec::_kver_yq:
# remove <dir>, the read's temporary directory, but only in the read's own
# process (<owner>, its BASHPID). A job of the read (the watchdog) that gets
# TERM before its own trap is set can run the read's EXIT trap as it ends
# (bash 5.2 does). Then it removed the directory while the read still needed
# it, and a yq that had ended well gave no output. yqbound.sh has its own
# copy (yqbound::_cleanup).
spec::_kver_cleanup() {
  [[ "${BASHPID}" != "${1}" ]] || rm -rf "${2}"
}

# spec::_kver_yq <cluster_yaml> <yq argument>... — one bounded yq read
# (--yaml-fix-merge-anchor-to-spec). Prints the yq output. On a failure it
# prints the problem and returns 1. The yq stderr goes to a file and never
# into the output that the caller reads.
#
# The time limit needs no GNU timeout (macOS has none): a watchdog subshell
# waits in `read -t N` on a FIFO that the read keeps open read-write
# (tickfd); at the time limit, the watchdog stops yq. When yq ends first,
# the read writes one line to the FIFO, and the watchdog ends without a
# signal: a line stays in the FIFO until the watchdog reads it, so the
# watchdog cannot miss it. A TERM to the watchdog could be lost: bash 5.2
# can drop a signal that a subshell gets before it resets the read's traps,
# and the watchdog then waited the full time limit and sent its TERM to the
# pid of a yq that had ended. No sleep process is left behind. A FIFO, not
# a process substitution opened read-write: on macOS a read on that ends at
# once, and every read timed out. yqbound::run (utils/yqbound.sh) has the
# same watchdog. The memory limit is `ulimit -d`, best effort: an OS that
# does not enforce it gets the time limit only. The body is a subshell, so
# its traps remove the temporary files on an interrupt and change nothing in
# the caller.
spec::_kver_yq() (
  cluster_yaml="${1}"
  shift
  unreadable="spec.kubernetes.version cannot be read:"
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/lo-kver.XXXXXX") || {
    printf '%s lo cannot create a temporary directory in %s. Correct TMPDIR.\n' "${unreadable}" "${TMPDIR:-/tmp}"
    exit 1
  }
  owner="${BASHPID}"
  trap 'spec::_kver_cleanup "${owner}" "${tmp}"' EXIT
  # On an interrupt: stop yq and the watchdog (spec::_kver_stop), then the
  # EXIT trap removes tmp. Until the pids of yq and of the watchdog are
  # kept, INT and TERM are only noted: a stop between a start and its `$!`
  # would not know that pid and would leave yq or the watchdog running.
  # Then the stop traps come, and a noted signal is sent again, so it stops
  # the read as a later one does. A stop first sets INT and TERM to do
  # nothing: a second signal would start a second stop inside the first
  # (bash runs its trap between the commands of the first).
  sig=""
  trap 'sig="${sig:-INT}"' INT
  trap 'sig="${sig:-TERM}"' TERM
  # A job that a signal stopped (yq on SIGSEGV or SIGKILL) gets a report
  # from bash on stderr ("line N: PID Segmentation fault ( ulimit -d … )"),
  # at the next notify point (after a DEBUG trap; bash 4.3: a command too) until a
  # `wait` collects the job. The read writes its own line for that cause,
  # so from the start of yq to the wait for the watchdog, stderr goes to
  # /dev/null (a set -x trace of these lines too). errfd keeps the caller's
  # stderr until then, and each job closes it: a job that kept a copy would
  # hold a caller's $(...) pipe open. Not a `{ ...; } 2>/dev/null` group:
  # bash keeps the caller's stderr in a copy that the watchdog would get.
  # yqbound::run (utils/yqbound.sh) does the same.
  exec {errfd}>&2 2>/dev/null || {
    printf '%s lo cannot open a file descriptor. Raise the open-file limit (ulimit -n).\n' "${unreadable}"
    exit 1
  }
  # tickfd: the FIFO of the watchdog, read-write, so that neither its open
  # nor a write blocks, and the watchdog's read sees no end of file. stdout
  # and errfd: the messages go to the caller's stdout, as all of this read's.
  if ! mkfifo "${tmp}/tick" 2>/dev/null || ! exec {tickfd}<>"${tmp}/tick"; then
    exec 2>&"${errfd}" {errfd}>&-
    printf '%s lo cannot create or open a FIFO in %s. Set TMPDIR to a local directory.\n' "${unreadable}" "${TMPDIR:-/tmp}"
    exit 1
  fi
  (
    ulimit -d "${_SPEC_KVER_MAX_KB}" 2>/dev/null
    exec yq --yaml-fix-merge-anchor-to-spec "${@}" "${cluster_yaml}" >"${tmp}/out" 2>"${tmp}/err"
  ) {errfd}>&- {tickfd}>&- &
  [[ -z "${_SPEC_KVER_TEST_DELAY_YQ}" ]] || sleep "${_SPEC_KVER_TEST_DELAY_YQ}"
  pid=$!
  # The watchdog: a line on tickfd (yq ended) ends it; at the time limit it
  # stops yq. A failed read stops yq too. The watchdog writes nothing, and it
  # holds no pipe of the caller: a caller that reads the output with $(...)
  # waits for all writers to close the pipe. A yq that ends just at the time
  # limit can still get the watchdog's TERM by its pid after bash reaped it.
  (
    read -r -t "${_SPEC_KVER_TIMEOUT}" -u "${tickfd}" && exit 0
    : >"${tmp}/timeout"
    kill -TERM "${pid}" 2>/dev/null
  ) >/dev/null 2>&1 {errfd}>&- &
  [[ -z "${_SPEC_KVER_TEST_DELAY_WATCHDOG}" ]] || sleep "${_SPEC_KVER_TEST_DELAY_WATCHDOG}"
  watchdog=$!
  trap 'trap : INT TERM; spec::_kver_stop "${pid}" "${watchdog}" "${tickfd}"; exit 130' INT
  trap 'trap : INT TERM; spec::_kver_stop "${pid}" "${watchdog}" "${tickfd}"; exit 143' TERM
  [[ -z "${sig}" ]] || kill -"${sig}" "${BASHPID}"
  rc=0
  wait "${pid}" 2>/dev/null || rc=$?
  # The line that ends the watchdog. `|| :`: a caller under `set -e` must
  # not stop here.
  { printf '\n' >&"${tickfd}"; } 2>/dev/null || :
  wait "${watchdog}" 2>/dev/null || :
  exec 2>&"${errfd}" {errfd}>&- {tickfd}>&-
  # yq ended well: its output is the answer. A missing output file is no
  # answer: an empty output read as "spec is ..., not a map". A failed read
  # of the file, or a failed write to stdout, is no answer either.
  if (( rc == 0 )); then
    if [[ ! -f "${tmp}/out" ]]; then
      printf '%s yq ended, but its output file %s does not exist. Make sure that no other process removes files from %s, then run the command again.\n' \
        "${unreadable}" "${tmp}/out" "${TMPDIR:-/tmp}"
      exit 1
    fi
    if cat "${tmp}/out" 2>/dev/null; then
      exit 0
    fi
    printf '%s yq ended, but lo cannot pass on its output: the read of %s, or the write of the output, failed. Run the command again.\n' \
      "${unreadable}" "${tmp}/out"
    exit 1
  fi
  if [[ -e "${tmp}/timeout" ]]; then
    printf '%s yq did not finish in %s s. Simplify the spec file.\n' "${unreadable}" "${_SPEC_KVER_TIMEOUT}"
  elif (( rc == 127 )); then
    # The shell's own status: `exec yq` found no yq on PATH.
    printf '%s lo cannot find yq on PATH. Install yq v4.54.3 or later (b install installs the pinned yq).\n' "${unreadable}"
  elif (( rc == 126 )); then
    # The shell's own status: the yq on PATH cannot run.
    printf '%s the yq on PATH cannot run: it has no execute permission, or it is not a program for this system. Install yq v4.54.3 or later (b install installs the pinned yq).\n' "${unreadable}"
  elif (( rc <= 128 )) && [[ ! -f "${tmp}/err" ]]; then
    # No stderr file of yq: its redirect failed (yq did not start), or the
    # files are gone. Not a yq error of the spec file.
    printf '%s yq did not start, or its files in %s do not exist. Make sure that %s is writable and has free space, and that no other process removes files from it. Then run the command again.\n' \
      "${unreadable}" "${tmp}" "${TMPDIR:-/tmp}"
  elif (( rc <= 128 )) && grep -q '^panic: ' "${tmp}/err"; then
    # A Go panic is a fault of the yq release, not of the spec file (yq
    # v4.52.5 stops with "index out of range [-1]" on these expressions).
    # It comes before the memory case: a nil pointer panic also prints
    # "[signal SIGSEGV". A process that a signal killed (rc > 128), and the
    # Go runtime's out-of-memory and stack-overflow errors print no
    # "panic: " line.
    printf '%s the installed yq (%s) failed on the version reader (%s). Install yq v4.54.3 or later (b install installs the pinned yq).\n' \
      "${unreadable}" "$(spec::_yq_version)" "$(grep -m 1 '^panic: ' "${tmp}/err")"
  elif (( rc > 128 )) || grep -qE 'out of memory|cannot allocate memory|stack overflow|goroutine stack exceeds|SIGSEGV' "${tmp}/err"; then
    printf '%s yq stopped at the memory limit of %s MiB, or a signal stopped it. Simplify the spec file.\n' "${unreadable}" "$(( _SPEC_KVER_MAX_KB / 1024 ))"
  elif grep -q 'unknown flag: --yaml-fix-merge-anchor-to-spec' "${tmp}/err"; then
    printf '%s the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.54.3 or later (b install installs the pinned yq).\n' "${unreadable}"
  else
    first=$(head -n 1 "${tmp}/err")
    printf '%s %s. Correct the spec file.\n' "${unreadable}" "${first#Error: }"
  fi
  exit 1
)

# spec::_yq_version — the version of the yq on PATH (vX.Y.Z, with its
# pre-release suffix), or "version unknown". The suffix stays: v4.54.3-rc1 is
# older than the release v4.54.3, and without the suffix the message would
# name the release that it tells the user to install.
spec::_yq_version() {
  local version
  version="$(yq --version 2>/dev/null)" || version=""
  if [[ "${version}" =~ v?([0-9]+\.[0-9]+\.[0-9]+[^[:space:]]*) ]]; then
    printf 'v%s' "${BASH_REMATCH[1]}"
  else
    printf 'version unknown'
  fi
}

# The yq expressions of the reader, all on a file with one YAML document
# (domain::spec_layout), bound to $root.
# _SPEC_KVER_DEREF (in the context of an alias node) is the node that the
# alias names: the last node with that anchor before the alias, as the YAML
# parser resolves it. It finds the node with `...`, which also visits the map
# keys (an anchor can sit on a key) and does not follow aliases, so it
# expands nothing. `.[-1:] | .[]`, not `.[-1]`: yq collects `[...]` also for
# an empty input, and `.[-1]` of an empty list fails.
# shellcheck disable=SC2016  # yq program text: $root, $a, $v are yq variables
_SPEC_KVER_DEREF='((alias) as $a | (line) as $l | (column) as $c | [$root | ... | select(anchor == $a) | select(line < $l or (line == $l and column < $c))] | .[-1:] | .[])'
_SPEC_KVER_NODE="((select(kind == \"alias\") | ${_SPEC_KVER_DEREF}), select(kind != \"alias\"))"
_SPEC_KVER_ID='((line | tostring) + ":" + (column | tostring))'
# The number of merge keys in the file whose value (an alias resolved once)
# is not a map or a list of maps: yq skips those without an error.
_SPEC_KVER_MERGES=". as \$root | [... | select(kind == \"map\") | to_entries | .[] | select(.key | tag == \"!!merge\") | .value | ${_SPEC_KVER_NODE} | select(kind != \"map\" and (kind != \"seq\" or ([.[]? | ${_SPEC_KVER_NODE} | select(kind != \"map\")] | length) > 0))] | length"
# The merge graph: one line "<map> <merged map>" per map that a merge key
# names (by position, line:column), once per merge key and list item.
_SPEC_KVER_EDGES=". as \$root | ... | select(kind == \"map\") | ${_SPEC_KVER_ID} as \$id | to_entries | .[] | select(.key | tag == \"!!merge\") | .value | ${_SPEC_KVER_NODE} | ((select(kind == \"map\") | \$id + \" \" + ${_SPEC_KVER_ID}), (select(kind == \"seq\") | .[] | ${_SPEC_KVER_NODE} | \$id + \" \" + ${_SPEC_KVER_ID}))"
# The value: "true"/"false" (a control character), the value as a problem
# shows it (each control character as '?', at most 80 characters), and the
# value itself when it has no control character. The control characters
# (the Go twin's controlRe: C0, C1, format characters, U+2028, U+2029) are
# found inside yq: bash command substitution drops a NUL byte.
# shellcheck disable=SC2016  # yq program text: $root, $a, $v are yq variables
_SPEC_KVER_VALUE='tostring | sub("\n+$"; "") as $v | ($v | test("[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]")) as $c | [($c | tostring), ($v | sub("[\p{Cc}\p{Cf}\p{Zl}\p{Zp}]"; "?") | sub("^(.{80}).+$"; "${1}..."))] + [$v | select($c | not)] | .[]'

# spec::_kver_reads <map> — the map reads that a yq path read of one key in
# <map> costs: the map, and each map that its merge keys name (once per merge
# key that names it), with their own merge keys. Reads the merge graph in
# _kver_edges (spec::_kver_read) and memoises in _kver_state/_kver_count; sets
# _kver_n, which stops growing at _SPEC_KVER_MAX_READS + 1. Returns 1 on a
# merge cycle. It visits every merge key, so a cycle is found whatever the
# limit. Go twin: yqsem.MergeReads.
spec::_kver_reads() {
  local id="${1}" src total=1
  case "${_kver_state[${id}]-}" in
    done) _kver_n="${_kver_count[${id}]}"; return 0 ;;
    open) return 1 ;;
  esac
  _kver_state[${id}]="open"
  for src in ${_kver_edges[${id}]-}; do
    spec::_kver_reads "${src}" || return 1
    total=$(( total + _kver_n ))
    (( total <= _SPEC_KVER_MAX_READS )) || total=$(( _SPEC_KVER_MAX_READS + 1 ))
  done
  _kver_state[${id}]="done"
  _kver_count[${id}]="${total}"
  _kver_n="${total}"
}

# spec::_kver_read <cluster_yaml> — the shared read. Prints nothing for an
# unset version, or two lines: the value as a problem shows it, and the
# value. On a problem prints it (no prefix, no indent) and returns 1.
spec::_kver_read() {
  local cluster_yaml="${1}" out at path name k t id from to
  local unreadable="spec.kubernetes.version cannot be read:"
  local -a lines
  local -A _kver_edges=() _kver_state=() _kver_count=()
  local _kver_n=0

  # One YAML document: another layout reads as a different spec in the two
  # implementations.
  if ! at=$(domain::spec_layout "${cluster_yaml}"); then
    printf '%s the spec file %s\n' "${unreadable}" "${at}"
    return 1
  fi

  # A merge key that yq would skip without an error (its value is not a map
  # or a list of maps) could hide a version: refuse it, anywhere in the file.
  out=$(spec::_kver_yq "${cluster_yaml}" -r "${_SPEC_KVER_MERGES}") || { printf '%s\n' "${out}"; return 1; }
  if [[ "${out}" != "0" ]]; then
    printf '%s a merge key (<<) in the spec file does not refer to a map or a list of maps. Correct the merge key.\n' "${unreadable}"
    return 1
  fi
  out=$(spec::_kver_yq "${cluster_yaml}" -r "${_SPEC_KVER_EDGES}") || { printf '%s\n' "${out}"; return 1; }
  while read -r from to; do
    [[ -z "${from}" ]] || _kver_edges[${from}]+="${to} "
  done <<<"${out}"

  # Walk the path. Each hop prints "<kind> <tag> <line:column>" of its node
  # (an alias as the node it names); the walk stops at the first null (unset)
  # and at the first node that is not a map. The merge keys of a map are
  # counted before yq reads a key in it.
  for path in . .spec .spec.kubernetes .spec.kubernetes.version; do
    out=$(spec::_kver_yq "${cluster_yaml}" -r ". as \$root | ${path} | ${_SPEC_KVER_NODE} | kind + \" \" + tag + \" \" + ${_SPEC_KVER_ID}") || {
      printf '%s\n' "${out}"
      return 1
    }
    read -r k t id <<<"${out}"
    [[ "${t}" != "!!null" ]] || return 0
    if [[ "${path}" == ".spec.kubernetes.version" ]]; then
      case "${k}" in
        map) printf '%s the value is a map, not a single value. Write spec.kubernetes.version as a single value.\n' "${unreadable}"; return 1 ;;
        seq) printf '%s the value is a list, not a single value. Write spec.kubernetes.version as a single value.\n' "${unreadable}"; return 1 ;;
      esac
      break
    fi
    name="${path#.}"
    [[ -n "${name}" ]] || name="the spec file"
    case "${k}" in
      map)
        if ! spec::_kver_reads "${id}"; then
          printf '%s the merge keys (<<) on the path to the value form a cycle. Remove the cycle.\n' "${unreadable}"
          return 1
        fi
        if (( _kver_n > _SPEC_KVER_MAX_READS )); then
          printf '%s the merge keys (<<) on the path to the value need more than %s map reads. Merge fewer maps.\n' "${unreadable}" "${_SPEC_KVER_MAX_READS}"
          return 1
        fi
        continue
        ;;
      seq) out="a list" ;;
      *)
        out=$(spec::_kver_yq "${cluster_yaml}" -r ". as \$root | ${path} | ${_SPEC_KVER_NODE} | ${_SPEC_KVER_VALUE}") || {
          printf '%s\n' "${out}"
          return 1
        }
        mapfile -t lines <<<"${out}"
        out="'${lines[1]-}'"
        ;;
    esac
    printf '%s %s is %s, not a map. Write %s as a YAML map.\n' "${unreadable}" "${name}" "${out}" "${name}"
    return 1
  done

  out=$(spec::_kver_yq "${cluster_yaml}" -r ". as \$root | .spec.kubernetes.version | ${_SPEC_KVER_NODE} | ${_SPEC_KVER_VALUE}") || {
    printf '%s\n' "${out}"
    return 1
  }
  mapfile -t lines <<<"${out}"
  if [[ "${lines[0]}" == "true" ]]; then
    printf "%s\n" "spec.kubernetes.version '${lines[1]-}' contains a control character (shown as '?'). Remove the control character."
    return 1
  fi
  [[ -n "${lines[2]-}" && "${lines[2]}" != "null" ]] || return 0
  printf '%s\n%s\n' "${lines[1]}" "${lines[2]}"
}

# spec::kubernetes_version <cluster_yaml> — the value of
# spec.kubernetes.version, with the shared rules above. Prints nothing when
# the version is unset: spec, spec.kubernetes or the key is missing or null
# (`~` too), or the value is an empty string or the literal string "null".
# A shape that can hold a version but cannot be read is an error, never
# unset: a layout other than one YAML document (domain::spec_layout), a merge
# key that does not name a map or a list of maps, merge keys on the path that
# form a cycle or need more than _SPEC_KVER_MAX_READS map reads, a spec or
# spec.kubernetes that is not a map, a version that is a map or a list, a
# value with a control character. On an error the function prints the
# problem (no prefix, no indent) on stdout and returns 1.
spec::kubernetes_version() {
  local out
  out=$(spec::_kver_read "${1}") || { printf '%s\n' "${out}"; return 1; }
  [[ -z "${out}" ]] || printf '%s\n' "${out#*$'\n'}"
}

# spec::kind_node_version <cluster_yaml> — the ONE reader for the kind node
# image tag of a Lo cluster (kindest/node:<tag>). Prints the tag, or nothing
# when the version is unset (spec::kubernetes_version): the kind config then
# names no image, and kind uses the default node image of its own release.
# Any OCI image tag (with an optional @sha256 digest) is valid: `latest` and
# the tags of `kind build node-image` too. A value that is not an image tag,
# and every problem of spec::kubernetes_version, is an error: the function
# prints the problem (no prefix, no indent) on stdout and returns 1.
spec::kind_node_version() {
  local out display value
  local hint="Set a kindest/node image tag, for example v1.31.4. To use the default node image of kind, remove the key."
  out=$(spec::_kver_read "${1}") || { printf '%s\n' "${out}"; return 1; }
  [[ -n "${out}" ]] || return 0
  display="${out%%$'\n'*}" value="${out#*$'\n'}"
  if spec::_is_image_tag "${value}"; then
    printf '%s\n' "${value}"
    return 0
  fi
  printf "%s\n" "spec.kubernetes.version '${display}' is not a valid image tag. ${hint}"
  return 1
}

# spec::_is_image_tag <value> — an OCI image tag (1 to 128 characters, no
# leading '.' or '-'), with an optional @sha256 digest. The C locale keeps the
# ranges ASCII: in a UTF-8 locale bash matches 'é' with [A-Za-z]. Go twin:
# nodeVersionRe.
spec::_is_image_tag() {
  local LC_ALL=C
  [[ "${1}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}(@sha256:[0-9a-f]{64})?$ ]]
}

# ── The kubectl range of system-upgrade-controller ──────────────────────────
# Go twin: internal/addons/suc.go (the same rules and lines). For a Plan with
# cordon or drain, the controller adds a cordon and a drain init container to
# the Plan's Job. Both run the kubectl image that the addon pins (kubectl.image
# in addons/system-upgrade-controller/values.yaml). rancher/kuberlr-kubectl
# v8.1.1 bundles kubectl v1.34.8, v1.35.5 and v1.36.1, and its binary downloads
# are off. kuberlr accepts one minor of skew, so the image serves Kubernetes
# 1.33 to 1.37 only. `lo doctor` and `lo lint` compare spec.kubernetes.version
# with the range. A Go test ties the range to the tag of the pin, and the
# shared doctor cases (tests/fixtures/doctor-suc-kubectl) tie this twin to Go.
_SPEC_SUC_KUBECTL_MIN=33
_SPEC_SUC_KUBECTL_MAX=37
# The default node image of the pinned kind release (Go: KindDefaultNodeVersion
# and KindVersion in internal/toolchain/pins.go). A Lo cluster without
# spec.kubernetes.version runs it when kind is the pinned release.
_SPEC_KIND_DEFAULT_NODE=v1.36.1
_SPEC_KIND_VERSION=v0.32.0

# spec::suc_kubectl <version> — compares a set value of spec.kubernetes.version
# with the range. Prints "in <major.minor>", "out <major.minor>" or
# "notversion". A version is vX.Y or vX.Y.Z, the v optional, with an optional
# @sha256 digest. Major and minor have no leading zero and at most three digits,
# so the arithmetic below reads them as decimal numbers.
spec::suc_kubectl() {
  local LC_ALL=C
  if [[ ! "${1}" =~ ^v?(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})(\.[0-9]+)?(@sha256:[0-9a-f]{64})?$ ]]; then
    echo notversion
    return 0
  fi
  local major="${BASH_REMATCH[1]}" minor="${BASH_REMATCH[2]}"
  if (( major != 1 || minor < _SPEC_SUC_KUBECTL_MIN || minor > _SPEC_SUC_KUBECTL_MAX )); then
    echo "out ${major}.${minor}"
  else
    echo "in ${major}.${minor}"
  fi
}

# spec::is_addon <dir> <name> — returns 0 when the dir of a bare
# spec.bootstrap entry names the addon <name>. It ignores a trailing "/" or
# "/." of the dir (also a sequence of them, such as "/./" or "//"), so
# `<name>/` and `<name>/.` name the addon. For these trailing forms the result
# is the same as in Go: the Go parser cleans the path of a bare entry
# (assets.Peek), and filepath.Base removes trailing separators. The bash
# parser keeps the entry as written (bootstrap::_parse_entry), so
# "${dir##*/}" alone gives an empty name or ".". The helper does not resolve
# a ".." element, so there the result can differ from Go: Go cleans
# `<name>/x/..` to <name>, and the helper reads "..".
spec::is_addon() {
  local d="${1}" want="${2}"
  while :; do
    case "${d}" in
      */) d="${d%/}" ;;
      */.) d="${d%/.}" ;;
      *) break ;;
    esac
  done
  [[ "${d##*/}" == "${want}" ]]
}

# spec::entry_base <key> <dir> — prints the last element of the dir of a
# spec.bootstrap entry as the Go parser reads it (filepath.Base of the parsed
# dir). <key> is the entry as written, <dir> is the dir that
# bootstrap::_parse_entry gives for it. Go keeps the dir of a path entry (a
# key that starts with "/", "./" or "../") as written, so the helper prints
# its basename: `./x/.` gives ".". Go cleans the dir of a bare entry
# (assets.Peek, path.Clean), so the helper ignores the empty and "." elements
# and resolves "..": `x/`, `x/.`, `x/./`, `x//` and `a/x/.` give "x", and
# `x/..` and `.` give "addons" (the dir is <lok8s>/addons/<key>). The bash
# parser keeps every dir as written.
#
# The helper splits the dir with parameter expansion only, so a newline in the
# dir is an ordinary character, as in Go, and it prints with printf, so a name
# such as `-n` prints as itself. A caller that reads the result through
# `$( … )` loses a trailing newline of the name.
spec::entry_base() {
  local key="${1}" dir="${2}"
  if [[ "${key}" == /* || "${key}" == ./* || "${key}" == ../* ]]; then
    # filepath.Base: drop the trailing slashes, then the last element.
    local d="${dir%"${dir##*[!/]}"}"
    if [[ -z "${dir}" ]]; then
      printf '%s\n' "."
    elif [[ -z "${d}" ]]; then
      printf '%s\n' "/"
    else
      printf '%s\n' "${d##*/}"
    fi
    return 0
  fi
  local -a out=()
  local rest="${dir}" p
  while :; do
    p="${rest%%/*}"
    case "${p}" in
      "" | .) ;;
      ..)
        if (( ${#out[@]} > 0 )) && [[ "${out[-1]}" != .. ]]; then
          unset 'out[-1]'
        elif [[ "${dir}" != /* ]]; then
          out+=("..")
        fi
        ;;
      *) out+=("${p}") ;;
    esac
    [[ "${rest}" == */* ]] || break
    rest="${rest#*/}"
  done
  if (( ${#out[@]} > 0 )); then
    printf '%s\n' "${out[-1]}"
  elif [[ "${dir}" == /* ]]; then
    printf '%s\n' "/"
  else
    printf '%s\n' "."
  fi
}

# spec::suc_kubectl_range — the range as the lines name it: "1.33 to 1.37".
spec::suc_kubectl_range() {
  echo "1.${_SPEC_SUC_KUBECTL_MIN} to 1.${_SPEC_SUC_KUBECTL_MAX}"
}

# spec::suc_kubectl_out_of_range_line <major.minor> — the warning of `lo doctor`
# and `lo lint` for a minor that the image does not serve.
spec::suc_kubectl_out_of_range_line() {
  local r
  r="$(spec::suc_kubectl_range)"
  echo "system-upgrade-controller: the kubectl image of the drain container serves Kubernetes ${r}, not ${1}, so a Plan with cordon or drain fails: set spec.kubernetes.version to a version from ${r}, or use a Plan without cordon or drain"
}
