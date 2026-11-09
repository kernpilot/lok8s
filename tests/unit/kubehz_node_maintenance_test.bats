#!/usr/bin/env bats
# kubehz_node_maintenance_test.bats — `lo kubehz node cordon|uncordon|drain`,
# one node of a Space (hosting: shared), through the platform's tenant
# node-maintenance routes. The Go twin is internal/kubehz/maintenance_test.go.
#
# What is pinned here
# -------------------
#   preflight  the hosting gate (shared only), HTTPS, KUBEHZ_TOKEN, and the
#              two ways a space id is found (--space-id, or the space list)
#   input      the node name, the space id and the drain --timeout are
#              checked BEFORE any request
#   request    the route, and the body: none for cordon and uncordon, only
#              the given fields for drain
#   answer     the lines on stdout, and the line for a replaced request
#   errors     each refusal code of the three routes, and the 404
#   status     the cordon and the maintenance state on the node lines of
#              `lo kubehz status`
#
# The curl mock is keyed on "<METHOD> <path>" and answers with the trailing
# status line that the -w $'\n%{http_code}' contract of kubehz::space_api
# expects: the mock shape of kubehz_node_test.bats.

setup() {
  load "../test_helper"
  # run --separate-stderr: the answer goes to stdout, a refusal to stderr.
  bats_require_minimum_version 1.5.0
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/shared"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/node"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  # argsh `:args` builtin: absent when a lib is sourced in bats (the stub of
  # kubehz_node_test.bats). A boolean is any --flag whose next token is
  # another flag or the end of argv. An empty next token is a value, as in
  # argsh: `--name ''` sets name to the empty string.
  :args() {
    shift # description
    while (( $# )); do
      case "$1" in
        --*)
          local _n="${1#--}"
          _n="${_n//-/_}"
          if (( $# < 2 )) || [[ "${2}" == --* ]]; then
            printf -v "${_n}" '%s' 1
            shift
          else
            printf -v "${_n}" '%s' "${2}"
            shift 2
          fi
          ;;
        *) shift ;;
      esac
    done
  }
  export -f :args

  mkdir -p "${PATH_CLUSTERS}/acme.example.org"
  touch "${PATH_CLUSTERS}/acme.example.org/cluster.lok8s.yaml"
  export DOMAIN_NAME="acme.example.org"

  export KUBEHZ_TOKEN="khz_test_token"
  export CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  export CURL_BODY="${BATS_TEST_TMPDIR}/curl-body.json"
  : > "${CURL_LOG}"

  # The defaults live HERE, never as `${VAR:-{...}}` in the mock: a `}` in
  # a default closes the expansion early (see kubehz_node_test.bats).
  export STUB_SPACES_BODY='{"ok":true,"data":[{"id":"sp-5","slug":"acme","status":"Active"}]}'
  export STUB_ACT_CODE="202"
  export STUB_ACT_BODY='{"ok":true,"data":{"name":"worker-1","status":"requested","replaced":null}}'
  export STUB_NODES_BODY='{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxNodes":2}}}'
}

teardown() {
  teardown_tmpdir
}

# ── stubs ────────────────────────────────────────────────

# yq stub for kubehz::read_config and kubehz::space_config: a Space on an
# HTTPS api with no space block (the slug is the first label, acme).
_stub_config() {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"')           echo "${STUB_HOSTING:-shared}" ;;
      '.spec.kubehz.apiUrl // ""')                echo "${STUB_API_URL:-https://api.example.test}" ;;
      '.spec.kubehz.access')                      echo "null" ;;
      '.spec.kubehz.connectHcloudToken // false') echo "false" ;;
      '.spec.kubehz.agent // "cronjob"')          echo "cronjob" ;;
      '.spec.kubehz.space.nodes[]?')              : ;;
      *'| type')                                  echo '!!null' ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# One curl mock for the routes these verbs touch. The three maintenance
