#!/usr/bin/env bats
# kubehz_heartbeat_test.bats — the in-cluster heartbeat agent (CronJob) script.
#
# Guards the POSIX-sh heartbeat embedded in
# .lok8s/libs/kubehz/manifests/agent/cronjob.yaml:
#   - node IDENTITY: names come from a targeted jsonpath, NEVER from a greedy
#     sed that also scrapes .status.runtimeHandlers[].name ("runc") or
#     .status.volumesAttached[].name (CSI handles) on modern k8s;
#   - per-node ROLES: control-plane vs worker, derived from the node-role label
#     KEY presence (an absent label must NOT read as control-plane);
#   - kubernetesVersion: the SERVER version, not the client/kubectl-image tag;
#   - the per-node "instanceType" field (node.kubernetes.io/instance-type, e.g.
#     Hetzner CCM cx32/cpx41/…) and its fail-soft empty-string fallback.
#
# The SHIPPED manifest is the fixture: the script is extracted with yq and run
# against a stubbed kubectl/curl, so a regression in the real heartbeat — a
# broken field, invalid JSON, a `set -e` abort — fails here. The stub is
# deliberately REALISTIC (two nodes, differing roles, runtimeHandlers +
# volumesAttached, distinct client/server versions) so this whole bug class —
# which slipped past a trivial single-node stub while the agent was DOA — is
# caught.

setup() {
  load "../test_helper"
  setup_tmpdir

  AGENT_DIR="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent"
  CRONJOB="${AGENT_DIR}/cronjob.yaml"

  # Extract the embedded heartbeat (containers[0].command[2] — the `-c` script).
  HEARTBEAT="${BATS_TEST_TMPDIR}/heartbeat.sh"
  command yq -r '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' \
    "${CRONJOB}" > "${HEARTBEAT}"

  # Stubs: a realistic TWO-node cluster — a control-plane node "cp-1" and a
  # worker "worker-1". The `get nodes -o json` fixture carries the modern-k8s
  # traps that made the shipped agent DOA: .status.runtimeHandlers[].name
  # ("runc"/"crun", present on k8s ≳1.27) and .status.volumesAttached[].name (a
  # CSI handle). The FIX extracts node names via a targeted jsonpath
  # (`get nodes -o jsonpath=…`); a regression to the greedy `… -o json | sed
  # …"name"…` scrapes those ghosts → `get node runc` NotFounds → set -e aborts
  # (caught by assert_success). STUB_ITYPE sets cp-1's instance-type label
  # (unset → cx32, empty string → unlabeled). curl captures the POSTed heartbeat
  # body (-d) to STUB_PAYLOAD_OUT so the test can assert the wire payload.
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
kubectl() {
  case "$*" in
    # An enrolled agent identity exists: the heartbeat reads its agent-token
    # back and authenticates with it. The empty-token guard skips the POST when
    # no token is readable, so the fixture must supply one for the heartbeat to
    # fire. claim-code is left to the default (empty) → the best-effort
    # self-register stays a no-op here.
    *"get secret kubehz-agent"*"agent-token"*) printf 'khz_agt_test' | base64 | tr -d '\n' ;;

    # kubernetesVersion = the SERVER version. The FIX reads the apiserver
    # /version endpoint (a single "gitVersion" → unambiguous). `version -o json`
    # below is the regression guard: it carries BOTH a client (v1.31.4) and a
    # DIFFERENT server (v1.35.5), so a revert to `version -o json | … | head -1`
    # (which grabs the CLIENT) is caught by the v1.35.5 assertion.
    *"/version"*) printf '{"major":"1","minor":"35","gitVersion":"v1.35.5","platform":"linux/amd64"}\n' ;;
    "version -o json") printf '{\n  "clientVersion": { "gitVersion": "v1.31.4" },\n  "serverVersion": { "gitVersion": "v1.35.5" }\n}\n' ;;

    # Node NAMES (the FIX): a targeted jsonpath over .items[].metadata.name.
    "get nodes -o jsonpath="*) printf 'cp-1\nworker-1\n' ;;
    # Realistic multi-node dump WITH the greedy-sed traps (runtimeHandlers +
    # volumesAttached). Dormant unless the extraction regresses to `-o json`.
    "get nodes -o json") printf '%s\n' '{"items":[{"metadata":{"name":"cp-1"},"status":{"runtimeHandlers":[{"name":"runc"},{"name":"crun"}],"volumesAttached":[{"name":"kubernetes.io/csi/csi.hetzner.cloud^0001"}]}},{"metadata":{"name":"worker-1"},"status":{"runtimeHandlers":[{"name":"runc"}]}}]}' ;;

    # Per-node queries. A name that isn't a real node (what greedy-sed scrapes)
    # NotFounds like real kubectl → the old code's unguarded status=$(…) aborts.
    "get node "*)
      _n=$3
      case "${_n}" in
        cp-1|worker-1) : ;;
        *) echo "Error from server (NotFound): nodes \"${_n}\" not found" >&2; return 1 ;;
      esac
      case "$*" in
        *"jsonpath={.status.conditions"*) printf 'True' ;;
        *"instance-type}"*)
          [ -n "${STUB_ITYPE_FAIL:-}" ] && return 1
          if [ "${_n}" = "cp-1" ]; then printf '%s' "${STUB_ITYPE-cx32}"; else printf 'cpx41'; fi
          return 0 ;;
        # roles source: the labels map, rendered as JSON by the pinned kubectl
        # (1.31). cp-1 carries the control-plane label KEY (empty value);
        # worker-1 does not — so absent MUST read as worker, not control-plane.
        *"jsonpath={.metadata.labels}"*)
          if [ "${_n}" = "cp-1" ]; then
            printf '%s' '{"kubernetes.io/hostname":"cp-1","node-role.kubernetes.io/control-plane":"","node.kubernetes.io/instance-type":"cx32"}'
          else
            printf '%s' '{"kubernetes.io/hostname":"worker-1","node.kubernetes.io/instance-type":"cpx41"}'
          fi ;;
        *) printf '' ;;
      esac ;;

    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;
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
  # …sourced from the GA label key, not the deprecated beta alias (pins the key
  # so a silent swap to beta.kubernetes.io/instance-type fails here).
  assert_output --partial 'node\.kubernetes\.io/instance-type'
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

