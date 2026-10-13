# shellcheck shell=bash disable=SC2034
# dnssearch.sh — the spec.network.dnsSearch check, shared by the Lo driver's
# read pass (lo::read_dns_search_config) and `lo lint` (lint::dns_search).
# The reason for the key is at lo::render_kind_config. Go twin:
# CheckDNSSearch (internal/driver/lo/dnssearch.go). Both print the same
# lines in the same order: keep the checks, their order and the wording in
# step.
#
# How the reads stay cheap and match the Go twin:
#   - Every read selects .spec.network.dnsSearch first and expands YAML
#     aliases on that node and on each entry only (explode(.) on an alias
#     node resolves that one alias). An alias cycle or a large anchor tree
#     anywhere else in the spec costs nothing, and a cycle inside the list
#     reads as "a list", as in Go (yqsem dereferences one alias per node).
#   - --yaml-fix-merge-anchor-to-spec: a merge key (`<<: *a`) on the path
#     merges the way the YAML spec says (a key of the map itself wins, then
#     the merged maps in order), the same rule as yqsem.LookupMerged. The
#     flag also stops yq's per-read warning about merge keys.
#   - Every yq call runs bounded (dnssearch::_read): a hit bound is one
#     problem line, not a stuck command or a host out of memory. Go reads the
#     tree in-process and needs no bound.
#   - The character check runs through yq's test(), the same Go regexp
#     engine as the Go twin, so both decide alike on non-ASCII text, control
#     characters and newlines.

# The most entries spec.network.dnsSearch takes: the Kubernetes limit for
# the search list of a pod (MaxDNSSearchPaths).
DNSSEARCH_MAX=32
# The bound of one yq call: wall-clock seconds and virtual memory (KiB).
DNSSEARCH_YQ_SECONDS=10
DNSSEARCH_YQ_KIB=1048576

