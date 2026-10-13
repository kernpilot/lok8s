# shellcheck shell=bash disable=SC2034
# dnssearch.sh — the spec.network.dnsSearch check, shared by the Lo driver's
# read pass (lo::read_dns_search_config) and `lo lint` (lint::dns_search).
# The reason for the key is at lo::render_kind_config. Go twin:
# CheckDNSSearch (internal/driver/lo/dnssearch.go). Both print the same
# lines in the same order: keep the checks, their order and the wording in
# step.
#
# Every yq read goes through explode(.), so an anchored list or an aliased
# entry reads like its value (the Go reader dereferences aliases too). The
# character check runs through yq's test(), the same Go regexp engine as
# the Go twin, so both decide alike on non-ASCII text, control characters
# and newlines.

# The most entries spec.network.dnsSearch takes: the Kubernetes limit for
# the search list of a pod (MaxDNSSearchPaths).
DNSSEARCH_MAX=32

# dnssearch::check <spec-file> — check spec.network.dnsSearch. Sets
# DNSSEARCH_NAMES (the names as written) and DNSSEARCH_PROBLEMS (one line per
# problem, no prefix: the caller adds `error: ` or the lint indent). A
# missing key, a null and a spec.network that is not a map give no names and
# no problems. A spec with a problem gets no names and returns 1.
dnssearch::check() {
  local file="${1}"
  DNSSEARCH_NAMES=()
  DNSSEARCH_PROBLEMS=()

  local tag
  # "" when spec.network is not a map: yq yields no value at all.
  tag=$(yq -r 'explode(.) | .spec.network.dnsSearch | tag' "${file}")
  [[ -n "${tag}" && "${tag}" != '!!null' ]] || return 0
  if [[ "${tag}" != '!!seq' ]]; then
    DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch must be a list of DNS names. Found $(dnssearch::_describe "${file}" '.spec.network.dnsSearch' "${tag}"). Write a YAML list, for example [corp.example.com].")
    return 1
  fi

  local -a tags=()
  mapfile -t tags < <(yq -r 'explode(.) | .spec.network.dnsSearch[] | tag' "${file}")
  if (( ${#tags[@]} > DNSSEARCH_MAX )); then
    local extra=$(( ${#tags[@]} - DNSSEARCH_MAX )) noun="entries"
    (( extra != 1 )) || noun="entry"
    DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch has ${#tags[@]} entries. The limit is ${DNSSEARCH_MAX}. Remove ${extra} ${noun}.")
    return 1
  fi

  local i value problem
  for i in "${!tags[@]}"; do
    value=""
    [[ "${tags[i]}" != '!!str' ]] || dnssearch::_raw value "${file}" ".spec.network.dnsSearch[${i}]"
    if [[ "${tags[i]}" == '!!null' || ( "${tags[i]}" == '!!str' && -z "${value}" ) ]]; then
      problem="is empty. Write a DNS name, or remove the entry."
    elif [[ "${tags[i]}" != '!!str' ]]; then
      problem="must be a DNS name. Found $(dnssearch::_describe "${file}" ".spec.network.dnsSearch[${i}]" "${tags[i]}"). Write the entry as text, for example corp.example.com."
    elif [[ "$(yq -r "explode(.) | .spec.network.dnsSearch[${i}] | test(\"^[A-Za-z0-9.-]*\$\")" "${file}")" != true ]]; then
      problem="has a character that is not valid in a DNS name: '${value}'. Use only letters, digits, hyphens and dots."
    else
      problem="$(dnssearch::_name_problem "${value}")"
    fi
    if [[ -n "${problem}" ]]; then
      DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch[${i}] ${problem}")
    else
      DNSSEARCH_NAMES+=("${value}")
    fi
  done
  (( ${#DNSSEARCH_PROBLEMS[@]} == 0 )) || { DNSSEARCH_NAMES=(); return 1; }
}

# dnssearch::_raw <var> <file> <path> — set <var> to the scalar at <path>,
# byte for byte. `yq -r` ends its output with one newline, and $(…) strips
# every trailing newline: the sentinel keeps a newline that belongs to the
# value (a "…\n" string or a `|` block scalar), so the check sees what the
# Go twin sees. printf -v, not $(…), hands the value back for the same
# reason.
dnssearch::_raw() {
  local raw
  raw="$(yq -r "explode(.) | ${3}" "${2}"; printf x)"
  raw="${raw%x}"
  printf -v "${1}" '%s' "${raw%$'\n'}"
}

# dnssearch::_describe <file> <path> <tag> — a YAML value in plain words
# (Go: describeYAMLValue).
dnssearch::_describe() {
  local scalar=""
  case "${3}" in
    '!!str' | '!!int' | '!!float' | '!!bool') dnssearch::_raw scalar "${1}" "${2}" ;;
  esac
  case "${3}" in
    '!!str') printf "the text '%s'" "${scalar}" ;;
    '!!int' | '!!float') printf 'the number %s' "${scalar}" ;;
    '!!bool') printf 'the boolean %s' "${scalar}" ;;
    '!!map') printf 'a map' ;;
    '!!seq') printf 'a list' ;;
    *) printf 'a value with the tag %s' "${3}" ;;
  esac
}

# dnssearch::_name_problem <name> — the length and label checks of a name
# that passed the character check (ASCII only), in the Go order. Prints
# the problem, or nothing for a valid name.
dnssearch::_name_problem() {
  local value="${1}"
  local name="${value%.}"
  if (( ${#name} > 253 )); then
    printf "has %d characters: '%s'. The limit is 253, without a trailing dot. Use a shorter name." "${#name}" "${value}"
    return 0
  fi
  # Split on every dot, keeping empty labels (`read -a` would drop a
  # trailing one): the Go twin uses strings.SplitSeq.
  local rest="${name}" label
  while :; do
    label="${rest%%.*}"
    if [[ -z "${label}" ]]; then
      printf "has an empty label: '%s'. Remove the extra dot." "${value}"
      return 0
    elif (( ${#label} > 63 )); then
      printf "has a label of %d characters: '%s'. The limit is 63 characters per label. Use a shorter label." "${#label}" "${value}"
      return 0
    elif [[ "${label}" == -* || "${label}" == *- ]]; then
      printf "has a label that starts or ends with a hyphen: '%s'. Remove that hyphen." "${value}"
      return 0
    fi
    [[ "${rest}" == *.* ]] || break
    rest="${rest#*.}"
  done
}
