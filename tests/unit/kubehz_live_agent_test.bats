#!/usr/bin/env bats
# kubehz_live_agent_test.bats — the Go live agent: the spec knob, the render,
# the apply order, the vendored RBAC, and the interlock that keeps exactly ONE
# heartbeat producer alive.
#
# Why the interlock matters, and what breaks without it: kubehz-api stores
# `lastHeartbeatSnapshot` LATEST-WINS. The CronJob speaks schema 1 (nodes,
# components, certificates); the Go agent speaks schema 2 (nodes with capacity,
# workloads, events, actions, machineIssues, inventory, agent identity). If both
# beat, every CronJob tick lands on top of the live view and blanks each field it
# does not send, and `expectedIntervalSeconds` flips 60 ↔ 300 so `connected`
# flaps. The dashboard shows a live view that empties itself every five minutes.
#
# Three interlocks, tested here:
#   1. the spec: ONE enum (spec.kubehz.agent), so a spec cannot ask for two;
#   2. the cluster: KUBEHZ_HEARTBEAT_OWNER on kubehz-agent-config — the CronJob
#      reads it and refuses to beat when the live agent owns the heartbeat.
#      ONE-DIRECTIONAL: it can silence the CronJob (even a hand-applied one),
#      but the Go agent has no equivalent switch and beats whenever it runs, so
#      what silences the live agent is deleting its Deployment;
#   3. the deploy: the apply ORDER, which differs per direction so that no
#      instant has two live producers — and which FAILS rather than continues
#      at every step, because continuing past a failed live-agent delete is how
#      you get the two producers back.

setup() {
  load "../test_helper"
  setup_tmpdir

  AGENT_DIR="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/agent"
  LIVE_DIR="${_PROJECT_ROOT}/.lok8s/libs/kubehz/manifests/live-agent"
  CRONJOB="${AGENT_DIR}/cronjob.yaml"
}

teardown() {
  teardown_tmpdir
}

# Build a runnable copy of the SHIPPED heartbeat script with stubs prepended.
# Deliberately minimal next to kubehz_heartbeat_test.bats's fixture: these tests
# only care about which network calls happen, so every kubectl read answers
# something harmless and curl just logs its URL.
_make_runner() {
  local stubs="${BATS_TEST_TMPDIR}/stubs.sh"
  cat > "${stubs}" <<'STUBS_EOF'
kubectl() {
  case "$*" in
    *"get secret kubehz-agent"*"agent-token"*) printf 'khz_agt_test' | base64 | tr -d '\n' ;;
    *"get secret kubehz-agent"*"claim-code"*)  printf 'khzc_test'   | base64 | tr -d '\n' ;;
    *"get secret kubehz-agent"*) return 0 ;;
    *"/version"*) printf '{"gitVersion":"v1.35.5"}\n' ;;
    "get nodes -o jsonpath="*) printf 'cp-1\n' ;;
    "get node "*) printf '' ;;
    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;
    "get configmap kubehz-agent-config"*) printf '%s' "${STUB_MARKER:-{\}}" ;;
    "annotate "*) return 0 ;;
    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  _url=""
  while [ "$#" -gt 0 ]; do
    case "$1" in http*://*) _url="$1" ;; esac
    shift
  done
  echo "${_url}" >> "${STUB_CURL_LOG}"
  printf ''
  return 0
}
STUBS_EOF

  local heartbeat="${BATS_TEST_TMPDIR}/heartbeat.sh"
  command yq -r '.spec.jobTemplate.spec.template.spec.containers[0].command[2]' \
    "${CRONJOB}" > "${heartbeat}"

  RUNNER="${BATS_TEST_TMPDIR}/run.sh"
  cat "${stubs}" "${heartbeat}" > "${RUNNER}"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
  export STUB_CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  : > "${STUB_CURL_LOG}"
}

# Source the deploy lib against the REAL manifests: PATH_LOK8S must point at the
# project tree before the lib is sourced, because the manifest paths are
# top-level constants (the libs/crds idiom).
_source_deploy() {
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/deploy"
}

