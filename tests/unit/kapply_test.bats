#!/usr/bin/env bats
# kapply_test.bats — unit tests for .lok8s/utils/kapply.sh
# Server-side apply with bounded, opt-in self-healing for the two states a
# plain apply can't reconcile: immutable fields and stuck-Terminating finalizers.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq required for kapply tests"

  import() { :; }
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"

  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"; : > "${KLOG}"
  unset LOK8S_FORCE_RECREATE APPLY_OUT GET_OUT
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
