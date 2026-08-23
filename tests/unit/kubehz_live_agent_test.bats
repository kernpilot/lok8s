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

@test "render: an apiUrl with '&' lands verbatim, not expanded by sed" {
  # `&` in a sed REPLACEMENT means "the whole matched text", and the apiUrl
  # validator admits `&` on purpose — a query string is a legal API URL. So
  # `?a=1&b=2` used to render as `?a=1KUBEHZ_API_URL_PLACEHOLDERb=2`. It failed
  # CLOSED, which is the right direction, but on the wrong message: the
  # placeholder scan told the operator the MANIFESTS still carried a
  # placeholder, pointing them at the one place the problem was not.
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/amp"
  mkdir -p "${work}"
  local url="https://api.example.com/heartbeat?cluster=a&mode=push"

  run kubehz::render_agent "${work}" "acme.example.com" "${url}" operator managed
  assert_success

  run grep -h 'KUBEHZ_API_URL:' "${work}/agent/configmap.yaml" "${work}/live-agent/base/configmap.yaml"
  assert_output --partial "${url}"
  refute_output --partial "PLACEHOLDER"
}

@test "sed_replacement: escapes every character sed reads as an instruction" {
  _source_deploy
  # Backslash, ampersand and the s-delimiter, each alone and then together in
  # the order that catches a wrong escaping ORDER: escaping '&' before '\'
  # would re-escape the backslashes just added and emit a literal '\' before
  # the match.
  assert_equal "$(kubehz::sed_replacement 'a&b')"   'a\&b'
  assert_equal "$(kubehz::sed_replacement 'a|b')"   'a\|b'
  assert_equal "$(kubehz::sed_replacement 'a\b')"   'a\\b'
  assert_equal "$(kubehz::sed_replacement 'a\&b')"  'a\\\&b'
  assert_equal "$(kubehz::sed_replacement 'plain')" 'plain'
  # A raw newline would end the `s` command itself — sed aborts the render
  # with "unterminated `s'". `owner` has no shape check, so it CAN carry one.
  assert_equal "$(kubehz::sed_replacement $'a\nb')" 'a\nb'

  # And the escaping is correct where it is USED, which is the claim that
  # matters: feed each value through the real substitution shape and get the
  # input back out, character for character.
  local v out
  for v in 'a&b' 'a|b' 'a\b' 'a\&b' '?x=1&y=2' 'plain' $'a\nb'; do
    out="$(printf 'TOK\n' | sed -e "s|TOK|$(kubehz::sed_replacement "${v}")|g")"
    assert_equal "${v} → ${out}" "${v} → ${v}"
  done
}

