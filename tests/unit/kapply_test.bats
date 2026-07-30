#!/usr/bin/env bats
# kapply_test.bats — unit tests for .lok8s/utils/kapply.sh
# Server-side apply with bounded, opt-in self-healing for the two states a
# plain apply can't reconcile: immutable fields and stuck-Terminating finalizers.

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1
  command -v yq &>/dev/null || skip "yq required for kapply tests"

  import() { :; }
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"

  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  unset LOK8S_FORCE_RECREATE APPLY_OUT GET_OUT KAPPLY_TTY
  export APPLY_RC=0

  # Stub kubectl: log every call; drive `apply` via APPLY_OUT/APPLY_RC and
  # `get` via GET_OUT; replace/patch just succeed. Exported so kapply's
  # command-substitution / pipeline subshells see it.
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    local cmd="$1"; [[ "$1" == "--kubeconfig" ]] && cmd="$3"
    case "${cmd}" in
      apply) [[ -n "${APPLY_OUT:-}" ]] && echo "${APPLY_OUT}"; return "${APPLY_RC:-0}" ;;
      get)   echo "${GET_OUT:-}"; return 0 ;;
      *)     return 0 ;;
    esac
  }
  export -f kubectl

  DEPLOY_MANIFEST='apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  selector:
    matchLabels: {app: web}'

  CR_MANIFEST='apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: db
  namespace: data
spec:
  instances: 1'
}

teardown() { teardown_tmpdir; }

_kapply() { printf '%s' "$1" | kapply::apply; }       # pipe manifest → kapply

@test "clean apply: returns 0, no healing" {
  export APPLY_RC=0
  run _kapply "${DEPLOY_MANIFEST}"
  assert_success
  run cat "${KLOG}"
  assert_output --partial 'apply --server-side'
  refute_output --partial 'replace --force'
}

@test "immutable WITHOUT --force-recreate: fails fast with a hint, no recreate" {
  export APPLY_RC=1
  export APPLY_OUT='Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable'
  run _kapply "${DEPLOY_MANIFEST}"
  assert_failure
  assert_output --partial 'force-recreate'
  run cat "${KLOG}"
  refute_output --partial 'replace --force'
}

@test "immutable WITH --force-recreate: recreates the named object, then re-applies" {
  export APPLY_RC=1
  export APPLY_OUT='Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable'
  export LOK8S_FORCE_RECREATE=1
  run _kapply "${DEPLOY_MANIFEST}"
  run cat "${KLOG}"
  assert_output --partial 'replace --force'
  # healed → re-applied: two apply calls total
  run bash -c "grep -c 'apply --server-side' '${KLOG}'"
  assert_output 2
}

@test "stuck Terminating WITH --force-recreate: clears finalizers on the CR" {
  export APPLY_RC=1
  export APPLY_OUT='Error from server: object is being deleted: clusters.postgresql.cnpg.io "db" already exists'
  export GET_OUT='2026-01-01T00:00:00Z'   # non-empty deletionTimestamp
  export LOK8S_FORCE_RECREATE=1
  run _kapply "${CR_MANIFEST}"
  run cat "${KLOG}"
  assert_output --partial 'patch Cluster db'
  assert_output --partial 'finalizers'
}

@test "stuck Terminating namespace (403) WITH --force-recreate: force-finalizes the ns, then re-applies" {
  # The wedged object is the NAMESPACE itself — named only in the 403 text, not
  # in the manifest. Heal = drop spec.finalizers via /finalize, then re-apply.
  export APPLY_RC=1
  export APPLY_OUT='Error from server (Forbidden): configmaps "ca-bundle" is forbidden: unable to create new content in namespace kubermatic because it is being terminated'
  export GET_OUT='2026-01-01T00:00:00Z'   # ns has a deletionTimestamp
  export LOK8S_FORCE_RECREATE=1
  export KAPPLY_NS_WAIT=0                  # don't poll-wait in the unit test
  run _kapply "${DEPLOY_MANIFEST}"
  run cat "${KLOG}"
  assert_output --partial 'replace --raw /api/v1/namespaces/kubermatic/finalize'
  # healed → re-applied: two server-side applies total
  run bash -c "grep -c 'apply --server-side' '${KLOG}'"
  assert_output 2
}

