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
# The timeout stub and stub_call come from tests/lib/agent_bound.sh.
kubectl() {
  stub_call kubectl "$@"
  case "$*" in
    # The identity read: the Secret holds both keys with a value
    # (resourceVersion 1, 2 entries).
    "get secret kubehz-agent -n kubehz-system -o go-template="*) printf '1 2 AC' ;;
    *"get secret kubehz-agent"*"agent-token"*) printf 'khz_agt_test' | base64 | tr -d '\n' ;;
    *"get secret kubehz-agent"*"claim-code"*)  printf 'khzc_test'   | base64 | tr -d '\n' ;;
    *"get secret kubehz-agent"*) return 0 ;;
    *"/version"*) printf '{"gitVersion":"v1.35.5"}\n' ;;
    "get --raw /api/v1/nodes") printf '{"items":[{"metadata":{"name":"cp-1"}}]}\n' ;;
    "get csr -o json") printf '{}\n' ;;
    *"/readyz"*) printf 'ok\n'; return 0 ;;
    "get configmap kubehz-agent-state"*) printf '%s' "${STUB_MARKER:-{\}}" ;;
    "annotate "*) return 0 ;;
    "get pods"*) printf ''; return 0 ;;
    *) printf '' ;;
  esac
}
curl() {
  stub_call curl "$@"
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
  cat "${_PROJECT_ROOT}/tests/lib/agent_bound.sh" "${stubs}" "${heartbeat}" > "${RUNNER}"

  export CLUSTER_ID="test.example.com"
  export KUBEHZ_API_URL="https://api.example.com"
  export STUB_CURL_LOG="${BATS_TEST_TMPDIR}/curl.log"
  : > "${STUB_CURL_LOG}"
  export STUB_TIMEOUT_FORM_LOG="${BATS_TEST_TMPDIR}/timeout-form.log"
  : > "${STUB_TIMEOUT_FORM_LOG}"
  # Each kubectl and curl call, with its bound (tests/lib/agent_bound.sh).
  export STUB_CALL_LOG="${BATS_TEST_TMPDIR}/calls.log"
  : > "${STUB_CALL_LOG}"
  source "${_PROJECT_ROOT}/tests/lib/agent_bound.bash"
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
  # main holds kubehz::bind_secret_ok, the one bind-secret check the deploy
  # shares with the register calls; lo always loads main before deploy.
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/deploy"
}

# ══ 1. The CronJob interlock (the in-cluster guarantee) ════════════════════

@test "interlock: KUBEHZ_HEARTBEAT_OWNER=operator stops the CronJob beating" {
  _make_runner
  export KUBEHZ_HEARTBEAT_OWNER="operator"

  run bash "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

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
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

  run grep -c '/heartbeat' "${STUB_CURL_LOG}"
  assert_output "1"
}

@test "interlock: an UNSET owner reads as cronjob (installs predating the key)" {
  _make_runner
  unset KUBEHZ_HEARTBEAT_OWNER

  run bash "${RUNNER}"
  assert_success
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded

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
  # Every kubectl and curl call ran under `timeout -k 2 SECS`.
  assert_agent_calls_bounded
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
  # with "unterminated `s'". deploy_agent's shape checks keep one out in
  # production; the escape is the layer below, so it is tested on its own.
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

  run kubehz::deploy_print "${work}" acme.example.com operator managed
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

  run kubehz::deploy_print "${work}" acme.example.com cronjob registered
  assert_success
  assert_output --partial "kind: CronJob"
  refute_output --partial "name: kubehz-live-agent"
  assert_output --partial "NOT deployed in cronjob mode"
}

# ══ 4. Apply order (no instant with two producers) ═════════════════════════

# The argv of the three calls around the stage: the Job delete, the pod poll
# and the patch that ends the suspend. Literal on purpose: a change of the
# selector or of the patch in the lib fails here.
AGENT_JOBS_DELETE='-n kubehz-system delete job -l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent --ignore-not-found=true'
AGENT_JOBS_POLL='-n kubehz-system get pods -l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent --field-selector=status.phase!=Succeeded,status.phase!=Failed -o name'
AGENT_RESUME='-n kubehz-system patch cronjob kubehz-heartbeat --type=merge -p {"spec":{"suspend":false}}'

# The texts of the stop of the pod wait and of an interrupt while the
# CronJob agent is suspended. Literal on purpose: a change in the lib fails
# here.
_AGENT_STAYS_SUSPENDED_TEXT="The CronJob agent stays suspended, so heartbeats can stop until the deploy runs again."
_AGENT_SUSPEND_SAFE_TEXT="That is the safe state: a suspended CronJob starts no Job."
_AGENT_PODS_STUCK_TEXT="A pod ends within about 30 s (its grace period) after the Job delete, unless its node does not answer: then the pod stays Terminating. Check the node of each pod: kubectl -n kubehz-system get pods -l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent -o wide. If the node is gone, delete the pod: kubectl -n kubehz-system delete pod <name> --force --grace-period=0. Then run 'lo kubehz deploy' again."
_AGENT_INTERRUPTED_TEXT="[error] kubehz: the deploy was interrupted. The CronJob agent can stay suspended, so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again: it ends the suspend."

_stub_kubectl_log() {
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  export AGENT_JOBS_POLL
  : > "${STUB_KUBECTL_LOG}"
  kubectl() {
    case "$*" in
      # The poll for the pods of the CronJob agent's Jobs: logged, and no
      # pods.
      "${AGENT_JOBS_POLL}") echo "$*" >> "${STUB_KUBECTL_LOG}"; return 0 ;;
      # The drain probe: report an idle cluster so the order test is not a
      # timing test.
      *"get pods"*) printf ''; return 0 ;;
      # The identity probe: the Secret holds an agent-token, so the deploy
      # runs no bootstrap.
      *"get secret kubehz-agent -o go-template="*)
        echo "$*" >> "${STUB_KUBECTL_LOG}"
        printf 'agent-token'
        return 0 ;;
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

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_success

  # Line 1 applies the CronJob tree (which carries KUBEHZ_HEARTBEAT_OWNER=
  # operator, stopping its beat), suspended; line 6 starts the live agent.
  # Reversed, the live agent would beat while the CronJob still did.
  run head -1 "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/agent"
  # Lines 2 to 4: the Jobs of the CronJob are deleted, their pods are waited
  # for, and only then the suspend ends.
  run sed -n '2p' "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_JOBS_DELETE}"
  run sed -n '3p' "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_JOBS_POLL}"
  run sed -n '4p' "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_RESUME}"
  # Line 5 reads the identity Secret; it holds an agent-token here, so no
  # bootstrap job runs and line 6 starts the live agent. The probe prints
  # only whether the key is there, never a value.
  run sed -n '5p' "${STUB_KUBECTL_LOG}"
  assert_output -- '-n kubehz-system get secret kubehz-agent -o go-template={{with .data}}{{if index . "agent-token"}}agent-token{{end}}{{end}}'
  run sed -n '6p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/live-agent/managed"
  run grep -c "create job" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  # Line 7 waits for the Deployment to be Ready. Accepted is not running, and
  # between the marker and Ready NOTHING owns the beat — so the command may not
  # report a successful handover until this returns.
  run sed -n '7p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "rollout status deployment/kubehz-live-agent"
}

@test "apply order (to operator): a first deploy runs the CronJob's bootstrap once and waits for its Job to complete" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1b"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  # The apply created the Secret empty; the one-off bootstrap Job fills it.
  export STUB_SECRET_FLAG="${BATS_TEST_TMPDIR}/secret-filled"
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent"*)
        echo "$*" >> "${STUB_KUBECTL_LOG}"
        [[ ! -f "${STUB_SECRET_FLAG}" ]] || printf 'agent-token'
        return 0 ;;
      *"wait --for=condition=complete"*) echo "$*" >> "${STUB_KUBECTL_LOG}"; touch "${STUB_SECRET_FLAG}"; return 0 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_success
  assert_output --partial "running the CronJob's bootstrap once"
  run grep -n "create job kubehz-heartbeat-bootstrap-" "${STUB_KUBECTL_LOG}"
  assert_success
  assert_output --partial -- "--from=cronjob/kubehz-heartbeat"
  # create, wait for completion, delete, then the live agent — in that order,
  # after the CronJob apply.
  local create_line wait_line del_line live_line
  create_line=$(grep -n "create job" "${STUB_KUBECTL_LOG}" | head -1 | cut -d: -f1)
  wait_line=$(grep -n "wait --for=condition=complete job/kubehz-heartbeat-bootstrap-" "${STUB_KUBECTL_LOG}" | head -1 | cut -d: -f1)
  del_line=$(grep -n "delete job kubehz-heartbeat-bootstrap-" "${STUB_KUBECTL_LOG}" | head -1 | cut -d: -f1)
  live_line=$(grep -n "apply -k ${work}/live-agent/managed" "${STUB_KUBECTL_LOG}" | head -1 | cut -d: -f1)
  [ "${create_line}" -gt 1 ] && [ "${create_line}" -lt "${wait_line}" ] && [ "${wait_line}" -lt "${del_line}" ] && [ "${del_line}" -lt "${live_line}" ]
  run grep -c -- "--timeout=150s" "${STUB_KUBECTL_LOG}"
  assert_output "1"
  # The wait and the delete name the Job the create made, not another.
  local name
  name=$(grep -oE "create job kubehz-heartbeat-bootstrap-[0-9]+-[0-9a-f]{4} " "${STUB_KUBECTL_LOG}" | head -1 | awk '{print $3}')
  [ -n "${name}" ]
  run grep -c "wait --for=condition=complete job/${name} " "${STUB_KUBECTL_LOG}"
  assert_output "1"
  run grep -c "delete job ${name} " "${STUB_KUBECTL_LOG}"
  assert_output "1"
}

@test "apply order (to operator): an identity Secret that is never filled FAILS the deploy before the live agent" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1c"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent"*) return 0 ;;
      *"wait --for=condition=complete"*) echo "error: timed out waiting for the condition" >&2; return 1 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl
  export KUBEHZ_IDENTITY_BOOTSTRAP_SECONDS=10

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_output --partial "did not complete within 10s"
  assert_output --partial "describe job kubehz-heartbeat-bootstrap-"
  assert_output --partial "A Job that failed stays until you delete it, or until the next 'lo kubehz deploy' deletes it."
  assert_output --partial "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again."
  run grep -c "apply -k ${work}/live-agent/managed" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  run grep -c "delete job kubehz-heartbeat-bootstrap-" "${STUB_KUBECTL_LOG}"
  assert_output "0"
}

@test "apply order (to operator): a Secret probe that exits without output names the exit status" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1f"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent"*) return 127 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_output --partial "could not read Secret kubehz-agent in kubehz-system: kubectl exited without output (exit status 127)"
  run grep -c "create job" "${STUB_KUBECTL_LOG}"
  assert_output "0"
}