# dnssearch::check <spec-file> — check spec.network.dnsSearch. Sets
# DNSSEARCH_NAMES (the names as written) and DNSSEARCH_PROBLEMS (one line per
# problem, no prefix: the caller adds `error: ` or the lint indent). A
# missing key, a null and a spec.network that is not a map give no names and
# no problems. A spec with a problem gets no names and returns 1.
dnssearch::check() {
  local file="${1}"
  DNSSEARCH_NAMES=()
  DNSSEARCH_PROBLEMS=()
  local list='.spec.network.dnsSearch | explode(.)'

  local tag
  # "" when spec.network is not a map: yq yields no value at all.
  dnssearch::_read tag "${file}" "${list} | tag" || { dnssearch::_stop $?; return $?; }
  [[ -n "${tag}" && "${tag}" != '!!null' ]] || return 0
  if [[ "${tag}" != '!!seq' ]]; then
    local found
    dnssearch::_describe found "${file}" "${list}" "${tag}" || { dnssearch::_stop $?; return $?; }
    DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch must be a list of DNS names. Found ${found}. Write a YAML list, for example [corp.example.com].")
    return 1
  fi

  local raw_tags
  dnssearch::_read raw_tags "${file}" "${list} | .[] | explode(.) | tag" || { dnssearch::_stop $?; return $?; }
  local -a tags=()
  [[ -z "${raw_tags}" ]] || mapfile -t tags <<< "${raw_tags}"
  if (( ${#tags[@]} > DNSSEARCH_MAX )); then
    local extra=$(( ${#tags[@]} - DNSSEARCH_MAX )) noun="entries"
    (( extra != 1 )) || noun="entry"
    DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch has ${#tags[@]} entries. The limit is ${DNSSEARCH_MAX}. Remove ${extra} ${noun}.")
    return 1
  fi

  local i entry value problem found matches
  for i in "${!tags[@]}"; do
    entry="${list} | .[${i}] | explode(.)"
    value=""
    if [[ "${tags[i]}" == '!!str' ]]; then
      dnssearch::_read value "${file}" "${entry}" || { dnssearch::_stop $?; return $?; }
    fi
    if [[ "${tags[i]}" == '!!null' || ( "${tags[i]}" == '!!str' && -z "${value}" ) ]]; then
      problem="is empty. Write a DNS name, or remove the entry."
    elif [[ "${tags[i]}" != '!!str' ]]; then
      dnssearch::_describe found "${file}" "${entry}" "${tags[i]}" || { dnssearch::_stop $?; return $?; }
      problem="must be a DNS name. Found ${found}. Write the entry as text, for example corp.example.com."
    else
      dnssearch::_read matches "${file}" "${entry} | test(\"^[A-Za-z0-9.-]*\$\")" || { dnssearch::_stop $?; return $?; }
      if [[ "${matches}" != true ]]; then
        problem="has a character that is not valid in a DNS name: '${value}'. Use only letters, digits, hyphens and dots."
      else
        problem="$(dnssearch::_name_problem "${value}")"
      fi
    fi
    if [[ -n "${problem}" ]]; then
      DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch[${i}] ${problem}")
    else
      DNSSEARCH_NAMES+=("${value}")
    fi
  done
  (( ${#DNSSEARCH_PROBLEMS[@]} == 0 )) || { DNSSEARCH_NAMES=(); return 1; }
}

# dnssearch::_read <var> <file> <expr> — set <var> to the output of
# `yq -r <expr> <file>`, byte for byte. `yq -r` ends its output with one
# newline, and $(…) strips every trailing newline: the sentinel keeps a
# newline that belongs to the value (a "…\n" string or a `|` block scalar),
# so the check sees what the Go twin sees. printf -v, not $(…), hands the
# value back for the same reason.
#
# The call runs with --yaml-fix-merge-anchor-to-spec, in a subshell with
# `ulimit -v DNSSEARCH_YQ_KIB` and `timeout DNSSEARCH_YQ_SECONDS`. Returns 0;
# 1 when yq failed on its own (an unreadable or unparsable file: its stderr
# goes to stderr as is, the other readers report the file); 2 when a bound
# hit (yq's stderr is then a Go runtime dump and is dropped).
dnssearch::_read() {
  local err out rc=0
  err="$(mktemp)" || return 1
  out="$( ( ulimit -v "${DNSSEARCH_YQ_KIB}" 2>/dev/null
            exec timeout "${DNSSEARCH_YQ_SECONDS}" yq --yaml-fix-merge-anchor-to-spec -r "${3}" "${2}"
          ) 2>"${err}"
          status=$?; printf x; exit "${status}" )" || rc=$?
  out="${out%x}"
  if (( rc == 0 )); then
    [[ ! -s "${err}" ]] || cat "${err}" >&2
    rm -f "${err}"
    printf -v "${1}" '%s' "${out%$'\n'}"
    return 0
  fi
  if (( rc == 124 || rc == 137 )) || grep -qiE 'out of memory|cannot allocate memory|stack exceeds' "${err}"; then
    rm -f "${err}"
    return 2
  fi
  cat "${err}" >&2
  rm -f "${err}"
  return 1
}

# dnssearch::_stop <rc> — end the check after a failed dnssearch::_read: a hit
# bound is one problem (returns 1); a yq error of its own reads as "no list"
# (returns 0), like the Go twin on an unparsable file.
dnssearch::_stop() {
  DNSSEARCH_NAMES=()
  DNSSEARCH_PROBLEMS=()
  (( ${1} == 2 )) || return 0
  DNSSEARCH_PROBLEMS+=("spec.network.dnsSearch could not be read: yq stopped at the limit of ${DNSSEARCH_YQ_SECONDS} s and $(( DNSSEARCH_YQ_KIB / 1024 )) MiB. Write spec.network without YAML aliases and merge keys.")
  return 1
}

# dnssearch::_describe <var> <file> <expr> <tag> — set <var> to the value at
# <expr> in plain words (Go: describeYAMLValue). Returns the status of the
# read, as dnssearch::_read.
dnssearch::_describe() {
  local scalar=""
  case "${4}" in
    '!!str' | '!!int' | '!!float' | '!!bool') dnssearch::_read scalar "${2}" "${3}" || return $? ;;
  esac
  case "${4}" in
    '!!str') printf -v "${1}" "the text '%s'" "${scalar}" ;;
    '!!int' | '!!float') printf -v "${1}" 'the number %s' "${scalar}" ;;
    '!!bool') printf -v "${1}" 'the boolean %s' "${scalar}" ;;
    '!!map') printf -v "${1}" 'a map' ;;
    '!!seq') printf -v "${1}" 'a list' ;;
    *) printf -v "${1}" 'a value with the tag %s' "${4}" ;;
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
