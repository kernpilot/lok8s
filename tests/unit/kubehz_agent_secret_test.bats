#!/usr/bin/env bats
# kubehz_agent_secret_test.bats — the P0 self-hosted agent-secret claim.
#
# Two surfaces are guarded here:
#
#   1. The in-cluster heartbeat agent (the CronJob script embedded in
#      .lok8s/libs/kubehz/manifests/agent/cronjob.yaml): it self-generates the
#      agent-token A (khz_agt_…) + claim-code C (khzc_…) into Secret
#      kubehz-agent, which the deploy creates empty (fill-if-empty with one
#      merge patch, NEVER rotate, never create), self-registers by POSTing
#      only sha256(A)/sha256(C) to /api/clusters/agent-register, and sends an
#      `Authorization: Bearer A` heartbeat.
#
#   2. `lo kubehz claim-code` (kubehz::claim-code in .lok8s/libs/kubehz/main):
#      prints C for the dashboard /claim page and NEVER prints A.
#
# The SHIPPED manifest is the fixture: the script is extracted with yq and run
# against a file-backed kubectl stub (a fake Secret store) + a curl capture, so
# a regression in the real agent — a rotated secret, a leaked plaintext, a
# missing Bearer header, invalid JSON — fails here.

setup() {
  load "../test_helper"
  setup_tmpdir

  AGENT_DIR="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent"
  CRONJOB="${AGENT_DIR}/cronjob.yaml"

  # The heartbeat script (containers[0].command[2] — the `-c` payload).
  HEARTBEAT="${BATS_TEST_TMPDIR}/heartbeat.sh"
  command yq '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' \
    "${CRONJOB}" > "${HEARTBEAT}"

  # Fake in-cluster Secret store + curl capture sinks. STORE_JSON holds
  # Secret kubehz-agent as `kubectl get secret -o json` prints it. A case
  # starts from a fixture of tests/fixtures/agent-identity-secret
  # (_store_load). The default is the Secret that the deploy creates: no
  # data, resourceVersion 41. STORE_ABSENT=1: no Secret at all.
  export FIXTURES="${_PROJECT_ROOT}/tests/fixtures/agent-identity-secret"
  export STORE_JSON="${BATS_TEST_TMPDIR}/secret.json"
  _store_load no-data
  export REGISTER_OUT="${BATS_TEST_TMPDIR}/register.json"
  export REGISTER_HDRS="${BATS_TEST_TMPDIR}/register.hdrs"
  export HEARTBEAT_OUT="${BATS_TEST_TMPDIR}/heartbeat.json"
  export HEARTBEAT_HDRS="${BATS_TEST_TMPDIR}/heartbeat.hdrs"
  # A timeout call in another form than `timeout -k 2 SECS`, and each
  # kubectl and curl call with its bound (tests/lib/agent_bound.sh).
  export STUB_TIMEOUT_FORM_LOG="${BATS_TEST_TMPDIR}/timeout-form.log"
  export STUB_CALL_LOG="${BATS_TEST_TMPDIR}/calls.log"
  source "${_PROJECT_ROOT}/tests/lib/agent_bound.bash"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
}

teardown() {
  teardown_tmpdir
}

# _store_load NAME: the store starts as the fixture NAME.
_store_load() {
  cp "${FIXTURES}/${1}.json" "${STORE_JSON}"
}
# _store_value KEY: the value of KEY in the store, decoded (nothing for a
# key that is not there).
_store_value() {
  command jq -r --arg k "${1}" '.data[$k] // empty' "${STORE_JSON}" | base64 -d
}
# _store_rv: the resourceVersion of the store.
_store_rv() {
  command jq -r '.metadata.resourceVersion' "${STORE_JSON}"
}

