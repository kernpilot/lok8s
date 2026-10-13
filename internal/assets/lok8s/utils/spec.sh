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
# nothing else, so an alias bomb or an alias cycle off the path costs
# nothing (an explode(.) of the file once cost seconds or ~1 GB). A key of
# the map wins over a merged key, and in `<<: [*a, *b]` the first map wins.
# Empty documents (a trailing `---`) do not count.

# _SPEC_KVER_TIMEOUT / _SPEC_KVER_MAX_KB: the limits of one yq read
# (spec::_kver_yq): seconds, and KiB of data memory (ulimit -d). A merge
# cycle on the path makes yq recurse without end; the limits stop it.
_SPEC_KVER_TIMEOUT=10
_SPEC_KVER_MAX_KB=262144

# spec::_kver_yq <cluster_yaml> <yq argument>... — one bounded yq read
# (--yaml-fix-merge-anchor-to-spec, the limits above). Prints the yq
# output. On a failure it prints the problem and returns 1. The yq stderr
# goes to a temporary file and never into the output that the caller reads.
spec::_kver_yq() {
  local cluster_yaml="${1}" err out first rc=0
  shift
  local unreadable="spec.kubernetes.version cannot be read:"
  local limit="the merge keys (<<) on the way to the value form a cycle, or the read passed its time or memory limit. Remove the cycle."
  err=$(mktemp "${TMPDIR:-/tmp}/lo-kver.XXXXXX") || {
    printf '%s a temporary file cannot be made in %s. Correct TMPDIR.\n' "${unreadable}" "${TMPDIR:-/tmp}"
    return 1
  }
  out=$(
    ulimit -d "${_SPEC_KVER_MAX_KB}" 2>/dev/null
    if command -v timeout >/dev/null 2>&1; then
      timeout "${_SPEC_KVER_TIMEOUT}" yq --yaml-fix-merge-anchor-to-spec "${@}" "${cluster_yaml}" 2>"${err}"
    else
      yq --yaml-fix-merge-anchor-to-spec "${@}" "${cluster_yaml}" 2>"${err}"
    fi
  ) || rc=$?
  first=$(head -n 1 "${err}")
  if (( rc == 0 )); then
    rm -f "${err}"
    printf '%s\n' "${out}"
    return 0
  fi
  if (( rc == 124 || rc > 128 )) || grep -qE 'out of memory|cannot allocate memory|stack overflow|goroutine stack exceeds|SIGSEGV' "${err}"; then
    printf '%s %s\n' "${unreadable}" "${limit}"
  elif grep -q 'unknown flag: --yaml-fix-merge-anchor-to-spec' "${err}"; then
    printf '%s the installed yq does not know the flag --yaml-fix-merge-anchor-to-spec. Install yq v4.47.1 or later (b install installs the pinned yq).\n' "${unreadable}"
  else
    printf '%s %s. Correct the spec file.\n' "${unreadable}" "${first#Error: }"
  fi
  rm -f "${err}"
  return 1
}

# The yq expressions of the reader. Each one runs once (`yq ea`) on the one
# document that is not empty (spec::_kver_read checks that there is only
# one), bound to $root. A per-document run would also print a line for each
# empty document: yq evaluates `[...]` on an empty input too.
# shellcheck disable=SC2016  # yq program text: $root, $a, $v are yq variables
_SPEC_KVER_DOC='[.] | map(select(tag != "!!null")) | .[] | . as $root'
# _SPEC_KVER_DEREF (in the context of an alias node) is the node that the
# alias names: the last node with that anchor before the alias, as the YAML
# parser resolves it. It finds the node with `..`, which does not follow
# aliases, so it expands nothing. `.[-1:] | .[]`, not `.[-1]`: yq collects
# `[...]` also for an empty input, and `.[-1]` of an empty list fails.
# shellcheck disable=SC2016  # yq program text: $root, $a, $v are yq variables
_SPEC_KVER_DEREF='((alias) as $a | (line) as $l | (column) as $c | [$root | .. | select(anchor == $a) | select(line < $l or (line == $l and column < $c))] | .[-1:] | .[])'
_SPEC_KVER_NODE="((select(kind == \"alias\") | ${_SPEC_KVER_DEREF}), select(kind != \"alias\"))"
# The number of merge keys in the file whose value (an alias resolved once)
# is not a map or a list of maps: yq skips those without an error.
_SPEC_KVER_MERGES="${_SPEC_KVER_DOC} | [.. | select(kind == \"map\") | to_entries | .[] | select(.key | tag == \"!!merge\") | .value | ${_SPEC_KVER_NODE} | select(kind != \"map\" and (kind != \"seq\" or ([.[]? | ${_SPEC_KVER_NODE} | select(kind != \"map\")] | length) > 0))] | length"
# The value: "true"/"false" (a control character), the value as a problem
# shows it (each control character as '?', at most 80 characters), and the
# value itself when it has no control character. The control characters
# are found inside yq: bash command substitution drops a NUL byte.
# shellcheck disable=SC2016  # yq program text: $root, $a, $v are yq variables
_SPEC_KVER_VALUE='tostring | sub("\n+$"; "") as $v | ($v | test("[[:cntrl:]]")) as $c | [($c | tostring), ($v | sub("[[:cntrl:]]"; "?") | sub("^(.{80}).+$"; "${1}..."))] + [$v | select($c | not)] | .[]'