@test "force-finalize namespace: declined (non-interactive, no flag) → never calls /finalize" {
  # Reaching _finalize_namespace without the flag and without a tty must NOT
  # nuke the namespace — the extra confirm refuses and the heal is skipped.
  export GET_OUT='2026-01-01T00:00:00Z'   # ns is terminating
  unset LOK8S_FORCE_RECREATE              # LOK8S_NONINTERACTIVE=1 from setup → no tty
  run kapply::_finalize_namespace kubermatic
  assert_success                          # a declined heal is not an error
  run cat "${KLOG}"
  refute_output --partial 'finalize'      # the destructive API call never happened
}

@test "progress: named rolling window collapses to a phase summary" {
  # KAPPLY_TTY redirects the live UI to a file so we can assert on the render.
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  local pass
  pass=$(printf 'configmap/a serverside-applied\nsecret/b serverside-applied\nservice/c serverside-applied\nrole/d serverside-applied\n' | kapply::_progress "cilium")
  # full output still passes through (for capture/logs)
  [[ "${pass}" == *"role/d serverside-applied"* ]]
  # the UI rendered a header named after the phase, used in-place cursor-up
  # redraws, and collapsed to a single "<phase> · N applied" summary.
  grep -q 'cilium' "${KAPPLY_TTY}"
  grep -qE $'\033\\[[0-9]+A' "${KAPPLY_TTY}"
  grep -q 'cilium · 4 resources' "${KAPPLY_TTY}"
  # window never exceeds 3 lines: the last redraw frame must not show line "a"
  local last_frame; last_frame=$(awk 'BEGIN{RS="\033\\[[0-9]+A"} END{print}' "${KAPPLY_TTY}")
  [[ "${last_frame}" != *"configmap/a"* ]]
}

@test "progress: summary is singular for a single resource" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  printf 'configmap/solo serverside-applied\n' | kapply::_progress "lonely" >/dev/null
  grep -q 'lonely · 1 resource$' "${KAPPLY_TTY}"   # "resource", not "resources"
}

@test "run: wraps a mixed-output phase into one collapsed block" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  _phase() { printf 'configmap/coredns unchanged\nservice/coredns-external annotated\ndeployment.apps/coredns restarted\n'; }
  run kapply::run coredns _phase
  assert_success
  grep -q 'coredns · 3 resources' "${KAPPLY_TTY}"   # apply + annotate + restart all counted
}

@test "run: a phase with no progress lines is shown as-is, not swallowed" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  _warn_only() { echo "warning: skipping (nothing to do)"; }
  run kapply::run registries _warn_only
  assert_success
  assert_output --partial 'warning: skipping'
}

@test "run: surfaces errors and the command's exit on failure" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  _boom() { echo 'configmap/ok unchanged'; echo 'Error from server: boom'; return 1; }
  run kapply::run things _boom
  assert_failure
  assert_output --partial 'Error from server: boom'
  refute_output --partial 'configmap/ok'   # success line collapsed, not re-shown
}

WL_MANIFEST='apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: default
spec:
  replicas: 1'

@test "wait_ready: a ready (manifest-scoped) deployment collapses to ✓" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  export GET_OUT='{"items":[{"kind":"Deployment","metadata":{"namespace":"default","name":"web"},"spec":{"replicas":1},"status":{"availableReplicas":1}}]}'
  run kapply::wait_ready "platform" 30 <<< "${WL_MANIFEST}"
  assert_success
  grep -q 'platform · ready' "${KAPPLY_TTY}"
}