# Build a self-contained runnable heartbeat: a file-backed kubectl (fake Secret
# store) + a URL-routing curl capture, prepended to the shipped script.
_build_runner() {
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
# _stub_not_found: the apiserver's answer for an absent Secret.
_stub_not_found() {
  echo 'Error from server (NotFound): secrets "kubehz-agent" not found' >&2
  return 1
}
# The identity read takes exactly this template. kubectl runs it with
# text/template on the object as JSON. _stub_id_read FILE gives its output
# for the Secret in FILE with jq, with the same field selection: the
# resourceVersion, a space, and for a Secret with entries their number, a
# space, A for an agent-token with a value and C for a claim-code with a
# value. `with .data` is false for a missing or empty data, `index` gives
# nil for a missing key, and nil and "" are false: the jq form gives the
# same for a missing key, null and "". (A Secret's data holds only
# strings.) TestAgentIdentityReadOnTheSecretFixtures runs the template on
# the fixtures of tests/fixtures/agent-identity-secret and checks the .want
# files, and a case below checks this function against the same files.
_STUB_ID_TEMPLATE='{{.metadata.resourceVersion}} {{with .data}}{{len .}} {{if index . "agent-token"}}A{{end}}{{if index . "claim-code"}}C{{end}}{{end}}'
_stub_id_read() {
  jq -j '"\(.metadata.resourceVersion) " + (if ((.data // {}) | length) > 0
      then "\(.data | length) " + (if (.data["agent-token"] // "") != "" then "A" else "" end)
        + (if (.data["claim-code"] // "") != "" then "C" else "" end)
      else "" end)' "${1}"
}
# _stub_store_merge: a merge patch (on stdin) on the store, as the apiserver
# takes it: each key of stringData into data (base64), and a new
# resourceVersion. The values stay off argv.
_stub_store_merge() {
  jq -c --slurpfile s "${STORE_JSON}" '. as $p | $s[0]
    | .data = ((.data // {}) + (($p.stringData // {}) | map_values(@base64)))
    | .metadata.resourceVersion = ((.metadata.resourceVersion | tonumber) + 1 | tostring)' \
    > "${STORE_JSON}.new" && mv "${STORE_JSON}.new" "${STORE_JSON}"
}
kubectl() {
  stub_call kubectl "$@"
  argv_log kubectl "$@"
  case "$*" in
    # The identity read, never a value. Another template is refused: the
    # stub answers only the one it mirrors. STORE_RACE=1: another tick
    # fills a Secret with no entry right after this read (its own values,
    # a new resourceVersion).
    "get secret kubehz-agent -n kubehz-system -o go-template="*)
      [ -z "${STORE_ABSENT:-}" ] || { _stub_not_found; return 1; }
      if [ "$*" != "get secret kubehz-agent -n kubehz-system -o go-template=${_STUB_ID_TEMPLATE}" ]; then
        echo "stub: the identity read sends another template: $*" >&2
        return 1
      fi
      _stub_id_read "${STORE_JSON}"
      if [ -n "${STORE_RACE:-}" ] && [ "$(jq '.data // {} | length' "${STORE_JSON}")" = 0 ]; then
        printf '{"stringData":{"agent-token":"%s","claim-code":"%s"}}' \
          "khz_agt_$(printf 'f%.0s' $(seq 1 64))" "khzc_$(printf 'e%.0s' $(seq 1 64))" | _stub_store_merge
      fi
      ;;
    # The fill: a merge patch on stdin, as the apiserver takes it. A
    # resourceVersion that is not the stored one is a 409 Conflict and
    # changes nothing. stringData sets the keys that it names.
    "patch secret kubehz-agent -n kubehz-system --type=merge --patch-file=/dev/stdin")
      _patch=$(cat)
      [ -z "${STORE_ABSENT:-}" ] || { _stub_not_found; return 1; }
      _rv=$(printf '%s' "${_patch}" | jq -r '.metadata.resourceVersion // ""')
      if [ -n "${_rv}" ] && [ "${_rv}" != "$(jq -r '.metadata.resourceVersion' "${STORE_JSON}")" ]; then
        echo 'Error from server (Conflict): Operation cannot be fulfilled on secrets "kubehz-agent": the object has been modified; please apply your changes to the latest version and try again' >&2
        return 1
      fi
      printf '%s' "${_patch}" | _stub_store_merge
      ;;
    # A value read: the base64 text of the key, as jsonpath prints it;
    # nothing for a key that is not there.
    *"get secret kubehz-agent -n kubehz-system -o jsonpath={.data.agent-token}"*)
      [ -z "${STORE_ABSENT:-}" ] || { _stub_not_found; return 1; }
      jq -j '.data["agent-token"] // ""' "${STORE_JSON}"
      ;;
    *"get secret kubehz-agent -n kubehz-system -o jsonpath={.data.claim-code}"*)
      [ -z "${STORE_ABSENT:-}" ] || { _stub_not_found; return 1; }
      jq -j '.data["claim-code"] // ""' "${STORE_JSON}"
      ;;
    # Any other call on the Secret (a create, a delete, an apply) is
    # refused, as the agent's RBAC does: get and patch on this one name.
    *"secret"*)
      echo "Error from server (Forbidden): the agent may not: kubectl $*" >&2
      return 1
      ;;
    # kubernetesVersion = the SERVER version. The FIX reads the apiserver
    # /version endpoint (single "gitVersion"). `version -o json` carries BOTH a
    # client (v1.31.4) and a DIFFERENT server (v1.35.5) — the regression guard.
    *"/version"*) printf '{"major":"1","minor":"35","gitVersion":"v1.35.5","platform":"linux/amd64"}\n' ;;
    "version -o json") printf '{\n  "clientVersion": { "gitVersion": "v1.31.4" },\n  "serverVersion": { "gitVersion": "v1.35.5" }\n}\n' ;;

    # The node list: ONE call for all nodes, read with jq. A realistic
    # two-node dump WITH the greedy-sed traps (runtimeHandlers +
    # volumesAttached). cp-1 carries the control-plane label KEY (empty
    # value); worker-1 does not.
    "get --raw /api/v1/nodes") printf '%s\n' '{"items":[{"metadata":{"name":"cp-1","labels":{"kubernetes.io/hostname":"cp-1","node-role.kubernetes.io/control-plane":"","node.kubernetes.io/instance-type":"cx32"}},"status":{"conditions":[{"type":"Ready","status":"True"}],"runtimeHandlers":[{"name":"runc"},{"name":"crun"}],"volumesAttached":[{"name":"kubernetes.io/csi/csi.hetzner.cloud^0001"}]}},{"metadata":{"name":"worker-1","labels":{"kubernetes.io/hostname":"worker-1","node.kubernetes.io/instance-type":"cpx41"}},"status":{"conditions":[{"type":"Ready","status":"True"}],"runtimeHandlers":[{"name":"runc"}]}}]}' ;;
    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;
    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  stub_call curl "$@"
  # curl_capture reads the call the way curl does: headers from -H and the
  # config (-K), the body from stdin.
  curl_capture "$@"
  _url="${CURL_URL}"; _body="${CURL_BODY}"; _hdrs=""
  _hdr_ifs="${IFS}"
  IFS='
