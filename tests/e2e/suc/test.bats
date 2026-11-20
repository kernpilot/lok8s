#!/usr/bin/env bats
# E2E: suc — slot 135. The system-upgrade-controller addon on a real
# cluster, with one real Plan.
#
# The addon replaces the chart's cluster-admin binding with a narrowed
# RBAC (.lok8s/addons/system-upgrade-controller/rbac.yaml). The Go tests in
# internal/addons check that RBAC against a list of calls written by hand,
# and render the chart. Neither runs the controller, and neither pulls the
# two images that values.yaml pins by digest. This scenario does both:
#   1. The bootstrap applies the addon as shipped. The controller
#      Deployment becomes Available on its pinned image, no binding gives
#      its ServiceAccount cluster-admin, and the API server says that the
#      ServiceAccount cannot do everything.
#   2. The controller creates its Plan CRD.
#   3. One Plan (plan.yaml) runs as the controller's ServiceAccount, with
#      cordon, drain and a Secret. Its Job completes on the pinned kubectl
#      image, the drain evicts a pod, the node carries the Plan label, and
#      the node is schedulable again.
#   4. Neither the controller log nor the Job pod logs say "forbidden".
#   5. After a restart the controller runs the Plan again (a new version),
#      and its log and the new Job pod logs say nothing "forbidden".
# The controller container never restarts (steps 1, 4 and 5): a crash
# loop keeps the pod phase Running, and its log loses the run that
# crashed.
#
# Steps 4 and 5 check what step 3 left. When step 3 did not complete, they
# fail at once and say so.
#
# A missing rule shows as "forbidden" in one of those logs, or as a Plan
# that never completes. A digest that does not resolve shows as a pod
# that never starts. Every wait is bounded, and its failure names the step
# and prints what the cluster shows.
#
# The go leg only: the subject is the addon, not the implementation.
#
# The node runs 1.35, the addon's target, not the framework default: the
# drain's kubectl image works on 1.33 to 1.37 only (cluster.lok8s.yaml).
#
# The budget, at most: provision 300 s, two waits for the controller and
# the CRD (240 s each), two Plan runs (300 s each) and after each a wait
# of 60 s for the node to be schedulable, the restart (240 s) and the
# destroy (120 s). That is 31 minutes, under the 60 of the CI job.

: "${E2E_SUC_READY_TIMEOUT:=240}"   # the controller Available, the CRD Established
: "${E2E_SUC_PLAN_TIMEOUT:=300}"    # one Plan run: Job images, cordon, drain, upgrade

_NS=cattle-system
_DEPLOY=system-upgrade-controller
_PLAN=e2e-suc

setup_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::require_e2e_enabled
  # init first: it puts the project's own toolchain on PATH, so the tool
  # check below reports on the binaries the run will actually use.
  e2e::init "${BATS_TEST_DIRNAME}" 135.lok8s.dev
  e2e::require_tools docker kind kubectl kustomize jq yq dig
  e2e::require_dns 135.lok8s.dev
  e2e::require_binary
  e2e::banner
  e2e::assert_provenance
  e2e::snapshot_world
  # A run must not read the pods, the logs or the Plan hash of an earlier
  # run.
  rm -rf -- "${E2E_STATE:?}/suc-run-1" "${E2E_STATE:?}/suc-run-2" "${E2E_STATE:?}/suc-run-1.hash"
  e2e::provision
}

teardown_file() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 135.lok8s.dev
  e2e::final_teardown
}

setup() {
  load "${BATS_TEST_DIRNAME}/../lib/helpers"
  e2e::init "${BATS_TEST_DIRNAME}" 135.lok8s.dev
  # The addon copy that the binary under test renders: the project's
  # .lok8s, which is the repo tree on the go leg.
  _VALUES="${PATH_LOK8S}/addons/system-upgrade-controller/values.yaml"
  # kind names the one node of the cluster after the cluster.
  _NODE="${LOK8S_CLUSTER_NAME}-control-plane"
}

# _kc <args...> — kubectl on the scenario's cluster. Bounded twice: the
# request timeout, and `timeout` for a call that hangs anywhere else.
_kc() {
  timeout 60 kubectl --kubeconfig "${KUBECONFIG}" --request-timeout=30s "$@" </dev/null
}

# _wait <seconds> <check...> — run <check> every 5 s until it succeeds or
# <seconds> pass. Returns 1 on a timeout. The caller names the step.
_wait() {
  local deadline=$(( SECONDS + $1 ))
  shift
  until "$@"; do
    (( SECONDS < deadline )) || return 1
    sleep 5
  done
}