@test "apply order (to operator): a Secret read that fails for any reason but NotFound STOPS the deploy without a bootstrap" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1d"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent"*) echo 'Error from server (Forbidden): secrets "kubehz-agent" is forbidden' >&2; return 1 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_output --partial "could not read Secret kubehz-agent"
  assert_output --partial "Forbidden"
  assert_output --partial "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so heartbeats can stop until the deploy runs again. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again."
  refute_output --partial "reporting NOTHING"
  run grep -c "create job" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  run grep -c "apply -k ${work}/live-agent/managed" "${STUB_KUBECTL_LOG}"
  assert_output "0"
}

# The apply just before created the Secret, and the agent cannot create it
# (its Role has no create): an absent Secret stops the deploy, also behind
# a warning line, and no bootstrap runs.
@test "apply order (to operator): an absent identity Secret (NotFound behind a warning line) STOPS the deploy without a bootstrap" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1e"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent"*)
        echo "Warning: the kubeconfig field exec.apiVersion v1alpha1 is deprecated" >&2
        echo 'Error from server (NotFound): secrets "kubehz-agent" not found' >&2; return 1 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_output --partial "Secret kubehz-agent does not exist in kubehz-system, but the deploy applied it just before"
  assert_output --partial "The agent cannot create it"
  assert_output --partial "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and this deploy did not apply the live agent, so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again."
  refute_output --partial "reporting NOTHING"
  run grep -c "create job" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  run grep -c "apply -k ${work}/live-agent/managed" "${STUB_KUBECTL_LOG}"
  assert_output "0"
}

# A bootstrap Job that completes but leaves no agent-token stops the deploy
# before the live agent; the Job stays for its log.
@test "apply order (to operator): a bootstrap that leaves no agent-token FAILS the deploy before the live agent" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/a1g"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent"*) printf 'Warning: a deprecated field\nclaim-code\n'; return 0 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_output --partial "completed but left no agent-token in Secret kubehz-agent"
  run grep -c "create job kubehz-heartbeat-bootstrap-" "${STUB_KUBECTL_LOG}"
  assert_output "1"
  run grep -c "delete job kubehz-heartbeat-bootstrap-" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  run grep -c "apply -k ${work}/live-agent/managed" "${STUB_KUBECTL_LOG}"
  assert_output "0"
}

@test "apply order (to operator): a live agent that never becomes Ready FAILS the deploy" {
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  kubectl() {
    case "$*" in
      *"get pods"*) printf ''; return 0 ;;
      # The identity Secret holds an agent-token: no bootstrap.
      *"get secret kubehz-agent -o go-template="*) echo "$*" >> "${STUB_KUBECTL_LOG}"; printf 'agent-token'; return 0 ;;
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

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  # The message must name the state the operator is now in — the beat can
  # stop — not just report a failed command.
  assert_output --partial "never became Ready"
  assert_output --partial "The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator) and the live agent is not Ready, so heartbeats can stop until the deploy runs again."
  refute_output --partial "NOTHING"
}

# A rollout wait that times out, then the pod read and the events read that
# name a pull that failed or hangs. STUB_PODS / STUB_EVENTS are the answers,
# STUB_PODS_RC / STUB_EVENTS_RC their exit codes. STUB_PODS_FILE /
# STUB_EVENTS_FILE, when set, name a file to answer from instead: a variable
# cannot hold a NUL byte. Every call lands in STUB_KUBECTL_LOG.
_ready_timeout() {
  _source_deploy
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${STUB_KUBECTL_LOG}"
  export STUB_PODS="${STUB_PODS:-}" STUB_EVENTS="${STUB_EVENTS:-}"
  export STUB_PODS_RC="${STUB_PODS_RC:-0}" STUB_EVENTS_RC="${STUB_EVENTS_RC:-0}"
  export STUB_PODS_FILE="${STUB_PODS_FILE:-}" STUB_EVENTS_FILE="${STUB_EVENTS_FILE:-}"
  kubectl() {
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    case "$*" in
      *"get pods -l app.kubernetes.io/name=kubehz-live-agent "*)
        if [[ -n "${STUB_PODS_FILE}" ]]; then cat "${STUB_PODS_FILE}"; else printf '%s' "${STUB_PODS}"; fi
        return "${STUB_PODS_RC}" ;;
      *"get events "*)
        if [[ -n "${STUB_EVENTS_FILE}" ]]; then cat "${STUB_EVENTS_FILE}"; else printf '%s' "${STUB_EVENTS}"; fi
        return "${STUB_EVENTS_RC}" ;;
      *"get pods"*) printf ''; return 0 ;;
      *"get secret kubehz-agent -o go-template="*) printf 'agent-token'; return 0 ;;
      *"rollout status"*) return 1 ;;
    esac
    return 0
  }
  export -f kubectl

  local work="${BATS_TEST_TMPDIR}/a3p"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_output --partial "never became Ready"
  assert_output --partial "(an image pull that fails or hangs, or an untolerated node taint, is the usual cause)"
}

AGENT_IMAGE="ghcr.io/kernpilot/kubehz-agent@sha256:474994f0a64acf179fa5d8524aac8ee963b347ae29407895c8a8f51ec6d064aa"

@test "ready timeout: a pull that started and did not finish is named, with the IPv6 check and the work-around" {
  # The first pod has no node yet (not a pull state) and is skipped. A
  # finished pull of another container does not count.
  STUB_PODS="kubehz-live-agent-6b7c9d8f5-q2w4r|||${AGENT_IMAGE}"$'\n'"kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|ContainerCreating|${AGENT_IMAGE}"$'\n'
  STUB_EVENTS=$'|Scheduled\nspec.containers{sidecar}|Pulling\nspec.containers{sidecar}|Pulled\nspec.containers{agent}|Pulling\n'
  _ready_timeout

  local want
  want="  Pod kubehz-live-agent-6b7c9d8f5-x2k4q on node worker-1 started to pull its image ${AGENT_IMAGE}, and the pull did not finish.
  A pull that hangs or times out can mean that the IPv6 path from the node to ghcr.io is broken.
  On that path, the connection opens but carries no data, and the pull does not try IPv4.
  To check, run these two commands on node worker-1. If only the IPv6 command times out, the IPv6 path is broken:
    curl -6 -sS -o /dev/null --max-time 15 https://ghcr.io/v2/
    curl -4 -sS -o /dev/null --max-time 15 https://ghcr.io/v2/
  ghcr.io sends the image layers from another host. Run the two commands against https://pkg-containers.githubusercontent.com/ too.
  To work around it, make the node reach ghcr.io over IPv4. Repair its IPv6 route, or run the node without IPv6.
  Then delete the pod, so that a new pod pulls the image again: kubectl -n kubehz-system delete pod kubehz-live-agent-6b7c9d8f5-x2k4q"
  [[ "${output}" == *"${want}" ]] || { echo "output does not end with the pull hint:"; echo "${output}"; false; }
  refute_output --partial "q2w4r on node"

  # The two reads, argv for argv (the Go binary sends the same).
  run grep -cxF -e '-n kubehz-system get pods -l app.kubernetes.io/name=kubehz-live-agent --request-timeout=10s -o jsonpath={range .items[*]}{.metadata.name}|{.spec.nodeName}|{.status.containerStatuses[?(@.name=="agent")].state.waiting.reason}|{.spec.containers[?(@.name=="agent")].image}{"\n"}{end}' "${STUB_KUBECTL_LOG}"
  assert_output "1"
  run grep -cxF -e '-n kubehz-system get events --field-selector involvedObject.kind=Pod,involvedObject.name=kubehz-live-agent-6b7c9d8f5-x2k4q --request-timeout=10s -o jsonpath={range .items[*]}{.involvedObject.fieldPath}|{.reason}{"\n"}{end}' "${STUB_KUBECTL_LOG}"
  assert_output "1"
  run grep -c "get events" "${STUB_KUBECTL_LOG}"
  assert_output "1"
}

@test "ready timeout: a failed pull is named from the waiting reason, and the registry comes from the pod's image" {
  local reason image="registry.example.com:5000/kubehz-agent@sha256:474994f0"
  for reason in ErrImagePull ImagePullBackOff; do
    STUB_PODS="kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|${reason}|${image}"$'\n'
    _ready_timeout
    assert_output --partial "  Pod kubehz-live-agent-6b7c9d8f5-x2k4q on node worker-1 cannot pull its image ${image} (${reason}). Read why: kubectl -n kubehz-system describe pod kubehz-live-agent-6b7c9d8f5-x2k4q"
    assert_output --partial "the IPv6 path from the node to registry.example.com:5000 is broken"
    assert_output --partial "    curl -6 -sS -o /dev/null --max-time 15 https://registry.example.com:5000/v2/"
    # The layer host line belongs to ghcr.io only.
    refute_output --partial "pkg-containers"
    # A failed pull needs no events read.
    run grep -c "get events" "${STUB_KUBECTL_LOG}"
    assert_output "0"
  done
}

@test "ready timeout: no pull hint without a pull that failed or hangs, or without a readable pod state" {
  local pod="kubehz-live-agent-6b7c9d8f5-x2k4q" creating sep=$'\x1f'
  creating="${pod}|worker-1|ContainerCreating|${AGENT_IMAGE}"$'\n'
  # pods, pods rc, events, events rc, joined by the unit separator 0x1f:
  # the answers hold '|' and newlines, and an IFS whitespace character
  # would merge the empty fields.
  local -a cases=(
    "${creating}${sep}0${sep}"$'|Scheduled\nspec.containers{agent}|Pulling\nspec.containers{agent}|Pulled\n|FailedMount\n'"${sep}0"  # the pull finished; a volume wait
    "${creating}${sep}0${sep}"$'|Scheduled\n|FailedMount\n'"${sep}0"                             # no pull started
    "${creating}${sep}0${sep}"$'|Scheduled\nspec.containers{sidecar}|Pulling\n'"${sep}0"        # another container pulls
    "${creating}${sep}0${sep}"$'Pulling\n'"${sep}0"                                              # a reason without its container
    "${creating}${sep}0${sep}"$'spec.containers{agent}|Pulling\n'"${sep}1"                       # events unreadable
    "${creating}${sep}1${sep}"$'spec.containers{agent}|Pulling\n'"${sep}0"                       # pod read fails
    "${pod}|||${AGENT_IMAGE}"$'\n'"${sep}0${sep}${sep}0"                                         # not scheduled
    "${pod}|worker-1||${AGENT_IMAGE}"$'\n'"${sep}0${sep}${sep}0"                                 # container running
    "${pod}|worker-1|CrashLoopBackOff|${AGENT_IMAGE}"$'\n'"${sep}0${sep}${sep}0"                 # another waiting reason
    "${pod}"$'\e'"[31m|worker-1|ImagePullBackOff|${AGENT_IMAGE}"$'\n'"${sep}0${sep}${sep}0"      # an escape in the pod name
    "${pod}|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-agent"$'\e'"[31m@sha256:474994f0"$'\n'"${sep}0${sep}${sep}0"      # an escape in the image
    "${pod}|worker-1|ImagePullBackOff|ghcr.io/kernpilot/"$'\xe2\x80\xae'"kubehz-agent@sha256:474994f0"$'\n'"${sep}0${sep}${sep}0" # a bidi override (U+202E) in the image
    "${pod}|worker-1|ImagePullBackOff|ghcr.io/x;\$(id)"$'\n'"${sep}0${sep}${sep}0"              # shell text in the image
    "${pod}|worker-1|ImagePullBackOff|${AGENT_IMAGE}|x"$'\n'"${sep}0${sep}${sep}0"               # a fifth field
    "${sep}0${sep}${sep}0"                                                                       # no pods
  )
  local c
  for c in "${cases[@]}"; do
    IFS="${sep}" read -r -d '' STUB_PODS STUB_PODS_RC STUB_EVENTS STUB_EVENTS_RC <<<"${c}" || true
    STUB_EVENTS_RC="${STUB_EVENTS_RC%$'\n'}"
    # A case that did not split into four fields would pass for no reason.
    [[ "${STUB_PODS_RC}" == [01] && "${STUB_EVENTS_RC}" == [01] ]] || { echo "bad case: ${c}"; false; }
    _ready_timeout
    refute_output --partial "IPv6"
    refute_output --partial "  Pod "
  done
}

