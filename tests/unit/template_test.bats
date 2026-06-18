#!/usr/bin/env bats
# template_test.bats — unit tests for .lok8s/utils/template.sh

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"

  # Create a test template
  cat > "${BATS_TEST_TMPDIR}/test.yaml" <<'TMPL'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CLUSTER_NAME}-config
  namespace: ${CLUSTER_NAMESPACE}
data:
  domain: ${CLUSTER_DOMAIN}
  version: ${K8S_VERSION}
TMPL

  # Create a test cluster spec
  cat > "${BATS_TEST_TMPDIR}/cluster.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-cluster
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: test.lok8s.dev
    namespace: test-ns
YAML
}

teardown() {
  teardown_tmpdir
}

# --- template::render ---

@test "template::render substitutes variables from cluster spec" {
  # We need real yq for this test
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.metadata.name') echo "test-cluster" ;;
        '.spec.cluster.namespace // "default"') echo "test-ns" ;;
        '.spec.cluster.domain') echo "test.lok8s.dev" ;;
        '.spec.kubernetes.version') echo "v1.31.10" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run template::render "${BATS_TEST_TMPDIR}/test.yaml" "${BATS_TEST_TMPDIR}/cluster.yaml"
  assert_success
  assert_output --partial "test-cluster-config"
  assert_output --partial "test.lok8s.dev"
  assert_output --partial "v1.31.10"
}

@test "template::render fails for missing template file" {
  run template::render "/nonexistent/template.yaml" "${BATS_TEST_TMPDIR}/cluster.yaml"
  assert_failure
  assert_output --partial "Template not found"
}

@test "template::render fails for missing cluster spec" {
  run template::render "${BATS_TEST_TMPDIR}/test.yaml" "/nonexistent/cluster.yaml"
  assert_failure
  assert_output --partial "Cluster spec not found"
}

# --- template::render_dir ---

@test "template::render_dir renders all .yaml files in a directory" {
  mkdir -p "${BATS_TEST_TMPDIR}/templates"

  cat > "${BATS_TEST_TMPDIR}/templates/one.yaml" <<'TMPL'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CLUSTER_NAME}-one
TMPL

  cat > "${BATS_TEST_TMPDIR}/templates/two.yaml" <<'TMPL'
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CLUSTER_NAME}-two
TMPL

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.metadata.name') echo "test-cluster" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        '.spec.cluster.domain') echo "test.lok8s.dev" ;;
        '.spec.kubernetes.version') echo "v1.31.10" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run template::render_dir "${BATS_TEST_TMPDIR}/templates" "${BATS_TEST_TMPDIR}/cluster.yaml"
  assert_success
  assert_output --partial "test-cluster-one"
  assert_output --partial "test-cluster-two"
  # Multi-document separator should appear between templates
  assert_output --partial "---"
}

@test "template::render_dir fails for missing directory" {
  run template::render_dir "/nonexistent/dir" "${BATS_TEST_TMPDIR}/cluster.yaml"
  assert_failure
  assert_output --partial "Template directory not found"
}

# --- CAPI template validity ---

@test "CAPI core templates produce valid YAML with envsubst" {
  # Skip if envsubst is not available
  command -v envsubst || skip "envsubst not available"

  # Set all required variables
  export CLUSTER_NAME="test-cluster"
  export CLUSTER_NAMESPACE="default"
  export CLUSTER_DOMAIN="test.lok8s.dev"
  export K8S_VERSION="v1.31.10"
  export CP_REPLICAS="3"
  export CREDENTIAL_SECRET_NAME="test-creds"
  export INFRA_API_VERSION="infrastructure.cluster.x-k8s.io/v1beta1"
  export INFRA_CLUSTER_KIND="HetznerCluster"
  export INFRA_MACHINE_TEMPLATE_KIND="HCloudMachineTemplate"
  export HCLOUD_REGION="fsn1"
  export HCLOUD_SSH_KEY_NAME="test-key"
  export POOL_NAME="general"
  export POOL_REPLICAS="2"
  export POOL_TYPE="cax21"

  local tmpl_dir="${_PROJECT_ROOT}/.lok8s/drivers/capi/cluster"

  for tmpl in "${tmpl_dir}"/core/*.yaml; do
    [ -f "${tmpl}" ] || continue
    run envsubst < "${tmpl}"
    assert_success

    # Verify no unsubstituted variables remain
    refute_output --partial '${CLUSTER_NAME}'
    refute_output --partial '${CLUSTER_NAMESPACE}'
  done
}

@test "CAPI Hetzner provider templates produce valid YAML with envsubst" {
  command -v envsubst || skip "envsubst not available"

  export CLUSTER_NAME="test-cluster"
  export CLUSTER_NAMESPACE="default"
  export CLUSTER_DOMAIN="test.lok8s.dev"
  export K8S_VERSION="v1.31.10"
  export CP_REPLICAS="3"
  export CREDENTIAL_SECRET_NAME="test-creds"
  export INFRA_API_VERSION="infrastructure.cluster.x-k8s.io/v1beta1"
  export INFRA_CLUSTER_KIND="HetznerCluster"
  export INFRA_MACHINE_TEMPLATE_KIND="HCloudMachineTemplate"
  export HCLOUD_REGION="fsn1"
  export HCLOUD_SSH_KEY_NAME="test-key"
  export POOL_NAME="general"
  export POOL_REPLICAS="2"
  export POOL_TYPE="cax21"

  local tmpl_dir="${_PROJECT_ROOT}/.lok8s/drivers/capi/cluster/providers/hetzner"

  for tmpl in "${tmpl_dir}"/*.yaml; do
    [ -f "${tmpl}" ] || continue
    run envsubst < "${tmpl}"
    assert_success
    refute_output --partial '${CLUSTER_NAME}'
    refute_output --partial '${HCLOUD_REGION}'
  done
}
