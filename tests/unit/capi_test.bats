#!/usr/bin/env bats
# capi_test.bats — unit tests for .lok8s/drivers/capi/generate

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/provider"
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/generate"

  # Copy CAPI templates to tmpdir (needed by capi::generate)
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster"
  cp -r "${_PROJECT_ROOT}/.lok8s/drivers/capi/cluster/"* \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster/"

  # Create Hetzner cluster spec fixture
  cp "${FIXTURES_DIR}/capi-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml"

  # Create an AWS cluster spec fixture (inline)
  cat > "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-aws
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: aws.lok8s.dev
    namespace: default
  aws:
    region: eu-central-1
    sshKeyName: test-aws-key
  controlPlane:
    replicas: 1
    type: t3.large
  workers:
    general:
      replicas: 2
      type: t3.xlarge
YAML

  # Copy full AWS fixture too
  if [[ -f "${FIXTURES_DIR}/aws-cluster.lok8s.yaml" ]]; then
    cp "${FIXTURES_DIR}/aws-cluster.lok8s.yaml" \
      "${BATS_TEST_TMPDIR}/aws-cluster-full.lok8s.yaml"
  fi
}

teardown() {
  teardown_tmpdir
}

# --- capi::detect_provider ---

@test "capi::detect_provider returns hetzner for hcloud spec" {
  if ! command -v yq &>/dev/null; then
    yq() {
      if [[ "$1" == "-e" && "$2" == ".spec.hcloud" ]]; then
        return 0
      fi
      return 1
    }
    export -f yq
  fi

  run capi::detect_provider "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml"
  assert_success
  assert_output "hetzner"
}

@test "capi::detect_provider returns aws for aws spec" {
  if ! command -v yq &>/dev/null; then
    yq() {
      if [[ "$1" == "-e" && "$2" == ".spec.hcloud" ]]; then
        return 1
      fi
      if [[ "$1" == "-e" && "$2" == ".spec.aws" ]]; then
        return 0
      fi
      return 1
    }
    export -f yq
  fi

  run capi::detect_provider "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml"
  assert_success
  assert_output "aws"
}

@test "capi::detect_provider fails for unknown provider" {
  cat > "${BATS_TEST_TMPDIR}/unknown-cluster.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-unknown
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: unknown.lok8s.dev
YAML

  if ! command -v yq &>/dev/null; then
    yq() { return 1; }
    export -f yq
  fi

  run capi::detect_provider "${BATS_TEST_TMPDIR}/unknown-cluster.yaml"
  assert_failure
  assert_output --partial "No provider found in cluster spec"
}

# --- capi::generate ---

@test "capi::generate produces CAPI Cluster resource for hetzner" {
  if ! command -v yq &>/dev/null; then
    skip "yq required for capi::generate test"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_success
  assert_output --partial "kind: Cluster"
  assert_output --partial "kind: KubeadmControlPlane"
  assert_output --partial "kind: HetznerCluster"
}

@test "capi::generate includes worker machine deployments" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_success
  assert_output --partial "kind: MachineDeployment"
}

@test "capi::generate fails for missing template directory" {
  rm -rf "${BATS_TEST_TMPDIR}/.lok8s/drivers/capi/cluster"

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.metadata.name') echo "test-production" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        '.spec.cluster.domain') echo "prod.lok8s.dev" ;;
        '.spec.kubernetes.version') echo "v1.31.10" ;;
        '.spec.controlPlane.replicas // 1') echo "3" ;;
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.hcloud.region') echo "fsn1" ;;
        '.spec.hcloud.sshKeyName') echo "test-key" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_failure
  assert_output --partial "CAPI template directory not found"
}

@test "capi::generate fails for unsupported provider" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.metadata.name') echo "test" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "gcp"
  assert_failure
  assert_output --partial "Unsupported CAPI provider"
}

# --- capi::ensure_credentials ---

@test "capi::ensure_credentials creates hetzner secret" {
  kubectl() { echo "kubectl $*"; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export HCLOUD_TOKEN="test-token"

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" \
    "hetzner" \
    "/tmp/kubeconfig.yaml"
  assert_success
}

@test "capi::ensure_credentials fails for unsupported provider" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" \
    "gcp" \
    "/tmp/kubeconfig.yaml"
  assert_failure
  assert_output --partial "unknown provider"
}