@test "ready timeout: a non-ASCII letter in the image is refused in a UTF-8 locale too, as in the Go binary" {
  # In en_US.UTF-8, bash's [A-Za-z] also matches letters like é; the Go
  # regexp does not. The hint matches with LC_ALL=C, so both refuse the line.
  export LC_ALL=en_US.UTF-8
  local re='^[A-Za-z]+$'
  [[ $'\xc3\xa9' =~ ${re} ]] || skip "no locale here in which [A-Za-z] matches é: the case would pass without LC_ALL=C"
  STUB_PODS="kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-ag"$'\xc3\xa9'"nt@sha256:474994f0"$'\n'
  _ready_timeout
  refute_output --partial "IPv6"
  refute_output --partial "  Pod "
  # The same line with an ASCII image, in the same locale, gets the hint.
  STUB_PODS="kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-agent@sha256:474994f0"$'\n'
  _ready_timeout
  assert_output --partial "  Pod kubehz-live-agent-6b7c9d8f5-x2k4q on node worker-1 cannot pull its image"
}

@test "ready timeout: the image may be at most 511 bytes, as in the Go binary" {
  # The limit is a separate check and not a regex bound: a bound above 255
  # (RE_DUP_MAX) does not compile on every libc. Go: liveAgentImageMaxLen.
  local pod="kubehz-live-agent-6b7c9d8f5-x2k4q" prefix="ghcr.io/kernpilot/kubehz-agent:" pad at_max
  printf -v pad '%*s' $(( 511 - ${#prefix} )) ''
  at_max="${prefix}${pad// /a}"
  [[ ${#at_max} -eq 511 ]] || { echo "image is ${#at_max} bytes, want 511"; false; }
  STUB_PODS="${pod}|worker-1|ImagePullBackOff|${at_max}"$'\n'
  _ready_timeout
  assert_output --partial "  Pod ${pod} on node worker-1 cannot pull its image ${at_max} (ImagePullBackOff)."
  STUB_PODS="${pod}|worker-1|ImagePullBackOff|${at_max}a"$'\n'
  _ready_timeout
  refute_output --partial "IPv6"
  refute_output --partial "  Pod "
}

@test "ready timeout: a NUL byte in the pod read or the events read gets no hint, as in the Go binary" {
  # Command substitution drops a NUL byte. Without the change to 0x01, the
  # lines below would match once the NUL is gone, and the Go binary refuses
  # them.
  local pod="kubehz-live-agent-6b7c9d8f5-x2k4q"
  export STUB_PODS_FILE="${BATS_TEST_TMPDIR}/pods" STUB_EVENTS_FILE="${BATS_TEST_TMPDIR}/events"
  printf '%s|worker-1|ImagePullBackOff|ghcr.io/kernpilot/kubehz-agent\0@sha256:474994f0\n' "${pod}" > "${STUB_PODS_FILE}"
  : > "${STUB_EVENTS_FILE}"
  [[ $(tr -dc '\000' < "${STUB_PODS_FILE}" | wc -c) -eq 1 ]] || { echo "the pods file holds no NUL"; false; }
  _ready_timeout
  refute_output --partial "IPv6"
  refute_output --partial "  Pod "
  refute_output --partial "null byte"

  printf '%s|worker-1|ContainerCreating|%s\n' "${pod}" "${AGENT_IMAGE}" > "${STUB_PODS_FILE}"
  printf 'spec.containers{agent}|Pul\0ling\n' > "${STUB_EVENTS_FILE}"
  [[ $(tr -dc '\000' < "${STUB_EVENTS_FILE}" | wc -c) -eq 1 ]] || { echo "the events file holds no NUL"; false; }
  _ready_timeout
  refute_output --partial "IPv6"
  refute_output --partial "  Pod "
  refute_output --partial "null byte"

  # The same answers without the NUL get the hint, so the stub files work.
  printf 'spec.containers{agent}|Pulling\n' > "${STUB_EVENTS_FILE}"
  _ready_timeout
  assert_output --partial "  Pod ${pod} on node worker-1 started to pull its image"
}

@test "ready timeout: the hint reads the container and the pods that the vendored Deployment names" {
  # A re-vendor that renames the container or changes the selector must fail
  # here, not silently turn the hint off.
  local deployment="${LIVE_DIR}/base/deployment.yaml" container name
  container=$(command yq -r '.spec.template.spec.containers[].name' "${deployment}")
  name=$(command yq -r '.spec.selector.matchLabels["app.kubernetes.io/name"]' "${deployment}")
  [[ -n "${container}" && "${container}" != *$'\n'* ]] || { echo "want exactly one container, got: ${container}"; false; }
  [[ -n "${name}" && "${name}" != null ]]
  run command yq -r '.spec.template.metadata.labels["app.kubernetes.io/name"]' "${deployment}"
  assert_output "${name}"

  STUB_PODS="kubehz-live-agent-6b7c9d8f5-x2k4q|worker-1|ContainerCreating|${AGENT_IMAGE}"$'\n'
  STUB_EVENTS="spec.containers{${container}}|Pulling"$'\n'
  _ready_timeout
  assert_output --partial "  Pod kubehz-live-agent-6b7c9d8f5-x2k4q on node worker-1 started to pull its image"
  run grep -c -F -e "get pods -l app.kubernetes.io/name=${name} " "${STUB_KUBECTL_LOG}"
  assert_output "1"
  run bash -c 'grep -o -F -e "$1" "$2" | wc -l' _ "@.name==\"${container}\"" "${STUB_KUBECTL_LOG}"
  assert_output "2"
}

@test "image registry: the image's host, else Docker Hub" {
  _source_deploy
  local -A want=(
    ["${AGENT_IMAGE}"]="ghcr.io"
    ["registry.example.com:5000/agent@sha256:a"]="registry.example.com:5000"
    ["localhost/agent:1"]="localhost"
    ["mirror:5000/agent:1"]="mirror:5000"
    ["docker.io/library/busybox:1"]="registry-1.docker.io"
    ["library/busybox:1"]="registry-1.docker.io"
    ["busybox@sha256:a"]="registry-1.docker.io"
  )
  local image
  for image in "${!want[@]}"; do
    run kubehz::image_registry "${image}"
    assert_success
    assert_output "${want[${image}]}"
  done
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

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
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

  run kubehz::deploy_apply "${work}" acme.example.com cronjob registered
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
  # CronJob's marker be rewritten, suspended.
  run sed -n '3p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/agent"
  # Then the Jobs of the CronJob are deleted, their pods are waited for, and
  # the suspend ends as the last step.
  run sed -n '4,6p' "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_JOBS_DELETE}
${AGENT_JOBS_POLL}
${AGENT_RESUME}"
  run wc -l < "${STUB_KUBECTL_LOG}"
  assert_output "6"

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

  run kubehz::deploy_apply "${work}" acme.example.com cronjob registered
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

  run kubehz::deploy_apply "${work}" acme.example.com cronjob registered
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

  run kubehz::deploy_apply "${work}" acme.example.com cronjob registered
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
      # Only the heartbeat probe fails; the poll for the pods of the CronJob
      # agent's Jobs (fail-hard) finds none.
      *"--field-selector=status.phase=Running"*) echo 'Error from server (Forbidden): pods is forbidden' >&2; return 1 ;;
      *"get pods"*) return 0 ;;
      *"get secret kubehz-agent -o go-template="*) echo "$*" >> "${STUB_KUBECTL_LOG}"; printf 'agent-token'; return 0 ;;
    esac
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    return 0
  }
  export -f kubectl

  local work="${BATS_TEST_TMPDIR}/a7"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_success
  assert_output --partial "could not check for an in-flight heartbeat pod"
  # Line 3 ends the suspend, line 4 reads the identity Secret, and the live
  # agent follows on line 5.
  run sed -n '3p' "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_RESUME}"
  run sed -n '5p' "${STUB_KUBECTL_LOG}"
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

@test "waits: an apiserver Warning is not a pod of the CronJob agent's Jobs" {
  # Fail-hard too: a warning line read as a pod would stop the deploy with
  # the CronJob suspended.
  _source_deploy
  _stub_kubectl_warns 'Warning: v1 ComponentStatus is deprecated in v1.19+'

  run kubehz::wait_agent_jobs_gone
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

@test "deploy_agent: refuses a spec.kubehz.agent outside the enum before templating" {
  _source_deploy
  export LOK8S_KUBEHZ_HOSTING="self" LOK8S_KUBEHZ_ACCESS="registered"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.cloud"
  # sed-escaping keeps these out of the s-command, but the value also lands in
  # a double-quoted YAML scalar, where a quote or backslash rewrites the
  # manifest. The enum check refuses BEFORE any of that can matter.
  local bad
  for bad in 'Operator' 'cronjob"extra' $'operator\nbeats'; do
    export LOK8S_KUBEHZ_AGENT="${bad}"
    run kubehz::deploy_agent "acme.example.com"
    assert_failure
    assert_output --partial "must be 'cronjob' or 'operator'"
  done
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
  run bash -c "command kustomize build '${LIVE_DIR}/base' | command yq -r 'select(.kind==\"ClusterRole\" or .kind==\"Role\") | .rules[] | ((.apiGroups // [])|join(\",\")) + \"|\" + ((.resources // .nonResourceURLs)|join(\",\")) + \"|\" + (.verbs|join(\",\"))' | command grep -v '^---$'"
  assert_success
  # The exact rule set, pinned. A new line here is a permission a customer did
  # not agree to, so this test is meant to fail on any widening.
  assert_line "|nodes,pods,events|get,list,watch"
  assert_line "lok8s.dev|clusterinventories|get,list,watch"
  assert_line "lok8s.dev|clusterinventories/status|patch"
  # v0.1.0: the CronJob's certificate-expiry and component-health reads,
  # ported. CSRs are list-only; the health URLs are get-only (non-resource
  # URLs have no apiGroups/resources, so the column carries the URL list).
  assert_line "certificates.k8s.io|certificatesigningrequests|list"
  assert_line "|/readyz,/version|get"
  assert_line "|secrets|get"
  # Exactly six rules. A seventh line is a permission a customer did not agree to.
  [ "${#lines[@]}" -eq 6 ]
}

@test "rbac: the machines DELETE verb exists ONLY in the managed overlay" {
  run bash -c "command kustomize build '${LIVE_DIR}/managed' | command yq -r 'select(.kind==\"ClusterRole\" or .kind==\"Role\") | .rules[] | ((.apiGroups // [])|join(\",\")) + \"|\" + ((.resources // .nonResourceURLs)|join(\",\")) + \"|\" + (.verbs|join(\",\"))' | command grep -v '^---$'"
  assert_success
  # Self-healing deletes a Machine and lets the MachineSet rebuild it — the
  # sharpest permission the agent holds, and the reason the overlay is opt-in.
  # Every read of the agent is one list per pass, so no get and no watch.
  assert_line "cluster.k8s.io|machines|list,delete"
  # Scaling + worker upgrades: patch, never update/create/delete.
  assert_line "cluster.k8s.io|machinedeployments|list,patch"
  # The eviction unwedge: pods DELETE only, no other verb, no other resource.
  assert_line "|pods|delete"
  # The acting Lease: create, then get and update on its one name (the test
  # below pins the name).
  assert_line "coordination.k8s.io|leases|create"
  assert_line "coordination.k8s.io|leases|get,update"
  # The six base rules ride along unchanged; the overlay adds exactly five.
  [ "${#lines[@]}" -eq 11 ]
}

@test "rbac: the Lease get and update are scoped by name to the acting Lease" {
  run bash -c "command kustomize build '${LIVE_DIR}/managed' | command yq -r 'select(.kind==\"Role\" and .metadata.name==\"kubehz-live-agent-lease\") | .metadata.namespace + \"|\" + ([.rules[] | ((.resourceNames // [\"*\"]) | join(\",\")) + \"=\" + (.verbs | join(\",\"))] | join(\";\"))'"
  assert_success
  # create cannot carry a name (the object does not exist yet); get and update
  # name the one Lease, so no other Lease in kubehz-system is in reach.
  assert_output "kubehz-system|*=create;kubehz-live-agent=get,update"
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

@test "rbac: the MANAGED overlay adds exactly three bindings, all to its own Roles" {
  run _bindings "${LIVE_DIR}/managed"
  assert_success
  assert_line "ClusterRoleBinding|-/kubehz-live-agent|ClusterRole/kubehz-live-agent|ServiceAccount/kubehz-system/kubehz-live-agent"
  assert_line "RoleBinding|kubehz-system/kubehz-live-agent-secret|Role/kubehz-live-agent-secret|ServiceAccount/kubehz-system/kubehz-live-agent"
  # The acting RBAC. The machinedeployments RoleBinding is namespaced to
  # kube-system — cluster-wide is a different permission entirely.
  assert_line "RoleBinding|kube-system/kubehz-live-agent-machinedeployments|Role/kubehz-live-agent-machinedeployments|ServiceAccount/kubehz-system/kubehz-live-agent"
  assert_line "ClusterRoleBinding|-/kubehz-live-agent-eviction-unwedge|ClusterRole/kubehz-live-agent-eviction-unwedge|ServiceAccount/kubehz-system/kubehz-live-agent"
  # The acting Lease, in the agent's own namespace.
  assert_line "RoleBinding|kubehz-system/kubehz-live-agent-lease|Role/kubehz-live-agent-lease|ServiceAccount/kubehz-system/kubehz-live-agent"
  [ "${#lines[@]}" -eq 5 ]
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
  for pair in base:2 managed:5; do
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

# The exact digest, pinned: the kubehz-agent v0.2.0 release (digest.txt at
# https://github.com/kernpilot/kubehz-agent/releases/tag/v0.2.0). The RBAC
# above is pinned rule for rule; the image the RBAC was reviewed against is
# pinned the same way, so a digest bump and its RBAC re-read land together.
# A re-pin updates this line in the same change as deployment.yaml.
PINNED_AGENT_IMAGE='ghcr.io/kernpilot/kubehz-agent@sha256:54bba61ef3e7900f8cd7af8439e3a2164b7968024c336a9d836c269598b24080'

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
  assert_output "${PINNED_AGENT_IMAGE}"
}

@test "image: the managed overlay runs the same pinned digest as the base" {
  # The managed overlay renders the base, but an `images:` entry in its own
  # kustomization.yaml would replace the digest. The tier that acts must run
  # the image that its RBAC was reviewed against.
  run bash -c "command kustomize build '${LIVE_DIR}/managed' | command yq -r 'select(.kind==\"Deployment\") | .spec.template.spec.containers[0].image'"
  assert_success
  assert_output "${PINNED_AGENT_IMAGE}"
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

    # The rest: config source, the pod name for the Lease holder, image, pull
    # policy, resource envelope.
    assert_line "containers.0.envFrom.0.configMapRef.name=kubehz-live-agent-config"
    assert_line "containers.0.env.0.name=KUBEHZ_POD_NAME"
    assert_line "containers.0.env.0.valueFrom.fieldRef.fieldPath=metadata.name"
    assert_line --regexp '^containers\.0\.image=ghcr\.io/kernpilot/kubehz-agent@sha256:[0-9a-f]{64}$'
    assert_line "containers.0.imagePullPolicy=IfNotPresent"
    assert_line "containers.0.resources.requests.cpu=25m"
    assert_line "containers.0.resources.requests.memory=64Mi"
    assert_line "containers.0.resources.limits.cpu=200m"
    assert_line "containers.0.resources.limits.memory=256Mi"

    # Scheduling: a control-plane-only cluster runs the agent on its
    # control plane; with workers present the agent prefers one of them.
    assert_line "tolerations.0.key=node-role.kubernetes.io/control-plane"
    assert_line "tolerations.0.operator=Exists"
    assert_line "tolerations.0.effect=NoSchedule"
    assert_line "tolerations.1.key=node-role.kubernetes.io/master"
    assert_line "tolerations.1.operator=Exists"
    assert_line "tolerations.1.effect=NoSchedule"
    assert_line "affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution.0.weight=100"
    assert_line "affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution.0.preference.matchExpressions.0.key=node-role.kubernetes.io/control-plane"
    assert_line "affinity.nodeAffinity.preferredDuringSchedulingIgnoredDuringExecution.0.preference.matchExpressions.0.operator=DoesNotExist"

    # THE GUARD. Thirty-nine leaves, and the thirty-nine above are all of
    # them. A second container, a second volume, an added env var, `hostPID:
    # true`, `hostNetwork: true`, `hostIPC: true`, `privileged: true`, a
    # hostPath — every one of those is a fortieth line, and fails here
    # without anyone having had to predict which one it would be.
    [ "${#lines[@]}" -eq 39 ] || fail \
      "${overlay}: the pod spec has ${#lines[@]} leaf fields, expected 39 — a field was added or removed:"$'\n'"${output}"
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

# ── stage_bind_secret ────────────────────────────────────

BIND_SECRET_VALUE="9f1c2b3a4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d5e6f70"
# The khzb_ form: khzb_ and 64 lowercase hex characters, 69 bytes. The hex
# holds digits and letters.
BIND_SECRET_PREFIXED="khzb_$(printf '0123456789abcdef%.0s' 1 2 3 4)"

# _bind_manifest <file>: what the stub dry-run create prints for the bytes of
# <file>, so the value can be followed to the stdin of the apply.
_bind_manifest() {
  printf 'kind: Secret\nmetadata:\n  name: kubehz-agent-bind\n  namespace: kubehz-system\ndata:\n  bind-secret: %s\n' "$(base64 -w0 < "${1}")"
}

# _stub_signal_target sets _stub_target to the shell that runs deploy_apply,
# for a kubectl stub that sends it a signal. A kubectl call through
# kapply::run_private runs in the command substitution of the private
# capture, so that shell is the parent of the stub's shell.
_stub_signal_target() {
  _stub_target="${BASHPID}"
  [[ " ${FUNCNAME[*]} " != *" kapply::run_private "* ]] || read -r _ _ _ _stub_target _ < "/proc/${BASHPID}/stat"
}

# _stub_kubectl_bind [fail]: log every call. The dry-run create saves the
# bytes of its stdin (STUB_CREATE_STDIN, and each call appends them to
# STUB_CREATE_STDINS) and prints _bind_manifest of them; the server-side
# apply saves its stdin (STUB_SSA_STDIN, or STUB_DRY_STDIN for the server
# dry run of the preflight). A call whose argv contains <fail> exits 1; with
# STUB_FAIL_AFTER_CRON set, only after the CronJob agent apply (so the
# preflight of the operator direction passes). With STUB_DRYRUN_FAIL set,
# the server dry run exits 1. With STUB_SWAP set, the namespace apply writes
# that value into the bind file: a change after the check. The calls around
# the stage have their own switches: STUB_JOBS_DELETE_FAIL fails the Job
# delete, STUB_POLL_FAIL fails each pod poll, STUB_PODS_STAY makes each pod
# poll answer a pod that has not ended (STUB_PODS_LINES, when it is set, is
# the answer), and STUB_RESUME_FAIL fails the patch that ends the suspend.
# With STUB_TERM_ON_NAMESPACE set, the namespace apply saves what <workdir>
# holds (STUB_TERM_SEEN) and sends TERM to the shell that runs deploy_apply.
# With STUB_SIGNAL_AT set to an argv, that call sends the signal STUB_SIGNAL
# to that shell, from the CronJob agent apply on (so not in the preflight),
# and runs nothing else. The shell is _stub_signal_target.
_stub_kubectl_bind() {
  export STUB_KUBECTL_LOG="${BATS_TEST_TMPDIR}/kubectl.log"
  export STUB_KUBECTL_FAIL="${1:-}"
  export STUB_SSA_STDIN="${BATS_TEST_TMPDIR}/ssa.stdin"
  export STUB_DRY_STDIN="${BATS_TEST_TMPDIR}/dry.stdin"
  export STUB_CREATE_STDIN="${BATS_TEST_TMPDIR}/create.stdin"
  export STUB_CREATE_STDINS="${BATS_TEST_TMPDIR}/create.stdins"
  export STUB_TERM_SEEN="${BATS_TEST_TMPDIR}/term.seen"
  export AGENT_JOBS_DELETE AGENT_JOBS_POLL AGENT_RESUME
  : > "${STUB_KUBECTL_LOG}"
  rm -f "${STUB_CREATE_STDIN}" "${STUB_CREATE_STDINS}" "${STUB_SSA_STDIN}" "${STUB_DRY_STDIN}" "${STUB_TERM_SEEN}"
  kubectl() {
    if [[ -n "${STUB_SIGNAL_AT:-}" && "$*" == "${STUB_SIGNAL_AT}" ]] \
      && { [[ "$*" == "apply -k "*"/agent" ]] || grep -qE '^apply -k .*/agent$' "${STUB_KUBECTL_LOG}"; }; then
      echo "$*" >> "${STUB_KUBECTL_LOG}"
      _stub_signal_target
      kill -"${STUB_SIGNAL}" "${_stub_target}"
      return 0
    fi
    case "$*" in
      "${AGENT_JOBS_POLL}")
        echo "$*" >> "${STUB_KUBECTL_LOG}"
        if [[ -n "${STUB_POLL_FAIL:-}" ]]; then
          echo 'Error from server (Forbidden): pods is forbidden' >&2
          return 1
        fi
        [[ -z "${STUB_PODS_STAY:-}" ]] || printf '%s\n' "${STUB_PODS_LINES:-pod/kubehz-heartbeat-29000000-x2k4q}"
        return 0 ;;
      *"get pods"*) printf ''; return 0 ;;
      "${AGENT_JOBS_DELETE}")
        echo "$*" >> "${STUB_KUBECTL_LOG}"
        [[ -z "${STUB_JOBS_DELETE_FAIL:-}" ]] || { echo 'Error from server (Forbidden): jobs.batch is forbidden' >&2; return 1; }
        return 0 ;;
      "${AGENT_RESUME}")
        echo "$*" >> "${STUB_KUBECTL_LOG}"
        [[ -z "${STUB_RESUME_FAIL:-}" ]] || { echo 'Error from server (Forbidden): cronjobs.batch is forbidden' >&2; return 1; }
        return 0 ;;
      "apply -f "*"/agent/namespace.yaml")
        if [[ -n "${STUB_TERM_ON_NAMESPACE:-}" ]]; then
          echo "$*" >> "${STUB_KUBECTL_LOG}"
          ls -A -- "${3%/agent/namespace.yaml}" > "${STUB_TERM_SEEN}"
          _stub_signal_target
          kill -TERM "${_stub_target}"
          return 0
        fi ;;
    esac
    local cron_seen=1
    [[ -z "${STUB_FAIL_AFTER_CRON:-}" ]] || grep -qE '^apply -k .*/agent$' "${STUB_KUBECTL_LOG}" || cron_seen=0
    echo "$*" >> "${STUB_KUBECTL_LOG}"
    if [[ -n "${STUB_DRYRUN_FAIL:-}" && "$*" == *"--dry-run=server"* ]]; then
      return 1
    fi
    if [[ -n "${STUB_KUBECTL_FAIL}" && "$*" == *"${STUB_KUBECTL_FAIL}"* ]] && (( cron_seen )); then
      return 1
    fi
    case "$*" in
      "apply -f "*"/agent/namespace.yaml")
        [[ -z "${STUB_SWAP:-}" ]] || printf %s "${STUB_SWAP}" > "${PATH_CLUSTERS}/acme.example.com/.kubehz-bind" ;;
      *"create secret generic kubehz-agent-bind"*)
        cat > "${STUB_CREATE_STDIN}"
        cat "${STUB_CREATE_STDIN}" >> "${STUB_CREATE_STDINS}"
        _bind_manifest "${STUB_CREATE_STDIN}" ;;
      *"apply --server-side"*"--dry-run=server"*) cat > "${STUB_DRY_STDIN}" ;;
      *"apply --server-side"*) cat > "${STUB_SSA_STDIN}" ;;
      # The identity Secret holds an agent-token: no bootstrap.
      *"get secret kubehz-agent -o go-template="*) printf 'agent-token' ;;
    esac
    return 0
  }
  export -f kubectl _bind_manifest _stub_signal_target
}

