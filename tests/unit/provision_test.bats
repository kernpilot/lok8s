#!/usr/bin/env bats
# provision_test.bats — unit tests for .lok8s/libs/provision

setup() {
  load "../test_helper"
  setup_tmpdir

  # Create a fake .lok8s domain structure in tmpdir
  export PATH_BASE="${BATS_TEST_TMPDIR}"
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo"

  # Copy Lo fixture as cluster spec
  cp "${FIXTURES_DIR}/lo-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  # Stub import since we're not in argsh
  import() { :; }
  export -f import

  # Stub kubehz hooks (libs/provision calls them; tested separately)
  kubehz::read_config() { :; }
  kubehz::validate_config() { return 0; }
  kubehz::register_cluster() { :; }
  export -f kubehz::read_config kubehz::validate_config kubehz::register_cluster
  export LOK8S_KUBEHZ_ACCESS="none"

  # Stub bootstrap::apply (tested separately in bootstrap_test.bats)
  bootstrap::apply() { :; }
  export -f bootstrap::apply
}

teardown() {
  teardown_tmpdir
}

# --- provision::resolve_spec ---

@test "provision::resolve_spec resolves cluster.lok8s.yaml" {
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  provision::resolve_spec "test.lok8s.dev"
  [ "${LOK8S_SPEC_KIND}" = "cluster" ]
  [ -n "${LOK8S_SPEC_FILE}" ]
  [[ "${LOK8S_SPEC_FILE}" == *"cluster.lok8s.yaml" ]]
}

@test "provision::resolve_spec resolves deploy.lok8s.yaml" {
  # Remove cluster spec and add deploy spec
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/deploy.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  provision::resolve_spec "test.lok8s.dev"
  [ "${LOK8S_SPEC_KIND}" = "deploy" ]
  [[ "${LOK8S_SPEC_FILE}" == *"deploy.lok8s.yaml" ]]
}

@test "provision::resolve_spec fails for missing domain" {
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::resolve_spec "nonexistent.domain"
  assert_failure
}

# --- provision::dispatch ---

@test "provision::dispatch reads .kind from cluster spec and sources kind script" {
  # Mock yq to return "Lo" as the kind
  yq() {
    case "$1" in
      -r)
        case "$2" in
          .kind) echo "Lo" ;;
          '.spec.gitops.provider // ""') echo "" ;;
          *) echo "" ;;
        esac
        ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Create a fake kind script that implements the contract
  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() {
  echo "provision_called: $1 $2"
}
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch "test.lok8s.dev"
  assert_success
  assert_output --partial "provision_called"
}

@test "provision::dispatch fails for deploy domains" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/deploy.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch "test.lok8s.dev"
  assert_failure
  assert_output --partial "Cannot provision a deployment domain"
}

@test "provision::dispatch fails for unknown kind" {
  yq() {
    case "$2" in
      .kind) echo "UnknownKind" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch "test.lok8s.dev"
  assert_failure
  assert_output --partial "Unknown cluster kind"
}

@test "provision::dispatch fails when kind script missing driver::provision" {
  yq() {
    case "$2" in
      .kind) echo "Lo" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Create a kind script that does NOT implement driver::provision
  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
# Missing driver::provision on purpose
driver::helper() { echo "helper"; }
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch "test.lok8s.dev"
  assert_failure
  assert_output --partial "Driver contract violation"
}

@test "provision::dispatch --bootstrap skips driver::provision but runs driver::export + bootstrap::apply" {
  yq() {
    case "$2" in
      .kind) echo "Lo" ;;
      .metadata.name) echo "test-cluster" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # An existing kubeconfig so the --bootstrap guard passes
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  touch "${BATS_TEST_TMPDIR}/.kubeconfig/test-cluster.yaml"

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() { echo "provision_called"; }
driver::export() { echo "export_called"; }
SCRIPT

  # Marker for bootstrap::apply (setup stubs it to a no-op)
  bootstrap::apply() { echo "bootstrap_applied: $1"; }
  export -f bootstrap::apply

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # bootstrap_only passed as the 2nd dispatch arg (the -b flag, provision-scoped)
  run provision::dispatch "test.lok8s.dev" 1
  assert_success
  assert_output --partial "export_called"
  assert_output --partial "bootstrap_applied: test.lok8s.dev"
  [[ "${output}" != *"provision_called"* ]]
}