@test "wait_ready: a not-ready workload times out to a ⚠ + names it (best-effort)" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  export GET_OUT='{"items":[{"kind":"Deployment","metadata":{"namespace":"default","name":"web"},"spec":{"replicas":1},"status":{"availableReplicas":0}}]}'
  run kapply::wait_ready "platform" 0 <<< "${WL_MANIFEST}"   # timeout 0 → immediate ⚠, no sleep
  assert_success                                              # best-effort: never fatal
  grep -q 'timed out' "${KAPPLY_TTY}"
  grep -q 'web' "${KAPPLY_TTY}"                               # shows the pending name
}

@test "wait_ready: a manifest with no workloads is a no-op" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  run kapply::wait_ready "networking" 30 <<< 'apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: default}'
  assert_success
  [ ! -s "${KAPPLY_TTY}" ]   # nothing rendered
}

@test "verbose (DEBUG): collapsing UI is disabled — print everything" {
  unset LOK8S_NONINTERACTIVE KAPPLY_TTY
  DEBUG=1 run kapply::_tty
  assert_failure   # _tty false → full output, no aggregation
}

@test "aggregate: identical error lines collapse to (×N); distinct kept" {
  local out
  out=$(printf 'webhook refused\nwebhook refused\nwebhook refused\nimmutable: foo\n' | kapply::_aggregate)
  [[ "$(grep -c 'webhook refused' <<<"${out}")" -eq 1 ]]   # the 3 dupes → one line
  grep -q '×3' <<<"${out}"
  grep -q 'immutable: foo' <<<"${out}"                     # distinct line preserved
}

@test "unknown error: passed through unchanged, no healing" {
  export APPLY_RC=1
  export APPLY_OUT='error: unable to connect to the server: connection refused'
  export LOK8S_FORCE_RECREATE=1
  run _kapply "${DEPLOY_MANIFEST}"
  assert_failure
  run cat "${KLOG}"
  refute_output --partial 'replace --force'
  refute_output --partial 'patch'
}

# ── kapply::render_captured (offline block for buffered bootstrap jobs) ───────

@test "render_captured: collapses routine apply lines to a resource count" {
  run kapply::render_captured cert-manager 0 <<'IN'
namespace/cert-manager serverside-applied
deployment.apps/cert-manager serverside-applied
customresourcedefinition.apiextensions.k8s.io/certificates.cert-manager.io condition met
IN
  [ "$status" -eq 0 ]
  [[ "$output" == *"✓"*"cert-manager"*"· 3 resources"* ]]
  # the whole point: the routine per-object lines are collapsed away
  [[ "$output" != *"serverside-applied"* ]]
  [[ "$output" != *"condition met"* ]]
}

@test "render_captured: failure marks ✗ and surfaces deduped error lines" {
  run kapply::render_captured networking 1 <<'IN'
secret/kubehz-tls serverside-applied
no matches for kind "GatewayClass" in version "gateway.networking.k8s.io/v1"
no matches for kind "GatewayClass" in version "gateway.networking.k8s.io/v1"
IN
  [ "$status" -eq 0 ]
  [[ "$output" == *"✗"*"networking"*"· 1 resource"* ]]
  [[ "$output" == *'no matches for kind "GatewayClass"'* ]]
  [[ "$output" == *"×2"* ]]
}

@test "render_captured: keeps a final line that lacks a trailing newline" {
  # a killed job's buffer can end mid-write; plain `read` would drop the line
  run kapply::render_captured broken 1 < <(printf 'secret/x serverside-applied\npartial error, no newline')
  [ "$status" -eq 0 ]
  [[ "$output" == *"partial error, no newline"* ]]
}

@test "render_captured: zero applied resources renders a bare header" {
  run kapply::render_captured cilium 0 <<'IN'
[bootstrap] cilium already deployed by the KubeOne driver — skipping
IN
  [ "$status" -eq 0 ]
  [[ "$output" == *"✓"*"cilium"* ]]
  [[ "$output" != *"resource"* ]]
  [[ "$output" == *"already deployed"* ]]
}