'
  for _h in ${CURL_HEADERS}; do _hdrs="${_hdrs} ${_h} ;"; done
  IFS="${_hdr_ifs}"
  case "${_url}" in
    *"/agent-register")
      printf '%s' "${_body}" > "${REGISTER_OUT}"
      printf '%s' "${_hdrs}" > "${REGISTER_HDRS}" ;;
    *"/heartbeat")
      printf '%s' "${_body}" > "${HEARTBEAT_OUT}"
      printf '%s' "${_hdrs}" > "${HEARTBEAT_HDRS}" ;;
  esac
  return 0
}
STUBS_EOF

  RUNNER="${BATS_TEST_TMPDIR}/run.sh"
  cat "${_PROJECT_ROOT}/tests/lib/curl_capture.sh" "${_PROJECT_ROOT}/tests/lib/agent_bound.sh" \
    "${stubs}" "${HEARTBEAT}" > "${RUNNER}"
}

# ── The shipped script stays valid POSIX sh ──────────────

@test "agent: the embedded heartbeat script passes sh -n" {
  run sh -n "${HEARTBEAT}"
  assert_success
}

# ── L1: fill-if-empty — first run mints A and C ──────────

@test "agent: first run fills the empty kubehz-agent Secret with khz_agt_ / khzc_ values" {
  _build_runner
  # The deploy created the Secret empty: no data.
  run command jq -e 'has("data") | not' "${STORE_JSON}"
  assert_success

  run sh "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

  # A = khz_agt_<64 hex>, C = khzc_<64 hex> — 256-bit, distinct prefixes.
  run _store_value agent-token
  assert_output --regexp '^khz_agt_[0-9a-f]{64}$'
  run _store_value claim-code
  assert_output --regexp '^khzc_[0-9a-f]{64}$'
  # One read, one patch with the read's resourceVersion, no create.
  run grep -c '^bound kubectl patch secret kubehz-agent -n kubehz-system --type=merge --patch-file=/dev/stdin$' "${STUB_CALL_LOG}"
  assert_output "1"
  run grep -E 'kubectl (create|apply|replace|delete) ' "${STUB_CALL_LOG}"
  assert_failure
  assert_equal "$(_store_rv)" "42"
}