@test "provision::dispatch --bootstrap fails when the cluster is not provisioned" {
  yq() {
    case "$2" in
      .kind) echo "Lo" ;;
      .metadata.name) echo "test-cluster" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() { echo "provision_called"; }
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # No .kubeconfig/test-cluster.yaml → the guard fails before any provision
  run provision::dispatch "test.lok8s.dev" 1
  assert_failure
  assert_output --partial "existing cluster"
  [[ "${output}" != *"provision_called"* ]]
}

# --- provision::dispatch_destroy ---

@test "provision::dispatch_destroy calls driver::destroy" {
  yq() {
    case "$2" in
      .kind) echo "Lo" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::destroy() {
  echo "destroy_called: $1 $2"
}
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch_destroy "test.lok8s.dev"
  assert_success
  assert_output --partial "destroy_called"
}

@test "provision::dispatch_destroy fails for deploy domains" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/deploy.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch_destroy "test.lok8s.dev"
  assert_failure
  assert_output --partial "Cannot destroy a deployment domain"
}

# --- provision::dispatch_status ---

@test "provision::dispatch_status calls driver::status" {
  yq() {
    case "$2" in
      .kind) echo "Lo" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::status() {
  echo "Running"
}
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch_status "test.lok8s.dev"
  assert_success
  assert_output "Running"
}

# --- ClusterRef chain resolution ---

@test "provision::dispatch_status follows clusterRef for deploy domains" {
  # Create deploy domain that references the cluster domain
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/staging.lok8s.dev"
  mkdir -p "${deploy_dir}"

  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" "${deploy_dir}/deploy.lok8s.yaml"

  yq() {
    case "$1" in
      -r)
        case "$2" in
          .kind) echo "Lo" ;;
          '.spec.clusterRef.domain // ""') echo "test.lok8s.dev" ;;
          *) echo "" ;;
        esac
        ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::status() {
  echo "Running"
}
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # dispatch_status on the deploy domain should follow clusterRef to test.lok8s.dev
  run provision::dispatch_status "staging.lok8s.dev"
  assert_success
  assert_output "Running"
}

@test "provision::dispatch_status fails for deploy domain without clusterRef" {
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/orphan.lok8s.dev"
  mkdir -p "${deploy_dir}"

  cat > "${deploy_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: orphan-apps
spec: {}
YAML

  yq() {
    case "$2" in
      '.spec.clusterRef.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch_status "orphan.lok8s.dev"
  assert_failure
  assert_output --partial "missing spec.clusterRef.domain"
}

@test "provision::resolve_spec prefers cluster.lok8s.yaml over deploy.lok8s.yaml" {
  # Create both spec files
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/deploy.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  provision::resolve_spec "test.lok8s.dev"
  [ "${LOK8S_SPEC_KIND}" = "cluster" ]
  [[ "${LOK8S_SPEC_FILE}" == *"cluster.lok8s.yaml" ]]
}

@test "provision::dispatch invokes driver::post_provision when defined" {
  # driver::bootstrap was removed — bootstrap is now framework-level
  # (libs/bootstrap). The remaining optional hook is driver::post_provision.
  yq() {
    case "$1" in
      -r)
        case "$2" in
          .kind) echo "Lo" ;;
          '.spec.gitops.provider // ""') echo "" ;;
          *) echo "" ;;
        esac
        ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() {
  echo "provisioned"
}
driver::post_provision() {
  echo "post_provisioned"
}
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::dispatch "test.lok8s.dev"
  assert_success
  assert_output --partial "post_provisioned"
}

@test "provision::dispatch triggers gitops bootstrap when configured" {
  local gitops_called=""

  yq() {
    case "$1" in
      -r)
        case "$2" in
          .kind) echo "Lo" ;;
          '.spec.gitops.provider // ""') echo "flux" ;;
          *) echo "" ;;
        esac
        ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  cat > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/main" <<'SCRIPT'
driver::provision() {
  echo "provisioned"
}
SCRIPT

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  gitops::bootstrap() { gitops_called="$1:$2"; }
  export -f gitops::bootstrap

  provision::dispatch "test.lok8s.dev"

  [ "${gitops_called}" = "test.lok8s.dev:flux" ]
}