# ── instance-type kubectl error → "" (fail-soft under set -e) ──

@test "heartbeat: an instance-type kubectl error is fail-soft (instanceType \"\", valid JSON)" {
  # kubectl returns non-zero for the instance-type lookup — the `|| itype=""`
  # guard must absorb it so `set -e` does not abort the whole heartbeat.
  export STUB_ITYPE_FAIL=1
  run bash "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  run command jq -r '.nodes[0].instanceType' "${STUB_PAYLOAD_OUT}"
  assert_output ""
}

# ── Node IDENTITY: names are the metadata names, never the greedy-sed ghosts ─
# P0 regression guard: on modern k8s `get nodes -o json` also carries
# .status.runtimeHandlers[].name ("runc"/"crun") and .status.volumesAttached[]
# .name (CSI handles). A greedy sed scraped those as node names, then
# `kubectl get node runc` NotFounded and `set -e` killed the beat before the
# POST — the agent was DOA on every real cluster.

@test "heartbeat: node names are EXACTLY the two metadata names (never a runtimeHandler/CSI handle)" {
  run bash "${RUNNER}"
  assert_success

  run command jq -e . "${STUB_PAYLOAD_OUT}"
  assert_success

  run command jq -rc '[.nodes[].name]' "${STUB_PAYLOAD_OUT}"
  assert_output '["cp-1","worker-1"]'

  # None of the greedy-sed ghosts may surface as a node (or anywhere else).
  run grep -F '"name":"runc"' "${STUB_PAYLOAD_OUT}"
  assert_failure
  run grep -Fi 'csi' "${STUB_PAYLOAD_OUT}"
  assert_failure
}

# ── Per-node ROLES: control-plane vs worker (not all control-plane) ──
# The role labels carry an EMPTY value, so a per-key jsonpath can't tell
# "absent" from "present-but-empty" — the old `… && echo control-plane` fired
# for EVERY node. The fix tests the label KEY's presence; absent → worker.

@test "heartbeat: roles are per-node — control-plane for cp-1, worker for worker-1" {
  run bash "${RUNNER}"
  assert_success

  run command jq -r '.nodes[] | select(.name == "cp-1") | .roles' "${STUB_PAYLOAD_OUT}"
  assert_output "control-plane"
  run command jq -r '.nodes[] | select(.name == "worker-1") | .roles' "${STUB_PAYLOAD_OUT}"
  assert_output "worker"

  # BOTH roles must be present — proof the worker is not misread as control-plane.
  run command jq -rc '[.nodes[].roles] | unique' "${STUB_PAYLOAD_OUT}"
  assert_output '["control-plane","worker"]'
}

# ── kubernetesVersion is the SERVER version, not the client image tag ──
# `kubectl version -o json` carries BOTH clientVersion (= the alpine/k8s image
# tag) and serverVersion; a first-match grabbed the client (v1.31.4). The fix
# reads the apiserver /version endpoint → the true server version.

@test "heartbeat: kubernetesVersion is the SERVER version (v1.35.5), not the client (v1.31.4)" {
  run bash "${RUNNER}"
  assert_success

  run command jq -r '.kubernetes.version' "${STUB_PAYLOAD_OUT}"
  assert_output "v1.35.5"

  # The client/kubectl-image version must not leak into the payload anywhere.
  run grep -F "v1.31.4" "${STUB_PAYLOAD_OUT}"
  assert_failure
}
