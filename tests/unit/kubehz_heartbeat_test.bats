#!/usr/bin/env bats
# kubehz_heartbeat_test.bats — the in-cluster heartbeat agent (CronJob) script.
#
# Guards the POSIX-sh heartbeat embedded in
# .lok8s/libs/kubehz/manifests/agent/cronjob.yaml, in particular the per-node
# "instanceType" field: the value of the well-known
# node.kubernetes.io/instance-type label (set by the cloud provider, e.g.
# Hetzner CCM: cx32/cpx41/…) that powers the self-hosted price overview, and
# its fail-soft empty-string fallback when the label is absent (bare metal,
# kind, no CCM).
#
# The SHIPPED manifest is the fixture: the script is extracted with yq and run
# against a stubbed kubectl/curl, so a regression in the real heartbeat — a
# broken field, invalid JSON, a `set -e` abort — fails here.

setup() {
  load "../test_helper"
  setup_tmpdir

  AGENT_DIR="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent"
  CRONJOB="${AGENT_DIR}/cronjob.yaml"

  # Extract the embedded heartbeat (containers[0].command[2] — the `-c` script).
  HEARTBEAT="${BATS_TEST_TMPDIR}/heartbeat.sh"
  command yq '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' \
    "${CRONJOB}" > "${HEARTBEAT}"

  # Stubs: a single node "cp-1". STUB_ITYPE is the instance-type label it
  # reports (empty = unlabeled). curl captures the POSTed heartbeat body (-d)
  # to STUB_PAYLOAD_OUT so the test can assert the wire payload.
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
kubectl() {
  case "$*" in
    "version -o json") printf '{"serverVersion":{"gitVersion":"v1.31.4"}}\n' ;;
    "get nodes -o json") printf '{"items":[{"metadata":{"name":"cp-1"}}]}\n' ;;
    *"jsonpath={.status.conditions"*) printf 'True' ;;
    *"control-plane}"*) printf ''; return 0 ;;
    *"instance-type}"*) printf '%s' "${STUB_ITYPE}"; return 0 ;;
    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;
    *"/version"*) printf 'ok\n'; return 0 ;;
    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  while [ "$#" -gt 0 ]; do
    [ "$1" = "-d" ] && printf '%s' "$2" > "${STUB_PAYLOAD_OUT}"
    shift
  done
  return 0
}
STUBS_EOF

  # Prepend the stubs to the real script → a self-contained, runnable heartbeat.
  RUNNER="${BATS_TEST_TMPDIR}/run.sh"
  cat "${stubs}" "${HEARTBEAT}" > "${RUNNER}"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
  export STUB_PAYLOAD_OUT="${BATS_TEST_TMPDIR}/payload.json"
}

teardown() {
  teardown_tmpdir
}

# ── The shipped script is valid POSIX sh ─────────────────

@test "heartbeat: the embedded script passes sh -n" {
  run sh -n "${HEARTBEAT}"
  assert_success
}

# ── The agent kustomization renders with the instanceType field ─

@test "heartbeat: agent kustomization renders and carries the instanceType field" {
  run command kustomize build "${AGENT_DIR}"
  assert_success
  assert_output --partial "kind: CronJob"
  assert_output --partial "name: kubehz-heartbeat"
  # The per-node field is present in the rendered heartbeat script.
  assert_output --partial '\"instanceType\":\"'
}

# ── Labeled node → instanceType is the label value ───────

@test "heartbeat: a labeled node reports its instanceType and emits valid JSON" {
  export STUB_ITYPE="cx32"
  run bash "${RUNNER}"
  assert_success

  # The captured payload is valid JSON …
  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  # … and the node carries instanceType: cx32 alongside the existing fields.
  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output "cx32"
  run command jq -r '.nodes[0].name' "${STUB_PAYLOAD_OUT}"
  assert_output "cp-1"
}

# ── Unlabeled node → instanceType is "" (fail-soft) ──────

@test "heartbeat: an unlabeled node reports instanceType \"\" and still emits valid JSON" {
  export STUB_ITYPE=""
  run bash "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  # The field is present but empty — never omitted, never a broken heartbeat.
  run command jq -r '.nodes[0] | has("instanceType")' "${STUB_PAYLOAD_OUT}"
  assert_output "true"
  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output ""
}