# --- provision::resolve_clusterref ---

@test "provision::resolve_clusterref resolves valid clusterRef" {
  # Create a deploy domain referencing the cluster domain
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/staging.lok8s.dev"
  mkdir -p "${deploy_dir}"
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" "${deploy_dir}/deploy.lok8s.yaml"

  yq() {
    case "$2" in
      '.spec.clusterRef.domain // ""') echo "test.lok8s.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::resolve_clusterref "staging.lok8s.dev"
  assert_success
  assert_output "test.lok8s.dev"
}

@test "provision::resolve_clusterref fails for non-deploy domain" {
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # test.lok8s.dev has cluster.lok8s.yaml, not deploy.lok8s.yaml
  run provision::resolve_clusterref "test.lok8s.dev"
  assert_failure
  assert_output --partial "No deploy.lok8s.yaml"
}

@test "provision::resolve_clusterref fails for missing clusterRef" {
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/orphan.lok8s.dev"
  mkdir -p "${deploy_dir}"
  cat > "${deploy_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: orphan
spec: {}
YAML

  yq() {
    case "$2" in
      '.spec.clusterRef.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::resolve_clusterref "orphan.lok8s.dev"
  assert_failure
  assert_output --partial "missing spec.clusterRef.domain"
}

@test "provision::resolve_clusterref fails when referenced domain missing" {
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/bad-ref.lok8s.dev"
  mkdir -p "${deploy_dir}"
  cat > "${deploy_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: bad-ref
spec:
  clusterRef:
    domain: nonexistent.lok8s.dev
YAML

  yq() {
    case "$2" in
      '.spec.clusterRef.domain // ""') echo "nonexistent.lok8s.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  run provision::resolve_clusterref "bad-ref.lok8s.dev"
  assert_failure
  assert_output --partial "clusterRef domain not found"
}

# --- _resolve_kubeconfig_for_domain ---

@test "_resolve_kubeconfig_for_domain falls back to <cluster metadata.name>.yaml" {
  # Regression (Fix B): a deploy domain resolves KUBECONFIG for its referenced
  # cluster. The canonical name is secret.<refDomain>.yaml, but a provisioned
  # cluster's kubeconfig is written as <metadata.name>.yaml (e.g. a domain
  # `example.com` provisions under metadata.name `my-cluster` -> my-cluster.yaml).
  # Without the fallback,
  # KUBECONFIG points at a non-existent secret.<refDomain>.yaml and deploy fails.
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/staging.lok8s.dev"
  mkdir -p "${deploy_dir}"
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" "${deploy_dir}/deploy.lok8s.yaml"

  # Only the <metadata.name> kubeconfig exists; the canonical secret.<domain> does not.
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  : > "${BATS_TEST_TMPDIR}/.kubeconfig/test-cluster.yaml"

  yq() {
    case "$2" in
      '.spec.clusterRef.domain // ""') echo "test.lok8s.dev" ;;
      '.metadata.name // ""') echo "test-cluster" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  domain="staging.lok8s.dev"
  cluster_override=""
  _resolve_kubeconfig_for_domain
  [ "${KUBECONFIG}" = "${BATS_TEST_TMPDIR}/.kubeconfig/test-cluster.yaml" ]
}

@test "_resolve_kubeconfig_for_domain prefers canonical secret.<domain>.yaml when present" {
  local deploy_dir="${BATS_TEST_TMPDIR}/clusters/staging.lok8s.dev"
  mkdir -p "${deploy_dir}"
  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" "${deploy_dir}/deploy.lok8s.yaml"

  # Both exist: the canonical secret.<domain> must win over the fallback.
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  : > "${BATS_TEST_TMPDIR}/.kubeconfig/secret.test.lok8s.dev.yaml"
  : > "${BATS_TEST_TMPDIR}/.kubeconfig/test-cluster.yaml"

  yq() {
    case "$2" in
      '.spec.clusterRef.domain // ""') echo "test.lok8s.dev" ;;
      '.metadata.name // ""') echo "test-cluster" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  domain="staging.lok8s.dev"
  cluster_override=""
  _resolve_kubeconfig_for_domain
  [ "${KUBECONFIG}" = "${BATS_TEST_TMPDIR}/.kubeconfig/secret.test.lok8s.dev.yaml" ]
}