# _pinned_digest <values key> — the sha256 digest that values.yaml pins
# for <key> (systemUpgradeController or kubectl). Empty when there is none.
_pinned_digest() {
  yq -r ".${1}.image.tag" "${_VALUES}" | grep -oE 'sha256:[0-9a-f]{64}$' || true
}

# _owned_pods <kind> — the pods in cattle-system that a <kind> owns. A
# ReplicaSet owns the controller pod. Ownership, not labels: the chart
# and the controller choose the labels.
_owned_pods() {
  local json
  json="$(_kc -n "${_NS}" get pods -o json)" || return 1
  jq -r --arg kind "$1" \
    '.items[] | select(any(.metadata.ownerReferences[]?; .kind == $kind)) | .metadata.name' \
    <<<"${json}"
}

# _plan_pods — one line "<pod> <job>" for each Plan pod in cattle-system:
# a pod that a Job owns, and the name of that Job. The Job name holds the
# Plan hash, so each Plan run has its own Job, and a retry pod belongs to
# the Job of its run.
_plan_pods() {
  local json
  json="$(_kc -n "${_NS}" get pods -o json)" || return 1
  jq -r '.items[]
    | .metadata.name as $pod
    | .metadata.ownerReferences[]? | select(.kind == "Job")
    | "\($pod) \(.name)"' <<<"${json}"
}

# _jobs_in <dir> — the names of the Jobs that own the pods saved in <dir>.
_jobs_in() {
  local -a pods=("$1"/*.json)
  [[ -e "${pods[0]}" ]] || return 0
  jq -r '.metadata.ownerReferences[]? | select(.kind == "Job") | .name' "${pods[@]}" | sort -u
}

# _capture_jobs <dir> [<skip-dir>] — save the status and the log of each
# container of every Plan pod into <dir>: <pod>.json and
# <pod>.<container>.log. A pod of a Job that <skip-dir> holds is left
# out, so a late retry pod of the first run is not a pod of the second.
# The controller can delete a finished Job before the test reads it, so
# the waits call this on every poll, and a read that fails keeps the last
# copy.
_capture_jobs() {
  local dir="$1" skip="${2:-}" old="" list pod job c out
  mkdir -p "${dir}"
  [[ -z "${skip}" ]] || old="$(_jobs_in "${skip}")"
  list="$(_plan_pods)" || return 0
  while read -r pod job; do
    [[ -n "${pod}" ]] || continue
    ! grep -qxF "${job}" <<<"${old}" || continue
    _kc -n "${_NS}" get pod "${pod}" -o json > "${dir}/${pod}.json.tmp" 2>/dev/null \
      && mv "${dir}/${pod}.json.tmp" "${dir}/${pod}.json"
    [[ -s "${dir}/${pod}.json" ]] || continue
    for c in $(jq -r '(.spec.initContainers // [])[].name, .spec.containers[].name' "${dir}/${pod}.json"); do
      out="$(_kc -n "${_NS}" logs "${pod}" -c "${c}" 2>/dev/null)" || continue
      [[ -z "${out}" ]] || printf '%s\n' "${out}" > "${dir}/${pod}.${c}.log"
    done
  done <<<"${list}"
  return 0
}

# _save_controller_logs <dir> — the log of each controller pod, as
# <dir>/controller.<pod>.log. Fails when there is no controller pod or
# its log is empty: a check over no log would pass whatever it says.
_save_controller_logs() {
  local dir="$1" pod pods
  mkdir -p "${dir}"
  pods="$(_owned_pods ReplicaSet)" || return 1
  [[ -n "${pods}" ]] || { echo "no controller pod in ${_NS}" >&2; return 1; }
  for pod in ${pods}; do
    _kc -n "${_NS}" logs "${pod}" > "${dir}/controller.${pod}.log" || return 1
    [[ -s "${dir}/controller.${pod}.log" ]] || { echo "the log of ${pod} is empty" >&2; return 1; }
  done
}

# _forbidden <dir> — every line of the logs in <dir> that says
# "forbidden", with its file. Fails when <dir> holds no log at all.
_forbidden() {
  local -a logs=("$1"/*.log)
  [[ -e "${logs[0]}" ]] || { echo "no log in $1" >&2; return 1; }
  grep -iH 'forbidden' "${logs[@]}" || true
}

# _diagnose [<dir>] — what the cluster shows, for a failed step. bats
# prints it with the failure. Every read is best-effort: a broken cluster
# is the case this exists for.
_diagnose() (
  set +e
  echo "--- Plan ${_NS}/${_PLAN} (status)"
  _kc -n "${_NS}" get plan "${_PLAN}" -o yaml 2>&1 | sed -n '/^status:/,$p'
  echo "--- nodes"
  _kc get nodes -o wide -L "plan.upgrade.cattle.io/${_PLAN}" 2>&1
  echo "--- pods in ${_NS}"
  _kc -n "${_NS}" get pods -o wide 2>&1
  echo "--- events in ${_NS} (the last 40)"
  _kc -n "${_NS}" get events --sort-by=.lastTimestamp 2>&1 | tail -n 40
  for pod in $(_owned_pods ReplicaSet); do
    echo "--- controller log ${pod} (the last 60 lines)"
    _kc -n "${_NS}" logs "${pod}" --tail=60 2>&1
  done
  if [[ -n "${1:-}" ]]; then
    echo "--- the lines that say forbidden in ${1##*/}"
    grep -iH 'forbidden' "$1"/*.log 2>&1
    for f in "$1"/*.log; do
      [[ -e "${f}" ]] || continue
      echo "--- ${f##*/} (the last 40 lines)"
      tail -n 40 "${f}"
    done
  fi
)

