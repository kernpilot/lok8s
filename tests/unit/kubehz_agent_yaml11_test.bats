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

@test "yaml11: each value of the table prints as its scalar, and a string as a key too" {
  local line json want out n=0 bad=0
  while IFS= read -r line; do
    [[ -n "${line}" && "${line}" != "#"* ]] || continue
    [[ "${line}" == *$'\t'* ]] || fail "not <json value> TAB <yaml scalar>: ${line}"
    json="${line%%$'\t'*}"
    want="${line#*$'\t'}"
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
  done < "${_PROJECT_ROOT}/tests/fixtures/yaml11-quote/cases"
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
