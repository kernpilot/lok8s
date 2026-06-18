#!/usr/bin/env bats
# kubehz_hosted_test.bats — unit tests for hosted provisioning API client

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"

  # argsh `:args` builtin — stub as no-op
  :args() { :; }
  export -f :args

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.kubehz.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
}

teardown() {
  teardown_tmpdir
}

# ── build_cluster_payload ────────────────────────────────

@test "build_cluster_payload: produces correct JSON with all fields" {
  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      '.kind') echo "KubeOne" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "nbg1" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "3" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # Use real jq for payload construction
  if ! command -v jq &>/dev/null; then
    skip "jq not available"
  fi

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  run kubehz::build_cluster_payload "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  # Verify JSON fields
  echo "${output}" | jq -e '.domain == "test.kubehz.dev"'
  echo "${output}" | jq -e '.kind == "KubeOne"'
  echo "${output}" | jq -e '.provider == "hetzner"'
  echo "${output}" | jq -e '.region == "nbg1"'
  echo "${output}" | jq -e '.kubernetesVersion == "v1.31.10"'
  echo "${output}" | jq -e '.controlPlaneReplicas == 3'
}

@test "build_cluster_payload: defaults provider to hetzner and replicas to 1" {
  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "default.kubehz.dev" ;;
      '.kind') echo "Capi" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "fsn1" ;;
      '.spec.kubernetes.version') echo "v1.30.0" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  if ! command -v jq &>/dev/null; then
    skip "jq not available"
  fi

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  run kubehz::build_cluster_payload "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  echo "${output}" | jq -e '.provider == "hetzner"'
  echo "${output}" | jq -e '.controlPlaneReplicas == 1'
}

# ── wait_for_cluster ─────────────────────────────────────

@test "wait_for_cluster: returns immediately when status is Running" {
  local _call_count=0
  curl() {
    echo '{"id":"cl-001","status":"Running"}'
  }
  export -f curl

  jq() {
    echo "Running"
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export KUBEHZ_TOKEN="test-token"

  run kubehz::wait_for_cluster "https://api.kubehz.dev" "cl-001" 30
  assert_success
}

@test "wait_for_cluster: fails when status is Failed" {
  curl() {
    echo '{"id":"cl-001","status":"Failed"}'
  }
  export -f curl

  jq() {
    echo "Failed"
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export KUBEHZ_TOKEN="test-token"

  run kubehz::wait_for_cluster "https://api.kubehz.dev" "cl-001" 30
  assert_failure
  assert_output --partial "failed"
}

# ── provision_hosted ─────────────────────────────────────

@test "provision_hosted: creates cluster, waits, and downloads kubeconfig" {
  local _curl_calls=()
  curl() {
    _curl_calls+=("$*")
    case "$*" in
      *POST*api/clusters*)
        echo '{"id":"cl-hosted-001","status":"Creating"}'
        ;;
      *api/clusters/cl-hosted-001/kubeconfig*)
        echo "apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://hosted.kubehz.dev:6443
  name: hosted"
        ;;
      *api/clusters/cl-hosted-001*)
        echo '{"id":"cl-hosted-001","status":"Running"}'
        ;;
      *) echo '{}' ;;
    esac
  }
  export -f curl

  jq() {
    case "$*" in
      *'.id'*) echo "cl-hosted-001" ;;
      *'.status'*) echo "Running" ;;
      *-n*) echo '{"domain":"test.kubehz.dev","kind":"KubeOne","provider":"hetzner","region":"fsn1","kubernetesVersion":"v1.31.10","controlPlaneReplicas":1}' ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  yq() {
    case "$2" in
      '.spec.cluster.domain') echo "test.kubehz.dev" ;;
      '.kind') echo "KubeOne" ;;
      '.spec.provider // "hetzner"') echo "hetzner" ;;
      '.spec.hcloud.region // .spec.aws.region // "fsn1"') echo "fsn1" ;;
      '.spec.kubernetes.version') echo "v1.31.10" ;;
      '.spec.controlPlane.replicas // 1') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  # Verify kubeconfig was written
  [ -f "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]
}

@test "provision_hosted: fails when API returns no cluster ID" {
  curl() {
    echo '{"error":"bad request"}'
  }
  export -f curl

  jq() {
    case "$*" in
      *'.id'*) echo "null" ;;
      *-n*) echo '{"domain":"x"}' ;;
      *) echo "" ;;
    esac
  }
  export -f jq

  yq() { echo ""; }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::provision_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "did not return a cluster ID"
}

# ── destroy_hosted ───────────────────────────────────────

