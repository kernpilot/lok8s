#!/usr/bin/env bats
# bootstrap_diff_input_test.bats — the diff input of a dry run with
# --ignore-nondeterministic (bootstrap::_without_objects in libs/bootstrap).
# kubectl reads the diff input as YAML 1.1, as it reads the real apply. So
# each object that stays must keep the scalar styles of the render: a quoted
# "yes" stays a string, and a plain yes stays as it is. The Go twin is
# withoutObjects in internal/bootstrap/dryrun.go. The strings come from
# tests/fixtures/yaml11-quote/cases, the table of the -o yaml quoting.
# kubectl is a function of each test that records its stdin.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # bootstrap::_diff_manifest calls kapply::output_matches: load it, so the
  # diff runs the code of a real dry run.
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"
  type -P yq >/dev/null || fail "yq is not on PATH"
}

teardown() {
  teardown_tmpdir
}

CASES="${BATS_TEST_DIRNAME}/../fixtures/yaml11-quote/cases"

# The sections of the table, one class of the YAML 1.1 set per line: the
# class, then the texts that start the comment of each of its sections.
CLASSES='bool	the bool words in any case
null	the null words in any case, and ~	the empty string
number	binary	octal 0o	decimal, also with a leading 0	hex	floats with a fraction or an exponent	.inf and .nan in any case
base60	sexagesimal
time	dates and times
equals	=, which PyYAML cannot load'

# _set_strings <class> — the JSON form of each string of the YAML 1.1 set
# (why = set) in the sections of the class, one per line. A section is the
# block of comment lines before its rows; it belongs to the class when one
# of its lines starts with a text of the class.
_set_strings() {
  awk -F'\t' -v class="${1}" -v classes="${CLASSES}" '
    BEGIN {
      n = split(classes, lines, "\n")
      for (i = 1; i <= n; i++) {
        m = split(lines[i], f, "\t")
        if (f[1] == class) for (j = 2; j <= m; j++) want["# " f[j]] = 1
      }
    }
    /^#/ {
      if (!incomment) mine = 0
      incomment = 1
      for (w in want) if (index($0, w) == 1) mine = 1
      next
    }
    /^$/ { next }
    { incomment = 0; if (mine && $3 == "set") print $1 }
  ' "${CASES}"
}

# _render_of <json string>... — a render of two objects. The ConfigMap holds
# each string as a value (data.vN) and as a key (an annotation), each one in
# double quotes, as a chart or kustomize writes a string that YAML 1.1 would
# read as another type. The Secret is the object that the dry run leaves
# out.
_render_of() {
  local s i=0
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kept\n  namespace: ns\n  annotations:\n'
  for s in "$@"; do i=$((i + 1)); printf '    %s: a%d\n' "${s}" "${i}"; done
  printf 'data:\n'
  i=0
  for s in "$@"; do i=$((i + 1)); printf '  v%d: %s\n' "${i}" "${s}"; done
  printf -- '---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: dropped\n  namespace: ns\ndata:\n  k: c2VjcmV0\n'
}

# _diff_input_of <render> — the diff input that kubectl gets from the dry
# run without the Secret ns/dropped: bootstrap::_without_objects, then
# bootstrap::_diff_manifest with a kubectl that records its stdin. Sets
# RECORD to the file.
_diff_input_of() {
  RECORD="${BATS_TEST_TMPDIR}/kubectl.stdin"
  export RECORD
  kubectl() { [[ " $* " == *" diff "*" -f - "* ]] || return 3; cat > "${RECORD}"; }
  local bootstrap_dry_boundary="${BATS_TEST_TMPDIR}" input status
  input="$(bootstrap::_without_objects "${1}" "v1 Secret ns/dropped")" || fail "_without_objects failed"
  status="$(bootstrap::_diff_manifest "${1}" 01-kept "${BATS_TEST_TMPDIR}/kubeconfig" kept "${input}")"
  [[ "${status}" == unchanged ]] || fail "the status is '${status}', not unchanged"
  [[ -s "${RECORD}" ]] || fail "kubectl got no input"
}