# --- capi::wait_ready ---

@test "capi::wait_ready returns when cluster is Provisioned" {
  kubectl() {
    echo "Provisioned"
  }
  export -f kubectl

  run capi::wait_ready "/tmp/kubeconfig.yaml" "test-cluster" 5
  assert_success
}

@test "capi::wait_ready fails on timeout" {
  kubectl() { echo "Pending"; }
  export -f kubectl

  # Use very short timeout (1 second) with a sleep override
  sleep() { :; }
  export -f sleep

  run capi::wait_ready "/tmp/kubeconfig.yaml" "test-cluster" 1
  assert_failure
  assert_output --partial "Timed out"
}

# --- AWS provider ---

@test "capi::generate produces AWSCluster resource for aws provider" {
  if ! command -v yq &>/dev/null; then
    skip "yq required for capi::generate test"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" "aws"
  assert_success
  assert_output --partial "kind: Cluster"
  assert_output --partial "kind: KubeadmControlPlane"
  assert_output --partial "kind: AWSCluster"
}

@test "capi::generate renders AWS machine templates for worker pools" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" "aws"
  assert_success
  assert_output --partial "kind: MachineDeployment"
  assert_output --partial "kind: AWSMachineTemplate"
}

@test "capi::generate sets correct AWS region in output" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" "aws"
  assert_success
  assert_output --partial "region: eu-central-1"
}

@test "capi::generate uses v1beta2 API version for AWS" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" "aws"
  assert_success
  assert_output --partial "infrastructure.cluster.x-k8s.io/v1beta2"
}

@test "capi::ensure_credentials creates aws secret" {
  kubectl() { echo "kubectl $*"; }
  export -f kubectl

  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-aws-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  export AWS_ACCESS_KEY_ID="AKIAIOSFODNN7EXAMPLE"
  export AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
  export AWS_REGION="eu-central-1"

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" \
    "aws" \
    "/tmp/kubeconfig.yaml"
  assert_success
}

@test "capi::ensure_credentials fails without AWS_ACCESS_KEY_ID" {
  if ! command -v yq &>/dev/null; then
    yq() {
      case "$2" in
        '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
        '.spec.cluster.namespace // "default"') echo "default" ;;
        *) echo "" ;;
      esac
    }
    export -f yq
  fi

  unset AWS_ACCESS_KEY_ID 2>/dev/null || true
  unset AWS_SECRET_ACCESS_KEY 2>/dev/null || true
  unset AWS_REGION 2>/dev/null || true

  run capi::ensure_credentials \
    "${BATS_TEST_TMPDIR}/aws-cluster.lok8s.yaml" \
    "aws" \
    "/tmp/kubeconfig.yaml"
  assert_failure
  assert_output --partial "AWS_ACCESS_KEY_ID"
}

# --- Conditional hrobot rendering ---

@test "capi::generate does not render hrobot template when no hrobot hosts" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-cluster.lok8s.yaml" "hetzner"
  assert_success
  refute_output --partial "HetznerBareMetalMachineTemplate"
}

@test "capi::generate renders hrobot template when hrobot hosts configured" {
  if ! command -v yq &>/dev/null; then
    skip "yq required"
  fi
  command -v envsubst || skip "envsubst not available"

  # Create a cluster spec with hrobot hosts
  cat > "${BATS_TEST_TMPDIR}/hetzner-hrobot.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-baremetal
spec:
  kubernetes:
    version: "v1.31.10"
  cluster:
    domain: bm.lok8s.dev
    namespace: capi-system
  credentials:
    secretName: bm-credentials
  hcloud:
    region: fsn1
    sshKeyName: admin-key
  hrobot:
    sshKeyName: robot-key
    hosts:
      - name: bm-01
        serverNumber: 12345
        rootDeviceHints:
          wwn: "0x50014ee2b5e1"
      - name: bm-02
        serverNumber: 67890
        rootDeviceHints:
          wwn: "0x50014ee2b5e2"
  controlPlane:
    replicas: 1
    type: cax21
YAML

  run capi::generate "${BATS_TEST_TMPDIR}/hetzner-hrobot.lok8s.yaml" "hetzner"
  assert_success
  assert_output --partial "HetznerBareMetalMachineTemplate"
  assert_output --partial "test-baremetal-bm-01"
  assert_output --partial "test-baremetal-bm-02"
}