# ══ 1. The CronJob interlock (the in-cluster guarantee) ════════════════════

@test "interlock: KUBEHZ_HEARTBEAT_OWNER=operator stops the CronJob beating" {
  _make_runner
  export KUBEHZ_HEARTBEAT_OWNER="operator"

  run bash "${RUNNER}"
  assert_success

  # The identity work still ran — the Go agent mints nothing and only ever
  # READS Secret kubehz-agent, so if the CronJob stopped enrolling, a cluster
  # that lost its kubehz-system namespace could never re-enroll.
  run grep -c '/api/clusters/agent-register' "${STUB_CURL_LOG}"
  assert_output "1"

  # …and the beat did not. This is the whole point: no second producer.
  refute [ "$(grep -c '/heartbeat' "${STUB_CURL_LOG}")" != "0" ]
  # The desired-state poll is the live agent's job too — the CronJob's
  # report-only poll would otherwise fight it for the ETag marker.
  refute [ "$(grep -c '/desired' "${STUB_CURL_LOG}")" != "0" ]
}

@test "interlock: KUBEHZ_HEARTBEAT_OWNER=cronjob keeps the CronJob beating" {
  _make_runner
  export KUBEHZ_HEARTBEAT_OWNER="cronjob"

  run bash "${RUNNER}"
  assert_success

  run grep -c '/heartbeat' "${STUB_CURL_LOG}"
  assert_output "1"
}

@test "interlock: an UNSET owner reads as cronjob (installs predating the key)" {
  _make_runner
  unset KUBEHZ_HEARTBEAT_OWNER

  run bash "${RUNNER}"
  assert_success

  # Fail-safe direction: an old ConfigMap without the key must keep beating,
  # never fall silent.
  run grep -c '/heartbeat' "${STUB_CURL_LOG}"
  assert_output "1"
}

@test "interlock: an UNKNOWN owner value reads as cronjob, never as silence" {
  _make_runner
  export KUBEHZ_HEARTBEAT_OWNER="something-else"

  run bash "${RUNNER}"
  assert_success
  run grep -c '/heartbeat' "${STUB_CURL_LOG}"
  assert_output "1"
}

@test "interlock: the ConfigMap ships the owner key as a substitutable placeholder" {
  run command kustomize build "${AGENT_DIR}"
  assert_success
  assert_output --partial "KUBEHZ_HEARTBEAT_OWNER"
  assert_output --partial "HEARTBEAT_OWNER_PLACEHOLDER"
}

# ══ 2. The spec knob ══════════════════════════════════════════════════════

_stub_yq() {
  local hosting="${1}" access="${2}" agent="${3}"
  eval "yq() {
    case \"\$2\" in
      '.spec.kubehz.hosting // \"self\"') echo '${hosting}' ;;
      '.spec.kubehz.apiUrl // \"\"') echo 'https://api.kubehz.dev' ;;
      '.spec.kubehz.access') echo '${access}' ;;
      '.spec.kubehz.connectHcloudToken // false') echo 'false' ;;
      '.spec.kubehz.agent // \"cronjob\"') echo '${agent}' ;;
      '.kind') echo 'KubeOne' ;;
      *) echo '' ;;
    esac
  }"
  export -f yq
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"
  SPEC="${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev/cluster.lok8s.yaml"
  : > "${SPEC}"
  export LOK8S_SPEC_FILE="${SPEC}"
}

@test "spec: spec.kubehz.agent defaults to cronjob" {
  _stub_yq self registered cronjob
  import() { :; }; export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${SPEC}"
  [ "${LOK8S_KUBEHZ_AGENT}" = "cronjob" ]
}

@test "spec: spec.kubehz.agent: operator is read and accepted" {
  _stub_yq self managed operator
  import() { :; }; export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${SPEC}"
  [ "${LOK8S_KUBEHZ_AGENT}" = "operator" ]
  run kubehz::validate_config
  assert_success
}

