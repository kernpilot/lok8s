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
  # KENV: one line per kubectl call, the verb and the KUBERC it ran with.
  export KENV="${BATS_TEST_TMPDIR}/kubectl.env"; : > "${KENV}"
  unset LOK8S_FORCE_RECREATE APPLY_OUT APPLY_ERR GET_OUT KAPPLY_TTY KAPPLY_PRIVATE_LOG KAPPLY_PRIVATE_DIR _KAPPLY_PRIVATE_MADE LO_SHOW_SENSITIVE_OUTPUT KUBERC DEBUG
  export APPLY_RC=0

  # Stub kubectl: log every call; drive `apply` via APPLY_OUT (stdout),
  # APPLY_ERR (stderr) and APPLY_RC, and `get` via GET_OUT; replace/patch
  # just succeed. Exported so kapply's command-substitution / pipeline
  # subshells see it.
  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    # The verb is the first argument after the global flags of the caller
    # (--kubeconfig <path>, --server <url>, --server=<url>). A replace adds
    # its next argument (--force, --raw) to the KENV line.
    local -a rest=("$@")
    while (( ${#rest[@]} )); do
      case "${rest[0]}" in
        --kubeconfig|--server) rest=("${rest[@]:2}") ;;
        --server=*)            rest=("${rest[@]:1}") ;;
        *)                     break ;;
      esac
    done
    local cmd="${rest[0]-}" verb="${rest[0]-}"
    [[ "${cmd}" != "replace" ]] || verb="replace ${rest[1]-}"
    echo "${verb} KUBERC=${KUBERC-<unset>}" >> "${KENV}"
    case "${cmd}" in
      apply)
        [[ -z "${APPLY_OUT:-}" ]] || echo "${APPLY_OUT}"
        [[ -z "${APPLY_ERR:-}" ]] || echo "${APPLY_ERR}" >&2
        return "${APPLY_RC:-0}" ;;
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

  SECRET_MANIFEST='apiVersion: v1
kind: Secret
metadata:
  name: zitadel-credentials
  namespace: zitadel
immutable: true
data: {}'

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