# Two ticks read the empty Secret at the same time: the patch of the
# second one carries the resourceVersion of its read, the apiserver
# refuses it (409), and the tick reads back the values of the first.
@test "agent: a fill that loses the race (409) keeps the values of the tick that won" {
  export STORE_RACE=1
  _build_runner
  run sh "${RUNNER}"
  assert_success
  assert_agent_calls_bounded
  assert_output --partial "the fill of Secret kubehz-agent did not land"
  local won_a won_c
  won_a="khz_agt_$(printf 'f%.0s' $(seq 1 64))"
  won_c="khzc_$(printf 'e%.0s' $(seq 1 64))"
  assert_equal "$(_store_value agent-token)" "${won_a}"
  assert_equal "$(_store_value claim-code)" "${won_c}"
  # The register and the beat use the winner's values.
  run command jq -r '.agentTokenHash' "${REGISTER_OUT}"
  assert_output "$(printf %s "${won_a}" | sha256sum | cut -d' ' -f1)"
  run cat "${HEARTBEAT_HDRS}"
  assert_output --partial "Authorization: Bearer ${won_a} ;"
}

# A Secret that holds one of the two keys is never written: a fill would
# rotate the key that is there.
@test "agent: a Secret with only an agent-token is not written" {
  local a="khz_agt_$(printf 'c%.0s' $(seq 1 64))"
  _store_load agent-token-only
  _build_runner
  run sh "${RUNNER}"
  assert_success
  assert_agent_calls_bounded
  assert_output --partial "kubehz: Secret kubehz-agent in kubehz-system has data, but not both an agent-token and a claim-code with a value; the agent does not write it"
  run grep -F 'kubectl patch ' "${STUB_CALL_LOG}"
  assert_failure
  cmp "${STORE_JSON}" "${FIXTURES}/agent-token-only.json"
  assert_equal "$(_store_value agent-token)" "${a}"
  # No claim code: no register; the beat goes out with A.
  [ ! -f "${REGISTER_OUT}" ]
  run cat "${HEARTBEAT_HDRS}"
  assert_output --partial "Authorization: Bearer ${a} ;"
}