# routes of worker-1 answer STUB_ACT_CODE/STUB_ACT_BODY; every request line
# goes to CURL_LOG, and a body to CURL_BODY (a request with no body writes
# no CURL_BODY). STUB_POST_CURL_RC makes every POST a transport failure:
# curl prints nothing and exits with that code.
_stub_curl() {
  curl() {
    local method="GET" url="" body="" want_status=0 ctype=""
    while (( $# )); do
      case "$1" in
        -X) method="$2"; shift 2 ;;
        -d) body="$2"; shift 2 ;;
        -H) [[ "$2" == Content-Type:* ]] && ctype="$2"; shift 2 ;;
        -w) want_status=1; shift 2 ;;
        https://*) url="$1"; shift ;;
        *) shift ;;
      esac
    done
    echo "${method} ${url}${ctype:+ ${ctype}}" >> "${CURL_LOG}"
    [[ -n "${body}" ]] && printf '%s' "${body}" > "${CURL_BODY}"
    if [[ "${method}" == POST && -n "${STUB_POST_CURL_RC:-}" ]]; then
      echo "curl: (${STUB_POST_CURL_RC}) stub failure" >&2
      return "${STUB_POST_CURL_RC}"
    fi

    local answer status
    case "${method} ${url##*api.example.test}" in
      "GET /api/spaces")
        answer="${STUB_SPACES_BODY}"; status="200" ;;
      "GET /api/spaces/sp-5/nodes")
        answer="${STUB_NODES_BODY}"; status="200" ;;
      "POST /api/spaces/"*"/nodes/worker-1/"*)
        answer="${STUB_ACT_BODY}"; status="${STUB_ACT_CODE}" ;;
      "POST /api/spaces/"*)
        answer='{"ok":false,"data":{"code":"NOT_FOUND","message":"Node not found in this space"}}'
        status="404" ;;
      *)
        answer='{"ok":false,"data":{"code":"UNMOCKED"}}'
        status="500" ;;
    esac
    if (( want_status )); then
      printf '%s\n%s' "${answer}" "${status}"
    else
      printf '%s' "${answer}"
    fi
  }
  export -f curl
}

_stub_all() {
  _stub_config
  _stub_curl
}

# A refusal body with a code, the api's message and its help.
_refusal() {
  printf '{"ok":false,"data":{"code":"%s","message":"The lease of this space ended","help":"After the end, the api refuses every change except the delete."}}' "$1"
}

# ── preflight ────────────────────────────────────────────

@test "preflight: a hosted domain is refused with the kubectl hint, before any request" {
  export STUB_HOSTING="hosted"
  _stub_all
  for verb in cordon uncordon drain; do
    run "node::${verb}" --name worker-1
    assert_failure
    assert_output --partial "kubehz: 'acme.example.org' is hosting: hosted. This command is for the nodes of a Space."
    assert_output --partial "A hosted cluster has its own API server. Use kubectl cordon, uncordon or drain there."
  done
  [[ ! -s "${CURL_LOG}" ]]
}

@test "preflight: a self-hosted domain is refused with the kubectl hint" {
  export STUB_HOSTING="self"
  _stub_all
  run node::drain --name worker-1
  assert_failure
  assert_output --partial "kubehz: 'acme.example.org' is hosting: self. This command is for the nodes of a Space."
  assert_output --partial "Use kubectl cordon, uncordon or drain against your own API server."
  [[ ! -s "${CURL_LOG}" ]]
}

@test "preflight: no domain, and no spec, are each refused" {
  _stub_all
  DOMAIN_NAME="" run node::cordon --name worker-1
  assert_failure
  assert_output --partial "No active domain. Use: lo use <domain>"

  run node::cordon --name worker-1 --domain other.example.org
  assert_failure
  assert_output --partial "No cluster.lok8s.yaml for domain: other.example.org"
}

@test "preflight: a plain-HTTP apiUrl is refused before any request" {
  export STUB_API_URL="http://api.example.org"
  _stub_all
  run node::cordon --name worker-1
  assert_failure
  assert_output --partial "spec.kubehz.apiUrl must use HTTPS: http://api.example.org"
  [[ ! -s "${CURL_LOG}" ]]
}

@test "preflight: KUBEHZ_TOKEN is required, before any request" {
  _stub_all
  KUBEHZ_TOKEN="" run node::drain --name worker-1
  assert_failure
  assert_output --partial "KUBEHZ_TOKEN is required to drain a node."
  assert_output --partial "Mint a clusters:write API token in the dashboard (Access -> API Tokens)."
  [[ ! -s "${CURL_LOG}" ]]
}