_deployment_available() {
  [[ "$(_kc -n "${_NS}" get deployment "${_DEPLOY}" \
    -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)" == "True" ]]
}

_crd_established() {
  [[ "$(_kc get crd plans.upgrade.cattle.io \
    -o jsonpath='{.status.conditions[?(@.type=="Established")].status}' 2>/dev/null)" == "True" ]]
}

# _cluster_admin_bindings — the ClusterRoleBindings that give the
# controller's ServiceAccount cluster-admin. Fails when the list cannot be
# read, so an unreadable list is not an empty one.
_cluster_admin_bindings() {
  local json
  json="$(_kc get clusterrolebindings -o json)" || return 1
  jq -r --arg ns "${_NS}" --arg sa "${_DEPLOY}" '
    .items[]
    | select(.roleRef.kind == "ClusterRole" and .roleRef.name == "cluster-admin")
    | select(any(.subjects[]?; .kind == "ServiceAccount" and .name == $sa and .namespace == $ns))
    | .metadata.name' <<<"${json}"
}

# _controller_restarts — one line "<pod>/<container> <count>" for each
# container of a controller pod that restarted.
_controller_restarts() {
  local json
  json="$(_kc -n "${_NS}" get pods -o json)" || return 1
  jq -r '.items[]
    | select(any(.metadata.ownerReferences[]?; .kind == "ReplicaSet"))
    | .metadata.name as $pod
    | (.status.containerStatuses // [])[]
    | select(.restartCount > 0)
    | "\($pod)/\(.name) \(.restartCount)"' <<<"${json}"
}

# _assert_no_restarts <step> — no container of a controller pod
# restarted. A crash loop keeps the pod phase Running, and a log read
# without --previous loses what the crashed run said, so on a failure
# this prints that earlier log.
_assert_no_restarts() {
  local restarts pod
  restarts="$(_controller_restarts)" || fail "$1: the controller pods could not be read"
  [[ -n "${restarts}" ]] || return 0
  for pod in $(_owned_pods ReplicaSet); do
    echo "--- controller log ${pod}, before its last restart (the last 60 lines)"
    _kc -n "${_NS}" logs "${pod}" --previous --tail=60 2>&1 || true
  done
  fail "$1: the controller restarted (${restarts//$'\n'/, })"
}

# _plan_done <dir> [<skip-dir> <old hash>] — the Plan ran on the node at
# its latest hash: the node label equals status.latestHash, and that hash
# is not <old hash>. After a new version, the old hash still equals the
# node label until the controller writes the new one. It saves the Plan
# pods into <dir> first, on every poll.
_plan_done() {
  _capture_jobs "$1" "${2:-}"
  local hash label
  hash="$(_kc -n "${_NS}" get plan "${_PLAN}" -o jsonpath='{.status.latestHash}' 2>/dev/null)" || return 1
  label="$(_kc get node "${_NODE}" \
    -o jsonpath="{.metadata.labels.plan\.upgrade\.cattle\.io/${_PLAN}}" 2>/dev/null)" || return 1
  [[ -n "${hash}" && "${hash}" != "${3:-}" && "${label}" == "${hash}" ]]
}

_node_schedulable() {
  [[ "$(_kc get node "${_NODE}" -o jsonpath='{.spec.unschedulable}' 2>/dev/null)" != "true" ]]
}

# _require_step3 <step> — step 3 completed: it wrote the Plan hash of the
# first run. Steps 4 and 5 check what that run left, so without it they
# fail at once.
_require_step3() {
  [[ -s "${E2E_STATE}/suc-run-1.hash" ]] \
    || fail "$1: step 3 did not complete, so there is no Plan run to check"
}

# _assert_run <dir> <step> — what one completed Plan run left in <dir>: a
# Plan pod that succeeded, with an init container on the pinned kubectl
# image (the drain) that exited 0 and evicted a pod, and the line of the
# upgrade container that proves the Secret was mounted. Then the node is
# schedulable again.
_assert_run() {
  local dir="$1" step="$2" digest ran pod c evicted=""
  digest="$(_pinned_digest kubectl)"
  [[ -n "${digest}" ]] || fail "${step}: no sha256 digest under kubectl.image.tag in ${_VALUES}"
  local -a pods=("${dir}"/*.json)
  [[ -e "${pods[0]}" ]] || fail "${step}: no Plan pod was seen in ${_NS}"
  ran="$(jq -r --arg d "${digest}" '
    select(.status.phase == "Succeeded")
    | . as $pod
    | (.spec.initContainers // [])[]
    | select(.image | endswith("@" + $d))
    | .name as $n
    | ($pod.status.initContainerStatuses // [])[]
    | select(.name == $n and .state.terminated.exitCode == 0)
    | "\($pod.metadata.name)/\($n)"' "${pods[@]}")"
  [[ -n "${ran}" ]] || fail "${step}: no Plan pod succeeded with an init container on the pinned kubectl image (${digest}). Init containers seen:
$(jq -r '"\(.metadata.name) \(.status.phase): " + ([(.status.initContainerStatuses // [])[] | "\(.name)=\(.image) \(.state | keys | join(","))"] | join("; "))' "${pods[@]}")"
  e2e::log "e2e: the drain ran on the pinned kubectl image: ${ran//$'\n'/ }"
  # kubectl drain prints "pod/<name> evicted" for each pod that it evicts,
  # and kind runs CoreDNS on the node. Without that line the run did not
  # use pods/eviction.
  while IFS=/ read -r pod c; do
    if grep -qsF 'evicted' "${dir}/${pod}.${c}.log"; then evicted=1; fi
  done <<<"${ran}"
  [[ -n "${evicted}" ]] \
    || fail "${step}: the drain log of ${ran//$'\n'/ } names no evicted pod, so the run did not use pods/eviction"
  grep -qsF 'e2e-suc: the Plan Secret is mounted' "${dir}"/*.log \
    || fail "${step}: the upgrade container did not print the line that proves the Secret was mounted"
  _wait 60 _node_schedulable || {
    _diagnose "${dir}"
    fail "${step}: node ${_NODE} is still unschedulable 60 s after the Plan completed"
  }
}

# _only_new_controller <old pods> — every controller pod is new (none of
# <old pods>) and Running. A pod that terminates is still listed, so this
# is false until the old pod is gone.
_only_new_controller() {
  local now pod
  now="$(_owned_pods ReplicaSet)" || return 1
  [[ -n "${now}" ]] || return 1
  for pod in ${now}; do
    ! grep -qxF "${pod}" <<<"$1" || return 1
    [[ "$(_kc -n "${_NS}" get pod "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null)" == "Running" ]] || return 1
  done
}

@test "step 1: the controller is Available on its pinned image, without cluster-admin" {
  local digest answer
  digest="$(_pinned_digest systemUpgradeController)"
  [[ -n "${digest}" ]] || fail "step 1: no sha256 digest under systemUpgradeController.image.tag in ${_VALUES}"
  _wait "${E2E_SUC_READY_TIMEOUT}" _deployment_available || {
    _diagnose
    fail "step 1 (the controller Deployment ${_NS}/${_DEPLOY} is Available): timed out after ${E2E_SUC_READY_TIMEOUT} s"
  }
  # Available means the pod runs, and the runtime pulls by the digest, so
  # the pinned digest resolved.
  run _kc -n "${_NS}" get deployment "${_DEPLOY}" -o jsonpath='{.spec.template.spec.containers[0].image}'
  assert_success
  assert_output --partial "@${digest}"

  # The narrowed RBAC is the subject. A binding that still gives the
  # ServiceAccount cluster-admin would let every later step pass whatever
  # rbac.yaml says.
  run _cluster_admin_bindings
  assert_success
  assert_output ""
  run _kc get clusterrolebinding lok8s-system-upgrade-controller lok8s-system-upgrade-controller-drainer -o name
  assert_success
  # A RoleBinding or a group subject can give the same, and the list above
  # does not see them. The API server answers for the ServiceAccount, with
  # its groups. kubectl exits 1 for "no", so the answer is the check.
  answer="$(_kc auth can-i '*' '*' --all-namespaces \
    --as="system:serviceaccount:${_NS}:${_DEPLOY}" 2>/dev/null)" || true
  [[ "${answer}" == "no" || "${answer}" == "no - "* ]] \
    || fail "step 1: kubectl auth can-i '*' '*' --all-namespaces as system:serviceaccount:${_NS}:${_DEPLOY} answers '${answer}', not no"
  _assert_no_restarts "step 1"
}

@test "step 2: the controller creates its Plan CRD" {
  _wait "${E2E_SUC_READY_TIMEOUT}" _crd_established || {
    _diagnose
    fail "step 2 (the CRD plans.upgrade.cattle.io is Established): timed out after ${E2E_SUC_READY_TIMEOUT} s"
  }
}

@test "step 3: one Plan runs as the controller's ServiceAccount, with cordon, drain and a Secret" {
  local run1="${E2E_STATE}/suc-run-1" hash
  run _kc apply -f "${BATS_TEST_DIRNAME}/plan.yaml"
  assert_success
  _wait "${E2E_SUC_PLAN_TIMEOUT}" _plan_done "${run1}" || {
    _diagnose "${run1}"
    fail "step 3 (the Plan completes and node ${_NODE} carries plan.upgrade.cattle.io/${_PLAN}): timed out after ${E2E_SUC_PLAN_TIMEOUT} s"
  }
  _capture_jobs "${run1}"
  _assert_run "${run1}" "step 3"
  # Steps 4 and 5 start from this file. Step 5 waits for a hash that is
  # not this one.
  hash="$(_kc -n "${_NS}" get plan "${_PLAN}" -o jsonpath='{.status.latestHash}')" \
    || fail "step 3: the Plan ${_NS}/${_PLAN} could not be read after its run"
  [[ -n "${hash}" ]] || fail "step 3: the Plan has no status.latestHash after its run"
  printf '%s\n' "${hash}" > "${E2E_STATE}/suc-run-1.hash"
}

@test "step 4: no forbidden in the controller log or the Plan pod logs" {
  local run1="${E2E_STATE}/suc-run-1"
  _require_step3 "step 4"
  local -a pods=("${run1}"/*.json)
  [[ -e "${pods[0]}" ]] || fail "step 4: ${run1} holds no Plan pod, so there are no Plan pod logs to check"
  _assert_no_restarts "step 4"
  _save_controller_logs "${run1}" || fail "step 4: the controller log could not be read"
  run _forbidden "${run1}"
  assert_success
  assert_output ""
}

@test "step 5: after a restart the controller runs the Plan again, and logs no forbidden" {
  local run1="${E2E_STATE}/suc-run-1" run2="${E2E_STATE}/suc-run-2" hash1 before
  _require_step3 "step 5"
  hash1="$(<"${E2E_STATE}/suc-run-1.hash")"
  before="$(_owned_pods ReplicaSet)"
  [[ -n "${before}" ]] || fail "step 5: no controller pod before the restart"
  run _kc -n "${_NS}" rollout restart deployment "${_DEPLOY}"
  assert_success
  # The second start reads and updates the Plan CRD and takes the lease
  # again: calls that the first start made once.
  _wait "${E2E_SUC_READY_TIMEOUT}" _only_new_controller "${before}" || {
    _diagnose
    fail "step 5 (a new controller pod replaces $(tr '\n' ' ' <<<"${before}")and runs): timed out after ${E2E_SUC_READY_TIMEOUT} s"
  }
  run _kc -n "${_NS}" patch plan "${_PLAN}" --type merge -p '{"spec":{"version":"e2e-2"}}'
  assert_success
  _wait "${E2E_SUC_PLAN_TIMEOUT}" _plan_done "${run2}" "${run1}" "${hash1}" || {
    _diagnose "${run2}"
    fail "step 5 (the Plan completes again after the restart, at a hash that is not ${hash1}): timed out after ${E2E_SUC_PLAN_TIMEOUT} s"
  }
  _capture_jobs "${run2}" "${run1}"
  _assert_run "${run2}" "step 5"
  _assert_no_restarts "step 5"
  _save_controller_logs "${run2}" || fail "step 5: the log of the new controller pod could not be read"
  run _forbidden "${run2}"
  assert_success
  assert_output ""
}