# The last value of the manifest is a `|+` block scalar that ends in three
# newlines: kubectl gets the bytes of stdin, with the newlines.
@test "apply: kubectl gets the manifest byte for byte" {
  export STDIN_COPY="${BATS_TEST_TMPDIR}/kubectl.stdin" WANT="${BATS_TEST_TMPDIR}/want.yaml"
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\ndata:\n  tail: |+\n    x\n\n\n' > "${WANT}"
  kubectl() { cat > "${STDIN_COPY}"; }
  run kapply::apply --label cfg < "${WANT}"
  assert_success
  cmp "${WANT}" "${STDIN_COPY}"
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

# The kind and the name of an immutable-field error come from kubectl's error
# text, which an admission webhook can write. The heal acts only on manifest
# objects and prints them from the manifest values, escaped.
@test "heal: a name in the error that is in no manifest object is not healed, and no part of it prints" {
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1
  export APPLY_OUT="Error from server (Invalid): Deployment.apps \"${CANARY_VALUE}\" is invalid: spec.selector: field is immutable
Error from server (Invalid): Deployment.apps \"web\" is invalid: spec.selector: field is immutable"
  run _kapply "${DEPLOY_MANIFEST}"
  refute_output --partial "${CANARY_VALUE}"
  assert_line '[warn] kapply: an immutable-field error named no object of this manifest'
  assert_output --partial 'recreating immutable Deployment/web'
  run grep -c 'replace --force' "${KLOG}"
  assert_output 1
}

@test "heal: the object line comes from the manifest, with its control bytes escaped" {
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1
  export APPLY_OUT=$'Error from server (Invalid): Deployment.apps "we\eb" is invalid: spec.selector: field is immutable'
  local m='apiVersion: apps/v1
kind: Deployment
metadata:
  name: "we\eb"
  namespace: default'
  run _kapply "${m}"
  assert_output --partial 'recreating immutable Deployment/we\x1bb'
  refute_output --partial $'\e'
  grep -q 'replace --force' "${KLOG}"
}

@test "escape: a control byte prints as \\xNN, a backslash as \\\\, other bytes unchanged" {
  run kapply::escape $'we\eb a\\b tab\there nl\n del\x7f \xc3\xa9'
  assert_success
  assert_output 'we\x1bb a\\b tab\x09here nl\x0a del\x7f é'
}

@test "sealed Secret WITH --force-recreate: recreates, but with a pointed RE-KEY warning" {
  export APPLY_RC=1
  export APPLY_OUT='Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable when `immutable` is set'
  export LOK8S_FORCE_RECREATE=1
  run _kapply "${SECRET_MANIFEST}"
  assert_output --partial 'RE-KEYING sealed Secret/zitadel-credentials'
  run cat "${KLOG}"
  assert_output --partial 'replace --force'
}

@test "sealed Secret declined (non-interactive, no flag): kept, never recreated" {
  # Reaching _heal_immutable without the flag and without a tty must NOT
  # re-key the Secret — the crown-jewel confirm refuses and the object is kept.
  unset LOK8S_FORCE_RECREATE                # LOK8S_NONINTERACTIVE=1 from setup → no tty
  local err='Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable'
  run kapply::_heal_immutable "${SECRET_MANIFEST}" "${err}"
  assert_output --partial 'keeping sealed Secret/zitadel-credentials'
  run cat "${KLOG}"
  refute_output --partial 'replace --force'
}

@test "UNSEALED Secret with an immutable-field conflict: generic heal, no re-key prompt" {
  # A plain Secret can hit "field is immutable" too (e.g. a type: change).
  # The crown-jewel confirm must NOT trigger — only the manifest's
  # immutable: true earns the pointed RE-KEY treatment.
  export APPLY_RC=1
  export APPLY_OUT='Error from server (Invalid): Secret "plain-creds" is invalid: type: field is immutable'
  export LOK8S_FORCE_RECREATE=1
  local plain='apiVersion: v1
kind: Secret
metadata:
  name: plain-creds
  namespace: default
data: {}'
  run _kapply "${plain}"
  refute_output --partial 'RE-KEYING'
  run cat "${KLOG}"
  assert_output --partial 'replace --force'
}

@test "sealed Secret interactive decline: 'n' at the pointed prompt keeps the Secret" {
  # The REACHABLE decline: an interactive operator answered y to the generic
  # heal prompt, then n to the crown-jewel confirm. Stub the tty seams — bats
  # has no /dev/tty to type into.
  unset LOK8S_FORCE_RECREATE
  kapply::_interactive() { return 0; }
  kapply::_ask() { return 1; }              # operator answers "n"
  local err='Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable'
  run kapply::_heal_immutable "${SECRET_MANIFEST}" "${err}"
  assert_output --partial 'keeping sealed Secret/zitadel-credentials'
  run cat "${KLOG}"
  refute_output --partial 'replace --force'
}

@test "sealed Secret interactive accept: 'y' at the pointed prompt recreates it" {
  unset LOK8S_FORCE_RECREATE
  kapply::_interactive() { return 0; }
  kapply::_ask() { return 0; }              # operator answers "y"
  local err='Error from server (Invalid): Secret "zitadel-credentials" is invalid: data: Forbidden: field is immutable'
  run kapply::_heal_immutable "${SECRET_MANIFEST}" "${err}"
  assert_output --partial 'recreating immutable Secret/zitadel-credentials'
  run cat "${KLOG}"
  assert_output --partial 'replace --force'
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

@test "progress: one in-place line with the count collapses to a phase summary" {
  # KAPPLY_TTY redirects the live UI to a file so we can assert on the render.
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  local pass
  pass=$(printf 'configmap/a serverside-applied\nsecret/b serverside-applied\nservice/c serverside-applied\nrole/d serverside-applied\n' | kapply::_progress "cilium")
  # full output still passes through (for capture/logs)
  [[ "${pass}" == *"role/d serverside-applied"* ]]
  # the UI redrew one line in place (the phase and the count so far) and
  # collapsed to a single "<phase> · N resources" summary. It draws no
  # kubectl line and moves no cursor up.
  grep -q 'cilium · 3' "${KAPPLY_TTY}"
  grep -q 'cilium · 4 resources' "${KAPPLY_TTY}"
  run grep -cE $'\033\\[[0-9]+A|serverside-applied' "${KAPPLY_TTY}"
  assert_output 0
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

# ── the kubectl output stays private ─────────────────────────────────────────
# CANARY_VALUE stands for a value of the manifest that kubectl quotes in its
# output (a field error prints "Invalid value: <value>"). It is a made-up
# string. lo prints fixed lines; the kubectl text prints only under
# LO_SHOW_SENSITIVE_OUTPUT=1.

CANARY_VALUE=zq7Canary9xLm2
CFG_MANIFEST='apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: default}'

_fixed_fail_line() {
  printf '[error] kapply: kubectl apply failed for %s (rc=%s). lo does not show the kubectl output: it can quote a value of the manifest. To see it, run again with LO_SHOW_SENSITIVE_OUTPUT=1' "$1" "$2"
}

@test "private: each apply pass runs kubectl with the user defaults off (KUBERC=off)" {
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1
  export APPLY_OUT='Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable'
  run _kapply "${DEPLOY_MANIFEST}"
  # the first pass and the pass after the heal
  run grep -c '^apply ' "${KENV}"
  assert_output 2
  run grep -c '^apply KUBERC=off$' "${KENV}"
  assert_output 2
}

# An immutable-field error and a 403 of a terminating namespace in one pass:
# the heal replaces the Deployment and finalizes the namespace.
BOTH_HEALS_OUT='Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable
Error from server (Forbidden): configmaps "ca-bundle" is forbidden: unable to create new content in namespace kubermatic because it is being terminated'

_kapply_flags() { local m="$1"; shift; printf '%s' "${m}" | kapply::apply "$@"; }

@test "kuberc: the replace of the immutable heal and of the namespace finalize run with KUBERC=off" {
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1 KAPPLY_NS_WAIT=0 GET_OUT='2026-01-01T00:00:00Z'
  export APPLY_OUT="${BOTH_HEALS_OUT}"
  run _kapply "${DEPLOY_MANIFEST}"
  run grep -c '^replace --force KUBERC=off$' "${KENV}"
  assert_output 1
  run grep -c '^replace --raw KUBERC=off$' "${KENV}"
  assert_output 1
  # the reads keep the environment of lo
  run grep -c '^get KUBERC=<unset>$' "${KENV}"
  refute_output 0
}

@test "kuberc: a --server <url> flag in front of the verb keeps KUBERC=off on each write" {
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1 KAPPLY_NS_WAIT=0 GET_OUT='2026-01-01T00:00:00Z'
  export APPLY_OUT="${BOTH_HEALS_OUT}"
  local flags
  for flags in '--server https://203.0.113.1:6443' '--server=https://203.0.113.1:6443'; do
    : > "${KLOG}"; : > "${KENV}"
    # shellcheck disable=SC2086 # the flags split into words on purpose
    run _kapply_flags "${DEPLOY_MANIFEST}" ${flags}
    [ "$(grep -c '^apply KUBERC=off$' "${KENV}")" -eq 2 ] || { cat "${KENV}"; return 1; }
    [ "$(grep -c '^replace --force KUBERC=off$' "${KENV}")" -eq 1 ] || { cat "${KENV}"; return 1; }
    [ "$(grep -c '^replace --raw KUBERC=off$' "${KENV}")" -eq 1 ] || { cat "${KENV}"; return 1; }
    grep -qF "kubectl ${flags} replace --force -f -" "${KLOG}" || { cat "${KLOG}"; return 1; }
  done
}

# The kubectl of PATH (kubectl_path_stub) is a child process: its log holds
# the KUBERC that lo gave it, with a KUBERC of the user set. The two apply
# passes, the replace of the immutable heal and the replace of the namespace
# finalize read stdin and run with KUBERC=off; the reads keep the KUBERC of
# the user. The Go twin is TestApplyChildrenGetKubeRCOffOnEachWrite.
@test "kuberc: a kubectl on PATH runs the apply, the heal's replace and the finalize with KUBERC=off" {
  kubectl_path_stub
  export KUBERC=/user/kuberc LOK8S_FORCE_RECREATE=1 KAPPLY_NS_WAIT=0 APPLY_OUT="${BOTH_HEALS_OUT}"
  export FIRST_APPLY="${BATS_TEST_TMPDIR}/first-apply"
  kubectl_path_answer() {
    [[ " $* " != *" -f - "* ]] || cat >/dev/null
    case "${1}" in
      apply)
        [[ ! -e "${FIRST_APPLY}" ]] || return 0
        : > "${FIRST_APPLY}"
        printf '%s\n' "${APPLY_OUT}" >&2
        return 1 ;;
      get) echo '2026-01-01T00:00:00Z' ;;
    esac
    return 0
  }
  export -f kubectl_path_answer
  run _kapply "${DEPLOY_MANIFEST}"
  assert_success
  local log="${KUBECTL_PATH_LOG}"
  [ "$(grep -c $'^KUBERC=off\tapply --server-side --force-conflicts -f -$' "${log}")" -eq 2 ] || { cat "${log}"; return 1; }
  [ "$(grep -c $'^KUBERC=off\treplace --force -f -$' "${log}")" -eq 1 ] || { cat "${log}"; return 1; }
  [ "$(grep -c $'^KUBERC=off\treplace --raw /api/v1/namespaces/kubermatic/finalize -f -$' "${log}")" -eq 1 ] || { cat "${log}"; return 1; }
  [ "$(grep -c $'^KUBERC=/user/kuberc\tget ' "${log}")" -ge 1 ] || { cat "${log}"; return 1; }
  [ "$(kubectl_path_file_calls | grep -c .)" -eq 4 ] || { cat "${log}"; return 1; }
  [ -z "$(kubectl_path_file_calls | grep -v $'^KUBERC=off\t')" ] || { cat "${log}"; return 1; }
}

# STDERR_CANARY stands for a value that kubectl quotes on its stderr (an
# error or a warning), apart from CANARY_VALUE on its stdout: a pass that
# keeps the stdout private but lets the stderr through to the terminal
# prints it. The Go twin is TestApplyStderrStaysPrivate.
STDERR_CANARY=zq7StderrCanary4kWp

@test "private: the kubectl stderr stays private too, off and on a tty, each level" {
  export APPLY_RC=1
  export APPLY_OUT="configmap/a serverside-applied
The ConfigMap \"cfg\" is invalid: data.v: Invalid value: \"${CANARY_VALUE}\""
  export APPLY_ERR="Warning: the value \"${STDERR_CANARY}\" of ConfigMap cfg is deprecated"
  local level tty
  for tty in "" "${BATS_TEST_TMPDIR}/ui.txt"; do
    for level in "" 1; do
      unset KAPPLY_TTY
      if [[ -n "${tty}" ]]; then
        export KAPPLY_TTY="${tty}"
        : > "${tty}"
      fi
      DEBUG="${level}" run _kapply "${CFG_MANIFEST}"
      assert_failure
      refute_output --partial "${STDERR_CANARY}"
      refute_output --partial "${CANARY_VALUE}"
      assert_line "$(_fixed_fail_line resources 1)"
      if [[ -n "${tty}" ]]; then
        run grep -c "${STDERR_CANARY}" "${tty}"
        assert_output 0
      fi
    done
  done
  # The private file of the scheduler gets the stderr with the stdout.
  unset KAPPLY_TTY
  _private_dir
  run _kapply "${CFG_MANIFEST}"
  refute_output --partial "${STDERR_CANARY}"
  grep -qF "${STDERR_CANARY}" "${KAPPLY_PRIVATE_LOG}"
}

@test "private: a failed pass prints the fixed line, never the kubectl output (off a tty, each level)" {
  export APPLY_RC=1
  export APPLY_OUT="The ConfigMap \"cfg\" is invalid: data.v: Invalid value: \"${CANARY_VALUE}\""
  local level
  for level in "" 1; do
    DEBUG="${level}" run _kapply "${CFG_MANIFEST}"
    assert_failure
    refute_output --partial "${CANARY_VALUE}"
    assert_line "$(_fixed_fail_line resources 1)"
  done
  assert_line '[debug] kapply: kubectl apply for resources exited 1 (0 resources)'
}

@test "private: the pass after a heal prints the fixed line too, never the kubectl output" {
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1
  export APPLY_OUT="Error from server (Invalid): Deployment.apps \"web\" is invalid: spec.selector: Invalid value: \"${CANARY_VALUE}\": field is immutable"
  local level fixed
  fixed="$(_fixed_fail_line resources 1)"
  for level in "" 1; do
    DEBUG="${level}" run _kapply "${DEPLOY_MANIFEST}"
    assert_failure
    refute_output --partial "${CANARY_VALUE}"
    assert_output --partial 'healing blocked objects, then re-applying once'
    [[ "$(grep -cxF "${fixed}" <<<"${output}")" -eq 2 ]]
  done
}

@test "private: on a tty a failed pass prints the fixed line, never the kubectl output" {
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"
  export APPLY_RC=1
  export APPLY_OUT="configmap/a serverside-applied
The ConfigMap \"cfg\" is invalid: data.v: Invalid value: \"${CANARY_VALUE}\""
  local level
  for level in "" 1; do
    : > "${KAPPLY_TTY}"
    DEBUG="${level}" run _kapply "${CFG_MANIFEST}"
    assert_failure
    refute_output --partial "${CANARY_VALUE}"
    assert_line "$(_fixed_fail_line resources 1)"
    grep -q 'resources · 1 resource' "${KAPPLY_TTY}"
    run grep -c "${CANARY_VALUE}" "${KAPPLY_TTY}"
    assert_output 0
  done
}

@test "private: a clean pass prints the summary line, never the kubectl output" {
  export APPLY_OUT="configmap/cfg serverside-applied
Warning: unknown field \"data.${CANARY_VALUE}\""
  local level
  for level in "" 1; do
    DEBUG="${level}" run _kapply "${CFG_MANIFEST}"
    assert_success
    refute_output --partial "${CANARY_VALUE}"
    assert_line '✓ resources · 1 resource'
  done
  assert_line '[debug] kapply: kubectl apply for resources exited 0 (1 resources)'
}

# _private_dir: a 0700 dir like the one of the bootstrap scheduler, with
# KAPPLY_PRIVATE_DIR and KAPPLY_PRIVATE_LOG set in this shell, not exported
# (the launch site of libs/bootstrap).
_private_dir() {
  KAPPLY_PRIVATE_DIR="$(mktemp -d "${BATS_TEST_TMPDIR}/sched.XXXXXX")"
  KAPPLY_PRIVATE_LOG="${KAPPLY_PRIVATE_DIR}/1.priv"
  export -n KAPPLY_PRIVATE_DIR KAPPLY_PRIVATE_LOG
}

PRIVATE_REFUSED='[warn] kapply: the private output file is not safe to write (lo did not set it, or it is a link, a file that was there before, a file open to others or a file in a directory that another user can rename): lo does not write the kubectl output to it'

# _assert_private_refused: the pass printed the fixed refusal line and the
# fixed failure line, never the kubectl output.
_assert_private_refused() {
  assert_failure
  assert_line "${PRIVATE_REFUSED}"
  assert_line "$(_fixed_fail_line resources 1)"
  refute_output --partial "${CANARY_VALUE}"
}

@test "private: the scheduler's private file gets the kubectl output (made fresh, 0600), the pass prints no summary" {
  _private_dir
  export APPLY_RC=1
  export APPLY_OUT="configmap/cfg serverside-applied
Invalid value: \"${CANARY_VALUE}\""
  run _kapply "${CFG_MANIFEST}"
  assert_failure
  refute_output --partial "${CANARY_VALUE}"
  refute_output --partial '✓ resources'
  refute_output --partial 'not safe to write'
  assert_line "$(_fixed_fail_line resources 1)"
  grep -qF "Invalid value: \"${CANARY_VALUE}\"" "${KAPPLY_PRIVATE_LOG}"
  [[ -f "${KAPPLY_PRIVATE_LOG}" && ! -L "${KAPPLY_PRIVATE_LOG}" ]]
  [ "$(find "${KAPPLY_PRIVATE_LOG}" -prune -perm 0600 -print)" = "${KAPPLY_PRIVATE_LOG}" ]
}

@test "private: the pass after a heal appends to the file that kapply made" {
  _private_dir
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1
  export APPLY_OUT="Error from server (Invalid): Deployment.apps \"web\" is invalid: spec.selector: Invalid value: \"${CANARY_VALUE}\": field is immutable"
  run _kapply "${DEPLOY_MANIFEST}"
  refute_output --partial 'not safe to write'
  refute_output --partial "${CANARY_VALUE}"
  [ "$(grep -c "${CANARY_VALUE}" "${KAPPLY_PRIVATE_LOG}")" -eq 2 ]
}

@test "private: a symlink at the private path is refused, nothing is written through it" {
  _private_dir
  local target="${BATS_TEST_TMPDIR}/elsewhere.log"; : > "${target}"
  ln -s "${target}" "${KAPPLY_PRIVATE_LOG}"
  export APPLY_RC=1 APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  run _kapply "${CFG_MANIFEST}"
  _assert_private_refused
  [ ! -s "${target}" ]
}

@test "private: a file that is already there and open to others is refused, nothing is written" {
  _private_dir
  ( umask 022; : > "${KAPPLY_PRIVATE_LOG}" )
  chmod 0644 "${KAPPLY_PRIVATE_LOG}"
  export APPLY_RC=1 APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  run _kapply "${CFG_MANIFEST}"
  _assert_private_refused
  [ ! -s "${KAPPLY_PRIVATE_LOG}" ]
}

@test "private: a FIFO at the private path is refused, nothing is written to it" {
  _private_dir
  mkfifo "${KAPPLY_PRIVATE_LOG}"
  # Hold the FIFO open for reading and writing: a write would not block, and
  # the read below would see it.
  local fd line=""
  exec {fd}<>"${KAPPLY_PRIVATE_LOG}"
  export APPLY_RC=1 APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  run _kapply "${CFG_MANIFEST}"
  read -r -t 1 -u "${fd}" line || true
  exec {fd}<&-
  _assert_private_refused
  [ -z "${line}" ]
}

@test "private: a value from the environment or a dir open to others is refused, nothing is written" {
  export APPLY_RC=1 APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  # exported: the value did not come from the shell code of lo
  _private_dir
  export KAPPLY_PRIVATE_DIR KAPPLY_PRIVATE_LOG
  run _kapply "${CFG_MANIFEST}"
  _assert_private_refused
  [ ! -e "${KAPPLY_PRIVATE_LOG}" ]
  export -n KAPPLY_PRIVATE_DIR KAPPLY_PRIVATE_LOG
  # a dir with mode 0755
  _private_dir
  chmod 0755 "${KAPPLY_PRIVATE_DIR}"
  run _kapply "${CFG_MANIFEST}"
  _assert_private_refused
  [ ! -e "${KAPPLY_PRIVATE_LOG}" ]
  # a file outside the dir
  _private_dir
  KAPPLY_PRIVATE_LOG="${BATS_TEST_TMPDIR}/outside.priv"
  run _kapply "${CFG_MANIFEST}"
  _assert_private_refused
  [ ! -e "${KAPPLY_PRIVATE_LOG}" ]
}

# A directory above the private dir that another user can write into, and
# that has no sticky bit, lets that user rename the dir and put a link to a
# dir of their own at its path. kapply refuses it before it makes the file.
@test "private: a directory above the private dir that others can write into, with no sticky bit, is refused, nothing is written" {
  export APPLY_RC=1 APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  local open="${BATS_TEST_TMPDIR}/open" mode
  mkdir "${open}"
  for mode in 0777 0770; do
    chmod "${mode}" "${open}"
    KAPPLY_PRIVATE_DIR="$(mktemp -d "${open}/sched.XXXXXX")"
    KAPPLY_PRIVATE_LOG="${KAPPLY_PRIVATE_DIR}/1.priv"
    export -n KAPPLY_PRIVATE_DIR KAPPLY_PRIVATE_LOG
    run _kapply "${CFG_MANIFEST}"
    _assert_private_refused
    [ ! -e "${KAPPLY_PRIVATE_LOG}" ] || { echo "mode ${mode}: the file was made" >&2; return 1; }
  done
  # With the sticky bit (as /tmp), another user cannot rename the dir.
  chmod 1777 "${open}"
  KAPPLY_PRIVATE_DIR="$(mktemp -d "${open}/sched.XXXXXX")"
  KAPPLY_PRIVATE_LOG="${KAPPLY_PRIVATE_DIR}/1.priv"
  export -n KAPPLY_PRIVATE_DIR KAPPLY_PRIVATE_LOG
  run _kapply "${CFG_MANIFEST}"
  refute_output --partial 'not safe to write'
  grep -qF "Invalid value: \"${CANARY_VALUE}\"" "${KAPPLY_PRIVATE_LOG}"
}

# The first pass makes the file and keeps its descriptor; the pass after a
# heal writes through that descriptor and checks that the path still holds
# its file. Here the heal (the replace) renames the dir and puts a new dir
# and a new file at the old path: the pass after it refuses, and the new
# file gets no byte.
@test "private: the pass after a heal writes through the descriptor of the first pass, never through a file put at the path between them" {
  _private_dir
  export APPLY_RC=1 LOK8S_FORCE_RECREATE=1
  export APPLY_OUT="Error from server (Invalid): Deployment.apps \"web\" is invalid: spec.selector: Invalid value: \"${CANARY_VALUE}\": field is immutable"
  eval "$(declare -f kubectl | sed '1s/^kubectl /_kubectl_stub /')"
  kubectl() {
    if [[ " $* " == *" replace "* && ! -e "${KAPPLY_PRIVATE_DIR}.moved" ]]; then
      mv -- "${KAPPLY_PRIVATE_DIR}" "${KAPPLY_PRIVATE_DIR}.moved"
      mkdir -m 0700 -- "${KAPPLY_PRIVATE_DIR}"
      ( umask 077; : > "${KAPPLY_PRIVATE_LOG}" )
    fi
    _kubectl_stub "$@"
  }
  export -f kubectl _kubectl_stub
  run _kapply "${DEPLOY_MANIFEST}"
  grep -q '^replace --force ' "${KENV}"
  assert_line "${PRIVATE_REFUSED}"
  refute_output --partial "${CANARY_VALUE}"
  [ ! -s "${KAPPLY_PRIVATE_LOG}" ]
  [ "$(grep -c "${CANARY_VALUE}" "${KAPPLY_PRIVATE_DIR}.moved/1.priv")" -eq 1 ]
}

@test "private: LO_SHOW_SENSITIVE_OUTPUT=1 prints the kubectl output after a [warn] line" {
  export LO_SHOW_SENSITIVE_OUTPUT=1 APPLY_RC=1
  export APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  local warn_line='[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1: lo shows the kubectl output for resources, which can quote a value of the manifest'
  # off a tty: the full output
  run _kapply "${CFG_MANIFEST}"
  assert_failure
  assert_line "${warn_line}"
  assert_output --partial "${CANARY_VALUE}"
  refute_output --partial 'kapply: kubectl apply failed for'
  # on a tty: the error lines (deduped)
  export KAPPLY_TTY="${BATS_TEST_TMPDIR}/ui.txt"; : > "${KAPPLY_TTY}"
  run _kapply "${CFG_MANIFEST}"
  assert_failure
  assert_line "${warn_line}"
  assert_output --partial "${CANARY_VALUE}"
}

@test "private: only LO_SHOW_SENSITIVE_OUTPUT=1 is the opt-in" {
  LO_SHOW_SENSITIVE_OUTPUT=1 run kapply::show_sensitive
  assert_success
  local v
  for v in "" 0 true yes; do
    LO_SHOW_SENSITIVE_OUTPUT="${v}" run kapply::show_sensitive
    assert_failure
  done
}

# ── kapply::run_private (one kubectl write that is not an apply pass) ───────
# The Go twin is internal/kapply/run_private_test.go.

@test "run_private: a failed write prints the fixed line, never the kubectl output" {
  export APPLY_RC=1 APPLY_OUT="configmap/cfg configured
Invalid value: \"${CANARY_VALUE}\""
  export APPLY_ERR="Warning: the value \"${STDERR_CANARY}\" of ConfigMap cfg is deprecated"
  local level
  for level in "" 1; do
    DEBUG="${level}" run kapply::run_private "the objects" apply -f - <<< "${CFG_MANIFEST}"
    assert_equal "${status}" 1
    refute_output --partial "${CANARY_VALUE}"
    refute_output --partial "${STDERR_CANARY}"
    assert_line "$(_fixed_fail_line "the objects" 1)"
    assert_line '✓ the objects · 1 resource'
  done
  assert_line '[debug] kapply: kubectl apply for the objects exited 1 (1 resources)'
}

@test "run_private: a clean write prints the summary line, never the kubectl output, and kubectl reads the stdin of the caller" {
  kubectl() {
    cat > "${BATS_TEST_TMPDIR}/stdin"
    printf 'configmap/cfg serverside-applied\nsecret/s configured\nWarning: unknown field "data.%s"\n' "${CANARY_VALUE}"
  }
  run kapply::run_private "the objects" apply -f - <<< "${CFG_MANIFEST}"
  assert_success
  assert_output '✓ the objects · 2 resources'
  run cat "${BATS_TEST_TMPDIR}/stdin"
  assert_output "${CFG_MANIFEST}"
}

@test "run_private: the KUBERC of the caller does not reach kubectl, and xtrace shows no output" {
  export KUBERC=/user/kuberc APPLY_OUT="Invalid value: \"${CANARY_VALUE}\""
  local trace="${BATS_TEST_TMPDIR}/trace"
  ( set -x; kapply::run_private "the objects" apply -f /dev/null ) > /dev/null 2> "${trace}"
  run grep -c '^apply KUBERC=off$' "${KENV}"
  assert_output 1
  run grep -c "${CANARY_VALUE}" "${trace}"
  assert_output 0
  [ "${KUBERC}" = /user/kuberc ]
}

@test "run_private: LO_SHOW_SENSITIVE_OUTPUT=1 prints the kubectl output after a [warn] line, and no summary" {
  export LO_SHOW_SENSITIVE_OUTPUT=1 APPLY_RC=1 APPLY_OUT="configmap/cfg configured
Invalid value: \"${CANARY_VALUE}\"" APPLY_ERR="Warning: ${STDERR_CANARY}"
  run kapply::run_private "the objects" apply -f - <<< "${CFG_MANIFEST}"
  assert_equal "${status}" 1
  assert_line --index 0 '[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1: lo shows the kubectl output for the objects, which can quote a value of the manifest'
  assert_output --partial "${CANARY_VALUE}"
  assert_output --partial "${STDERR_CANARY}"
  refute_output --partial '✓ the objects'
  refute_output --partial 'kapply: kubectl apply failed for'
}

@test "counted_lines: keeps the <type>/<name> lines that end in an OK verb, nothing else" {
  run kapply::counted_lines < <(printf 'configmap/a serverside-applied\nWarning: %s configured\n  indented/x created\nInvalid value: "%s"\nsecret/b unchanged' "${CANARY_VALUE}" "${CANARY_VALUE}")
  assert_success
  assert_output $'configmap/a serverside-applied\nsecret/b unchanged'
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