@test "spec: an unknown spec.kubehz.agent is refused by name" {
  _stub_yq self registered sidecar
  import() { :; }; export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${SPEC}"
  run kubehz::validate_config
  assert_failure
  assert_output --partial "invalid spec.kubehz.agent: sidecar"
}

@test "spec: agent: operator with access: none is refused" {
  _stub_yq self none operator
  import() { :; }; export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${SPEC}"
  run kubehz::validate_config
  assert_failure
  assert_output --partial "access: registered or managed"
}

@test "spec: agent: operator with hosting: shared is refused (a Space has no agent)" {
  _stub_yq shared registered operator
  import() { :; }; export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  kubehz::read_config "${SPEC}"
  run kubehz::validate_config
  assert_failure
  assert_output --partial "not valid with hosting: shared"
}

# ══ 3. Render ═════════════════════════════════════════════════════════════

@test "render: substitutes every placeholder in BOTH agent trees" {
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/render"
  mkdir -p "${work}"

  run kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  assert_success

  # No placeholder may survive: an unsubstituted owner reads as cronjob and
  # would silently arm a SECOND producer next to the live agent.
  run grep -rl 'PLACEHOLDER' "${work}"
  assert_failure

  run grep -h 'CLUSTER_ID:' "${work}/agent/configmap.yaml" "${work}/live-agent/base/configmap.yaml"
  assert_output --partial "acme.example.com"
  run grep -h 'KUBEHZ_API_URL:' "${work}/agent/configmap.yaml" "${work}/live-agent/base/configmap.yaml"
  assert_output --partial "https://api.kubehz.cloud"
  run grep 'KUBEHZ_HEARTBEAT_OWNER:' "${work}/agent/configmap.yaml"
  assert_output --partial "operator"
}

@test "render: access decides the overlay — registered gets base, managed gets managed" {
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r1"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator registered
  [ "${KUBEHZ_LIVE_AGENT_OVERLAY}" = "${work}/live-agent/base" ]

  local work2="${BATS_TEST_TMPDIR}/r2"
  mkdir -p "${work2}"
  kubehz::render_agent "${work2}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  [ "${KUBEHZ_LIVE_AGENT_OVERLAY}" = "${work2}/live-agent/managed" ]
}

@test "render: a surviving placeholder refuses the whole render" {
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r3"
  mkdir -p "${work}"

  # Substitute nothing by making sed a no-op — the same observable state as a
  # renamed placeholder token in a manifest.
  sed() { return 0; }
  export -f sed

  run kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  assert_failure
  assert_output --partial "still carry a placeholder"
}

@test "render: both rendered trees are valid kustomizations" {
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r4"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run command kustomize build "${work}/agent"
  assert_success
  assert_output --partial "kind: CronJob"

  run command kustomize build "${work}/live-agent/managed"
  assert_success
  assert_output --partial "name: kubehz-live-agent"
  # The live agent must NOT own the shared Namespace: `kubectl delete -k` is how
  # a switch back to cronjob mode removes it, and deleting kubehz-system would
  # take the never-rotated identity Secret with it.
  refute_output --partial "kind: Namespace"
}

# ══ 4. Apply order (no instant with two producers) ═════════════════════════

_stub_kubectl_log() {
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  kubectl() {
    case "$*" in
      # The drain probe: report an idle cluster so the order test is not a
      # timing test.
      *"get pods"*) printf ''; return 0 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl
}

@test "apply order (to operator): the owner marker lands BEFORE the live agent starts" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" operator managed
  assert_success

  # Line 1 applies the CronJob tree (which carries KUBEHZ_HEARTBEAT_OWNER=
  # operator, stopping its beat); line 2 starts the live agent. Reversed, the
  # live agent would beat while the CronJob still did.
  run head -1 "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/agent"
  run sed -n '2p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/live-agent/managed"
  # Line 3 waits for the Deployment to be Ready. Accepted is not running, and
  # between the marker and Ready NOTHING owns the beat — so the command may not
  # report a successful handover until this returns.
  run sed -n '3p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "rollout status deployment/kubehz-live-agent"
}