@test "render_captured: retry passes count each resource once (no inflation)" {
  # the CRD-race retry re-applies the whole manifest — the summary must count
  # distinct resources, not lines-per-pass
  run kapply::render_captured cnpg 0 <<'IN'
namespace/cnpg-system serverside-applied
deployment.apps/cnpg serverside-applied
namespace/cnpg-system serverside-applied
deployment.apps/cnpg serverside-applied
customresourcedefinition.apiextensions.k8s.io/clusters.cnpg.io condition met
IN
  [ "$status" -eq 0 ]
  [[ "$output" == *"· 3 resources"* ]]
}

@test "render_captured: off-tty output is plain (no ANSI, embedded escapes stripped)" {
  # bats stdout is a pipe, so this exercises the non-tty path directly
  run kapply::render_captured networking 1 < <(printf 'secret/x serverside-applied\n\033[31mcolored controller error\033[0m\n')
  [ "$status" -eq 0 ]
  [[ "$output" != *$'\033'* ]]                       # zero escape sequences
  [[ "$output" == *"✗ networking · 1 resource"* ]]   # exact plain header
  [[ "$output" == *"colored controller error"* ]]    # payload kept, colors gone
}

@test "render_captured: indented OK-verb line surfaces instead of crashing the shell" {
  # an empty first token would be seen_resource[""]= — a FATAL bad-subscript
  # under set -e that killed the scheduler mid-reap (round-2 review find)
  run kapply::render_captured weird 0 < <(printf 'secret/x serverside-applied\n    patched\n')
  [ "$status" -eq 0 ]
  [[ "$output" == *"· 1 resource"* ]]
  [[ "$output" == *"patched"* ]]
}

@test "render_captured: prose ending in an OK verb is surfaced, not counted" {
  run kapply::render_captured warn 0 <<'IN'
namespace/x serverside-applied
Warning: resource configmaps/y lacks the last-applied annotation and was configured
IN
  [ "$status" -eq 0 ]
  [[ "$output" == *"· 1 resource"* ]]
  [[ "$output" == *"Warning: resource configmaps/y"* ]]
}

@test "render_captured: off-tty strips carriage returns from captured lines" {
  run kapply::render_captured cr-test 1 < <(printf 'secret/x serverside-applied\nprogress line\rovertyped error\n')
  [ "$status" -eq 0 ]
  [[ "$output" != *$'\r'* ]]
  [[ "$output" == *"overtyped error"* ]]
}

# --- kapply::preflight -------------------------------------------------------
# The pre-apply sweep for stuck-Terminating objects (Tilt's k8s_yaml path
# never goes through kapply::apply, so it can't self-heal there). The stub
# distinguishes the two GET shapes so the batched manifest path is actually
# exercised: `get -f -` answers GET_OUT, `get ns|namespace …` answers
# NS_GET_OUT — a regression that stopped feeding the manifest to kubectl
# could not stay green on the referenced-namespace fetch alone.

_preflight() { printf '%s' "$1" | kapply::preflight; }
_preflight_opts() { local manifest="$1"; shift; printf '%s' "${manifest}" | kapply::preflight "$@"; }
_preflight_stub() {
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    case "$1" in
      get)
        case "$2" in
          -f)           echo "${GET_OUT:-}" ;;
          ns|namespace)
            # With NS_FINALIZE_SUCCEEDS the stub turns stateful: after the
            # /finalize replace, the namespace reads as GONE — so the
            # honesty re-check's success branch is reachable. Without it,
            # the stub keeps answering "still terminating" (failure branch).
            if [[ -n "${NS_FINALIZE_SUCCEEDS:-}" && -f "${BATS_TEST_TMPDIR}/ns.finalized" ]]; then
              :
            else
              echo "${NS_GET_OUT:-}"
            fi ;;
          crd)
            # CRD existence probe: after the drain (first instance patch)
            # cleanup "completes" and the CRD reads as gone — unless
            # CRD_STAYS pins it wedged (exercises the force/still-stuck
            # branches).
            if [[ -z "${CRD_STAYS:-}" && -f "${BATS_TEST_TMPDIR}/crd.drained" ]]; then
              return 1
            fi
            return 0 ;;
          "${CRD_NAME:-none}") echo "${CRD_INSTANCES:-}" ;;
          *)            echo "${GET_OUT:-}" ;;
        esac
        return 0 ;;
      replace) touch "${BATS_TEST_TMPDIR}/ns.finalized"; return 0 ;;
      patch)
        [[ "$2" == "${CRD_NAME:-none}" ]] && touch "${BATS_TEST_TMPDIR}/crd.drained"
        return "${PATCH_RC:-0}" ;;
      *)       return 0 ;;
    esac
  }
  export -f kubectl
}

