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
# (internal/driver/lo/nodeimage.go). The driver and `lo lint` read the version
# through it. The spec export (lo::export_spec_envs) does not: it must not stop
# a command that does not use the version, so it reads the raw value and only
# maps "null" to "".
#
# Prints the tag. Prints nothing when the version is unset: spec,
# spec.kubernetes or the key is missing or null (`~` too), or the value is an
# empty string or the literal string "null". For an unset version the kind
# config names no image, and kind uses the default node image of its own
# release.
#
# Every read runs `explode(.)` under --yaml-fix-merge-anchor-to-spec: the
# aliases and the merge keys resolve the way the YAML spec defines (a key of
# the map wins over a merged key; in `<<: [*a, *b]` the first map wins). The
# command substitution removes the trailing newlines of the value (a block
# scalar), and so does the Go twin.
#
# A shape that can hold a version but cannot be read is an error, never unset:
# a file with more than one YAML document, a merge key that explode(.) refuses
# (anywhere in the file), a spec or spec.kubernetes that is not a map, a
# version that is a map or a list. A scalar that is not an OCI image tag (with
# an optional @sha256 digest) is an error too. Any tag is valid: `latest` and
# the tags of `kind build node-image` too. On an error the function prints the
# problem (no prefix, no indent) on stdout and returns 1. The caller prints it.
# The shared cases are in tests/fixtures/kind-node-version/.
spec::kind_node_version() {
  local cluster_yaml="${1}" out path name shape value
  local -a yq_spec=(yq --yaml-fix-merge-anchor-to-spec)
  local unreadable="spec.kubernetes.version cannot be read:"
  local hint="Set a kindest/node image tag, for example v1.31.4. To use the default node image of kind, remove the key."

  # One document only: a second document can hold a version too.
  if ! out=$("${yq_spec[@]}" ea '[.] | length' "${cluster_yaml}" 2>&1) || [[ ! "${out}" =~ ^[0-9]+$ ]]; then
    out=$(head -n 1 <<<"${out}")
    printf '%s %s. Correct the spec file.\n' "${unreadable}" "${out#Error: }"
    return 1
  fi
  if (( out > 1 )); then
    printf '%s the spec file has %s YAML documents. Keep only the cluster spec in the file.\n' "${unreadable}" "${out}"
    return 1
  fi

  # Walk the path. Each hop prints "<kind> <tag>" of its node; the walk stops
  # at the first null (unset) and at the first node that is not a map.
  for path in . .spec .spec.kubernetes .spec.kubernetes.version; do
    if ! shape=$("${yq_spec[@]}" "explode(.) | ${path} | kind + \" \" + tag" "${cluster_yaml}" 2>&1); then
      if [[ "${shape}" == *"can only use merge anchors"* ]]; then
        printf '%s a merge key (<<) in the spec file does not refer to a map or a list of maps. Correct the merge key.\n' "${unreadable}"
      else
        shape=$(head -n 1 <<<"${shape}")
        printf '%s %s. Correct the spec file.\n' "${unreadable}" "${shape#Error: }"
      fi
      return 1
    fi
    [[ "${shape}" != *" !!null" ]] || return 0
    if [[ "${path}" == ".spec.kubernetes.version" ]]; then
      case "${shape}" in
        "map "*) printf '%s the value is a map, not an image tag. %s\n' "${unreadable}" "${hint}"; return 1 ;;
        "seq "*) printf '%s the value is a list, not an image tag. %s\n' "${unreadable}" "${hint}"; return 1 ;;
      esac
      break
    fi
    name="${path#.}"
    [[ -n "${name}" ]] || name="the spec file"
    case "${shape}" in
      "map "*) continue ;;
      "seq "*) shape="a list" ;;
      *) shape="'$("${yq_spec[@]}" -r "explode(.) | ${path}" "${cluster_yaml}")'" ;;
    esac
    printf '%s %s is %s, not a map. Write %s as a YAML map.\n' "${unreadable}" "${name}" "${shape}" "${name}"
    return 1
  done

  if ! value=$("${yq_spec[@]}" -r 'explode(.) | .spec.kubernetes.version' "${cluster_yaml}"); then
    printf '%s yq cannot read the value. Correct the spec file.\n' "${unreadable}"
    return 1
  fi
  [[ -n "${value}" && "${value}" != "null" ]] || return 0
  if spec::_is_image_tag "${value}"; then
    printf '%s\n' "${value}"
    return 0
  fi
  printf "%s\n" "spec.kubernetes.version '${value}' is not a valid image tag. ${hint}"
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