@test "preflight: --cluster/-s is refused with the --space-id hint" {
  _stub_all
  for flag in "--cluster=x" "-sx"; do
    run node::cordon --name worker-1 "${flag}"
    assert_failure
    assert_output --partial "kubehz: --cluster/-s names the kind cluster lo manages, not the target here."
    assert_output --partial "  Name the space with:  --space-id sp-xxxxxxxx"
    refute_output --partial "--cluster-id"
  done
  # --cluster-id is no --cluster: it passes this gate.
  run node::reject_space_cluster_flag --cluster-id cl-1
  assert_success
}

# ── input ────────────────────────────────────────────────

@test "input: a name that is not a DNS label is refused before any request" {
  _stub_all
  for name in "Worker-1" "../spaces" "a_b" "-x" "$(printf 'a%.0s' {1..64})"; do
    run node::cordon --name "${name}"
    assert_failure
    assert_output --partial "is not a node name the platform accepts."
  done
  [[ ! -s "${CURL_LOG}" ]]
}

@test "input: a space id that is not one is refused before any request" {
  _stub_all
  for id in "../sp-5" "sp-5/nodes" "sp 5" "sp-5?x=1" "sp-5%2F"; do
    run node::uncordon --name worker-1 --space-id "${id}"
    assert_failure
    assert_output --partial "kubehz: '${id}' is not a space id."
    assert_output --partial "A space id looks like sp-xxxxxxxx. Read yours in the dashboard."
  done
  [[ ! -s "${CURL_LOG}" ]]
}

@test "input: an empty --name is given, and the name check refuses it" {
  _stub_all
  run node::drain --name '' --timeout 600
  assert_failure
  assert_output --partial "kubehz: '' is not a node name the platform accepts."
  [[ ! -s "${CURL_LOG}" ]]
}

@test "input: a typed backslash in the name or the space id prints as typed" {
  _stub_all
  run node::cordon --name '\e[2J'
  assert_failure
  [[ "${output}" != *$'\e'* ]]
  assert_output --partial "kubehz: '\e[2J' is not a node name the platform accepts."

  local id='a\\b\e[31m'
  run node::cordon --name worker-1 --space-id "${id}"
  assert_failure
  [[ "${output}" != *$'\e'* ]]
  assert_output --partial "kubehz: '${id}' is not a space id."
}

@test "input: drain --timeout outside 60..3600, or not a whole number, is refused before any request" {
  _stub_all
  for v in 59 3601 0 00000 abc -60 60s " 600" 6e2 99999; do
    run node::drain --name worker-1 --timeout "${v}"
    assert_failure
    assert_output --partial "kubehz: --timeout takes a whole number of seconds from 60 to 3600."
  done
  [[ ! -s "${CURL_LOG}" ]]
}

@test "input: drain_timeout drops leading zeros and never reads octal" {
  run node::drain_timeout 0600
  assert_success
  assert_output "600"
  run node::drain_timeout 0000060
  assert_output "60"
  run node::drain_timeout 3600
  assert_output "3600"
  run node::drain_timeout 0900
  assert_success
  assert_output "900"
}

@test "input: the timeout is checked before the preflight" {
  _stub_all
  run node::drain --name worker-1 --timeout 10 --domain other.example.org
  assert_failure
  assert_output --partial "--timeout takes a whole number"
  refute_output --partial "No cluster.lok8s.yaml"
}

# ── request ──────────────────────────────────────────────

@test "request: cordon and uncordon POST the route with no body" {
  _stub_all
  for verb in cordon uncordon; do
    : > "${CURL_LOG}"
    rm -f "${CURL_BODY}"
    run "node::${verb}" --name worker-1
    assert_success
    grep -qxF "POST https://api.example.test/api/spaces/sp-5/nodes/worker-1/${verb}" "${CURL_LOG}"
    [[ ! -e "${CURL_BODY}" ]]
  done
}

@test "request: drain sends only the fields it was given, as JSON" {
  _stub_all
  run node::drain --name worker-1
  assert_success
  grep -qxF "POST https://api.example.test/api/spaces/sp-5/nodes/worker-1/drain Content-Type: application/json" "${CURL_LOG}"
  [[ "$(cat "${CURL_BODY}")" == '{}' ]]

  run node::drain --name worker-1 --timeout 0600
  assert_success
  [[ "$(cat "${CURL_BODY}")" == '{"timeoutSeconds":600}' ]]

  run node::drain --name worker-1 --evict-unmanaged
  assert_success
  [[ "$(cat "${CURL_BODY}")" == '{"force":true}' ]]

  run node::drain --name worker-1 --timeout 600 --evict-unmanaged
  assert_success
  [[ "$(cat "${CURL_BODY}")" == '{"timeoutSeconds":600,"force":true}' ]]
}