@test "apply order (to operator): a live agent that never becomes Ready FAILS the deploy" {
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      # ImagePullBackOff, as the apiserver reports it: the object was accepted,
      # the pod never came up.
      *"rollout status"*) echo "$*" >> "${STUB_KUBECTL_LOG}"; return 1 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  local work="${BATS_TEST_TMPDIR}/a3"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" operator managed
  assert_failure
  # The message must name the state the operator is now in — nothing beating —
  # not just report a failed command.
  assert_output --partial "never became Ready"
  assert_output --partial "NOTHING owns the heartbeat"
}

@test "apply order (to cronjob): the live agent is removed BEFORE the CronJob beats again" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a2"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered

  run kubehz::deploy_apply "${work}" cronjob registered
  assert_success

  # The delete targets the MANAGED overlay: it is a superset of the base, so
  # one delete removes an install made at either tier.
  run head -1 "${STUB_KUBECTL_LOG}"
  assert_output --partial "delete -k ${work}/live-agent/managed"
  # Then the label sweep, which catches a Deployment an older lok8s installed
  # under a different NAME — the rendered tree above cannot name it.
  run sed -n '2p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "delete deployment -l app.kubernetes.io/part-of=kubehz,app.kubernetes.io/component=live-view"
  # Only once the pods are gone (the stub reports an empty namespace) may the
  # CronJob's marker be rewritten.
  run sed -n '3p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/agent"

  # And nothing re-applies the live agent in cronjob mode.
  refute [ "$(grep -c "apply -k ${work}/live-agent" "${STUB_KUBECTL_LOG}")" != "0" ]
}

@test "apply order (to cronjob): a FAILED live-agent delete never re-arms the CronJob" {
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      # RBAC denied / apiserver down. --ignore-not-found already covers the
      # legitimate "nothing to delete" case, so this can only be a real failure.
      *"delete -k"*) echo "$*" >> "${STUB_KUBECTL_LOG}"; return 1 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  local work="${BATS_TEST_TMPDIR}/a4"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered

  run kubehz::deploy_apply "${work}" cronjob registered
  assert_failure
  assert_output --partial "could not remove the live agent"

  # THE POINT: the CronJob's beat must not be re-armed beside a live agent that
  # is still running. Swallowing the delete failure and applying anyway is the
  # double-producer state this whole command exists to prevent.
  refute [ "$(grep -c "apply -k ${work}/agent" "${STUB_KUBECTL_LOG}")" != "0" ]
}

@test "apply order (to cronjob): a live-agent pod that will not terminate blocks the re-arm" {
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  # The Deployment deletes fine, but its pod hangs around — kubectl delete
  # returns when the OBJECTS are gone, not when the pods are, and the Go agent
  # beats until its process exits.
  kubectl() {
    case "$*" in
      *"get pods"*) printf 'pod/kubehz-live-agent-abc123\n'; return 0 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl
  sleep() { :; }
  export -f sleep
  KUBEHZ_LIVE_AGENT_DRAIN_SECONDS=10

  local work="${BATS_TEST_TMPDIR}/a5"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered

  run kubehz::deploy_apply "${work}" cronjob registered
  assert_failure
  assert_output --partial "still running"
  refute [ "$(grep -c "apply -k ${work}/agent" "${STUB_KUBECTL_LOG}")" != "0" ]
}

@test "deploy_agent: refuses hosting: shared and access: none" {
  _source_deploy
  export LOK8S_KUBEHZ_HOSTING="shared" LOK8S_KUBEHZ_ACCESS="none"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.cloud" LOK8S_KUBEHZ_AGENT="cronjob"
  run kubehz::deploy_agent "acme.example.com"
  assert_failure
  assert_output --partial "no in-cluster agent to deploy"

  export LOK8S_KUBEHZ_HOSTING="self"
  run kubehz::deploy_agent "acme.example.com"
  assert_failure
  assert_output --partial "access is 'none'"
}