# _write_bind_secret [value]: the stored bind secret (default BIND_SECRET_VALUE).
_write_bind_secret() {
  mkdir -p "${PATH_CLUSTERS}/acme.example.com"
  printf %s "${1:-${BIND_SECRET_VALUE}}" > "${PATH_CLUSTERS}/acme.example.com/.kubehz-bind"
}

# _assert_bind_staged_after <work> <n> <value>: line <n> applies the CronJob
# agent (suspended), lines n+1 and n+2 delete its Jobs and poll for their
# pods, lines n+3 to n+5 stage the Secret (namespace, dry-run create from
# stdin, server-side apply of the dry-run output on stdin), and line n+6
# ends the suspend. The create read <value> byte for byte. No copy of the
# value is left in <work>.
_assert_bind_staged_after() {
  local work="${1}" n="${2}" value="${3}" expect="${BATS_TEST_TMPDIR}/bind.expect"
  run sed -n "${n}p" "${STUB_KUBECTL_LOG}"
  assert_output "apply -k ${work}/agent"
  run sed -n "$(( n + 1 ))p" "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_JOBS_DELETE}"
  run sed -n "$(( n + 2 ))p" "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_JOBS_POLL}"
  run sed -n "$(( n + 3 ))p" "${STUB_KUBECTL_LOG}"
  assert_output "apply -f ${work}/agent/namespace.yaml"
  run sed -n "$(( n + 4 ))p" "${STUB_KUBECTL_LOG}"
  assert_output -- "-n kubehz-system create secret generic kubehz-agent-bind --from-file=bind-secret=/dev/stdin --dry-run=client -o yaml"
  run sed -n "$(( n + 5 ))p" "${STUB_KUBECTL_LOG}"
  assert_output "apply --server-side --force-conflicts -f -"
  run sed -n "$(( n + 6 ))p" "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_RESUME}"
  # The dry-run create read the value on stdin, byte for byte.
  printf %s "${value}" > "${expect}"
  run cmp "${expect}" "${STUB_CREATE_STDIN}"
  assert_success
  # The apply reads what the dry run printed, on stdin.
  run cat "${STUB_SSA_STDIN}"
  assert_output "$(_bind_manifest "${expect}")"
  # The value never reaches the command line. An upsert in place: no delete
  # leaves a running CronJob without the Secret.
  run grep -c -e "from-literal" -e "${value}" -e "delete secret kubehz-agent-bind" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  run ls -A "${work}"
  assert_output "agent
live-agent"
}

