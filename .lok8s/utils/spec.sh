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

# spec::kind_node_version <cluster_yaml> — the ONE reader for the kind node
# image tag of a Lo cluster (kindest/node:<tag>). Go twin: lodriver.NodeVersion
# (internal/driver/lo/nodeimage.go). The driver, the export and `lo lint` read
# the version through it.
#
# Prints the tag. Prints nothing when spec.kubernetes.version is unset: the key
# is missing, null (`~` too), an empty string, or the literal string "null".
# For an unset version the kind config names no image, and kind uses the
# default node image of its own release. `yq -r` prints `null` for a missing
# key and `~` for a tilde null; both once reached the image as
# kindest/node:null. The tag read tells a null from a string first.
#
# A value of a different shape is malformed: the function prints the problem
# (no prefix, no indent) on stdout and returns 1. The caller prints it.
spec::kind_node_version() {
  local cluster_yaml="${1}" tag version
  local hint="Use the form vMAJOR.MINOR.PATCH, with an optional @sha256:<digest>, or remove the key to use the default node image of kind."
  tag=$(yq -r '.spec.kubernetes.version | tag' "${cluster_yaml}") || return 1
  case "${tag}" in
    '!!null') return 0 ;;
    '!!map')  printf '%s\n' "spec.kubernetes.version is a map, not a kind node version. ${hint}"; return 1 ;;
    '!!seq')  printf '%s\n' "spec.kubernetes.version is a list, not a kind node version. ${hint}"; return 1 ;;
  esac
  version=$(yq -r '.spec.kubernetes.version' "${cluster_yaml}") || return 1
  [[ -n "${version}" && "${version}" != "null" ]] || return 0
  if [[ "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(@sha256:[0-9a-f]{64})?$ ]]; then
    printf '%s\n' "${version}"
    return 0
  fi
  printf '%s\n' "spec.kubernetes.version '${version}' is not a kind node version. ${hint}"
  return 1
}