# A Secret with any entry is never written, whatever its value: a key with
# an empty value, another key, or one half of the identity. The tick logs
# one line (no value) and uses what is there: a heartbeat needs a value
# for agent-token, the register a value for both keys.
@test "agent: a Secret with data but not a full identity is never written: empty values, another key, one half" {
  local f a="khz_agt_$(printf 'c%.0s' $(seq 1 64))"
  _build_runner
  for f in empty-values empty-agent-token empty-claim-code another-key-only another-key-empty claim-code-only agent-token-only; do
    _store_load "${f}"
    : > "${STUB_CALL_LOG}"
    rm -f "${REGISTER_OUT}" "${HEARTBEAT_OUT}" "${HEARTBEAT_HDRS}"
    run sh "${RUNNER}"
    assert_success
    assert_output --partial "kubehz: Secret kubehz-agent in kubehz-system has data, but not both an agent-token and a claim-code with a value; the agent does not write it"
    run grep -E 'kubectl (patch|create|apply|replace|delete) ' "${STUB_CALL_LOG}"
    assert_failure
    cmp "${STORE_JSON}" "${FIXTURES}/${f}.json"
    [ ! -f "${REGISTER_OUT}" ]
    run grep -c '^UNBOUND ' "${STUB_CALL_LOG}"
    assert_output "0"
    case "${f}" in
      empty-claim-code|agent-token-only)
        run cat "${HEARTBEAT_HDRS}"
        assert_output --partial "Authorization: Bearer ${a} ;" ;;
      *)
        [ ! -f "${HEARTBEAT_OUT}" ] ;;
    esac
  done
}

# A Secret with the identity and another key holds the identity: no write,
# no line, and the register and the beat use its values.
@test "agent: a Secret with the identity and another key is not written and logs nothing" {
  local f
  _build_runner
  for f in identity identity-and-another-key identity-and-a-key-before; do
    _store_load "${f}"
    : > "${STUB_CALL_LOG}"
    run sh "${RUNNER}"
    assert_success
    assert_agent_calls_bounded
    refute_output --partial "the agent does not write it"
    run grep -F 'kubectl patch ' "${STUB_CALL_LOG}"
    assert_failure
    cmp "${STORE_JSON}" "${FIXTURES}/${f}.json"
    run command jq -r '.agentTokenHash' "${REGISTER_OUT}"
    assert_output "$(_store_value agent-token | sha256sum | cut -d' ' -f1)"
  done
}

