#!/usr/bin/env bats
# kubehz_claim_test.bats — the mode-3 claim nonce CLI (`lo kubehz claim`).
#
# claim: validates the dashboard-minted khzn_ nonce SHAPE locally (never
# echoing the value), then places nonce + epoch stamp on the agent's marker
# ConfigMap in ONE annotate call — via the operator's kubeconfig, never the
# agent's RBAC.

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
}

teardown() {
  teardown_tmpdir
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

