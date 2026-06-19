#!/usr/bin/env bats
# capi_bootstrap_test.bats — unit tests for capi::bootstrap in .lok8s/drivers/capi/main

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/provider.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/generate"
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  # argsh `:args` builtin — minimal stub: scan the caller's `args`
  # array for bare positional names and assign each to the matching
  # positional arg. Covers the `:args "desc" "$@"` pattern used by
  # driver::provision etc.
  :args() {
    shift  # drop description
    local -a _pos_names=()
    local i
    for ((i=0; i<${#args[@]}; i+=2)); do
      [[ "${args[i]}" == *"|"* ]] && continue
      _pos_names+=("${args[i]}")
    done
    local name
    for name in "${_pos_names[@]}"; do
      if (( $# > 0 )); then
        printf -v "${name}" '%s' "$1"
        shift
      fi
    done
  }
  export -f :args

  # kubehz hooks — stub as no-ops (tested separately).
  kubehz::read_config() { :; }
  kubehz::validate_config() { :; }
  kubehz::register_cluster() { :; }
  export -f kubehz::read_config kubehz::validate_config kubehz::register_cluster

  # capi::wait_ready polls kubectl on a real 600s loop — stub it so
  # bootstrap tests don't block on a cluster that isn't coming up.
  capi::wait_ready() { return 0; }
  export -f capi::wait_ready

  # Copy CAPI templates to tmpdir
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster"
  cp -r "${_PROJECT_ROOT}/.lok8s/drivers/capi/cluster/"* \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster/"

  # Create management cluster spec fixture at the canonical location
  # (capi::bootstrap reads PATH_CLUSTERS/<domain>/cluster.lok8s.yaml).
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/mgmt.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/mgmt.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: mgmt-production
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: mgmt.lok8s.dev
    namespace: capi-system
  managementCluster:
    domain: mgmt.lok8s.dev
  credentials:
    secretName: mgmt-credentials
  provider:
    name: hetzner
    config:
      region: fsn1
      sshKeyName: admin-key
  hcloud:
    region: fsn1
    sshKeyName: admin-key
  controlPlane:
    replicas: 1
    type: cax21
YAML

  # Legacy compat: some tests still reference this path by variable
  cp "${BATS_TEST_TMPDIR}/clusters/mgmt.lok8s.dev/cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/mgmt-cluster.lok8s.yaml"

  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
}

teardown() {
  teardown_tmpdir
}

# --- capi::bootstrap ---

@test "capi::bootstrap calls clusterctl init with correct provider" {
  kind() {
    case "$1" in
      get)
        if [[ "$2" == "clusters" ]]; then echo ""; fi
        if [[ "$2" == "kubeconfig" ]]; then echo "kubeconfig-content"; fi
        ;;
      create) return 0 ;;
      delete) return 0 ;;
    esac
  }
  export -f kind

  # Log clusterctl invocations to a file so we can inspect them after
  # `run` returns (subshell state doesn't propagate to locals).
  local clusterctl_log="${BATS_TEST_TMPDIR}/clusterctl.log"
  export CLUSTERCTL_LOG="${clusterctl_log}"
  clusterctl() {
    echo "clusterctl $*" >> "${CLUSTERCTL_LOG}"
    case "$1" in
      get) echo "kubeconfig-content" ;;
    esac
  }
  export -f clusterctl

  kubectl() { return 0; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  export HCLOUD_TOKEN="test-token"

  run capi::bootstrap "mgmt.lok8s.dev"
  assert_success
  assert_output --partial "creating bootstrap cluster"
  assert_output --partial "installing CAPI on bootstrap cluster"
  assert_output --partial "cleaning up bootstrap cluster"

  # clusterctl init was called with --infrastructure hetzner
  run grep -E 'clusterctl init .*--infrastructure hetzner' "${clusterctl_log}"
  assert_success
}

@test "capi::bootstrap creates and removes bootstrap kind cluster" {
  local create_called="" delete_called="" cluster_name=""

  kind() {
    case "$1" in
      get)
        if [[ "$2" == "clusters" ]]; then echo ""; fi
        if [[ "$2" == "kubeconfig" ]]; then echo "kubeconfig-content"; fi
        ;;
      create)
        create_called="true"
        while [[ $# -gt 0 ]]; do
          if [[ "$1" == "--name" ]]; then cluster_name="$2"; fi
          shift
        done
        ;;
      delete)
        delete_called="true"
        ;;
    esac
  }
  export -f kind

  clusterctl() {
    case "$1" in
      init) return 0 ;;
      get) echo "kubeconfig-content" ;;
      move) return 0 ;;
    esac
  }
  export -f clusterctl

  kubectl() { return 0; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  export HCLOUD_TOKEN="test-token"

  run capi::bootstrap "mgmt.lok8s.dev"
  assert_success
  assert_output --partial "bootstrap"
}