@test "request: --space-id skips the space list and names the id" {
  _stub_all
  run node::cordon --name worker-1 --space-id sp-explicit
  assert_success
  assert_output --partial "kubehz: cordon of node 'worker-1' in space sp-explicit requested."
  run grep -c "GET https://api.example.test/api/spaces$" "${CURL_LOG}"
  assert_output "0"
  grep -qxF "POST https://api.example.test/api/spaces/sp-explicit/nodes/worker-1/cordon" "${CURL_LOG}"
}

@test "request: no space for the slug, or a row with no usable id, sends nothing" {
  export STUB_SPACES_BODY='{"ok":true,"data":[{"id":"sp-9","slug":"other"}]}'
  _stub_all
  run node::cordon --name worker-1
  assert_failure
  assert_output --partial "kubehz: no space 'acme' found for acme.example.org."
  assert_output --partial "Create it with 'lo provision', or name the space with:  --space-id sp-xxxxxxxx"

  for row in '{"slug":"acme"}' '{"id":"../x","slug":"acme"}' '{"id":"","slug":"acme"}'; do
    export STUB_SPACES_BODY="{\"ok\":true,\"data\":[${row}]}"
    run node::uncordon --name worker-1
    assert_failure
    assert_output --partial "kubehz: the space row carries no id this CLI can use."
  done
  run grep -c "^POST" "${CURL_LOG}"
  assert_output "0"
}

# ── answer ───────────────────────────────────────────────

