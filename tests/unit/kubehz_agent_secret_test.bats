#!/usr/bin/env bats
# kubehz_agent_secret_test.bats — the P0 self-hosted agent-secret claim.
#
# Two surfaces are guarded here:
#
#   1. The in-cluster heartbeat agent (the CronJob script embedded in
#      .lok8s/libs/kubehz/manifests/agent/cronjob.yaml): it self-generates the
#      agent-token A (khz_agt_…) + claim-code C (khzc_…) into Secret
#      kubehz-agent (create-if-absent, NEVER rotate), self-registers by POSTing
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

  # Fake in-cluster Secret store + curl capture sinks.
  export STORE_A="${BATS_TEST_TMPDIR}/secret.agent-token"
  export STORE_C="${BATS_TEST_TMPDIR}/secret.claim-code"
  export REGISTER_OUT="${BATS_TEST_TMPDIR}/register.json"
  export REGISTER_HDRS="${BATS_TEST_TMPDIR}/register.hdrs"
  export HEARTBEAT_OUT="${BATS_TEST_TMPDIR}/heartbeat.json"
  export HEARTBEAT_HDRS="${BATS_TEST_TMPDIR}/heartbeat.hdrs"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
}

teardown() {
  teardown_tmpdir
}

# Build a self-contained runnable heartbeat: a file-backed kubectl (fake Secret
# store) + a URL-routing curl capture, prepended to the shipped script.
_build_runner() {
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
kubectl() {
  case "$*" in
    "get secret kubehz-agent -n kubehz-system")
      # Existence probe (no -o): present iff the store file exists.
      [ -f "${STORE_A}" ] && return 0 || return 1
      ;;
    *"get secret kubehz-agent"*"agent-token"*)
      [ -f "${STORE_A}" ] || return 0
      base64 < "${STORE_A}" | tr -d '\n'
      ;;
    *"get secret kubehz-agent"*"claim-code"*)
      [ -f "${STORE_C}" ] || return 0
      base64 < "${STORE_C}" | tr -d '\n'
      ;;
    "create secret generic kubehz-agent"*)
      # create-if-absent: mimic kubectl (fails if the Secret already exists),
      # which is what proves the guard never rotates.
      [ -f "${STORE_A}" ] && return 1
      _a=""; _c=""
      for _arg in "$@"; do
        case "${_arg}" in
          --from-literal=agent-token=*) _a="${_arg#--from-literal=agent-token=}" ;;
          --from-literal=claim-code=*)  _c="${_arg#--from-literal=claim-code=}" ;;
        esac
      done
      printf '%s' "${_a}" > "${STORE_A}"
      printf '%s' "${_c}" > "${STORE_C}"
      return 0
      ;;
    "version -o json") printf '{"serverVersion":{"gitVersion":"v1.31.4"}}\n' ;;
    "get nodes -o json") printf '{"items":[{"metadata":{"name":"cp-1"}}]}\n' ;;
    *"jsonpath={.status.conditions"*) printf 'True' ;;
    *"control-plane}"*) printf ''; return 0 ;;
    *"instance-type}"*) printf 'cx32'; return 0 ;;
    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;
    *"/version"*) printf 'ok\n'; return 0 ;;
    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  _url=""; _body=""; _hdrs=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -d) _body="$2"; shift ;;
      -H) _hdrs="${_hdrs} ${2} ;"; shift ;;
      http://*|https://*) _url="$1" ;;
    esac
    shift
  done
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
  cat "${stubs}" "${HEARTBEAT}" > "${RUNNER}"
}

# ── The shipped script stays valid POSIX sh ──────────────

@test "agent: the embedded heartbeat script passes sh -n" {
  run sh -n "${HEARTBEAT}"
  assert_success
}

# ── L1: create-if-absent — first run mints A and C ───────

@test "agent: first run creates the kubehz-agent Secret with khz_agt_ / khzc_ values" {
  _build_runner
  # No store yet → absent.
  [ ! -f "${STORE_A}" ]

  run bash "${RUNNER}"
  assert_success

  # A = khz_agt_<64 hex>, C = khzc_<64 hex> — 256-bit, distinct prefixes.
  run cat "${STORE_A}"
  assert_output --regexp '^khz_agt_[0-9a-f]{64}$'
  run cat "${STORE_C}"
  assert_output --regexp '^khzc_[0-9a-f]{64}$'
}

# ── L1: never rotate — a second run reuses the same A and C ─

@test "agent: a second run NEVER rotates A or C (idempotent secret)" {
  _build_runner

  run bash "${RUNNER}"
  assert_success
  local a1 c1
  a1="$(cat "${STORE_A}")"
  c1="$(cat "${STORE_C}")"

  # Re-run against the SAME store (mirrors a re-applied / re-fired agent).
  run bash "${RUNNER}"
  assert_success

  assert_equal "$(cat "${STORE_A}")" "${a1}"
  assert_equal "$(cat "${STORE_C}")" "${c1}"

  # And the beat still authenticates with that same, unrotated A.
  run cat "${HEARTBEAT_HDRS}"
  assert_output --partial "Authorization: Bearer ${a1}"
}

# ── L2: agent-register carries the two HASHES, never plaintext ─

@test "agent: agent-register posts sha256(A)/sha256(C) — never the plaintext secrets" {
  _build_runner
  run bash "${RUNNER}"
  assert_success

  # Valid JSON with the domain and both hashes.
  run command jq -e . "${REGISTER_OUT}"
  assert_success
  run command jq -r '.domain' "${REGISTER_OUT}"
  assert_output "test.example.com"

  local a c expect_ah expect_ch
  a="$(cat "${STORE_A}")"
  c="$(cat "${STORE_C}")"
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
  run bash "${RUNNER}"
  assert_success

  local a
  a="$(cat "${STORE_A}")"

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
  # Namespaced Secret RBAC (get+create only — never rotate).
  assert_output --partial "name: kubehz-agent-secret"
  assert_output --partial "secrets"
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