@test "capi::bootstrap skips kind create if bootstrap cluster exists" {
  kind() {
    case "$1" in
      get)
        if [[ "$2" == "clusters" ]]; then echo "lok8s-bootstrap"; fi
        if [[ "$2" == "kubeconfig" ]]; then echo "kubeconfig-content"; fi
        ;;
      create) echo "should-not-be-called"; return 1 ;;
      delete) return 0 ;;
    esac
  }
  export -f kind

  clusterctl() {
    case "$1" in
      init) return 0 ;;
      get) echo "kubeconfig-content" ;;
      move) return 0 ;;
    esac
  }
  export -f clusterctl

  kubectl() { return 0; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  export HCLOUD_TOKEN="test-token"

  run capi::bootstrap "mgmt.lok8s.dev"
  assert_success
  refute_output --partial "should-not-be-called"
}

@test "capi::bootstrap fails for unsupported provider" {
  cat > "${BATS_TEST_TMPDIR}/gcp-cluster.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-gcp
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: gcp.lok8s.dev
  managementCluster:
    domain: gcp.lok8s.dev
  gcp:
    region: us-central1
YAML

  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  run capi::bootstrap "gcp.lok8s.dev" "${BATS_TEST_TMPDIR}/gcp-cluster.yaml"
  assert_failure
}

# --- driver::provision (bootstrap path) ---

@test "driver::provision triggers bootstrap when domain equals mgmt_domain and no kubeconfig" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  local bootstrap_called=""

  # Override capi::bootstrap to track calls
  capi::bootstrap() {
    bootstrap_called="true"
    echo "info: bootstrapping management cluster $1" >&2
  }
  export -f capi::bootstrap

  run driver::provision "mgmt.lok8s.dev"
  assert_success
  assert_output --partial "bootstrapping management cluster"
}

@test "driver::provision returns error when mgmt kubeconfig missing and not self-referencing" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  # Cluster that references a different mgmt domain — placed at the
  # canonical location so driver::provision finds it by domain.
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/prod.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/prod.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: work-prod
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: prod.lok8s.dev
  managementCluster:
    domain: mgmt.lok8s.dev
  provider:
    name: hetzner
  hcloud:
    region: fsn1
    sshKeyName: key
YAML

  run driver::provision "prod.lok8s.dev"
  assert_failure
  assert_output --partial "management cluster kubeconfig not found"
  assert_output --partial "provision the management cluster first"
}

# --- Stub removal verification ---

@test "capi::detect_provider is provided by .lok8s/drivers/capi/generate (not stub)" {
  # After sourcing both files, capi::detect_provider should be the .lok8s/drivers/capi/generate version
  # which uses 'error' function instead of 'echo "error:"'
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi

  cat > "${BATS_TEST_TMPDIR}/unknown.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test
spec:
  kubernetes:
    version: "v1.31.10"
YAML

  run capi::detect_provider "${BATS_TEST_TMPDIR}/unknown.yaml"
  assert_failure
  assert_output --partial "No provider found in cluster spec"
}

@test "capi::generate is provided by .lok8s/drivers/capi/generate (not stub)" {
  if ! command -v yq &>/dev/null; then
    skip "yq required for generate test"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/mgmt-cluster.lok8s.yaml" "hetzner"
  assert_success
  assert_output --partial "kind: Cluster"
}

@test "capi::wait_ready is provided by .lok8s/drivers/capi/generate (not stub)" {
  kubectl() { echo "Provisioned"; }
  export -f kubectl

  run capi::wait_ready "/tmp/test.yaml" "test-cluster" 5
  assert_success
}
