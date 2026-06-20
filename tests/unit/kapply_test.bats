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