# The stub answers the identity read as kubectl does: on each fixture, its
# jq form gives the .want file, which TestAgentIdentityReadOnTheSecretFixtures
# checks against text/template with the template of the script. And the
# script sends exactly the template that the stub answers.
@test "agent: the stub of the identity read prints what kubectl prints on each Secret fixture" {
  local f n=0
  _build_runner
  for f in "${FIXTURES}"/*.json; do
    run sh -c '. "${1}" && _stub_id_read "${2}"' _ "${BATS_TEST_TMPDIR}/stubs.sh" "${f}"
    assert_success
    assert_equal "[${output}]" "$(cat "${f%.json}.want")"
    n=$(( n + 1 ))
  done
  [ "${n}" -ge 10 ]
  run sh -c '. "${1}" && printf "%s" "${_STUB_ID_TEMPLATE}"' _ "${BATS_TEST_TMPDIR}/stubs.sh"
  assert_success
  [ "$(grep -cF -- "-o 'go-template=${output}'" "${HEARTBEAT}")" = 1 ]
}

# The patch takes the resourceVersion as the read gave it. Only digits pass:
# a quote in it could end the JSON string and add a key to the patch.
@test "agent: a resourceVersion that is not digits stops the fill" {
  command jq '.metadata.resourceVersion = "41\",\"data\":{\"x\":\"y"' "${FIXTURES}/no-data.json" > "${STORE_JSON}"
  _build_runner
  run sh "${RUNNER}"
  assert_success
  assert_output --partial "has no resourceVersion of digits; the agent does not fill it"
  refute_output --partial "has data, but not both"
  run grep -F 'kubectl patch ' "${STUB_CALL_LOG}"
  assert_failure
  run command jq -e 'has("data") | not' "${STORE_JSON}"
  assert_success
}

# The agent cannot create the Secret (no create in its Role). Without the
# Secret the tick has no identity: no write, no register, no beat, and a
# line that names the fix.
@test "agent: an absent Secret is not created; the tick has no identity" {
  export STORE_ABSENT=1
  _build_runner
  run sh "${RUNNER}"
  assert_success
  assert_output --partial "cannot read Secret kubehz-agent in kubehz-system; the deploy creates it: run 'lo kubehz deploy'"
  assert_output --partial "agent-token unavailable; skipping heartbeat"
  run grep -E 'kubectl (patch|create|apply|replace|delete) ' "${STUB_CALL_LOG}"
  assert_failure
  cmp "${STORE_JSON}" "${FIXTURES}/no-data.json"
  [ ! -f "${REGISTER_OUT}" ]
  [ ! -f "${HEARTBEAT_OUT}" ]
  run grep -c '^UNBOUND ' "${STUB_CALL_LOG}"
  assert_output "0"
}

# The stub catches a call without the bound: a runner with one kubectl call
# outside timeout fails the assertion.
@test "agent: assert_agent_calls_bounded fails on a call without the bound" {
  _build_runner
  sed -i 's/bounded 5 kubectl get csr -o json/kubectl get csr -o json/' "${RUNNER}"
  run grep -c 'kubectl get csr -o json' "${RUNNER}"
  assert_output "1"
  run sh "${RUNNER}"
  assert_success
  run assert_agent_calls_bounded
  assert_failure
  assert_output --partial "UNBOUND kubectl get csr -o json"
}

# ── L1: never rotate — a second run reuses the same A and C ─

@test "agent: a second run NEVER rotates A or C (idempotent secret)" {
  _build_runner

  run sh "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded
  local a1 c1
  a1="$(_store_value agent-token)"
  c1="$(_store_value claim-code)"

  # Re-run against the SAME store (mirrors a re-applied / re-fired agent).
  run sh "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

  assert_equal "$(_store_value agent-token)" "${a1}"
  assert_equal "$(_store_value claim-code)" "${c1}"

  # And the beat still authenticates with that same, unrotated A.
  run cat "${HEARTBEAT_HDRS}"
  assert_output --partial "Authorization: Bearer ${a1}"
}

# ── L2: agent-register carries the two HASHES, never plaintext ─

@test "agent: A, C and the bind secret never reach a command line" {
  # Every process on the node can read argv (ps, /proc/<pid>/cmdline). The
  # stubs log the argv of each curl and kubectl call (curl_capture.sh).
  # Spies for the tools that a filter or redaction step could hand a
  # secret to (sed, grep, base64, …) log their argv too.
  export ARGV_LOG="${BATS_TEST_TMPDIR}/argv.log"
  export BIND_SECRET="5e175e175e175e175e175e175e175e175e175e175e175e175e175e175e175e17"
  source "${_PROJECT_ROOT}/tests/lib/argv_sentinels.bash"
  argv_spy "${ARGV_SPY_TOOLS[@]}"
  _build_runner
  run sh "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

  local a c
  a="$(_store_value agent-token)"
  c="$(_store_value claim-code)"
  assert_argv_clean "${a}" "${c}" "${BIND_SECRET}"
  # They still arrive: the bind secret in the register body, A as the
  # bearer of the beat.
  run command jq -r '.bindSecret' "${REGISTER_OUT}"
  assert_output "${BIND_SECRET}"
  run cat "${HEARTBEAT_HDRS}"
  assert_output --partial "Authorization: Bearer ${a} ;"
}

# The bind secret comes in two forms: 64 lowercase hex, or khzb_ and 64
# lowercase hex. The register sends either one byte for byte.
@test "agent: a khzb_ bind secret goes into the register body as it is" {
  export BIND_SECRET="khzb_$(printf 'ab%.0s' $(seq 1 32))"
  _build_runner
  run sh "${RUNNER}"
  assert_success
  assert_agent_calls_bounded
  run command jq -r '.bindSecret' "${REGISTER_OUT}"
  assert_output "${BIND_SECRET}"
}

# Any other value is left out of the register body, and the body stays
# valid JSON: a wrong case, a wrong prefix, a length next to 64 or 69, and
# a line end or a newline inside.
@test "agent: a bind secret of any other shape is left out of the register body" {
  local hex name
  hex="$(printf '5e%.0s' $(seq 1 32))"
  _build_runner
  for name in upper-prefix upper-hex khza prefixed-63 prefixed-65 long65 short63 \
    prefixed-newline prefixed-cr prefixed-crlf nl64 nl69 nl69-first; do
    case "${name}" in
      upper-prefix) BIND_SECRET="KHZB_${hex}" ;;
      upper-hex) BIND_SECRET="khzb_${hex^^}" ;;
      khza) BIND_SECRET="khza_${hex}" ;;
      prefixed-63) BIND_SECRET="khzb_${hex:1}" ;;
      prefixed-65) BIND_SECRET="khzb_${hex}a" ;;
      long65) BIND_SECRET="${hex}a" ;;
      short63) BIND_SECRET="${hex:1}" ;;
      prefixed-newline) BIND_SECRET="khzb_${hex}"$'\n' ;;
      prefixed-cr) BIND_SECRET="khzb_${hex}"$'\r' ;;
      prefixed-crlf) BIND_SECRET="khzb_${hex}"$'\r\n' ;;
      nl64) BIND_SECRET="${hex:0:32}"$'\n'"${hex:33}" ;;
      nl69) BIND_SECRET="${hex}"$'\nabcd' ;;
      nl69-first) BIND_SECRET=$'abcd\n'"${hex}" ;;
    esac
    export BIND_SECRET
    _store_load no-data
    rm -f "${REGISTER_OUT}"
    run sh "${RUNNER}"
    assert_success
    run command jq -e 'has("agentTokenHash") and (has("bindSecret") | not)' "${REGISTER_OUT}"
    [ "${status}" -eq 0 ] || { echo "the register body of ${name} is not valid JSON without bindSecret"; return 1; }
  done
}

@test "agent: agent-register posts sha256(A)/sha256(C) — never the plaintext secrets" {
  _build_runner
  run sh "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

  # Valid JSON with the domain and both hashes.
  run command jq -e . "${REGISTER_OUT}"
  assert_success
  run command jq -r '.domain' "${REGISTER_OUT}"
  assert_output "test.example.com"

  local a c expect_ah expect_ch
  a="$(_store_value agent-token)"
  c="$(_store_value claim-code)"
  expect_ah="$(printf %s "${a}" | sha256sum | cut -d' ' -f1)"
  expect_ch="$(printf %s "${c}" | sha256sum | cut -d' ' -f1)"

  run command jq -r '.agentTokenHash' "${REGISTER_OUT}"
  assert_output "${expect_ah}"
  run command jq -r '.claimCodeHash' "${REGISTER_OUT}"
  assert_output "${expect_ch}"

  # The wire payload must NOT contain either plaintext secret.
  run grep -F "${a}" "${REGISTER_OUT}"
  assert_failure
  run grep -F "${c}" "${REGISTER_OUT}"
  assert_failure
  run grep -Eq 'khz_agt_|khzc_' "${REGISTER_OUT}"
  assert_failure
}

# ── L3: the heartbeat is Bearer-authenticated with A ─────

@test "agent: the heartbeat sends Authorization: Bearer khz_agt_… and valid JSON" {
  _build_runner
  run sh "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

  local a
  a="$(_store_value agent-token)"

  run cat "${HEARTBEAT_HDRS}"
  assert_output --partial "Authorization: Bearer khz_agt_"
  assert_output --partial "Authorization: Bearer ${a}"

  # The heartbeat body is still valid JSON with the expected fields.
  run command jq -e . "${HEARTBEAT_OUT}"
  assert_success
  run command jq -r '.clusterId' "${HEARTBEAT_OUT}"
  assert_output "test.example.com"
  run command jq -r '.nodes[0].instanceType' "${HEARTBEAT_OUT}"
  assert_output "cx32"

  # Node identity, per-node roles, and the SERVER version — the P0 heartbeat
  # parsing fixes. Names are exactly the two metadata names (never a
  # runtimeHandler/CSI-handle ghost); cp-1 is control-plane, worker-1 is worker
  # (an absent role label is not misread as control-plane); the kubernetes
  # version is the server's (v1.35.5), not the client image tag (v1.31.4).
  run command jq -rc '[.nodes[].name]' "${HEARTBEAT_OUT}"
  assert_output '["cp-1","worker-1"]'
  run command jq -r '.nodes[] | select(.name == "cp-1") | .roles' "${HEARTBEAT_OUT}"
  assert_output "control-plane"
  run command jq -r '.nodes[] | select(.name == "worker-1") | .roles' "${HEARTBEAT_OUT}"
  assert_output "worker"
  run command jq -r '.kubernetes.version' "${HEARTBEAT_OUT}"
  assert_output "v1.35.5"

  # A must never appear in the heartbeat BODY — only in the Authorization header.
  run grep -F "${a}" "${HEARTBEAT_OUT}"
  assert_failure
}

# ── The shipped kustomization renders the whole wiring ───

@test "agent: kustomization renders agent-register, the Bearer header, and the Secret RBAC" {
  run command kustomize build "${AGENT_DIR}"
  assert_success
  assert_output --partial "kind: CronJob"
  assert_output --partial "/api/clusters/agent-register"
  assert_output --partial "Authorization: Bearer"
  # Namespaced Secret RBAC: get and patch on the one name, no create.
  assert_output --partial "name: kubehz-agent-secret"
  assert_output --partial "secrets"
  # The deploy creates the identity Secret empty: no data, no stringData.
  run bash -c "command kustomize build '${AGENT_DIR}' | command yq -e 'select(.kind == \"Secret\" and .metadata.name == \"kubehz-agent\") | (has(\"data\") or has(\"stringData\")) | not'"
  assert_success
  assert_output "true"
}

# ── The agent image must be a shell-capable image, pinned by digest ─
# The bootstrap/heartbeat script is bash/sh — it CANNOT run on the distroless
# registry.k8s.io/kubectl image (no shell). Guard the shell-capable image and
# its digest pin (supply chain) so a future edit can't silently regress either.

@test "agent: the CronJob image is shell-capable and pinned by digest" {
  local image
  image="$(command yq '.spec.jobTemplate.spec.template.spec.containers[0].image' "${CRONJOB}")"
  # Pinned by digest (immutable, supply-chain safe).
  [[ "${image}" == *"@sha256:"* ]]
  # Not the distroless kubectl image (no shell → the script is DOA on it).
  [[ "${image}" != registry.k8s.io/kubectl:* ]]
}

# ── L4: `lo kubehz claim-code` prints C and NEVER A ──────

@test "claim-code: prints C (khzc_) and never A (khz_agt_)" {
  export TEST_A="khz_agt_$(printf 'a%.0s' $(seq 1 64))"
  export TEST_C="khzc_$(printf 'b%.0s' $(seq 1 64))"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  # kubectl mock: the Secret exists; agent-token would surface A if queried,
  # claim-code returns C. claim-code must only ever read the claim-code key.
  kubectl() {
    case "$*" in
      "-n kubehz-system get secret kubehz-agent") return 0 ;;
      *"agent-token"*) printf '%s' "${TEST_A}" | base64 | tr -d '\n' ;;
      *"claim-code"*)  printf '%s' "${TEST_C}" | base64 | tr -d '\n' ;;
      *) return 0 ;;
    esac
  }
  export -f kubectl

  run kubehz::claim-code
  assert_success
  assert_output --partial "${TEST_C}"
  refute_output --partial "khz_agt_"
  refute_output --partial "${TEST_A}"
}

# ── claim-code: no Secret yet → a clear, non-zero error ──

@test "claim-code: errors clearly when the kubehz-agent Secret is absent" {
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubectl() { return 1; }   # secret not found
  export -f kubectl

  run kubehz::claim-code
  assert_failure
  assert_output --partial "not found"
}
