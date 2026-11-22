#!/usr/bin/env bats
# kubehz_agent_yaml11_test.bats — the -o yaml quoting of the agent tools
# (kubehz::agent_yaml in libs/kubehz/agent) over the table that the Go twin
# reads too: tests/fixtures/yaml11-quote/cases, with
# internal/cli/output_yaml11_test.go. A Go test keeps the two patterns equal.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/agent"
}

teardown() {
  teardown_tmpdir
}

# _cases — the rows of the table as <json> TAB <scalar> TAB <why>, with the
# comments and the empty lines dropped. A row of another shape fails.
_cases() {
  local line
  while IFS= read -r line; do
    [[ -n "${line}" && "${line}" != "#"* ]] || continue
    [[ "${line}" =~ ^[^$'\t']+$'\t'[^$'\t']+$'\t'(set|encoder|plain|value)$ ]] \
      || { echo "not <json value> TAB <yaml scalar> TAB <why>: ${line}" >&2; return 1; }
    printf '%s\n' "${line}"
  done < "${_PROJECT_ROOT}/tests/fixtures/yaml11-quote/cases"
}

@test "yaml11: the pattern matches each string of the set and no other string" {
  local rows json want why s got n=0 bad=0
  rows="$(_cases)" || fail "the table has a row of another shape"
  while IFS=$'\t' read -r json want why; do
    [[ "${why}" != value ]] || continue
    s="$(jq -r . <<<"${json}")" || fail "${json}: jq failed"
    got="$(S="${s}" KUBEHZ_AGENT_YAML11="${KUBEHZ_AGENT_YAML11}" yq -n 'strenv(S) | test(strenv(KUBEHZ_AGENT_YAML11))')" \
      || fail "${json}: yq failed"
    n=$(( n + 1 ))
    if [[ "${got}" != "$([[ "${why}" == set ]] && echo true || echo false)" ]]; then
      printf 'the pattern matches %s: %s (%s)\n' "${json}" "${got}" "${why}"
      bad=$(( bad + 1 ))
    fi
  done <<< "${rows}"
  (( n >= 100 )) || fail "${n} strings checked, want at least 100"
  (( bad == 0 )) || fail "${bad} of ${n} strings (see above)"
}

@test "yaml11: each value of the table prints as its scalar, and a string as a key too" {
  local rows json want why out n=0 bad=0
  rows="$(_cases)" || fail "the table has a row of another shape"
  while IFS=$'\t' read -r json want why; do
    n=$(( n + 1 ))
    if ! out="$(printf '{"v":%s}\n' "${json}" | kubehz::agent_yaml)"; then
      echo "${json}: yq failed"
      bad=$(( bad + 1 ))
      continue
    fi
    if [[ "${out}" != "v: ${want}" ]]; then
      printf '%s prints %q, want %q\n' "${json}" "${out}" "v: ${want}"
      bad=$(( bad + 1 ))
    fi
    [[ "${json}" == '"'* ]] || continue
    if ! out="$(printf '{%s:1}\n' "${json}" | kubehz::agent_yaml)"; then
      echo "the key ${json}: yq failed"
      bad=$(( bad + 1 ))
      continue
    fi
    if [[ "${out}" != "${want}: 1" ]]; then
      printf 'the key %s prints %q, want %q\n' "${json}" "${out}" "${want}: 1"
      bad=$(( bad + 1 ))
    fi
  done <<< "${rows}"
  (( n >= 100 )) || fail "the table has ${n} cases, want at least 100"
  (( bad == 0 )) || fail "${bad} of ${n} cases print another scalar (see above)"
}

@test "yaml11: a document keeps its block form, and only the strings of the set get quotes" {
  run kubehz::agent_yaml <<<'{"id":"sp-1a2b3c4d","name":"on","nodes":[{"name":"worker-1","status":"Ready","ready":true}],"labels":{"off":"08","note":"1.35.5"},"cap":256,"lease":null,"empty":{},"list":[]}'
  assert_success
  assert_output 'id: sp-1a2b3c4d
name: "on"
nodes:
  - name: worker-1
    status: Ready
    ready: true
labels:
  "off": "08"
  note: 1.35.5
cap: 256
lease: null
empty: {}
list: []'
}