# _check_class <class> — each string of the class reaches kubectl as the
# same string, in double quotes, as a value and as a key; the Secret does
# not reach it.
_check_class() {
  local -a strs
  mapfile -t strs < <(_set_strings "${1}")
  (( ${#strs[@]} > 0 )) || fail "the class ${1} has no string in ${CASES}"
  _diff_input_of "$(_render_of "${strs[@]}")"
  local want got
  want="$(printf '%s\n' "${strs[@]}" | jq -s -c '.')"
  got="$(yq -o=json -I=0 '[.data[]]' "${RECORD}" | jq -c '.')"
  [[ "${got}" == "${want}" ]] || fail "the values are ${got}, not ${want}"
  got="$(yq -o=json -I=0 '[.metadata.annotations | keys | .[]]' "${RECORD}" | jq -c '.')"
  [[ "${got}" == "${want}" ]] || fail "the keys are ${got}, not ${want}"
  got="$(yq '[(.data[], (.metadata.annotations | keys | .[])) | tag + " " + style] | unique | .[]' "${RECORD}")"
  [[ "${got}" == '!!str double' ]] || fail "a value or a key of the class ${1} is not a string in double quotes: ${got}"
  ! grep -q 'kind: Secret' "${RECORD}" || fail "the Secret that the dry run leaves out is in the diff input"
}

@test "diff input: the classes cover each string of the YAML 1.1 set one time" {
  local class all total
  all="$(while IFS=$'\t' read -r class _; do _set_strings "${class}"; done <<<"${CLASSES}")"
  total="$(awk -F'\t' '!/^#/ && $3 == "set"' "${CASES}" | wc -l)"
  (( total > 0 )) || fail "the table has no string of the set"
  [[ "$(wc -l <<<"${all}")" -eq "${total}" ]] || fail "the classes hold $(wc -l <<<"${all}") strings, the table ${total}"
  [[ -z "$(sort <<<"${all}" | uniq -d)" ]] || fail "a string is in two classes: $(sort <<<"${all}" | uniq -d)"
}

@test "diff input: the bool words in double quotes stay strings" {
  _check_class bool
}

@test "diff input: the null words and the empty string in double quotes stay strings" {
  _check_class null
}

@test "diff input: the numbers of each base in double quotes stay strings" {
  _check_class number
}

@test "diff input: the base 60 numbers in double quotes stay strings" {
  _check_class base60
}

@test "diff input: the dates and times in double quotes stay strings" {
  _check_class time
}

@test "diff input: = in double quotes stays a string" {
  _check_class equals
}

# The control: the dry run does not quote a scalar that the render wrote
# plain. kubectl reads a plain yes as a bool in the real apply, so the diff
# input keeps it plain too. Each other style stays.
@test "diff input: a plain word stays plain, and each other style stays" {
  _diff_input_of "$(printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: kept\n  namespace: ns\ndata:\n  plainWord: hello\n  plainYes: yes\n  plainTime: 1:20\n  single: '"'on'"'\n  literal: |\n    a\n    b\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: dropped\n  namespace: ns\n')"
  local got
  got="$(yq '.data | to_entries | .[] | .key + " " + (.value | tag) + " [" + (.value | style) + "]"' "${RECORD}")"
  [[ "${got}" == $'plainWord !!str []\nplainYes !!str []\nplainTime !!str []\nsingle !!str [single]\nliteral !!str [literal]' ]] \
    || fail "the styles are: ${got}"
}

# The label of each object is the label of bootstrap::_object_lines (the
# list of the objects to leave out comes from it): its label leaves out that
# object and only that object, and the others keep their order. The stream
# has a namespaced object, an object with no namespace, a cluster-scoped
# kind, a List, an object with no metadata, a name that is a number, a name
# that YAML reads as a date, no apiVersion, an empty namespace, metadata
# that is not a map, and a quoted name.
@test "diff input: the label of each object leaves out that object only, and the order stays" {
  local stream
  stream="$(cat <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: a
  namespace: ns
marker: d1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: b
marker: d2
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: reader
marker: d3
---
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: ConfigMap
    metadata:
      name: inner
      namespace: ns
marker: d4
---
apiVersion: v1
kind: Pod
marker: d5
---
apiVersion: v1
kind: Service
metadata:
  name: 7
marker: d6
---
apiVersion: v1
kind: Endpoints
metadata:
  name: 2026-11-20
marker: d7
---
kind: Thing
metadata:
  name: t
marker: d8
---
apiVersion: v1
kind: Secret
metadata:
  name: s
  namespace: ""
marker: d9
---
apiVersion: v1
kind: Event
metadata: x
marker: d10
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: "yes"
  namespace: ns
marker: d11
YAML
)"
  local -a labels
  mapfile -t labels < <(bootstrap::_object_lines "${stream}" | cut -f1)
  [[ "${#labels[@]}" -eq 11 ]] || fail "_object_lines gave ${#labels[@]} labels, not 11"
  [[ -z "$(printf '%s\n' "${labels[@]}" | sort | uniq -d)" ]] || fail "two objects have one label"
  local all i want got
  all="$(printf 'd%d\n' {1..11})"
  for i in "${!labels[@]}"; do
    want="$(grep -vx "d$((i + 1))" <<<"${all}")"
    got="$(bootstrap::_without_objects "${stream}" "${labels[i]}" | grep -o '^marker: d[0-9]*$' | cut -d' ' -f2)"
    [[ "${got}" == "${want}" ]] || fail "without '${labels[i]}': got $(paste -sd, <<<"${got}"), want $(paste -sd, <<<"${want}")"
  done
  # Two labels at one time; then all of them leave nothing.
  got="$(bootstrap::_without_objects "${stream}" "${labels[1]}" "${labels[8]}" | grep -o '^marker: d[0-9]*$' | cut -d' ' -f2 | paste -sd,)"
  [[ "${got}" == "d1,d3,d4,d5,d6,d7,d8,d10,d11" ]] || fail "without d2 and d9: got ${got}"
  got="$(bootstrap::_without_objects "${stream}" "${labels[@]}")"
  [[ -z "${got//[[:space:]]/}" ]] || fail "without all the objects, the output is: ${got}"
}

# A failed yq pass fails bootstrap::_without_objects with the status of yq,
# so that its caller can tell a failure from an empty diff input (nothing
# left to diff). lo prints no message of yq: it can quote the manifest.
@test "diff input: a failed yq pass fails bootstrap::_without_objects with its status" {
  yq() { cat > /dev/null; echo "Error: bad document: CANARY-MANIFEST-VALUE" >&2; return 7; }
  run bootstrap::_without_objects "$(printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n')" "v1 Secret none"
  [ "${status}" -eq 7 ] || fail "the status is ${status}, not 7: ${output}"
  [ -z "${output}" ] || fail "the output is: ${output}"
}

# A document that is not a map (an empty document, a document with only a
# comment, a scalar or a list) is not in the diff input, at the start of the
# stream and in the middle, as before: bootstrap::_object_lines does not
# list it either.
@test "diff input: a document that is not a map is not in the diff input" {
  local stream got
  stream="$(printf '%s\n' '# a document with only a comment' '---' '---' \
    'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: a' 'marker: m1' \
    '---' '# a comment in the middle' '---' 'just a scalar' '---' '- a' '- list' '---' '' '---' \
    'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: b' 'marker: m2')"
  got="$(bootstrap::_without_objects "${stream}" "v1 Secret none")"
  [[ "$(grep -o '^marker: m[0-9]*$' <<<"${got}" | cut -d' ' -f2 | paste -sd,)" == m1,m2 ]] \
    || fail "the maps of the diff input: ${got}"
  ! grep -qE 'only a comment|in the middle|just a scalar|^- (a|list)$' <<<"${got}" \
    || fail "a document that is not a map is in the diff input: ${got}"
  [[ "$(yq '[.] | length' <<<"${got}" | grep -c '^1$')" -eq 2 ]] || fail "the diff input has not two documents: ${got}"
}