# Fixture pair for the CRD-policy tests: the stuck CRD arrives via the
# manifest get; its instances via `get <crd-name> -A`.
_stuck_crd_fixture() {
  export CRD_NAME="kubehzclusters.kubehz.dev"
  export GET_OUT='{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"kubehzclusters.kubehz.dev","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["customresourcecleanup.apiextensions.k8s.io"]}}'
  export CRD_INSTANCES='{"items":[
    {"metadata":{"namespace":"kubehz-system","name":"k6-burst-1","finalizers":["kubehz.dev/cluster-cleanup"]}},
    {"metadata":{"namespace":"kubehz-system","name":"k6-burst-2","finalizers":["kubehz.dev/cluster-cleanup"]}}]}'
  export KAPPLY_CRD_WAIT=1 KAPPLY_POLL_INTERVAL=0
}

@test "preflight: nothing terminating → no patches, reports nothing stuck" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  export GET_OUT='{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app"}}'
  run _preflight "${DEPLOY_MANIFEST}"
  assert_success
  assert_output --partial 'nothing stuck'
  run cat "${KLOG}"
  refute_output --partial ' patch '
}

@test "preflight: object stuck past the age threshold → finalizers cleared with qualified type" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  export GET_OUT='{"apiVersion":"postgresql.cnpg.io/v1","kind":"Database","metadata":{"name":"status","namespace":"kubehz-system","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["cnpg.io/deleteDatabase"]}}'
  run _preflight "${CR_MANIFEST}"
  assert_success
  assert_output --partial 'cnpg.io/deleteDatabase'
  run cat "${KLOG}"
  # The stuck object must have come in via the BATCHED manifest get…
  assert_output --partial 'get -f - -o json --ignore-not-found'
  # …and go out via a fully-qualified patch (bare kind would be ambiguous).
  assert_output --partial 'patch Database.v1.postgresql.cnpg.io status -n kubehz-system --type merge'
}

@test "preflight: deletion younger than the age threshold is left to drain" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  local fresh; fresh=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  export GET_OUT='{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app","deletionTimestamp":"'"${fresh}"'","finalizers":["kubernetes.io/pvc-protection"]}}'
  run _preflight "${DEPLOY_MANIFEST}"
  assert_success
  assert_output --partial 'letting it drain'
  run cat "${KLOG}"
  refute_output --partial ' patch '
}

@test "preflight: unparseable deletionTimestamp is skipped, never force-cleared" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  export GET_OUT='{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app","deletionTimestamp":"not-a-time","finalizers":["kubernetes.io/pvc-protection"]}}'
  run _preflight "${DEPLOY_MANIFEST}"
  assert_success
  assert_output --partial 'unparseable deletionTimestamp'
  run cat "${KLOG}"
  refute_output --partial ' patch '
}

@test "preflight: stuck namespace arrives via the referenced-ns fetch, finalizes via /finalize" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  # GET_OUT stays empty: the namespace is REFERENCED by the manifest (ns
  # `default`), not declared in it — this pins the second fetch path.
  export NS_GET_OUT='{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"mla","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["kubernetes"]}}'
  export KAPPLY_NS_WAIT=0
  run _preflight "${DEPLOY_MANIFEST}"
  assert_success
  # The static stub keeps answering with a deletionTimestamp after the
  # finalize, so the honesty re-check must report the wedge, not "patched".
  assert_output --partial 'could not finalize namespace/mla'
  run cat "${KLOG}"
  assert_output --partial 'replace --raw /api/v1/namespaces/mla/finalize'
}