# _assert_bind_proved_first <work> <value>: lines 1 to 3 are the preflight of
# the stage of the operator direction (namespace, dry-run create from stdin,
# server-side apply with --dry-run=server of the dry-run output on stdin),
# and line 4 applies the CronJob agent. The preflight read <value> byte for
# byte.
_assert_bind_proved_first() {
  local work="${1}" value="${2}" expect="${BATS_TEST_TMPDIR}/proof.expect"
  run sed -n '1p' "${STUB_KUBECTL_LOG}"
  assert_output "apply -f ${work}/agent/namespace.yaml"
  run sed -n '2p' "${STUB_KUBECTL_LOG}"
  assert_output -- "-n kubehz-system create secret generic kubehz-agent-bind --from-file=bind-secret=/dev/stdin --dry-run=client -o yaml"
  run sed -n '3p' "${STUB_KUBECTL_LOG}"
  assert_output "apply --server-side --force-conflicts --dry-run=server -f -"
  run sed -n '4p' "${STUB_KUBECTL_LOG}"
  assert_output "apply -k ${work}/agent"
  printf %s "${value}" > "${expect}"
  run cat "${STUB_DRY_STDIN}"
  assert_output "$(_bind_manifest "${expect}")"
}

@test "apply order (to operator): the stage has a preflight (a server dry run) BEFORE the CronJob agent, then is staged while the CronJob is suspended and BEFORE the identity bootstrap" {
  _source_deploy
  _stub_kubectl_bind
  local work="${BATS_TEST_TMPDIR}/bind1"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  _write_bind_secret

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_success
  refute_output --partial "no bind secret"
  _assert_bind_proved_first "${work}" "${BIND_SECRET_VALUE}"
  _assert_bind_staged_after "${work}" 4 "${BIND_SECRET_VALUE}"
  # The preflight and the stage read the same bytes.
  printf '%s%s' "${BIND_SECRET_VALUE}" "${BIND_SECRET_VALUE}" > "${BATS_TEST_TMPDIR}/twice.expect"
  run cmp "${BATS_TEST_TMPDIR}/twice.expect" "${STUB_CREATE_STDINS}"
  assert_success
  run sed -n '11p' "${STUB_KUBECTL_LOG}"
  assert_output --partial "get secret kubehz-agent "
}

@test "apply order (to operator): a failed preflight of the stage stops BEFORE the CronJob agent and stages nothing" {
  _source_deploy
  _write_bind_secret
  local spec fail changed work n=0
  for spec in "dryrun The deploy applied only the namespace kubehz-system." \
              "--dry-run=client The deploy applied only the namespace kubehz-system." \
              "namespace.yaml The namespace apply can have changed the namespace kubehz-system. The deploy applied no other object."; do
    read -r fail changed <<< "${spec}"
    n=$(( n + 1 ))
    if [[ "${fail}" == dryrun ]]; then
      export STUB_DRYRUN_FAIL=1
      _stub_kubectl_bind
    else
      unset STUB_DRYRUN_FAIL
      _stub_kubectl_bind "${fail}"
    fi
    work="${BATS_TEST_TMPDIR}/proof${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

    run kubehz::deploy_apply "${work}" acme.example.com operator managed
    assert_failure
    assert_output --partial "kubehz: the preflight of the bind secret stage failed (a server-side dry run of Secret kubehz-system/kubehz-agent-bind with the value from clusters/acme.example.com/.kubehz-bind), so the deploy stopped before the CronJob agent apply. ${changed} The kubeconfig needs patch on Secrets in kubehz-system, and create when the Secret is new. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. To deploy without the secret, remove the file and the Secret (kubectl -n kubehz-system delete secret kubehz-agent-bind --ignore-not-found), then claim the new row with the code from 'lo kubehz claim-code'."
    refute_output --partial "Nothing else was changed."
    refute_output --partial "Only the namespace kubehz-system was applied."
    refute_output --partial "proof"
    refute_output --partial "${BIND_SECRET_VALUE}"
    if [[ "${fail}" == namespace.yaml ]]; then
      # The namespace apply failed: no dry-run create and no server dry run.
      run grep -c -e "--dry-run" "${STUB_KUBECTL_LOG}"
      assert_output "0"
    fi
    # No agent changed, and no Secret was staged.
    run grep -c -e "apply -k " -e "apply --server-side --force-conflicts -f -" -e "get secret kubehz-agent " \
      -e "create job" -e "rollout status" -e "delete job" -e "patch cronjob" "${STUB_KUBECTL_LOG}"
    assert_output "0"
    run ls -A "${work}"
    assert_output "agent
live-agent"
  done
  unset STUB_DRYRUN_FAIL
}