@test "deploy_agent: refuses a plain-HTTP apiUrl before templating a ConfigMap" {
  _source_deploy
  export LOK8S_KUBEHZ_HOSTING="self" LOK8S_KUBEHZ_ACCESS="registered"
  export LOK8S_KUBEHZ_API_URL="http://api.kubehz.cloud" LOK8S_KUBEHZ_AGENT="cronjob"
  run kubehz::deploy_agent "acme.example.com"
  assert_failure
}

# ══ 5. The vendored RBAC (least privilege, verbatim from kubehz-agent) ═════

@test "rbac: the live-agent BASE grants no acting permission" {
  run command kustomize build "${LIVE_DIR}/base"
  assert_success

  # Read-only on the three informer resources + the lok8s ClusterInventory.
  assert_output --partial "clusterinventories"
  # The single base write is the ClusterInventory STATUS mirror — dashboard
  # data on the cluster's own reporting object, not an acting permission.
  assert_output --partial "clusterinventories/status"

  # Nothing that can move a machine, a pod, or a credential.
  refute_output --partial "machinedeployments"
  refute_output --partial "cluster.k8s.io"
  refute_output --partial "kubehz-live-agent-eviction-unwedge"
}

@test "rbac: the base never grants delete, create, update or patch on core resources" {
  run bash -c "command kustomize build '${LIVE_DIR}/base' | command yq -r 'select(.kind==\"ClusterRole\" or .kind==\"Role\") | .rules[] | (.apiGroups|join(\",\")) + \"|\" + (.resources|join(\",\")) + \"|\" + (.verbs|join(\",\"))' | command grep -v '^---$'"
  assert_success
  # The exact rule set, pinned. A new line here is a permission a customer did
  # not agree to, so this test is meant to fail on any widening.
  assert_line "|nodes,pods,events|get,list,watch"
  assert_line "lok8s.dev|clusterinventories|get,list,watch"
  assert_line "lok8s.dev|clusterinventories/status|patch"
  assert_line "|secrets|get"
  # Exactly four rules — a fifth line is a permission a customer did not agree to.
  [ "${#lines[@]}" -eq 4 ]
}

@test "rbac: the machines DELETE verb exists ONLY in the managed overlay" {
  run bash -c "command kustomize build '${LIVE_DIR}/managed' | command yq -r 'select(.kind==\"ClusterRole\" or .kind==\"Role\") | .rules[] | (.apiGroups|join(\",\")) + \"|\" + (.resources|join(\",\")) + \"|\" + (.verbs|join(\",\"))' | command grep -v '^---$'"
  assert_success
  # Self-healing deletes a Machine and lets the MachineSet rebuild it — the
  # sharpest permission the agent holds, and the reason the overlay is opt-in.
  assert_line "cluster.k8s.io|machines|get,list,watch,delete"
  # Scaling + worker upgrades: patch, never update/create/delete.
  assert_line "cluster.k8s.io|machinedeployments|get,list,watch,patch"
  # The eviction unwedge: pods DELETE only, no other verb, no other resource.
  assert_line "|pods|delete"
  # Base rules ride along unchanged; the overlay adds exactly three.
  [ "${#lines[@]}" -eq 7 ]
}

@test "rbac: the Secret read is scoped by name to the agent's own identity Secret" {
  run bash -c "command kustomize build '${LIVE_DIR}/base' | command yq -r 'select(.kind==\"Role\" and .metadata.name==\"kubehz-live-agent-secret\") | .rules[0].resourceNames|join(\",\")'"
  assert_success
  assert_output "kubehz-agent"
}