@test "preflight: successfully finalized namespace reports force-finalized, not a wedge" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  export NS_GET_OUT='{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"mla","deletionTimestamp":"2020-01-01T00:00:00Z","finalizers":["kubernetes"]}}'
  export NS_FINALIZE_SUCCEEDS=1 KAPPLY_NS_WAIT=0
  run _preflight "${DEPLOY_MANIFEST}"
  assert_success
  assert_output --partial 'force-finalized Namespace/mla'
  refute_output --partial 'could not finalize'
}

@test "preflight: stuck CRD drains its instances by default, never touches the CRD finalizer" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  run _preflight "${CR_MANIFEST}"
  assert_success
  assert_output --partial 'drained 2 instance(s) of CustomResourceDefinition/kubehzclusters.kubehz.dev'
  run cat "${KLOG}"
  assert_output --partial 'patch kubehzclusters.kubehz.dev k6-burst-1 -n kubehz-system'
  assert_output --partial 'patch kubehzclusters.kubehz.dev k6-burst-2 -n kubehz-system'
  refute_output --partial 'patch crd '
}

@test "preflight: crds=skip refuses the CRD entirely" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  run _preflight_opts "${CR_MANIFEST}" --crds skip
  assert_success
  assert_output --partial 'refusing to force stuck CustomResourceDefinition/kubehzclusters.kubehz.dev'
  run cat "${KLOG}"
  refute_output --partial ' patch '
}

@test "preflight: crds=drain on a CRD still wedged after drain reports it, does not escalate" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  export CRD_STAYS=1
  run _preflight "${CR_MANIFEST}"
  assert_success
  assert_output --partial 'still terminating after draining 2 instance(s)'
  run cat "${KLOG}"
  refute_output --partial 'patch crd '
}

@test "preflight: crds=force escalates to stripping the CRD finalizer after a failed drain" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  export CRD_STAYS=1
  run _preflight_opts "${CR_MANIFEST}" --crds force
  assert_success
  assert_output --partial 'FORCED CustomResourceDefinition/kubehzclusters.kubehz.dev'
  run cat "${KLOG}"
  assert_output --partial 'patch crd kubehzclusters.kubehz.dev'
}

@test "preflight: crds=force allowlist tolerates csv whitespace" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  export CRD_STAYS=1
  run _preflight_opts "${CR_MANIFEST}" --crds force --crd-allow 'other.example.com, kubehzclusters.kubehz.dev'
  assert_success
  assert_output --partial 'FORCED CustomResourceDefinition/kubehzclusters.kubehz.dev'
}

@test "preflight: non-numeric KAPPLY_CRD_WAIT falls back instead of aborting the report" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  export KAPPLY_CRD_WAIT="20s"
  run _preflight "${CR_MANIFEST}"
  assert_success
  assert_output --partial 'drained 2 instance(s)'
}

@test "preflight: crds=force respects the allowlist — non-listed CRD is not stripped" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub; _stuck_crd_fixture
  export CRD_STAYS=1
  run _preflight_opts "${CR_MANIFEST}" --crds force --crd-allow other.example.com
  assert_success
  assert_output --partial 'not forcing its finalizer'
  run cat "${KLOG}"
  refute_output --partial 'patch crd '
}

@test "preflight: failed patch reports the object and still exits 0" {
  command -v jq &>/dev/null || skip "jq required"
  _preflight_stub
  export GET_OUT='{"apiVersion":"v1","kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"app","deletionTimestamp":"2020-01-01T00:00:00Z"}}'
  export PATCH_RC=1
  run _preflight "${DEPLOY_MANIFEST}"
  assert_success
  assert_output --partial 'could not clear finalizers on PersistentVolumeClaim/data'
}
