#!/usr/bin/env bats
# kubehz_claim_test.bats — the mode-3 claim nonce CLI (`lo kubehz claim`) and
# the agent-token re-enrollment CLI (`lo kubehz re-enroll`).
#
# claim: validates the dashboard-minted khzn_ nonce SHAPE locally (never
# echoing the value), then places nonce + epoch stamp on the agent's marker
# ConfigMap in ONE annotate call — via the operator's kubeconfig, never the
# agent's RBAC. re-enroll: reads the CURRENT in-cluster agent token, hashes it
# (sha256 lowercase hex), resolves the platform cluster id from the registry
# (client-side domain filter) and POSTs the hash to /clusters/{id}/agent-token
# with the USER's bearer — the §1.7 R2 recovery for a regenerated Secret.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # re-enroll re-asserts HTTPS before any network call.
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  # argsh `:args` builtin — absent when a lib is sourced in bats (house
  # pattern, see kkp_test.bats). Minimal stub: assign each `--name value`
  # pair to the matching variable; the subcommands under test read `nonce`
  # (claim) and fall back to env `domain` (re-enroll), so long-option
  # assignment is all the stub needs.
  :args() {
    shift # description
    while (( $# )); do
      case "$1" in
        --*)
          local _n="${1#--}"
          _n="${_n//-/_}"
          shift
          printf -v "${_n}" '%s' "${1:-}"
          ;;
      esac
      shift || true
    done
  }
  export -f :args

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"
  touch "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"

  export KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  export CURL_BODY_OUT="${BATS_TEST_TMPDIR}/curl-body.json"
  export STUB_AT_BODY='{"rotated":true,"clusterId":"cl-123"}'
  export STUB_AT_CODE="200"
}

teardown() {
  teardown_tmpdir
}

# yq stub for kubehz::read_config: a self-hosted registered cluster with an
# HTTPS api. Individual tests override via STUB_API_URL.
_stub_config() {
  yq() {
    case "$2" in
      '.spec.kubehz.hosting // "self"') echo "self" ;;
      '.spec.kubehz.apiUrl // ""') echo "${STUB_API_URL:-https://api.kubehz.dev}" ;;
      '.spec.kubehz.access') echo "registered" ;;
      '.spec.kubehz.connectHcloudToken // false') echo "false" ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# kubectl stub: logs every call; serves the marker ConfigMap get and the
# kubehz-agent Secret's agent-token (base64, as jsonpath emits it).
_stub_kubectl() {
  kubectl() {
    printf '%s\n' "$*" >> "${KUBECTL_LOG}"
    case "$*" in
      *"get configmap kubehz-agent-config"*)
        [ -z "${STUB_CM_MISSING:-}" ] || return 1
        return 0 ;;
      *"annotate"*) return 0 ;;
      *"get secret kubehz-agent"*)
        [ -z "${STUB_SECRET_MISSING:-}" ] || return 1
        printf 'khz_agt_bats' | base64 | tr -d '\n' ;;
    esac
    return 0
  }
  export -f kubectl
}

# curl stub: the registry list resolves cl-123 for test.kubehz.dev; the
# agent-token POST captures the body and answers STUB_AT_BODY + the trailing
# `\n<code>` line the CLI's `-w '\n%{http_code}'` parsing expects.
_stub_curl() {
  curl() {
    local a=("$@") body="" url=""
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -d) body="$2"; shift ;;
        http*://*) url="$1" ;;
      esac
      shift
    done
    if [[ "${url}" == *"/agent-token" ]]; then
      printf '%s' "${body}" > "${CURL_BODY_OUT}"
      [[ " ${a[*]} " == *"Authorization: Bearer khzt_bats"* ]] \
        || { echo "agent-token POST missing the user bearer: ${a[*]}" >&2; return 1; }
      printf '%s\n%s' "${STUB_AT_BODY}" "${STUB_AT_CODE}"
      return 0
    fi
    # GET /api/clusters (registry list) — two rows so the client-side domain
    # filter is what picks the right one (never .data[0]).
    printf '%s' '{"data":[{"id":"cl-999","domain":"other.kubehz.dev"},{"id":"cl-123","domain":"test.kubehz.dev"}]}'
  }
  export -f curl
}

# ── claim: shape validation (value never echoed) ─────────

@test "claim: rejects a malformed nonce without touching the cluster" {
  _stub_kubectl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::claim --nonce "khzt_wrong_prefix_value_000000"
  assert_failure
  assert_output --partial "invalid claim nonce"
  # The value is a claim ticket — never echoed, not even on rejection.
  refute_output --partial "khzt_wrong_prefix_value_000000"
  # No kubectl call was made.
  [ ! -s "${KUBECTL_LOG}" ]
}

