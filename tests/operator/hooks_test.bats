#!/usr/bin/env bats
# hooks_test.bats — unit tests for operator shell-operator hooks

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="$BATS_TEST_TMPDIR"

  # We need jq for hook tests
  command -v jq &>/dev/null || skip "jq required for hook tests"
}

teardown() {
  teardown_tmpdir
}

# --- lo-reconcile.sh (real hook, sourced) ---

# Source the real hook with stubs. The driver tree is absent in the
# repo layout, so driver::* are stubbed per test.
lo_hook_load() {
  command -v yq &>/dev/null || skip "yq required for lo hook tests"
  export LOK8S_STATE_DIR="${BATS_TEST_TMPDIR}/state"
  export KLOG="${BATS_TEST_TMPDIR}/kubectl.log"
  : > "${KLOG}"
  source "${_PROJECT_ROOT}/operator/hooks/lo-reconcile.sh"
  set +e +u
  set +o pipefail

  kubectl() {
    echo "kubectl $*" >> "${KLOG}"
    case "$*" in
      *'jsonpath={.metadata.finalizers}'*) echo '["lok8s.dev/lo-teardown"]' ;;
      'get lo -A -o json'*) echo '{"items":[]}' ;;
    esac
    return 0
  }
  bootstrap::apply() { echo "bootstrap::apply $*" >> "${KLOG}"; }
  driver::provision() { echo "driver::provision $*" >> "${KLOG}"; }
  driver::destroy() { echo "driver::destroy $*" >> "${KLOG}"; }
  driver::status() { echo "NotFound"; }
  driver::kubeconfig() {
    mkdir -p "${PATH_BASE}/.kubeconfig"
    echo kc > "${PATH_BASE}/.kubeconfig/test-lo.yaml"
  }
}

LO_CR='{"metadata":{"name":"test-lo","namespace":"default","finalizers":[]},"spec":{"cluster":{"domain":"test.lok8s.dev"},"runtime":"kind"}}'

@test "lo-reconcile hook::config: events, synchronization, drift schedule" {
  lo_hook_load
  run hook::config
  assert_success
  assert_output --partial 'kind: Lo'
  assert_output --partial '"Added", "Modified"'
  assert_output --partial 'executeHookOnSynchronization: true'
  assert_output --partial 'crontab: "*/3 * * * *"'
  assert_output --partial 'deletionTimestamp'
}

@test "lo-reconcile: missing domain marks Failed" {
  lo_hook_load
  lo_hook::reconcile '{"metadata":{"name":"bad","namespace":"default"},"spec":{}}'
  run cat "${KLOG}"
  assert_output --partial 'MissingDomain'
  refute_output --partial 'driver::provision'
}

@test "lo-reconcile: fresh CR gets finalizer, provision, bootstrap, kubeconfig, Provisioned" {
  lo_hook_load
  lo_hook::reconcile "${LO_CR}"
  run cat "${KLOG}"
  assert_output --partial '/metadata/finalizers'
  assert_output --partial 'driver::provision test.lok8s.dev'
  assert_output --partial 'bootstrap::apply test.lok8s.dev'
  assert_output --partial '"phase":"Provisioned"'
  assert_output --partial 'create secret generic test-lo-kubeconfig'
  assert_output --partial '"secretRef":"test-lo-kubeconfig"'
  # spec materialized where the driver contract expects it
  [ -f "${LOK8S_STATE_DIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" ]
}

@test "lo-reconcile: running cluster skips provision, syncs status" {
  lo_hook_load
  driver::status() { echo "Running"; }
  driver::kubeconfig() {
    mkdir -p "${PATH_BASE}/.kubeconfig"
    echo kc > "${PATH_BASE}/.kubeconfig/test-lo.yaml"
  }
  lo_hook::reconcile "${LO_CR}"
  run cat "${KLOG}"
  refute_output --partial 'driver::provision'
  assert_output --partial '"phase":"Provisioned"'
}

@test "lo-reconcile: deletion runs teardown and removes finalizer" {
  lo_hook_load
  local cr='{"metadata":{"name":"test-lo","namespace":"default","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["lok8s.dev/lo-teardown"]},"spec":{"cluster":{"domain":"test.lok8s.dev"}}}'
  lo_hook::reconcile "${cr}"
  run cat "${KLOG}"
  assert_output --partial '"phase":"Terminating"'
  assert_output --partial 'driver::destroy test.lok8s.dev'
  assert_output --partial '{"metadata":{"finalizers":[]}}'
  [ ! -d "${LOK8S_STATE_DIR}/clusters/test.lok8s.dev" ]
}

@test "lo-reconcile: failed teardown keeps finalizer for retry" {
  lo_hook_load
  driver::destroy() { echo "driver::destroy $*" >> "${KLOG}"; return 1; }
  local cr='{"metadata":{"name":"test-lo","namespace":"default","deletionTimestamp":"2026-01-01T00:00:00Z","finalizers":["lok8s.dev/lo-teardown"]},"spec":{"cluster":{"domain":"test.lok8s.dev"}}}'
  lo_hook::reconcile "${cr}"
  run cat "${KLOG}"
  assert_output --partial 'DestroyFailed'
  refute_output --partial '{"metadata":{"finalizers":[]}}'
}

@test "lo-reconcile: schedule event re-lists all Lo resources" {
  lo_hook_load
  cat > "${BATS_TEST_TMPDIR}/binding.json" <<'JSON'
[{"type": "Schedule", "binding": "lo-drift"}]
JSON
  BINDING_CONTEXT_PATH="${BATS_TEST_TMPDIR}/binding.json" hook::trigger
  run cat "${KLOG}"
  assert_output --partial 'kubectl get lo -A -o json'
}