@test "overlay: access decides the overlay — registered gets base, managed gets managed" {
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r1"

  run kubehz::live_agent_overlay "${work}" registered
  assert_output "${work}/live-agent/base"
  run kubehz::live_agent_overlay "${work}" managed
  assert_output "${work}/live-agent/managed"
  # Anything that is not 'managed' is read-only: an unknown tier must never
  # fall through to the overlay that grants acting RBAC.
  run kubehz::live_agent_overlay "${work}" ""
  assert_output "${work}/live-agent/base"
  run kubehz::live_agent_overlay "${work}" Managed
  assert_output "${work}/live-agent/base"
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

@test "render: a placeholder scan that ERRORS refuses the render, like one that found something" {
  # grep exits 1 for "matched nothing" and 2 for "the scan itself failed".
  # Reading the two the same way turns a check that could not run into a check
  # that passed — and this is the check that keeps an unsubstituted CLUSTER_ID
  # out of a customer cluster.
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r7"
  mkdir -p "${work}"

  grep() { echo "grep: ${work}: Permission denied" >&2; return 2; }
  export -f grep

  run kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  unset -f grep
  assert_failure
  assert_output --partial "could not scan the rendered manifests"
  assert_output --partial "Permission denied"
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

@test "dry run: renders with the kustomize the APPLY uses, not the standalone binary" {
  # The apply is `kubectl apply -k`, so the dry run is `kubectl kustomize`. Two
  # renderers means two kustomize versions (the embedded one lags the released
  # one), and a dry run that can differ from the apply is untrustworthy for the
  # cases you would run it for. A standalone `kustomize` on PATH must not be
  # what produces this output.
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r5"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  kustomize() { echo "STANDALONE-KUSTOMIZE-RAN $*"; }
  export -f kustomize

  run kubehz::deploy_print "${work}" operator managed
  assert_success
  refute_output --partial "STANDALONE-KUSTOMIZE-RAN"
  assert_output --partial "kind: CronJob"
  assert_output --partial "name: kubehz-live-agent"
  # managed was asked for, so the acting RBAC is what the operator is shown.
  assert_output --partial "machinedeployments"
}

@test "dry run: cronjob mode prints the CronJob only, and says the live agent goes" {
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/r6"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered

  run kubehz::deploy_print "${work}" cronjob registered
  assert_success
  assert_output --partial "kind: CronJob"
  refute_output --partial "name: kubehz-live-agent"
  assert_output --partial "NOT deployed in cronjob mode"
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

@test "waits: the rollout timeout comes from the environment, so a slow pull can be given room" {
  # A fail-hard guard the operator cannot extend turns a cold first image pull
  # on a slow link into "NOTHING owns the heartbeat" with no way out but a
  # source edit. The default stays 120s; the override must reach kubectl.
  export KUBEHZ_LIVE_AGENT_ROLLOUT_SECONDS=600
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a8"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" operator managed
  assert_success
  run grep -F 'rollout status deployment/kubehz-live-agent' "${STUB_KUBECTL_LOG}"
  assert_output --partial "--timeout=600s"
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

@test "apply order (to cronjob): a pod probe that CANNOT SEE the pods never re-arms the CronJob" {
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  # The live agent's pod IS still running — but the kubeconfig may delete a
  # Deployment and not list pods in kubehz-system, which is an ordinary narrow
  # role. "No pods" and "cannot see pods" print the same empty stdout, so a
  # probe that reads only the output is blind to the difference and calls a
  # still-beating cluster drained.
  kubectl() {
    case "$*" in
      *"get pods"*)
        echo 'Error from server (Forbidden): pods is forbidden: User "deployer" cannot list resource "pods" in API group "" in the namespace "kubehz-system"' >&2
        return 1
        ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl
  sleep() { :; }
  export -f sleep
  KUBEHZ_LIVE_AGENT_DRAIN_SECONDS=10

  local work="${BATS_TEST_TMPDIR}/a6"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered

  run kubehz::deploy_apply "${work}" cronjob registered
  assert_failure
  # Refused, and honest about WHY: unknown, not "gone" and not "still there".
  assert_output --partial "could not tell whether"
  assert_output --partial "Forbidden"
  # THE POINT, again: an unreadable probe must not re-arm the CronJob's beat.
  refute [ "$(grep -c "apply -k ${work}/agent" "${STUB_KUBECTL_LOG}")" != "0" ]
}

@test "apply order (to operator): an unreadable heartbeat probe WARNS and continues" {
  # The mirror image, and deliberately different: the drain wait before the
  # live agent starts is the one fail-soft step. Its worst case is one stray
  # schema-1 beat that the live agent overwrites — not a standing second
  # producer — so it must not block a deploy. It must still SAY so.
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  kubectl() {
    case "$*" in
      *"get pods"*) echo 'Error from server (Forbidden): pods is forbidden' >&2; return 1 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  local work="${BATS_TEST_TMPDIR}/a7"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" operator managed
  assert_success
  assert_output --partial "could not check for an in-flight heartbeat pod"
  run sed -n '2p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/live-agent/managed"
}

# Both drain probes capture stdout AND stderr, because the error text is what
# makes their messages useful. That makes stderr chatter indistinguishable from
# a pod unless the capture is parsed: `kubectl get pods -o name` writes one
# `pod/<name>` line per pod to STDOUT, and an apiserver `Warning:` — a
# deprecation, a finalizer nudge — to stderr, with exit status 0 either way.
# One warning line used to read as "a pod is running" in both probes.
_stub_kubectl_warns() {
  export STUB_KUBECTL_WARNING="${1}"
  kubectl() {
    case "$*" in
      *"get pods"*) echo "${STUB_KUBECTL_WARNING}" >&2; return 0 ;;
    esac
    return 0
  }
  export -f kubectl
  export STUB_SLEEP_LOG="${BATS_TEST_TMPDIR}/slept"
  : > "${STUB_SLEEP_LOG}"
  sleep() { echo "slept $*" >> "${STUB_SLEEP_LOG}"; }
  export -f sleep
}

@test "waits: an apiserver Warning is not an in-flight heartbeat pod" {
  # Fail-soft direction. The old parse cost the deploy the whole drain window
  # and then warned about a heartbeat pod that was never running — a message
  # that sends an operator looking for a pod that does not exist.
  _source_deploy
  _stub_kubectl_warns 'Warning: v1 ComponentStatus is deprecated in v1.19+'

  run kubehz::wait_heartbeat_idle
  assert_success
  assert_output ""
  [ ! -s "${STUB_SLEEP_LOG}" ] || fail "the probe waited on a warning: $(cat "${STUB_SLEEP_LOG}")"
}

@test "waits: an apiserver Warning is not a live-agent pod that will not terminate" {
  # Same parse, higher stakes. This wait is FAIL-HARD, so one warning line on
  # stderr aborted the whole switch and told the operator to force-delete a pod
  # that had already gone.
  _source_deploy
  _stub_kubectl_warns 'Warning: metadata.finalizers: "foregroundDeletion": prefer a domain-qualified finalizer name'

  run kubehz::wait_live_agent_gone
  assert_success
  assert_output ""
  [ ! -s "${STUB_SLEEP_LOG}" ] || fail "the probe waited on a warning: $(cat "${STUB_SLEEP_LOG}")"
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

@test "rbac: the digest pin covers every RBAC file IN THE TREE, and nothing else" {
  # `sha256sum --check` only verifies files that are LISTED, so the pin is
  # blind to an ADDED file by construction: drop a second RBAC document in
  # beside the vendored ones and the check above still passes, guarding
  # nothing. So the expected list is derived from the directory itself, not
  # written out by hand — a new rbac*.yaml must be pinned (and therefore
  # digest-matched against upstream) before this test can go green again.
  run bash -c "cd '${LIVE_DIR}' && command find . -type f -name 'rbac*.yaml' | command sed 's|^\\./||' | command sort"
  assert_success
  local expected="${output}"

  run bash -c "command grep -vE '^[[:space:]]*(#|\$)' '${LIVE_DIR}/UPSTREAM.sha256' | command awk '{print \$2}' | command sort"
  assert_success
  [ "${output}" = "${expected}" ]
  # …and the two we expect are the two that are there.
  assert_line "base/rbac.yaml"
  assert_line "managed/rbac-managed.yaml"
  [ "${#lines[@]}" -eq 2 ]
}

@test "rbac: the vendored tree holds no file that nobody accounts for" {
  # The pin covers rbac*.yaml; the rest of the tree is lok8s's own and is
  # enumerated here. A file in NEITHER set — an extra manifest quietly added
  # to an overlay, say — fails this test, which is the only place that reads
  # the directory as a whole.
  run bash -c "cd '${LIVE_DIR}' && command find . -type f | command sed 's|^\\./||' | command sort"
  assert_success
  assert_line "UPSTREAM.sha256"
  assert_line "base/configmap.yaml"
  assert_line "base/deployment.yaml"
  assert_line "base/kustomization.yaml"
  assert_line "base/rbac.yaml"
  assert_line "base/serviceaccount.yaml"
  assert_line "managed/kustomization.yaml"
  assert_line "managed/rbac-managed.yaml"
  [ "${#lines[@]}" -eq 8 ]
}

# A ClusterRole nobody is bound to grants nothing; a BINDING is what turns a
# rule into a permission. The rule-count guards above select ClusterRole/Role
# only, so a binding contributes no rule and slips past every one of them —
# `cluster-admin` bound to the agent's ServiceAccount would have been invisible
# to this whole file. These two tests pin the bindings themselves: which Role
# each one names, and who it names as subject.
_bindings() {
  local overlay="${1}"
  command kustomize build "${overlay}" \
    | command yq -r 'select(.kind=="RoleBinding" or .kind=="ClusterRoleBinding")
        | .kind + "|" + (.metadata.namespace // "-") + "/" + .metadata.name
        + "|" + .roleRef.kind + "/" + .roleRef.name
        + "|" + ([.subjects[] | .kind + "/" + (.namespace // "-") + "/" + .name] | join(","))' \
    | command grep -v '^---$'
}

@test "rbac: the BASE binds only its own two Roles, to its own ServiceAccount" {
  run _bindings "${LIVE_DIR}/base"
  assert_success
  assert_line "ClusterRoleBinding|-/kubehz-live-agent|ClusterRole/kubehz-live-agent|ServiceAccount/kubehz-system/kubehz-live-agent"
  assert_line "RoleBinding|kubehz-system/kubehz-live-agent-secret|Role/kubehz-live-agent-secret|ServiceAccount/kubehz-system/kubehz-live-agent"
  # Exactly two. A third binding is a permission a customer did not agree to,
  # however narrow the roleRef looks.
  [ "${#lines[@]}" -eq 2 ]
}

@test "rbac: the MANAGED overlay adds exactly two bindings, both to its own Roles" {
  run _bindings "${LIVE_DIR}/managed"
  assert_success
  assert_line "ClusterRoleBinding|-/kubehz-live-agent|ClusterRole/kubehz-live-agent|ServiceAccount/kubehz-system/kubehz-live-agent"
  assert_line "RoleBinding|kubehz-system/kubehz-live-agent-secret|Role/kubehz-live-agent-secret|ServiceAccount/kubehz-system/kubehz-live-agent"
  # The acting RBAC. The machinedeployments RoleBinding is namespaced to
  # kube-system — cluster-wide is a different permission entirely.
  assert_line "RoleBinding|kube-system/kubehz-live-agent-machinedeployments|Role/kubehz-live-agent-machinedeployments|ServiceAccount/kubehz-system/kubehz-live-agent"
  assert_line "ClusterRoleBinding|-/kubehz-live-agent-eviction-unwedge|ClusterRole/kubehz-live-agent-eviction-unwedge|ServiceAccount/kubehz-system/kubehz-live-agent"
  [ "${#lines[@]}" -eq 4 ]
}

# A `.rules` list is not the only place a ClusterRole's permissions can come
# from. `aggregationRule` leaves `.rules` exactly as committed and has the RBAC
# controller FILL IT IN AT RUNTIME from every ClusterRole matching the
# selector — and `cluster-admin` ships with the label
# `kubernetes.io/bootstrapping: rbac-defaults` and `*` on apiGroups, resources
# and verbs, so a four-line selector is total escalation.
#
# Every guard above reads the STATIC `.rules`, so an aggregated role is
# invisible to all of them at once: it adds no rule line, changes no rule
# count, moves no digest, adds no file, and binds nothing new. The shape of the
# document is the only thing left that can see it — so pin the shape. A Role or
# ClusterRole may carry these four keys and no fifth, whatever RBAC grows next.
@test "rbac: no Role or ClusterRole may carry a field beyond its own rules" {
  local pair overlay expected line
  for pair in base:2 managed:4; do
    overlay="${pair%%:*}"
    expected="${pair##*:}"
    run bash -c "command kustomize build '${LIVE_DIR}/${overlay}' | command yq -r 'select(.kind==\"ClusterRole\" or .kind==\"Role\") | .metadata.name + \"|\" + ([keys[]] | sort | join(\",\"))' | command grep -v '^---\$'"
    assert_success
    for line in "${lines[@]}"; do
      [ "${line#*|}" = "apiVersion,kind,metadata,rules" ] || fail \
        "${overlay}: ${line} — a Role/ClusterRole carries a field beyond apiVersion/kind/metadata/rules. If that field is 'aggregationRule', the RBAC controller fills .rules at runtime from cluster-admin and every rule pin in this file still passes."
    done
    # …and the roles counted are the roles that exist, so an overlay that
    # rendered no role at all could not make the loop above vacuous.
    [ "${#lines[@]}" -eq "${expected}" ] || fail \
      "${overlay}: expected ${expected} Role/ClusterRole documents, got ${#lines[@]}"
  done
}

@test "rbac: neither live-agent kustomization may PATCH what it renders" {
  # UPSTREAM.sha256 pins rbac*.yaml; the kustomizations that ASSEMBLE them are
  # not pinned, and a kustomization can rewrite any field of any resource
  # downstream of it. `patches:` bolting an aggregationRule onto the ClusterRole
  # is the sharpest version and touches no pinned byte at all.
  #
  # So the two kustomizations are held to the keys that cannot rewrite a
  # rendered object:
  #   apiVersion, kind — the header;
  #   resources        — names files that are either digest-pinned or covered
  #                      by the file inventory above;
  #   images           — the one documented override (mirror/air-gap), and the
  #                      image it can change is pinned by the pod-spec test
  #                      below, which renders THROUGH kustomize.
  # patches, patchesStrategicMerge, patchesJson6902, replacements, components,
  # transformers, the generators and the label/name transformers all fail here.
  # Widening this list is a review decision, which is exactly the point.
  local overlay key
  for overlay in base managed; do
    run bash -c "command yq -r '[keys[]] | sort | join(\",\")' '${LIVE_DIR}/${overlay}/kustomization.yaml'"
    assert_success
    for key in ${output//,/ }; do
      case "${key}" in
        apiVersion | kind | resources | images) ;;
        *) fail "${overlay}/kustomization.yaml carries '${key}' — only apiVersion/kind/resources/images may appear here, because anything else can rewrite a rendered object without touching a digest-pinned file" ;;
      esac
    done
  done
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
  # A customer cluster can only pull what it can reach, so the whole ref is
  # pinned, host included: a build registry or a mirror that reaches only the
  # developer who set it would install an agent nobody else can start.
  assert_output --regexp '^ghcr\.io/kernpilot/kubehz-agent@sha256:[0-9a-f]{64}$'
  # A digest is the only ref cosign verification and a rollback can reason about.
  refute_output --partial ":latest"
  refute_output --partial ":main"
}

# ══ 7. The pod spec (what the agent may do ON THE NODE) ═══════════════════

# Everything above pins what the agent may ask the APISERVER for. None of it
# says anything about what its pod may do on the NODE, and the two are not the
# same risk: `privileged: true`, `hostPID: true`, `runAsUser: 0` and a hostPath
# mount of /etc/kubernetes need no RBAC whatsoever — and on a kubeadm or
# KubeOne control-plane node that path holds admin.conf, which is cluster-admin
# in a file. Until this section existed the only assertion about deployment.yaml
# in this entire file was its image.
#
# The pod spec is therefore pinned WHOLE, as flattened `path=value` leaves:
# every field that is there, with its value, and nothing else. Positive, not a
# blacklist — the dangerous field that matters next is the one nobody has
# thought to forbid yet, and a new leaf fails the count below by default.
_podspec() {
  local overlay="${1}"
  command kustomize build "${overlay}" \
    | command yq -r 'select(.kind=="Deployment") | .spec.template.spec' \
    | command yq -r '[.. | select(tag!="!!map" and tag!="!!seq")
        | (path | join(".")) + "=" + (. | tostring)] | sort | .[]'
}

@test "pod: the rendered pod spec is pinned field for field, in BOTH overlays" {
  local overlay
  for overlay in base managed; do
    run _podspec "${LIVE_DIR}/${overlay}"
    assert_success

    # Identity, and the one Secret key the agent needs — read-only, by name.
    assert_line "serviceAccountName=kubehz-live-agent"
    assert_line "automountServiceAccountToken=true"
    assert_line "volumes.0.name=agent-token"
    assert_line "volumes.0.secret.secretName=kubehz-agent"
    assert_line "volumes.0.secret.items.0.key=agent-token"
    assert_line "volumes.0.secret.items.0.path=agent-token"
    assert_line "volumes.0.secret.defaultMode=256" # 0400
    assert_line "containers.0.volumeMounts.0.name=agent-token"
    assert_line "containers.0.volumeMounts.0.mountPath=/var/run/secrets/kubehz"
    assert_line "containers.0.volumeMounts.0.readOnly=true"

    # Pod-level hardening: non-root, and the runtime's seccomp profile.
    assert_line "securityContext.runAsNonRoot=true"
    assert_line "securityContext.runAsUser=65532"
    assert_line "securityContext.runAsGroup=65532"
    assert_line "securityContext.fsGroup=65532"
    assert_line "securityContext.seccompProfile.type=RuntimeDefault"

    # Container-level hardening: no escalation, no writable root, no caps.
    assert_line "containers.0.name=agent"
    assert_line "containers.0.securityContext.allowPrivilegeEscalation=false"
    assert_line "containers.0.securityContext.readOnlyRootFilesystem=true"
    assert_line "containers.0.securityContext.runAsNonRoot=true"
    assert_line "containers.0.securityContext.runAsUser=65532"
    assert_line "containers.0.securityContext.capabilities.drop.0=ALL"

    # The rest: config source, image, pull policy, resource envelope.
    assert_line "containers.0.envFrom.0.configMapRef.name=kubehz-live-agent-config"
    assert_line --regexp '^containers\.0\.image=ghcr\.io/kernpilot/kubehz-agent@sha256:[0-9a-f]{64}$'
    assert_line "containers.0.imagePullPolicy=IfNotPresent"
    assert_line "containers.0.resources.requests.cpu=25m"
    assert_line "containers.0.resources.requests.memory=64Mi"
    assert_line "containers.0.resources.limits.cpu=200m"
    assert_line "containers.0.resources.limits.memory=256Mi"

    # THE GUARD. Twenty-eight leaves, and the twenty-eight above are all of
    # them. A second container, a second volume, an added env var, `hostPID:
    # true`, `hostNetwork: true`, `hostIPC: true`, `privileged: true`, a
    # hostPath — every one of those is a twenty-ninth line, and fails here
    # without anyone having had to predict which one it would be.
    [ "${#lines[@]}" -eq 28 ] || fail \
      "${overlay}: the pod spec has ${#lines[@]} leaf fields, expected 28 — a field was added or removed:"$'\n'"${output}"
  done
}

# ══ 8. The default is robust, not just absent ═════════════════════════════

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