@test "claim: rejects a too-short khzn_ value" {
  _stub_kubectl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::claim --nonce "khzn_short"
  assert_failure
  assert_output --partial "invalid claim nonce"
}

# ── claim: placement (one annotate call, operator kubeconfig) ─

@test "claim: places nonce + epoch stamp in ONE annotate call and never prints the value" {
  _stub_kubectl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  local nonce="khzn_batsPlacedNonce_43charsBase64urlValue000"
  run kubehz::claim --nonce "${nonce}"
  assert_success
  assert_output --partial "claim nonce placed"
  assert_output --partial "15 minutes"
  refute_output --partial "${nonce}"

  # ONE annotate call carries BOTH annotations (the agent clears a stampless
  # nonce as unsourced, so a half-written pair must be impossible).
  run grep -c "annotate" "${KUBECTL_LOG}"
  assert_output "1"
  run grep "annotate" "${KUBECTL_LOG}"
  assert_output --partial "kubehz.cloud/claim-nonce=${nonce}"
  assert_output --partial "kubehz.cloud/claim-nonce-placed="
  assert_output --partial "--overwrite"
}

@test "claim: a missing agent ConfigMap errors with deploy guidance" {
  _stub_kubectl
  export STUB_CM_MISSING=1
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  run kubehz::claim --nonce "khzn_batsPlacedNonce_43charsBase64urlValue000"
  assert_failure
  assert_output --partial "not found"
  assert_output --partial "Deploy the heartbeat agent"
}

# ── re-enroll: happy path ────────────────────────────────

@test "re-enroll: hashes the in-cluster token, resolves the id by domain, reports rotated" {
  _stub_config
  _stub_kubectl
  _stub_curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_bats"

  run kubehz::re-enroll
  assert_success
  assert_output --partial "agent token re-enrolled for test.kubehz.dev (cl-123)"
  assert_output --partial "heartbeats resume"
  # The token plaintext never surfaces.
  refute_output --partial "khz_agt_bats"

  # The POSTed hash is the sha256 (lowercase hex) of the in-cluster token.
  local expect
  expect=$(printf %s "khz_agt_bats" | sha256sum | cut -d' ' -f1)
  run command jq -r '.agentTokenHash' "${CURL_BODY_OUT}"
  assert_output "${expect}"
}

@test "re-enroll: an already-live token reports no rotation (idempotent)" {
  _stub_config
  _stub_kubectl
  _stub_curl
  export STUB_AT_BODY='{"rotated":false,"clusterId":"cl-123"}'
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_bats"

  run kubehz::re-enroll
  assert_success
  assert_output --partial "already the live one"
  refute_output --partial "re-enrolled for"
}

# ── re-enroll: honest failure reporting ──────────────────

@test "re-enroll: a 409 AGENT_TOKEN_CONFLICT surfaces the code and fails" {
  _stub_config
  _stub_kubectl
  _stub_curl
  export STUB_AT_CODE="409"
  export STUB_AT_BODY='{"statusCode":409,"data":{"code":"AGENT_TOKEN_CONFLICT","message":"That agent token is already registered"}}'
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_bats"

  run kubehz::re-enroll
  assert_failure
  assert_output --partial "HTTP 409"
  assert_output --partial "AGENT_TOKEN_CONFLICT"
}

@test "re-enroll: requires KUBEHZ_TOKEN" {
  _stub_config
  _stub_kubectl
  _stub_curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  unset KUBEHZ_TOKEN

  run kubehz::re-enroll
  assert_failure
  assert_output --partial "KUBEHZ_TOKEN is required"
}

@test "re-enroll: a missing in-cluster Secret errors with agent guidance" {
  _stub_config
  _stub_kubectl
  _stub_curl
  export STUB_SECRET_MISSING=1
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_bats"

  run kubehz::re-enroll
  assert_failure
  assert_output --partial "no agent-token"
}

@test "re-enroll: refuses a plain-HTTP apiUrl before any network call" {
  _stub_config
  _stub_kubectl
  export STUB_API_URL="http://api.kubehz.dev"
  curl() { echo "curl should not run over plain HTTP" >&2; return 99; }
  export -f curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_bats"

  run kubehz::re-enroll
  assert_failure
  assert_output --partial "must use HTTPS"
}

@test "re-enroll: an unresolvable cluster id fails with registry guidance" {
  _stub_config
  _stub_kubectl
  curl() { printf '%s' '{"data":[]}'; }
  export -f curl
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export domain="test.kubehz.dev" DOMAIN_NAME="test.kubehz.dev"
  export KUBEHZ_TOKEN="khzt_bats"

  run kubehz::re-enroll
  assert_failure
  assert_output --partial "no cluster found for test.kubehz.dev"
}