# Vendored VERBATIM on purpose: the permissions a customer grants must be the
# ones the agent's own public, auditable repo documents — with nothing added on
# the way through lok8s.
#
# That claim used to be checked ONLY by diffing against a sibling kubehz-agent
# checkout, which CI does not have — so the check skipped everywhere it
# mattered and the claim was never evaluated. The guard is now the committed
# digest pin, which needs no checkout and therefore cannot go quiet.
@test "rbac: the vendored copies match the committed upstream digest pin" {
  run bash -c "cd '${LIVE_DIR}' && command sha256sum --check --strict --quiet UPSTREAM.sha256"
  assert_success
}

@test "rbac: the digest pin covers BOTH vendored files and nothing else" {
  # A pin that lost a line would still pass the check above while guarding
  # nothing — the same silent-absence failure the pin was written to end.
  run bash -c "command grep -vE '^[[:space:]]*(#|\$)' '${LIVE_DIR}/UPSTREAM.sha256' | command awk '{print \$2}'"
  assert_success
  assert_line "base/rbac.yaml"
  assert_line "managed/rbac-managed.yaml"
  [ "${#lines[@]}" -eq 2 ]
}

@test "rbac: cross-check the pin against a local upstream checkout, when there is one" {
  local upstream="${_PROJECT_ROOT}/../_standalone/kubehz-agent/deploy"
  [ -d "${upstream}" ] || skip "no kubehz-agent checkout beside lok8s — this is a developer convenience; the always-on digest pin above is the guard"

  # Catches a pin regenerated from a locally-edited vendored file: the digests
  # must describe UPSTREAM's bytes, not just our own.
  run diff -u "${upstream}/base/rbac.yaml" "${LIVE_DIR}/base/rbac.yaml"
  assert_success
  run diff -u "${upstream}/managed/rbac-managed.yaml" "${LIVE_DIR}/managed/rbac-managed.yaml"
  assert_success
  run bash -c "cd '${upstream}' && command sha256sum --check --strict --quiet '${LIVE_DIR}/UPSTREAM.sha256'"
  assert_success
}

# ══ 6. The image (digest-pinning discipline) ══════════════════════════════

@test "image: the live agent is pinned to a public GHCR digest, never a tag" {
  run bash -c "command kustomize build '${LIVE_DIR}/base' | command yq -r 'select(.kind==\"Deployment\") | .spec.template.spec.containers[0].image'"
  assert_success
  # Public registry: a customer cluster cannot pull our private Harbor.
  assert_output --partial "ghcr.io/kernpilot/kubehz-agent@sha256:"
  refute_output --partial "docker.kubehz.in.net"
  # A digest is the only ref cosign verification and a rollback can reason about.
  refute_output --partial ":latest"
  refute_output --partial ":main"
}

# ══ 7. The default is robust, not just absent ═════════════════════════════

@test "spec: an empty or null spec.kubehz.agent falls back to cronjob" {
  # yq's `//` covers a MISSING key. A key written with no value (`agent:`)
  # answers "null", and an empty scalar answers "". Both mean "not chosen": a
  # spec whose only sin is a blank line must not be refused.
  local val
  for val in null ""; do
    _stub_yq self registered "${val}"
    import() { :; }; export -f import
    source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
    source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
    source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

    kubehz::read_config "${SPEC}"
    [ "${LOK8S_KUBEHZ_AGENT}" = "cronjob" ]
    run kubehz::validate_config
    assert_success
  done
}

@test "spec: validate_config tolerates an UNSET agent (callers that skip read_config)" {
  import() { :; }; export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"

  export LOK8S_KUBEHZ_HOSTING="self" LOK8S_KUBEHZ_ACCESS="none" LOK8S_KUBEHZ_API_URL=""
  export LOK8S_SPEC_FILE="${BATS_TEST_TMPDIR}/dummy.yaml"
  unset LOK8S_KUBEHZ_AGENT

  # Unset means "not chosen", never "invalid": validate_config is reachable
  # from callers that set the LOK8S_KUBEHZ_* vars themselves.
  run kubehz::validate_config
  assert_success
}