@test "apply order (to cronjob): the bind secret is staged right AFTER the live agent goes and the CronJob agent returns" {
  _source_deploy
  _stub_kubectl_bind
  local work="${BATS_TEST_TMPDIR}/bind2"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered
  _write_bind_secret

  run kubehz::deploy_apply "${work}" acme.example.com cronjob registered
  assert_success
  run head -1 "${STUB_KUBECTL_LOG}"
  assert_output "delete -k ${work}/live-agent/managed --ignore-not-found=true"
  _assert_bind_staged_after "${work}" 3 "${BIND_SECRET_VALUE}"
  # The patch that ends the suspend is the last call.
  run wc -l < "${STUB_KUBECTL_LOG}"
  assert_output "9"
}

@test "apply order (both directions): a khzb_ bind secret is staged as it is, right AFTER the CronJob agent" {
  _source_deploy
  local mode owner access cron work n=0 bind_file="${PATH_CLUSTERS}/acme.example.com/.kubehz-bind"
  # The operator direction runs the preflight of the stage in lines 1 to 3
  # first.
  for mode in "operator managed 4" "cronjob registered 3"; do
    read -r owner access cron <<< "${mode}"
    n=$(( n + 1 ))
    _stub_kubectl_bind
    work="${BATS_TEST_TMPDIR}/prefixed${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"
    _write_bind_secret "${BIND_SECRET_PREFIXED}"

    run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
    assert_success
    refute_output --partial "no bind secret"
    refute_output --partial "${BIND_SECRET_PREFIXED}"
    _assert_bind_staged_after "${work}" "${cron}" "${BIND_SECRET_PREFIXED}"
    # The file the Secret comes from keeps its 69 bytes.
    run cat "${bind_file}"
    assert_output "${BIND_SECRET_PREFIXED}"
    run wc -c < "${bind_file}"
    assert_output "69"
  done
}

@test "stage_bind_secret: the deploy stages the bytes it checked, even when the file changes after the check" {
  _source_deploy
  local mode owner access cron value work n=0 bind_file="${PATH_CLUSTERS}/acme.example.com/.kubehz-bind"
  export STUB_SWAP="khzb_$(printf '0f%.0s' $(seq 1 32))"
  # The operator direction runs the preflight of the stage in lines 1 to 3
  # first.
  for mode in "operator managed 4" "cronjob registered 3"; do
    read -r owner access cron <<< "${mode}"
    for value in "${BIND_SECRET_VALUE}" "${BIND_SECRET_PREFIXED}"; do
      n=$(( n + 1 ))
      _stub_kubectl_bind
      work="${BATS_TEST_TMPDIR}/swap${n}"
      mkdir -p "${work}"
      kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"
      _write_bind_secret "${value}"

      run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
      assert_success
      # The stub changed the file between the check and the create.
      run cat "${bind_file}"
      assert_output "${STUB_SWAP}"
      _assert_bind_staged_after "${work}" "${cron}" "${value}"
    done
  done
  unset STUB_SWAP
}

@test "stage_bind_secret: a failed stage with a bind secret on disk stops with the CronJob suspended, in both directions" {
  _source_deploy
  _write_bind_secret
  local mode owner access step work n=0
  local want="kubehz: could not stage the bind secret from clusters/acme.example.com/.kubehz-bind into Secret kubehz-system/kubehz-agent-bind. The stage can have applied the namespace or the Secret. The CronJob agent stays suspended, so heartbeats can stop until the deploy runs again. The kubeconfig needs patch on Secrets in kubehz-system, and create when the Secret is new. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. If you deploy without the secret, the agent can register a separate pending row if it has no binding and no staged value yet. To deploy without the secret, remove the file and the Secret (kubectl -n kubehz-system delete secret kubehz-agent-bind --ignore-not-found), then claim the new row with the code from 'lo kubehz claim-code'."
  # The stub fails the step only after the CronJob agent apply, so the
  # preflight of the operator direction passes and the real stage fails.
  export STUB_FAIL_AFTER_CRON=1
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    for step in "namespace.yaml" "--dry-run=client" "apply --server-side"; do
      n=$(( n + 1 ))
      _stub_kubectl_bind "${step}"
      work="${BATS_TEST_TMPDIR}/fail${n}"
      mkdir -p "${work}"
      kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

      run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
      assert_failure
      assert_output --partial "${want}"
      refute_output --partial "reporting NOTHING"
      refute_output --partial "The deploy applied the CronJob agent and then stopped."
      refute_output --partial "pending-pool"
      # The CronJob agent apply is the last call that changes an agent: the
      # Job delete and the pod poll follow it, then the failed stage. No
      # resume, no live agent, no bootstrap Job, no identity read.
      run grep -c -e "apply -k ${work}/agent\$" "${STUB_KUBECTL_LOG}"
      assert_output "1"
      run grep -c -F -x -e "${AGENT_JOBS_DELETE}" -e "${AGENT_JOBS_POLL}" "${STUB_KUBECTL_LOG}"
      assert_output "2"
      run grep -c -e "patch cronjob" -e "apply -k ${work}/live-agent" -e "create job" -e "get secret kubehz-agent " -e "rollout status" "${STUB_KUBECTL_LOG}"
      assert_output "0"
      run ls -A "${work}"
      assert_output "agent
live-agent"
    done
  done
  unset STUB_FAIL_AFTER_CRON
}

@test "stage_bind_secret: no bind secret on disk warns, touches no Secret, and still deletes the Jobs, waits and ends the suspend" {
  _source_deploy
  _stub_kubectl_log
  local work="${BATS_TEST_TMPDIR}/bind3"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed

  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_success
  assert_output --partial "[warn] kubehz: no bind secret at clusters/acme.example.com/.kubehz-bind. If 'lo kubehz register' ran in another checkout, the agent can register a separate pending row if it has no binding and no staged value yet. The announced row then stays as it is and gets no heartbeats. Claim the new row with the code from 'lo kubehz claim-code'."
  run grep -c -e "kubehz-agent-bind" -e "namespace.yaml" "${STUB_KUBECTL_LOG}"
  assert_output "0"
  run head -1 "${STUB_KUBECTL_LOG}"
  assert_output --partial "apply -k ${work}/agent"
  run sed -n '2,4p' "${STUB_KUBECTL_LOG}"
  assert_output -- "${AGENT_JOBS_DELETE}
${AGENT_JOBS_POLL}
${AGENT_RESUME}"
}

@test "stage_bind_secret: a file that is not a bind secret is never staged, and the deploy says so" {
  _source_deploy
  local fixture work n=0 bind_file="${PATH_CLUSTERS}/acme.example.com/.kubehz-bind"
  local v="${BIND_SECRET_VALUE}" p="${BIND_SECRET_PREFIXED}"
  # nl69 is the trap of a check per line: 64 hex, a newline and 4 bytes make
  # 69 bytes, and the first line matches. nul69 is the same trap with a NUL:
  # a grep in binary mode can read the NUL as a line end.
  for fixture in directory newline upper short unreadable cr crlf prefixed-newline prefixed-cr prefixed-crlf \
    nl64 nl69 nl69-first prefixed-upper prefixed-upper-hex khza long65 prefixed-short68 prefixed-long70 \
    nul69 nul69-first suffix hex69 nothex prefixed-nothex oversized; do
    [[ "${fixture}" != unreadable || "$(id -u)" != 0 ]] || continue  # root reads a 0000 file
    n=$(( n + 1 ))
    _stub_kubectl_log
    rm -rf "${bind_file}"
    mkdir -p "${PATH_CLUSTERS}/acme.example.com"
    case "${fixture}" in
      directory) mkdir -p "${bind_file}" ;;
      newline) printf '%s\n' "${v}" > "${bind_file}" ;;
      upper) printf %s "${v^^}" > "${bind_file}" ;;
      short) printf %s "${v:1}" > "${bind_file}" ;;
      unreadable) printf %s "${v}" > "${bind_file}"; chmod 000 "${bind_file}" ;;
      cr) printf '%s\r' "${v}" > "${bind_file}" ;;
      crlf) printf '%s\r\n' "${v}" > "${bind_file}" ;;
      prefixed-newline) printf '%s\n' "${p}" > "${bind_file}" ;;
      prefixed-cr) printf '%s\r' "${p}" > "${bind_file}" ;;
      prefixed-crlf) printf '%s\r\n' "${p}" > "${bind_file}" ;;
      nl64) printf '%s\n%s' "${v:0:32}" "${v:33}" > "${bind_file}" ;;
      nl69) printf '%s\nabcd' "${v}" > "${bind_file}" ;;
      nl69-first) printf 'abcd\n%s' "${v}" > "${bind_file}" ;;
      prefixed-upper) printf %s "KHZB_${v}" > "${bind_file}" ;;
      prefixed-upper-hex) printf %s "khzb_${v^^}" > "${bind_file}" ;;
      khza) printf %s "khza_${v}" > "${bind_file}" ;;
      long65) printf %s "${v}a" > "${bind_file}" ;;
      prefixed-short68) printf %s "khzb_${v:1}" > "${bind_file}" ;;
      prefixed-long70) printf %s "khzb_${v}a" > "${bind_file}" ;;
      nul69) printf '%s\0abcd' "${v}" > "${bind_file}" ;;
      nul69-first) printf 'abcd\0%s' "${v}" > "${bind_file}" ;;
      suffix) printf %s "${v}khzb_" > "${bind_file}" ;;
      hex69) printf %s "${v}${v:0:5}" > "${bind_file}" ;;
      # 64 or 69 bytes with one letter that is not hex.
      nothex) printf %s "${v:0:63}g" > "${bind_file}" ;;
      prefixed-nothex) printf %s "khzb_${v:0:63}g" > "${bind_file}" ;;
      # 64 kB that start with a valid value: the copy holds 70 bytes.
      oversized) { printf %s "${v}"; head -c 65536 /dev/zero | tr '\0' a; } > "${bind_file}" ;;
    esac
    work="${BATS_TEST_TMPDIR}/malformed${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" cronjob registered

    run kubehz::deploy_apply "${work}" acme.example.com cronjob registered
    assert_success
    assert_output --partial "[warn] kubehz: clusters/acme.example.com/.kubehz-bind holds no bind secret (a readable file of 64 lowercase hex characters, or khzb_ and 64 lowercase hex characters). The deploy does not stage it, and a Secret kubehz-agent-bind from an earlier deploy stays as it is. Then the agent can register a separate pending row if it has no binding and no staged value yet. The announced row then stays as it is and gets no heartbeats. Claim the new row with the code from 'lo kubehz claim-code'."
    assert_output --partial "If the file holds a newer form of bind secret, upgrade lok8s (the lo binary and, for a bash install, the synced tree), then run 'lo kubehz deploy' again."
    run grep -c "kubehz-agent-bind" "${STUB_KUBECTL_LOG}"
    assert_output "0"
    run head -1 "${STUB_KUBECTL_LOG}"
    assert_output --partial "delete -k ${work}/live-agent/managed"
    # The check left no copy of the file in the render directory.
    run ls -A "${work}"
    assert_output "agent
live-agent"
  done
  chmod -R u+rwx "${PATH_CLUSTERS}" 2>/dev/null || true
}

