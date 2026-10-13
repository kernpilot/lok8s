# shellcheck shell=bash
# spec.sh — the ONE reader for spec.workers, shared by every driver, and for
# the kind node version of a Lo cluster (spec::kind_node_version, at the end).
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

# spec::_kver_yq <cluster_yaml> <yq argument>... — one bounded yq read
# (--yaml-fix-merge-anchor-to-spec). Prints the yq output. On a failure it
# prints the problem and returns 1. The yq stderr goes to a file and never
# into the output that the caller reads.
#
# The time limit is pure bash, so it also holds without GNU timeout (macOS): a
# watchdog stops yq after _SPEC_KVER_TIMEOUT seconds. The memory limit is
# `ulimit -d`, best effort: an OS that does not enforce it gets the time limit
# only. The body is a subshell, so its traps remove the temporary files on an
# interrupt and change nothing in the caller.
spec::_kver_yq() (
  cluster_yaml="${1}"
  shift
  unreadable="spec.kubernetes.version cannot be read:"
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/lo-kver.XXXXXX") || {
    printf '%s lo cannot create a temporary directory in %s. Correct TMPDIR.\n' "${unreadable}" "${TMPDIR:-/tmp}"
    exit 1
  }
  trap 'rm -rf "${tmp}"' EXIT
  # On an interrupt: stop yq and the watchdog, then the EXIT trap removes tmp.
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 130' INT
  trap 'kill -TERM ${pid:-} ${watchdog:-} 2>/dev/null; exit 143' TERM
  (
    ulimit -d "${_SPEC_KVER_MAX_KB}" 2>/dev/null
    exec yq --yaml-fix-merge-anchor-to-spec "${@}" "${cluster_yaml}" >"${tmp}/out" 2>"${tmp}/err"
  ) &
  pid=$!
  # The watchdog sleeps in `read -t` (no sleep process to leave behind).
  (
    read -r -t "${_SPEC_KVER_TIMEOUT}" <> <(:) || :
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
    exit 0
  fi
  if [[ -e "${tmp}/timeout" ]]; then
    printf '%s yq did not finish in %s s. Simplify the spec file.\n' "${unreadable}" "${_SPEC_KVER_TIMEOUT}"
  elif (( rc > 128 )) || grep -qE 'out of memory|cannot allocate memory|stack overflow|goroutine stack exceeds|SIGSEGV' "${tmp}/err"; then
    printf '%s yq stopped at the memory limit of %s MiB, or a signal stopped it. Simplify the spec file.\n' "${unreadable}" "$(( _SPEC_KVER_MAX_KB / 1024 ))"
  elif grep -q 'unknown flag: --yaml-fix-merge-anchor-to-spec' "${tmp}/err"; then
    printf '%s the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.47.1 or later (b install installs the pinned yq).\n' "${unreadable}"
  else
    first=$(head -n 1 "${tmp}/err")
    printf '%s %s. Correct the spec file.\n' "${unreadable}" "${first#Error: }"
  fi
  exit 1
)

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
    printf "%s the spec file holds more than one YAML document (line %s: '%s'). Keep one document: remove line %s and every document after the first.\n" \
      "${unreadable}" "${at%% *}" "${at#* }" "${at%% *}"
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
