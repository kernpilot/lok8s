#!/usr/bin/env bats
# descriptor_json_test.bats — descriptor loading must not throw the variable
# expansion away, and the hosted-cluster id lookup must survive every body shape.
#
# Both halves of issue #65.
#
# 1. template::descriptor_json. The pattern it replaces was
#
#        desc=$(envsubst < "${file}")
#        echo "${desc}" | jq empty || desc=$(yq -o json '.' "${file}")
#                                                          ^^^^^^^^
#    — on the YAML path it re-read the RAW file, so every ${VAR} reached the
#    provider unexpanded. Five sites in the hetzner provider did this, two of
#    which never expanded at all. A JSON descriptor worked, which is why it went
#    unnoticed: the bug only bites the YAML branch.
#
# 2. The hosted-cluster id lookup. `//` does NOT rescue an error, so
#    `.data[0].id // .data.id // .[0].id // .id // empty` aborted (jq rc 5) on
#    any body that was not an enveloped list — measured: four of five shapes,
#    including the empty `{"data":[]}`. With `|| true` swallowing it, destroy
#    read "no hosted cluster found, nothing to destroy" for a cluster that
#    exists, which then kept running and billing.

setup() {
  load "../test_helper"
  setup_tmpdir
  import() { :; }
  export -f import
  command -v yq &>/dev/null || skip "yq not available"
  command -v jq &>/dev/null || skip "jq not available"
  command -v envsubst &>/dev/null || skip "envsubst not available"
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh" 2>/dev/null || true
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
}

teardown() { teardown_tmpdir; }

@test "a YAML descriptor keeps its variable expansion" {
  local f="${BATS_TEST_TMPDIR}/d.yaml"
  cat > "${f}" <<'EOF'
cluster: "${DESC_TEST_VAR}"
server:
  - name: "n-${DESC_TEST_VAR}"
EOF
  export DESC_TEST_VAR=EXPANDED
  run template::descriptor_json "${f}"
  [ "$status" -eq 0 ] || { echo "$output" >&2; return 1; }
  [[ "$output" == *EXPANDED* ]] || {
    echo "the expansion was discarded — the YAML branch re-read the raw file:" >&2
    echo "$output" >&2; return 1; }
  [[ "$output" != *'${DESC_TEST_VAR}'* ]] || {
    echo "the literal placeholder survived into the output" >&2; return 1; }
  # …and it is JSON, not YAML passed through.
  printf '%s' "$output" | jq -e '.server[0].name == "n-EXPANDED"' >/dev/null
}

@test "a JSON descriptor also expands (and stays JSON)" {
  # The shape that always worked; pinned so a 'fix' cannot regress it.
  local f="${BATS_TEST_TMPDIR}/d.json"
  printf '{"cluster":"${DESC_TEST_VAR}"}\n' > "${f}"
  export DESC_TEST_VAR=JEXPANDED
  run template::descriptor_json "${f}"
  [ "$status" -eq 0 ]
  printf '%s' "$output" | jq -e '.cluster == "JEXPANDED"' >/dev/null
}

@test "a missing descriptor fails rather than emitting empty JSON" {
  run template::descriptor_json "${BATS_TEST_TMPDIR}/nope.yaml"
  [ "$status" -ne 0 ]
}

@test "a descriptor that is neither JSON nor YAML fails loudly" {
  local f="${BATS_TEST_TMPDIR}/bad.yaml"
  printf 'key: [unclosed\n  - :::\n' > "${f}"
  run template::descriptor_json "${f}"
  [ "$status" -ne 0 ] || { echo "garbage parsed as a descriptor: $output" >&2; return 1; }
}

# ── the cluster-id lookup, per body shape ───────────────────────────────────
# The lookup lives in kubehz::resolve_cluster_id (libs/kubehz/main) — hosted
# destroy and deregister both route through it. Exercised DIRECTLY with a
# mocked curl per shape, so the test cannot drift from the code by quietly
# keeping its own copy of the jq expression. A foreign body shape must resolve
# or report "no row" (rc 2) quietly — never a jq abort ('//' does not rescue a
# jq ERROR; that abort is how destroy once read a live cluster as "no hosted
# cluster found").

@test "the cluster-id lookup resolves every body shape without erroring" {
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  local body want out rc
  # shape → expected id ('' means "no row": rc 2 and no output, not an error)
  while IFS='|' read -r body want; do
    [[ -n "${body}" ]] || continue
    export CURL_BODY="${body}"
    curl() { printf '%s' "${CURL_BODY}"; }
    export -f curl
    rc=0
    out=$(kubehz::resolve_cluster_id "test.kubehz.dev" "https://api.test") || rc=$?
    if [[ -n "${want}" ]]; then
      [ "${rc}" -eq 0 ] || { echo "resolve FAILED (rc=${rc}) on ${body}" >&2; return 1; }
      [ "${out}" = "${want}" ] || { echo "shape ${body}: expected '${want}', got '${out}'" >&2; return 1; }
    else
      [ "${rc}" -eq 2 ] || { echo "shape ${body}: expected rc 2 (no row), got rc=${rc} out='${out}'" >&2; return 1; }
      [ -z "${out}" ] || { echo "shape ${body}: expected no output, got '${out}'" >&2; return 1; }
    fi
  done <<'SHAPES'
{"data":[{"id":"A1","domain":"test.kubehz.dev"}]}|A1
{"data":{"id":"B2","domain":"test.kubehz.dev"}}|B2
[{"id":"C3","domain":"test.kubehz.dev"}]|C3
{"id":"D4","domain":"test.kubehz.dev"}|D4
{"data":[]}|
{}|
[]|
SHAPES
}