@test "deploy_apply: a failed agent step says what it can have changed, and stages nothing after it" {
  _source_deploy
  local spec owner access fail want on_disk work n=0
  local cron="kubehz: could not apply the CronJob agent (identity bootstrap). The apply can have applied part of the agent objects; an applied CronJob is suspended, so heartbeats can stop until the deploy runs again. Fix the cause, then run 'lo kubehz deploy' again."
  local remove="kubehz: could not remove the live agent. The delete can have removed part of the live agent objects. The deploy did not apply the CronJob agent, so heartbeats can stop until the deploy runs again."
  local sweep="kubehz: could not sweep live-agent Deployments by label. The live agent delete before it passed, and the sweep can have removed some Deployments. The deploy did not apply the CronJob agent, so heartbeats can stop until the deploy runs again."
  local back="kubehz: could not apply the CronJob agent. The live agent is gone. The apply can have applied part of the agent objects; an applied CronJob is suspended, so heartbeats can stop until the deploy runs again. Fix the cause, then run 'lo kubehz deploy' again."
  local live="kubehz: could not apply the live agent. The apply can have applied part of the live agent objects. The CronJob does not beat (KUBEHZ_HEARTBEAT_OWNER=operator), so heartbeats can stop until the deploy runs again. Run 'lo kubehz deploy' again, or set spec.kubehz.agent back to cronjob and run it again."
  # The last word of each row names the variable with the expected error.
  for spec in "operator managed apply|-k cron" \
              "cronjob registered delete|-k remove" \
              "cronjob registered delete|deployment|-l sweep" \
              "cronjob registered apply|-k back" \
              "operator managed live-agent/managed live"; do
    read -r owner access fail want <<< "${spec}"
    fail="${fail//|/ }"
    for on_disk in 1 0; do
      n=$(( n + 1 ))
      rm -f "${PATH_CLUSTERS}/acme.example.com/.kubehz-bind"
      if (( on_disk )); then _write_bind_secret; fi
      _stub_kubectl_bind "${fail}"
      work="${BATS_TEST_TMPDIR}/changed${n}"
      mkdir -p "${work}"
      kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

      run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
      assert_failure
      assert_output --partial "${!want}"
      refute_output --partial "othing else was changed"
      refute_output --partial "Only the namespace kubehz-system was applied."
      refute_output --partial "reporting NOTHING"
      refute_output --partial "still one producer"
      if [[ "${want}" == live ]]; then
        # The live agent apply comes after the stage and the end of the
        # suspend.
        run grep -c -F -x -e "${AGENT_RESUME}" "${STUB_KUBECTL_LOG}"
        assert_output "1"
        run grep -c -e "apply --server-side --force-conflicts -f -" "${STUB_KUBECTL_LOG}"
        assert_output "${on_disk}"
      else
        # No stage, no Job delete and no end of the suspend.
        run grep -c -e "apply --server-side --force-conflicts -f -" -e "delete job" -e "patch cronjob" "${STUB_KUBECTL_LOG}"
        assert_output "0"
      fi
      if [[ "${owner}" == operator ]] && (( on_disk )); then
        # The preflight of the stage ran before the CronJob agent apply.
        run grep -c -e "--dry-run=server" "${STUB_KUBECTL_LOG}"
        assert_output "1"
      elif [[ "${want}" != live ]]; then
        run grep -c "kubehz-agent-bind" "${STUB_KUBECTL_LOG}"
        assert_output "0"
      fi
    done
  done
}

@test "deploy_print: the dry run names the suspend, the Job delete and the wait, the Secret, and the end of the suspend" {
  _source_deploy
  local mode owner access next work n=0
  local jobs="# The CronJob agent is applied suspended. The deploy then deletes its Jobs (-l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent) and waits until their pods have ended."
  local resume="# The deploy then sets spec.suspend to false on cronjob/kubehz-heartbeat."
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    n=$(( n + 1 ))
    work="${BATS_TEST_TMPDIR}/print${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"
    _write_bind_secret
    kubectl() { echo "kind: Rendered"; }
    export -f kubectl
    next="# The live agent is NOT deployed in cronjob mode; a previous install would be removed."
    [[ "${owner}" != operator ]] || next="# --- Live agent (kubehz-live-agent) — managed tier RBAC ---"

    run kubehz::deploy_print "${work}" acme.example.com "${owner}" "${access}"
    assert_success
    assert_line --index 0 "# --- CronJob agent (kubehz-heartbeat) — identity + enrollment; heartbeat owner: ${owner} ---"
    assert_line --index 1 "kind: Rendered"
    assert_line --index 2 -- "---"
    assert_line --index 3 "${jobs}"
    assert_line --index 4 -- "---"
    assert_line --index 5 "# --- Secret kubehz-agent-bind (the bind secret from clusters/acme.example.com/.kubehz-bind) — applied after the CronJob agent; the value is not printed ---"
    assert_line --index 6 -- "---"
    assert_line --index 7 "${resume}"
    assert_line --index 8 -- "---"
    assert_line --index 9 "${next}"
    refute_output --partial "${BIND_SECRET_VALUE}"
    run ls -A "${work}"
    assert_output "agent
live-agent"

    rm -f "${PATH_CLUSTERS}/acme.example.com/.kubehz-bind"
    run kubehz::deploy_print "${work}" acme.example.com "${owner}" "${access}"
    assert_success
    refute_output --partial "kubehz-agent-bind"
    assert_line --index 3 "${jobs}"
    assert_line --index 4 -- "---"
    assert_line --index 5 "${resume}"
  done
}

@test "cronjob: the CronJob agent ships suspended, and its jobTemplate labels the Jobs as the pods" {
  run command yq -r '.spec.suspend' "${CRONJOB}"
  assert_output "true"
  run command yq -r '.spec.jobTemplate.metadata.labels.app' "${CRONJOB}"
  assert_output "kubehz-heartbeat"
  run command yq -r '.spec.jobTemplate.metadata.labels["app.kubernetes.io/name"]' "${CRONJOB}"
  assert_output "kubehz-agent"
  run command yq -r '.spec.jobTemplate.spec.template.metadata.labels.app' "${CRONJOB}"
  assert_output "kubehz-heartbeat"
  run command yq -r '.spec.jobTemplate.spec.template.metadata.labels["app.kubernetes.io/name"]' "${CRONJOB}"
  assert_output "kubehz-agent"
}

@test "settle_cron_agent: Jobs that do not go stop the deploy before the stage, with the CronJob suspended, in both directions" {
  _source_deploy
  _write_bind_secret
  # After 100 sleeps the stub sends TERM: a wait without its bound ends with
  # 143 and does not hang.
  sleep() {
    _sleeps=$(( ${_sleeps:-0} + 1 ))
    (( _sleeps < 100 )) || kill -TERM "${BASHPID}"
  }
  export -f sleep
  local mode owner access switch want work n=0
  local suspended="${_AGENT_STAYS_SUSPENDED_TEXT}"
  local delete="kubehz: could not delete the Jobs of the CronJob agent (-l app=kubehz-heartbeat,app.kubernetes.io/name=kubehz-agent). The delete can have removed some of them. ${suspended} The kubeconfig needs list and delete on Jobs in kubehz-system. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again."
  local poll="kubehz: could not list the pods of the CronJob agent's Jobs in kubehz-system for 1s: Error from server (Forbidden): pods is forbidden. ${suspended} ${_AGENT_SUSPEND_SAFE_TEXT} The kubeconfig needs list on Pods in kubehz-system. ${_AGENT_PODS_STUCK_TEXT}"
  local stay="kubehz: pods of the CronJob agent's Jobs did not end within 1s after the deploy deleted the Jobs. The pods that have not ended: kubehz-heartbeat-29000000-x2k4q. ${suspended} ${_AGENT_SUSPEND_SAFE_TEXT} ${_AGENT_PODS_STUCK_TEXT}"
  KUBEHZ_AGENT_JOBS_DRAIN_SECONDS=1
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    for switch in "STUB_JOBS_DELETE_FAIL delete" "STUB_POLL_FAIL poll" "STUB_PODS_STAY stay"; do
      read -r switch want <<< "${switch}"
      n=$(( n + 1 ))
      unset STUB_JOBS_DELETE_FAIL STUB_POLL_FAIL STUB_PODS_STAY
      export "${switch}=1"
      _stub_kubectl_bind
      work="${BATS_TEST_TMPDIR}/jobs${n}"
      mkdir -p "${work}"
      kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

      run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
      assert_failure
      assert_output --partial "${!want}"
      # The pod wait names the force delete for a pod on a node that is
      # gone; the Job delete has no pod to name.
      if [[ "${want}" == delete ]]; then
        refute_output --partial "delete pod"
        refute_output --partial -- "--force"
      else
        assert_output --partial -- "--force --grace-period=0"
      fi
      # The Job delete ran; the poll ran once unless the delete failed. No
      # stage, no end of the suspend, no live agent.
      run grep -c -F -x -e "${AGENT_JOBS_DELETE}" "${STUB_KUBECTL_LOG}"
      assert_output "1"
      run grep -c -F -x -e "${AGENT_JOBS_POLL}" "${STUB_KUBECTL_LOG}"
      if [[ "${want}" == delete ]]; then assert_output "0"; else assert_output "1"; fi
      run grep -c -e "apply --server-side --force-conflicts -f -" -e "patch cronjob" -e "apply -k ${work}/live-agent" "${STUB_KUBECTL_LOG}"
      assert_output "0"
      run ls -A "${work}"
      assert_output "agent
live-agent"
    done
  done
  unset STUB_JOBS_DELETE_FAIL STUB_POLL_FAIL STUB_PODS_STAY
}

@test "wait_agent_jobs_gone: a pod that never ends stops the deploy at the end of the window, names the pods, and keeps the CronJob suspended, in both directions" {
  # A pod on a node that does not answer stays Terminating, in the phase
  # Running. KUBEHZ_AGENT_JOBS_DRAIN_SECONDS is the default 130: 26 polls,
  # each followed by a sleep of 5 s. Only names of the DNS shape are printed.
  _source_deploy
  _write_bind_secret
  export STUB_SLEEP_LOG="${BATS_TEST_TMPDIR}/sleep.log"
  # The sleep is instant, so only the bound of the wait ends it. After 1000
  # sleeps the stub sends TERM: a wait without its bound then ends with 143,
  # not 1, and does not hang.
  sleep() {
    echo "slept $*" >> "${STUB_SLEEP_LOG}"
    (( $(wc -l < "${STUB_SLEEP_LOG}") < 1000 )) || kill -TERM "${BASHPID}"
  }
  export -f sleep
  export STUB_PODS_STAY=1
  export STUB_PODS_LINES=$'pod/kubehz-heartbeat-29000000-x2k4q\npod/kubehz-heartbeat-29000005-b7m2p\npod/Bad\e[31mName'
  local want="[error] kubehz: pods of the CronJob agent's Jobs did not end within 130s after the deploy deleted the Jobs. The pods that have not ended: kubehz-heartbeat-29000000-x2k4q, kubehz-heartbeat-29000005-b7m2p. ${_AGENT_STAYS_SUSPENDED_TEXT} ${_AGENT_SUSPEND_SAFE_TEXT} ${_AGENT_PODS_STUCK_TEXT}"
  local mode owner access work n=0
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    n=$(( n + 1 ))
    : > "${STUB_SLEEP_LOG}"
    _stub_kubectl_bind
    work="${BATS_TEST_TMPDIR}/never${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

    run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
    assert_equal "${status}" 1
    assert_line "${want}"
    refute_output --partial "Bad"
    run grep -c -F -x -e "${AGENT_JOBS_POLL}" "${STUB_KUBECTL_LOG}"
    assert_output "26"
    run grep -c -F -x -e "slept 5" "${STUB_SLEEP_LOG}"
    assert_output "26"
    # spec.suspend stays true: no stage, no patch, no live agent.
    run grep -c -e "apply --server-side --force-conflicts -f -" -e "patch cronjob" -e "apply -k ${work}/live-agent" "${STUB_KUBECTL_LOG}"
    assert_output "0"
  done
  unset STUB_PODS_STAY STUB_PODS_LINES
}