# spec::_kver_read <cluster_yaml> — the shared read. Prints nothing for an
# unset version, or two lines: the value as a problem shows it, and the
# value. On a problem prints it (no prefix, no indent) and returns 1.
spec::_kver_read() {
  local cluster_yaml="${1}" out path name shape tag n=0
  local unreadable="spec.kubernetes.version cannot be read:"
  local -a lines

  # One document: a second document that is not empty can hold a version.
  # Empty documents (a trailing `---`) do not count; yq also drops some of
  # them while it reads, so the count is the only rule both readers share.
  out=$(spec::_kver_yq "${cluster_yaml}" ea -r '[.] | .[] | tag') || { printf '%s\n' "${out}"; return 1; }
  mapfile -t lines <<<"${out}"
  for tag in "${lines[@]}"; do
    [[ -z "${tag}" || "${tag}" == "!!null" ]] || n=$(( n + 1 ))
  done
  if (( n > 1 )); then
    printf '%s the spec file has %s YAML documents. Keep only the cluster spec in the file.\n' "${unreadable}" "${n}"
    return 1
  fi
  (( n == 1 )) || return 0

  # A merge key that yq would skip without an error (its value is not a map
  # or a list of maps) could hide a version: refuse it, anywhere in the file.
  out=$(spec::_kver_yq "${cluster_yaml}" ea -r "${_SPEC_KVER_MERGES}") || { printf '%s\n' "${out}"; return 1; }
  if [[ "${out}" != "0" ]]; then
    printf '%s a merge key (<<) in the spec file does not refer to a map or a list of maps. Correct the merge key.\n' "${unreadable}"
    return 1
  fi

  # Walk the path. Each hop prints "<kind> <tag>" of its node (an alias as
  # the node it names); the walk stops at the first null (unset) and at the
  # first node that is not a map.
  for path in . .spec .spec.kubernetes .spec.kubernetes.version; do
    out=$(spec::_kver_yq "${cluster_yaml}" ea -r "${_SPEC_KVER_DOC} | ${path} | ${_SPEC_KVER_NODE} | kind + \" \" + tag") || {
      printf '%s\n' "${out}"
      return 1
    }
    [[ "${out}" != *" !!null" ]] || return 0
    if [[ "${path}" == ".spec.kubernetes.version" ]]; then
      case "${out}" in
        "map "*) printf '%s the value is a map, not a single value. Write spec.kubernetes.version as a single value.\n' "${unreadable}"; return 1 ;;
        "seq "*) printf '%s the value is a list, not a single value. Write spec.kubernetes.version as a single value.\n' "${unreadable}"; return 1 ;;
      esac
      break
    fi
    name="${path#.}"
    [[ -n "${name}" ]] || name="the spec file"
    case "${out}" in
      "map "*) continue ;;
      "seq "*) shape="a list" ;;
      *)
        shape=$(spec::_kver_yq "${cluster_yaml}" ea -r "${_SPEC_KVER_DOC} | ${path} | ${_SPEC_KVER_NODE} | ${_SPEC_KVER_VALUE}") || {
          printf '%s\n' "${shape}"
          return 1
        }
        mapfile -t lines <<<"${shape}"
        shape="'${lines[1]-}'"
        ;;
    esac
    printf '%s %s is %s, not a map. Write %s as a YAML map.\n' "${unreadable}" "${name}" "${shape}" "${name}"
    return 1
  done

  out=$(spec::_kver_yq "${cluster_yaml}" ea -r "${_SPEC_KVER_DOC} | .spec.kubernetes.version | ${_SPEC_KVER_NODE} | ${_SPEC_KVER_VALUE}") || {
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
# unset: a second YAML document that is not empty, a merge key that does not
# name a map or a list of maps, a merge cycle on the way to the value, a
# spec or spec.kubernetes that is not a map, a version that is a map or a
# list, a value with a control character. On an error the function prints
# the problem (no prefix, no indent) on stdout and returns 1.
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
