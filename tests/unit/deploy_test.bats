#!/usr/bin/env bats
# deploy_test.bats — unit tests for .lok8s/libs/deploy
# Post-refactor: deploy reads targets from .lok8s/<domain>/targets/*/,
# not from spec.syncWave. Ordering is alphabetical and not semantic.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/targets.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/deploy"

  kubectl() {
    case "$1" in
      apply) echo "applied" ;;
      wait) echo "waited" ;;
      *) echo "kubectl $*" ;;
    esac
  }
  export -f kubectl

  # Build a minimal domain structure with target dirs + pre-built artifacts
  local domain="test.lok8s.dev"
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/${domain}"
  mkdir -p "${domain_dir}/targets/crds"
  mkdir -p "${domain_dir}/targets/networking"
  mkdir -p "${domain_dir}/targets/platform"
  mkdir -p "${domain_dir}/artifacts/crds"
  mkdir -p "${domain_dir}/artifacts/networking"
  mkdir -p "${domain_dir}/artifacts/platform"

  cat > "${domain_dir}/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-local
spec:
  bootstrap:
    - cilium
YAML

  cat > "${domain_dir}/artifacts/crds/artifacts.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.test.lok8s.dev
YAML

  cat > "${domain_dir}/artifacts/networking/artifacts.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: networking
YAML

  cat > "${domain_dir}/artifacts/platform/artifacts.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
YAML
}

teardown() {
  teardown_tmpdir
}

# --- deploy::apply ---

@test "deploy::apply discovers targets from targets/ directory" {
  yq() { cat; }
  export -f yq
  run deploy::apply "test.lok8s.dev"
  assert_success
  assert_output --partial "applied"
}

@test "deploy::apply uses explicit target args when provided" {
  yq() { cat; }
  export -f yq
  # kapply pipes the manifest on stdin (-f -); identify the target by content.
  # The networking artifact is a Namespace named "networking"; the crds
  # artifact is the "widgets" CRD — so only one should ever reach kubectl.
  kubectl() {
    local m; m=$(cat)
    grep -q 'name: networking' <<<"${m}" && echo "applied:networking"
    grep -q 'widgets' <<<"${m}"          && echo "applied:crds"
    return 0
  }
  export -f kubectl

  run deploy::apply "test.lok8s.dev" "networking"
  assert_success
  assert_output --partial "applied:networking"
  refute_output --partial "applied:crds"
}

@test "deploy::apply warns when no targets exist" {
  rm -rf "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/targets"
  yq() { cat; }
  export -f yq
  run deploy::apply "test.lok8s.dev"
  assert_success
  assert_output --partial "No targets to deploy"
}

@test "deploy::apply skips targets with no artifacts file" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts/platform/artifacts.yaml"
  yq() { cat; }
  export -f yq
  run deploy::apply "test.lok8s.dev"
  assert_success
  assert_output --partial "No artifacts for target platform"
}

# --- deploy::apply_filtered ---

@test "deploy::apply_filtered selects resources by label" {
  yq() {
    case "$1" in
      "select"*) echo "filtered_output" ;;
      *) cat ;;
    esac
  }
  export -f yq
  kubectl() { echo "applied filtered"; }
  export -f kubectl

  run deploy::apply_filtered "test.lok8s.dev" "type" "system"
  assert_success
}

@test "deploy::apply_filtered rejects injection in label key" {
  run deploy::apply_filtered "test.lok8s.dev" "key; rm -rf /" "value"
  assert_failure
  assert_output --partial "Invalid filter"
}

@test "deploy::apply_filtered rejects injection in label value" {
  run deploy::apply_filtered "test.lok8s.dev" "type" "value; echo pwned"
  assert_failure
  assert_output --partial "Invalid filter"
}

# --- deploy::wait_crds ---

@test "deploy::wait_crds waits for CRDs to become established" {
  kubectl() {
    if [[ "$1" == "wait" ]]; then
      echo "condition met"
    fi
  }
  export -f kubectl
  yq() { echo "widgets.test.lok8s.dev"; }
  export -f yq

  run deploy::wait_crds "apiVersion: v1"
  assert_success
}