@test "resume_cron_agent: a failed end of the suspend stops the deploy and says the CronJob can stay suspended" {
  _source_deploy
  _write_bind_secret
  local mode owner access work n=0
  local want="kubehz: could not set spec.suspend to false on cronjob/kubehz-heartbeat. The CronJob agent can stay suspended, so heartbeats can stop until the deploy runs again. The kubeconfig needs patch on CronJobs in kubehz-system. Fix the kubeconfig or the RBAC, then run 'lo kubehz deploy' again. The deploy sets it with: kubectl -n kubehz-system patch cronjob kubehz-heartbeat --type=merge -p '{\"spec\":{\"suspend\":false}}'."
  export STUB_RESUME_FAIL=1
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    n=$(( n + 1 ))
    _stub_kubectl_bind
    work="${BATS_TEST_TMPDIR}/resume${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

    run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
    assert_failure
    assert_output --partial "${want}"
    # The stage ran before the patch; nothing after it.
    run grep -c -e "apply --server-side --force-conflicts -f -" "${STUB_KUBECTL_LOG}"
    assert_output "1"
    run tail -1 "${STUB_KUBECTL_LOG}"
    assert_output -- "${AGENT_RESUME}"
  done
  unset STUB_RESUME_FAIL
}

@test "deploy_apply: a TERM during the stage removes the private copy, in both directions" {
  _source_deploy
  _write_bind_secret
  local mode owner access work n=0
  export STUB_TERM_ON_NAMESPACE=1
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    n=$(( n + 1 ))
    _stub_kubectl_bind
    work="${BATS_TEST_TMPDIR}/term${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

    run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
    assert_equal "${status}" 143
    # The copy was there when the TERM came, and it is gone after it.
    run cat "${STUB_TERM_SEEN}"
    assert_line "bind-secret"
    run ls -A "${work}"
    assert_output "agent
live-agent"
    # Nothing ran after the namespace apply that got the TERM.
    run tail -1 "${STUB_KUBECTL_LOG}"
    assert_output "apply -f ${work}/agent/namespace.yaml"
  done
  unset STUB_TERM_ON_NAMESPACE
}

@test "deploy_apply: an INT or a TERM from the CronJob agent apply to the end of the suspend says that the CronJob can stay suspended and ends with 130 or 143, in both directions" {
  # At each step: the CronJob agent apply, the Job delete, the sleep of the
  # pod wait, the namespace apply of the stage and the patch that ends the
  # suspend. One line in place of the error of the step, and no step after
  # it. A stub in the shell that runs deploy_apply sends the signal to that
  # shell, not to the test.
  _source_deploy
  _write_bind_secret
  export STUB_SLEEP_LOG="${BATS_TEST_TMPDIR}/sleep.log"
  sleep() {
    echo "slept $*" >> "${STUB_SLEEP_LOG}"
    [[ "${STUB_SIGNAL_AT:-}" != sleep ]] || kill -"${STUB_SIGNAL}" "${BASHPID}"
  }
  export -f sleep
  local mode owner access work at sig want_rc last n=0
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    for at in cron delete sleep stage resume; do
      for sig in INT TERM; do
        n=$(( n + 1 ))
        work="${BATS_TEST_TMPDIR}/signal${n}"
        mkdir -p "${work}"
        kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"
        unset STUB_PODS_STAY
        case "${at}" in
          cron) STUB_SIGNAL_AT="apply -k ${work}/agent" ;;
          delete) STUB_SIGNAL_AT="${AGENT_JOBS_DELETE}" ;;
          sleep) STUB_SIGNAL_AT=sleep; export STUB_PODS_STAY=1 ;;
          stage) STUB_SIGNAL_AT="apply -f ${work}/agent/namespace.yaml" ;;
          resume) STUB_SIGNAL_AT="${AGENT_RESUME}" ;;
        esac
        export STUB_SIGNAL_AT STUB_SIGNAL="${sig}"
        _stub_kubectl_bind
        : > "${STUB_SLEEP_LOG}"

        run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
        want_rc=130
        [[ "${sig}" == INT ]] || want_rc=143
        assert_equal "${status}" "${want_rc}"
        # The line, once, and no error of the step.
        assert_equal "$(grep -c -F -x -e "${_AGENT_INTERRUPTED_TEXT}" <<< "${output}")" 1
        refute_output --partial "could not"
        refute_output --partial "cannot list"
        # No step after the signal.
        last="${STUB_SIGNAL_AT}"
        [[ "${at}" != sleep ]] || last="${AGENT_JOBS_POLL}"
        run tail -1 "${STUB_KUBECTL_LOG}"
        assert_output -- "${last}"
        if [[ "${at}" != resume ]]; then
          run grep -c -e "patch cronjob" "${STUB_KUBECTL_LOG}"
          assert_output "0"
        fi
        run grep -c -e "apply -k ${work}/live-agent" -e "create job" "${STUB_KUBECTL_LOG}"
        assert_output "0"
        # The private copy is gone.
        run ls -A "${work}"
        assert_output "agent
live-agent"
      done
    done
  done
  unset STUB_SIGNAL_AT STUB_SIGNAL STUB_PODS_STAY
}

@test "deploy_agent: an INT or a TERM while the CronJob agent is suspended ends the deploy with 130 or 143, not 1, and prints no summary" {
  # The subshell of deploy_apply ends with the status of its trap, and
  # deploy_agent passes it on, as the Go binary exits 128+n.
  _source_deploy
  export LOK8S_KUBEHZ_HOSTING="self" LOK8S_KUBEHZ_ACCESS="registered"
  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.cloud" LOK8S_KUBEHZ_AGENT="cronjob"
  _write_bind_secret
  sleep() { kill -"${STUB_SIGNAL}" "${BASHPID}"; }
  export -f sleep
  export STUB_PODS_STAY=1
  local sig want_rc
  for sig in INT TERM; do
    export STUB_SIGNAL="${sig}"
    _stub_kubectl_bind
    run kubehz::deploy_agent acme.example.com
    want_rc=130
    [[ "${sig}" == INT ]] || want_rc=143
    assert_equal "${status}" "${want_rc}"
    assert_line "${_AGENT_INTERRUPTED_TEXT}"
    refute_output --partial "heartbeat CronJob deployed"
    run tail -1 "${STUB_KUBECTL_LOG}"
    assert_output -- "${AGENT_JOBS_POLL}"
  done
  unset STUB_SIGNAL STUB_PODS_STAY
}

# DEPLOY_CANARY stands for a value of the manifest that kubectl quotes on its
# stdout, DEPLOY_ERR_CANARY for one on its stderr. Both are made up.
DEPLOY_CANARY=zq7DeployCanary3vQs
DEPLOY_ERR_CANARY=zq7DeployStderrCanary8nTb

# _stub_kubectl_leaking [fail]: _stub_kubectl_bind [fail], and each kubectl apply also
# prints one object line and the canaries on its stdout and stderr.
_stub_kubectl_leaking() {
  _stub_kubectl_bind "$@"
  eval "$(declare -f kubectl | sed '1s/^kubectl /_kubectl_bound /')"
  kubectl() {
    local rc=0
    _kubectl_bound "$@" || rc=$?
    if [[ "${1-}" == apply ]]; then
      printf 'configmap/x configured\nInvalid value: "%s"\n' "${DEPLOY_CANARY}"
      printf 'Warning: the value "%s" is deprecated\n' "${DEPLOY_ERR_CANARY}" >&2
    fi
    return "${rc}"
  }
  export -f kubectl _kubectl_bound
  export DEPLOY_CANARY DEPLOY_ERR_CANARY
}

@test "deploy_apply: each kubectl apply keeps the kubectl output private, in both directions" {
  # The Go twin: TestDeployApplyKeepsTheKubectlOutputPrivate.
  _source_deploy
  _write_bind_secret
  local mode owner access work label n=0
  for mode in "operator managed" "cronjob registered"; do
    read -r owner access <<< "${mode}"
    n=$(( n + 1 ))
    _stub_kubectl_leaking
    work="${BATS_TEST_TMPDIR}/private${n}"
    mkdir -p "${work}"
    kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" "${owner}" "${access}"

    run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
    assert_success
    refute_output --partial "${DEPLOY_CANARY}"
    refute_output --partial "${DEPLOY_ERR_CANARY}"
    for label in "the namespace kubehz-system" "the bind secret" "the CronJob agent"; do
      assert_line "✓ ${label} · 1 resource"
    done
    [[ "${owner}" != operator ]] || assert_line "✓ the live agent · 1 resource"
    refute_output --partial 'kapply: kubectl apply failed'

    LO_SHOW_SENSITIVE_OUTPUT=1 run kubehz::deploy_apply "${work}" acme.example.com "${owner}" "${access}"
    assert_success
    assert_output --partial "${DEPLOY_CANARY}"
    assert_output --partial "${DEPLOY_ERR_CANARY}"
    assert_line '[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1: lo shows the kubectl output for the bind secret, which can quote a value of the manifest'
  done
}

@test "deploy_apply: a failed apply prints the fixed line of its label and the error of the step, never the kubectl output" {
  # The Go twin: TestDeployApplyFailedApplyPrintsTheFixedLine.
  _source_deploy
  local work="${BATS_TEST_TMPDIR}/failed"
  mkdir -p "${work}"
  kubehz::render_agent "${work}" "acme.example.com" "https://api.kubehz.cloud" operator managed
  _stub_kubectl_leaking "apply -k ${work}/live-agent"
  run kubehz::deploy_apply "${work}" acme.example.com operator managed
  assert_failure
  assert_line "[error] kapply: kubectl apply failed for the live agent (rc=1). lo does not show the kubectl output: it can quote a value of the manifest. To see it, run again with LO_SHOW_SENSITIVE_OUTPUT=1"
  assert_output --partial "kubehz: could not apply the live agent."
  refute_output --partial "${DEPLOY_CANARY}"
  refute_output --partial "${DEPLOY_ERR_CANARY}"
}