@test "answer: cordon prints the request, the note and where to read the phase" {
  _stub_all
  run --separate-stderr node::cordon --name worker-1
  assert_success
  [[ "${output}" == "kubehz: cordon of node 'worker-1' in space acme requested.
  The scheduler puts no new pods on the node. The pods on it keep running.
  The change is asynchronous. Read its phase with:  lo kubehz status" ]]
  [[ -z "${stderr}" ]]
}

@test "answer: uncordon names the cordon that the platform holds" {
  _stub_all
  run --separate-stderr node::uncordon --name worker-1
  assert_success
  [[ "${output}" == "kubehz: uncordon of node 'worker-1' in space acme requested.
  The scheduler can put new pods on the node again. Evicted pods do not move back.
  A cordon that the platform holds (a migration, a node retirement) stays.
  The change is asynchronous. Read its phase with:  lo kubehz status" ]]
}

@test "answer: drain names the deadline and the pods with no controller" {
  _stub_all
  run --separate-stderr node::drain --name worker-1
  assert_success
  [[ "${output}" == "kubehz: drain of node 'worker-1' in space acme requested.
  The platform cordons the node and evicts its pods.
  It tries until the deadline the platform sets by default.
  A pod that no controller recreates stays, and the drain waits as Blocked.
  To evict such pods too, add:  --evict-unmanaged
  The change is asynchronous. Read its phase with:  lo kubehz status" ]]

  run --separate-stderr node::drain --name worker-1 --timeout 600 --evict-unmanaged
  assert_success
  [[ "${output}" == "kubehz: drain of node 'worker-1' in space acme requested.
  The platform cordons the node and evicts its pods.
  It tries for at most 600 seconds.
  It also evicts the pods that no controller recreates. Nothing recreates them.
  The change is asynchronous. Read its phase with:  lo kubehz status" ]]
}

@test "answer: a replaced request gets its own line, before the last line" {
  _stub_all
  local last="  The change is asynchronous. Read its phase with:  lo kubehz status"

  local keeps="  It replaces a pending drain of this node. The first deadline stays if the machine is the same."

  export STUB_ACT_BODY='{"ok":true,"data":{"status":"requested","replaced":"drain"}}'
  run node::drain --name worker-1
  assert_success
  [[ "${output}" == *$'\n'"  To evict such pods too, add:  --evict-unmanaged"$'\n'"${keeps}"$'\n'"${last}" ]]
  # A drain over a drain keeps the first timeout too: a new --timeout has no
  # effect, and the output says so.
  run node::drain --name worker-1 --timeout 0600
  assert_success
  [[ "${output}" == *$'\n'"${keeps}"$'\n'"  For the same machine, the first timeout stays too, so --timeout 600 has no effect."$'\n'"${last}" ]]
  run node::cordon --name worker-1
  [[ "${output}" == *$'\n'"  It cancels a pending drain. Pods that the drain did not evict stay on the node."$'\n'"${last}" ]]
  run node::uncordon --name worker-1
  [[ "${output}" == *$'\n'"  It cancels a pending drain. Pods that the drain did not evict stay on the node."$'\n'"${last}" ]]

  export STUB_ACT_BODY='{"ok":true,"data":{"status":"requested","replaced":"cordon"}}'
  run node::uncordon --name worker-1
  [[ "${output}" == *$'\n'"  It replaces a pending cordon of this node."$'\n'"${last}" ]]
  # A drain --timeout over a cordon starts its own deadline: no timeout line.
  run node::drain --name worker-1 --timeout 600
  [[ "${output}" == *$'\n'"  It tries for at most 600 seconds."$'\n'* ]]
  [[ "${output}" == *$'\n'"  It replaces a pending cordon of this node."$'\n'"${last}" ]]

  # A bare (unenveloped) body reads the same.
  export STUB_ACT_BODY='{"status":"requested","replaced":"uncordon"}'
  run node::drain --name worker-1
  [[ "${output}" == *$'\n'"  It replaces a pending uncordon of this node."$'\n'"${last}" ]]
}

@test "answer: nothing replaced, or an unknown value, prints no replaced line" {
  _stub_all
  for body in '{"ok":true,"data":{"status":"requested","replaced":null}}' '{"ok":true,"data":{"status":"requested","replaced":"reboot"}}' '{"ok":true,"data":{"status":"requested"}}'; do
    export STUB_ACT_BODY="${body}"
    run node::cordon --name worker-1
    assert_success
    assert_output --partial "kubehz: cordon of node 'worker-1' in space acme requested."
    refute_output --partial "replaces"
    refute_output --partial "cancels"
  done
}

# ── errors ───────────────────────────────────────────────

@test "errors: a 404 names the node and the space" {
  _stub_all
  run node::drain --name worker-9
  assert_failure
  assert_output --partial "kubehz: the platform knows no node 'worker-9' in space acme."
  assert_output --partial "Check the node name and the space with:  lo kubehz status"
  refute_output --partial "requested."
}

@test "errors: each refusal code of the routes is an instruction on stderr" {
  _stub_all
  _check() {
    local verb="$1" code="$2" body="$3"; shift 3
    export STUB_ACT_CODE="${code}" STUB_ACT_BODY="${body}"
    run --separate-stderr "node::${verb}" --name worker-1
    assert_failure
    [[ -z "${output}" ]] || { echo "stdout: ${output}"; return 1; }
    local w
    for w in "$@"; do
      [[ "${stderr}" == *"${w}"* ]] || { echo "stderr lacks: ${w}"; echo "${stderr}"; return 1; }
    done
    [[ "${stderr}" != *"requested."* ]]
  }
  _check cordon 409 "$(_refusal NODE_NOT_JOINED)" \
    "kubehz: node 'worker-1' has not joined the space yet." \
    "  Nothing changed. Wait until 'lo kubehz status' shows the node Ready, then try again."
  _check drain 409 "$(_refusal NODE_MAINTENANCE_CONFLICT)" \
    "kubehz: a maintenance request of another space holds the request name of node 'worker-1'." \
    "  Nothing changed. Contact kubehz support."
  _check uncordon 503 "$(_refusal NODE_MAINTENANCE_UNAVAILABLE)" \
    "kubehz: the uncordon request did not reach the control plane." \
    "  Nothing changed. Try again shortly."
  _check cordon 503 "$(_refusal NODE_MAINTENANCE_AUDIT_UNAVAILABLE)" \
    "kubehz: the platform cannot record the cordon request, so it did not send it." \
    "  Nothing changed. Try again shortly."
  _check drain 503 "$(_refusal NODE_MAINTENANCE_OUTCOME_UNKNOWN)" \
    "kubehz: the result of the drain request is unknown. Possibly, the request got to the control plane." \
    "  Read the phase of node 'worker-1' with 'lo kubehz status' before you try again." \
    "  A drain that you send again can start a new deadline."
  _check cordon 403 "$(_refusal TOKEN_SCOPE_MISSING)" \
    "kubehz: KUBEHZ_TOKEN does not carry the clusters:write scope."
  _check drain 403 "$(_refusal STEP_UP_REQUIRED)" \
    "kubehz: the platform asks for a fresh, two-factor sign-in."
  _check drain 403 "$(_refusal STEP_UP_MFA_REQUIRED)" \
    "kubehz: the platform asks for a fresh, two-factor sign-in."
  # Any other code prints the api's words with the context.
  _check uncordon 409 "$(_refusal LEASE_ENDED)" \
    "Failed to uncordon node 'worker-1' (HTTP 409): The lease of this space ended" \
    "  After the end, the api refuses every change except the delete."
}

# The three lines of an open result, on stderr, with the cause in brackets.
_unknown_lines() {
  printf '[error] kubehz: the result of the %s request is unknown (%s). Possibly, the request got to the control plane.\n' "$1" "$2"
  printf "  Read the phase of node 'worker-1' with 'lo kubehz status' before you try again.\n"
  printf "  A drain that you send again can start a new deadline."
}

@test "errors: a gateway answer or a 2xx that is not the route's answer is an open result" {
  _stub_all
  _check_unknown() {
    local verb="$1" code="$2" body="$3" cause="$4"
    export STUB_ACT_CODE="${code}" STUB_ACT_BODY="${body}"
    run --separate-stderr "node::${verb}" --name worker-1
    assert_failure
    [[ -z "${output}" ]] || { echo "stdout: ${output}"; return 1; }
    local want; want=$(_unknown_lines "${verb}" "${cause}")
    [[ "${stderr}" == "${want}" ]] || { echo "stderr: ${stderr}"; echo "want:   ${want}"; return 1; }
  }
  _check_unknown cordon 502 '' "HTTP 502"
  _check_unknown drain 504 'upstream request timeout' "HTTP 504"
  _check_unknown uncordon 503 'no healthy upstream' "HTTP 503"
  _check_unknown drain 503 '{"statusCode":503,"data":{"message":"busy"}}' "HTTP 503"
  _check_unknown cordon 202 '<html>accepted</html>' "HTTP 202 with an answer this CLI cannot read"
  _check_unknown cordon 202 '' "HTTP 202 with an answer this CLI cannot read"
  _check_unknown drain 202 '{"ok":true,"data":{"name":"worker-1","status":"queued","replaced":null}}' "HTTP 202 with an answer this CLI cannot read"
  _check_unknown uncordon 200 '{"ok":true,"data":{"replaced":null}}' "HTTP 200 with an answer this CLI cannot read"

  # A 503 with a code of the route, and a 500, keep their own lines: the
  # api answered them itself.
  export STUB_ACT_CODE=503 STUB_ACT_BODY='{"ok":false,"data":{"code":"NODE_MAINTENANCE_UNAVAILABLE"}}'
  run node::cordon --name worker-1
  assert_failure
  assert_output --partial "kubehz: the cordon request did not reach the control plane."
  refute_output --partial "unknown"
  export STUB_ACT_CODE=500 STUB_ACT_BODY='{"ok":false,"data":{"message":"Internal error"}}'
  run node::cordon --name worker-1
  assert_failure
  assert_output --partial "Failed to cordon node 'worker-1' (HTTP 500): Internal error"
  refute_output --partial "unknown"
}

@test "errors: a transport failure after the send is an open result, one before it is unreachable" {
  _stub_all
  local rc
  # curl -sS prints its own error line first (the stub: "curl: (<rc>) stub
  # failure"); the Go twin has no such line.
  # After the send: no reply, a receive failure, a reset of an HTTP/2
  # stream, and a timeout (curl cannot tell its phase, so it counts here).
  for rc in 52 56 92 28; do
    STUB_POST_CURL_RC="${rc}" run --separate-stderr node::drain --name worker-1
    assert_failure
    [[ -z "${output}" ]]
    [[ "${stderr}" == "curl: (${rc}) stub failure"$'\n'"$(_unknown_lines drain "no answer")" ]] || { echo "rc ${rc}: ${stderr}"; return 1; }
    [[ "${stderr}" != *"unreachable"* ]]
  done
  # Before the send: the host name, the connect, the TLS handshake, the
  # certificate. Nothing reached the api.
  for rc in 6 7 35 60; do
    STUB_POST_CURL_RC="${rc}" run --separate-stderr node::cordon --name worker-1
    assert_failure
    [[ -z "${output}" ]]
    [[ "${stderr}" == "curl: (${rc}) stub failure"$'\n'"[error] kubehz API unreachable (POST /api/spaces/sp-5/nodes/worker-1/cordon)" ]] || { echo "rc ${rc}: ${stderr}"; return 1; }
  done
  # The token check refuses a token with a control character before curl
  # runs: its own line, and no second one.
  KUBEHZ_TOKEN=$'khz_test_token\r' run --separate-stderr node::cordon --name worker-1 --space-id sp-5
  assert_failure
  [[ "${stderr}" == *"environment variable KUBEHZ_TOKEN must not contain a control character"* ]]
  [[ "${stderr}" != *"unreachable"* && "${stderr}" != *"unknown"* ]]
}

@test "errors: a hostile refusal loses its control characters" {
  export STUB_ACT_CODE="400"
  export STUB_ACT_BODY='{"ok":false,"data":{"code":"BAD_REQUEST","message":"Refused \u001b[2J now","help":"\u001b[HAsk support"}}'
  _stub_all
  run node::cordon --name worker-1
  assert_failure
  [[ "${output}" != *$'\e'* ]]
  assert_output --partial "Failed to cordon node 'worker-1' (HTTP 400): Refused [2J now"
  assert_output --partial "  [HAsk support"
}

# ── lo kubehz status ─────────────────────────────────────

@test "status: a Space node line shows the cordon and the last maintenance request" {
  _stub_all
  local long; long=$(printf 'ü%.0s' {1..300})
  local clipped; clipped=$(printf 'ü%.0s' {1..256})
  export STUB_NODES_BODY='{"ok":true,"data":{"nodes":['\
'{"name":"worker-1","status":"Ready","lane":"metal","unschedulable":true,"maintenance":{"action":"drain","phase":"Blocked","reason":"2 pods have no controller \u001b[31mand no force"}},'\
'{"name":"worker-2","status":"Ready","lane":"hcloud","unschedulable":false,"maintenance":{"action":"uncordon","phase":"Done"}},'\
'{"name":"worker-3","status":"NotReady","lane":"metal","unschedulable":true,"maintenance":null},'\
'{"name":"worker-4","status":"Ready","lane":"metal","unschedulable":"true","maintenance":{"phase":7,"reason":"'"${long}"'"}},'\
'{"name":"worker-5","status":"Ready","lane":"metal","maintenance":{"action":"cordon","phase":"Pending","reason":""}},'\
'{"name":"worker-6","status":"Ready","lane":"metal","maintenance":"drain"},'\
'{"name":"worker-7","status":"Ready","lane":"metal","maintenance":{"action":"drain\tx","phase":"Failed","reason":"no\n  worker-9  Ready  metal\tforged"}}'\
'],"usage":{"nodes":7,"maxNodes":7}}}'
  export LOK8S_KUBEHZ_API_URL="https://api.example.test"
  run kubehz::space_status "acme.example.org" "/dev/null"
  assert_success
  assert_line "  worker-1  Ready  metal  cordoned  drain Blocked: 2 pods have no controller [31mand no force"
  assert_line "  worker-2  Ready  hcloud  uncordon Done"
  assert_line "  worker-3  NotReady  metal  cordoned"
  assert_line "  worker-4  Ready  metal  - -: ${clipped}…"
  assert_line "  worker-5  Ready  metal  cordon Pending"
  assert_line "  worker-6  Ready  metal"
  # A newline or a tab in a server string becomes a space: one node, one
  # line.
  assert_line "  worker-7  Ready  metal  drain x Failed: no   worker-9  Ready  metal forged"
  refute_line --regexp '^  worker-9'
  [[ "${output}" != *$'\t'* ]]
  [[ "${output}" != *$'\e'* ]]
}

# ── the usage tree ───────────────────────────────────────

@test "usage: cordon and uncordon are idempotent, drain is destructive" {
  local src="${_PROJECT_ROOT}/.lok8s/libs/kubehz/node"
  grep -qE "^\s+'cordon@idempotent'\s+'Mark one node of a Space unschedulable \(its pods keep running\)'" "${src}"
  grep -qE "^\s+'uncordon@idempotent'\s+'Make one node of a Space schedulable again'" "${src}"
  grep -qE "^\s+'drain@destructive'\s+'Cordon one node of a Space and evict its pods'" "${src}"
}