@test "destroy_hosted: looks up cluster and sends DELETE" {
  curl() {
    case "$*" in
      *"GET"*|*"api/clusters?domain"*)
        echo '{"id":"cl-001","status":"Running"}'
        ;;
      *DELETE*)
        echo '{"success":true}'
        ;;
      *) echo '{}' ;;
    esac
  }
  export -f curl

  jq() {
    echo "cl-001"
  }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  # Create a kubeconfig to verify cleanup
  touch "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success

  # Verify kubeconfig was cleaned up
  [ ! -f "${BATS_TEST_TMPDIR}/.kubeconfig/test.kubehz.dev.yaml" ]
}

@test "destroy_hosted: succeeds silently when no cluster found" {
  curl() {
    return 1
  }
  export -f curl

  jq() { echo ""; }
  export -f jq

  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/main"
  source "${_PROJECT_ROOT}/.lok8s/libs/kubehz/hosted"

  export LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
  export KUBEHZ_TOKEN="test-token"

  run kubehz::destroy_hosted "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
}

# ── KubeOne driver hosted branch ─────────────────────────

@test "KubeOne driver::provision branches to hosted when hosting=hosted" {
  # Mock kubehz functions
  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="hosted"
    LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
    export LOK8S_KUBEHZ_HOSTING LOK8S_KUBEHZ_API_URL
  }
  export -f kubehz::read_config

  local _hosted_called=0
  kubehz::provision_hosted() {
    _hosted_called=1
    echo "hosted_provision_called domain=$1"
  }
  export -f kubehz::provision_hosted

  # These should NOT be called in hosted path
  kubeone::detect_provider() { echo "SHOULD_NOT_REACH"; return 1; }
  export -f kubeone::detect_provider

  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  run driver::provision "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "hosted_provision_called"
}

@test "KubeOne driver::provision continues self-hosted when hosting=self" {
  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="self"
    export LOK8S_KUBEHZ_HOSTING
  }
  export -f kubehz::read_config

  # Mock the self-hosted path — detect_provider is the first call after the hosted check
  kubeone::detect_provider() { echo "hetzner"; }
  export -f kubeone::detect_provider

  kubeone::validate_credentials() { :; }
  export -f kubeone::validate_credentials

  kubeone::extract_vars() { :; }
  export -f kubeone::extract_vars

  hetzner::provision() { :; }
  export -f hetzner::provision

  hetzner::generate_tfjson() { echo '{}'; }
  export -f hetzner::generate_tfjson

  kubeone::generate_config() { :; }
  export -f kubeone::generate_config

  kubeone::apply() { :; }
  export -f kubeone::apply

  yq() {
    case "$2" in
      '.metadata.name') echo "test-self" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubeone::kubeconfig_path() {
    local path="${BATS_TEST_TMPDIR}/kubeconfig-test"
    touch "${path}"
    echo "${path}"
  }
  export -f kubeone::kubeconfig_path

  # Stub provider::provision — libs/provision would normally load a
  # provider before driver::provision runs, so we satisfy the contract.
  provider::provision() { :; }
  export -f provider::provision
  # Other kubeone internals that get called once the self-hosted
  # branch proceeds — stub to no-ops for this unit test.
  _tfjson_from_output() { echo '{}'; }
  export -f _tfjson_from_output
  kubeone() { :; }
  export -f kubeone

  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"

  # We only care that the hosting=self branch proceeds past the
  # provision_hosted guard — don't care if later kubeone/real-network
  # steps succeed.
  run driver::provision "test.kubehz.dev"
  refute_output --partial "KubeOne driver requires spec.provider"
  refute_output --partial "hosted"
}

# ── CAPI driver hosted branch ────────────────────────────

@test "CAPI driver::provision branches to hosted when no mgmt_domain + hosting=hosted" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="hosted"
    LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
    export LOK8S_KUBEHZ_HOSTING LOK8S_KUBEHZ_API_URL
  }
  export -f kubehz::read_config

  kubehz::provision_hosted() {
    echo "capi_hosted_provision_called domain=$1"
  }
  export -f kubehz::provision_hosted

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::provision "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "capi_hosted_provision_called"
}

@test "CAPI driver::provision errors when no mgmt_domain + hosting=self" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="self"
    export LOK8S_KUBEHZ_HOSTING
  }
  export -f kubehz::read_config

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::provision "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.managementCluster.domain is required for self-hosted CAPI"
}

@test "CAPI driver::destroy uses hosted path when no mgmt_domain + hosting=hosted" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      '.metadata.name') echo "test-cluster" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubehz::read_config() {
    LOK8S_KUBEHZ_HOSTING="hosted"
    LOK8S_KUBEHZ_API_URL="https://api.kubehz.dev"
    export LOK8S_KUBEHZ_HOSTING LOK8S_KUBEHZ_API_URL
  }
  export -f kubehz::read_config

  kubehz::destroy_hosted() {
    echo "capi_hosted_destroy_called domain=$1"
  }
  export -f kubehz::destroy_hosted

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::destroy "test.kubehz.dev" "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "capi_hosted_destroy_called"
}