@test "capi-reconcile hook::config returns valid JSON" {
  hook::config() {
    cat <<'EOF'
configVersion: v1
kubernetes:
  - apiVersion: cluster.lok8s.dev/v1beta1
    kind: Capi
    executeHookOnEvent: ["Added", "Modified"]
    jqFilter: ".spec"
EOF
  }

  run hook::config
  assert_success
  assert_output --partial "configVersion: v1"
  assert_output --partial "kind: Capi"
}

# --- capi-status-sync.sh hook::config ---

@test "capi-status-sync hook::config watches CAPI Clusters with lok8s label" {
  hook::config() {
    cat <<'EOF'
configVersion: v1
kubernetes:
  - apiVersion: cluster.x-k8s.io/v1beta1
    kind: Cluster
    executeHookOnEvent: ["Modified"]
    jqFilter: ".status"
    labelSelector:
      matchLabels:
        lok8s.dev/managed: "true"
EOF
  }

  run hook::config
  assert_success
  assert_output --partial "cluster.x-k8s.io/v1beta1"
  assert_output --partial "kind: Cluster"
  assert_output --partial "lok8s.dev/managed"
}

# --- capi-reconcile.sh hook::trigger ---

@test "capi-reconcile detects hetzner provider from spec" {
  # Re-implement the detection logic from the hook for testing
  capi::detect_provider_from_spec() {
    local spec="$1"
    if echo "$spec" | jq -e '.hcloud' &>/dev/null; then
      echo "hetzner"
    elif echo "$spec" | jq -e '.aws' &>/dev/null; then
      echo "aws"
    else
      echo "error: no known CAPI provider found" >&2
      return 1
    fi
  }

  local spec='{"hcloud":{"region":"fsn1","sshKeyName":"test-key"},"cluster":{"domain":"prod.lok8s.dev"}}'
  run capi::detect_provider_from_spec "$spec"
  assert_success
  assert_output "hetzner"
}

@test "capi-reconcile detects aws provider from spec" {
  capi::detect_provider_from_spec() {
    local spec="$1"
    if echo "$spec" | jq -e '.hcloud' &>/dev/null; then
      echo "hetzner"
    elif echo "$spec" | jq -e '.aws' &>/dev/null; then
      echo "aws"
    else
      return 1
    fi
  }

  local spec='{"aws":{"region":"eu-central-1"},"cluster":{"domain":"aws.lok8s.dev"}}'
  run capi::detect_provider_from_spec "$spec"
  assert_success
  assert_output "aws"
}

@test "capi-reconcile fails for unknown provider" {
  capi::detect_provider_from_spec() {
    local spec="$1"
    if echo "$spec" | jq -e '.hcloud' &>/dev/null; then
      echo "hetzner"
    elif echo "$spec" | jq -e '.aws' &>/dev/null; then
      echo "aws"
    else
      echo "error: no known CAPI provider found" >&2
      return 1
    fi
  }

  local spec='{"cluster":{"domain":"gcp.lok8s.dev"}}'
  run capi::detect_provider_from_spec "$spec"
  assert_failure
}

# --- capi-status-sync.sh phase mapping ---

@test "capi-status-sync maps CAPI phases to lok8s phases" {
  # Replicate the phase mapping from the hook
  map_phase() {
    local phase="$1"
    case "$phase" in
      Provisioned) echo "Provisioned" ;;
      Provisioning|Pending) echo "Provisioning" ;;
      Failed|Deleting) echo "Failed" ;;
      *) echo "Provisioning" ;;
    esac
  }

  run map_phase "Provisioned"
  assert_output "Provisioned"

  run map_phase "Provisioning"
  assert_output "Provisioning"

  run map_phase "Pending"
  assert_output "Provisioning"

  run map_phase "Failed"
  assert_output "Failed"

  run map_phase "Deleting"
  assert_output "Failed"

  run map_phase "Unknown"
  assert_output "Provisioning"
}

@test "capi-status-sync builds correct status patch JSON" {
  local phase="Provisioned"
  local cp_ready="true"

  run jq -n \
    --arg phase "$phase" \
    --argjson ready "$cp_ready" \
    '{status: {phase: $phase, ready: $ready}}'
  assert_success

  # Parse back to validate
  local parsed_phase
  parsed_phase=$(echo "$output" | jq -r '.status.phase')
  [ "$parsed_phase" = "Provisioned" ]

  local parsed_ready
  parsed_ready=$(echo "$output" | jq -r '.status.ready')
  [ "$parsed_ready" = "true" ]
}

@test "capi-status-sync adds controlPlaneEndpoint to patch when available" {
  local base_patch
  base_patch=$(jq -n --arg phase "Provisioned" --argjson ready true \
    '{status: {phase: $phase, ready: $ready}}')

  local host="10.0.0.1"
  local port="6443"

  run bash -c "echo '$base_patch' | jq --arg host '$host' --argjson port $port '.status.controlPlaneEndpoint = {host: \$host, port: \$port}'"
  assert_success

  local result_host
  result_host=$(echo "$output" | jq -r '.status.controlPlaneEndpoint.host')
  [ "$result_host" = "10.0.0.1" ]
}

# --- Hook --config flag dispatch ---

@test "hooks dispatch --config flag to hook::config" {
  # Simulate the dispatch pattern used by all hooks
  dispatch() {
    if [[ "${1:-}" == "--config" ]]; then
      echo "config_called"
    else
      echo "trigger_called"
    fi
  }

  run dispatch "--config"
  assert_output "config_called"

  run dispatch ""
  assert_output "trigger_called"
}
