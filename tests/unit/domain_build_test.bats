#!/usr/bin/env bats
# domain_build_test.bats — integration-flavoured test for the domain-based
# build → deploy pipeline, using a small FIXTURE domain whose
# kustomization.yaml composes real targets and the REAL kustomize/yq (no
# mocked renderer). Asserts:
#   (a) `lo build` produces ONE clusters/<domain>/artifacts.yaml,
#   (b) a domain missing its kustomization.yaml errors clearly,
#   (c) `lo deploy -l <key=value>` selects the right subset of that artifact.
# kubectl is the only mock (there is no cluster).

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1
  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/targets.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"
  source "${_PROJECT_ROOT}/.lok8s/libs/deploy"

  # Real kustomize + yq drive the composition and the label filter.
  command -v kustomize &>/dev/null || skip "kustomize not available"
  command -v yq &>/dev/null || skip "yq not available"

  # Build path needs the envsubst whitelist helper (normally imported).
  template::envsubst_whitelist() { echo ""; }
  export -f template::envsubst_whitelist

  # No readiness polling against the fake cluster.
  kapply::wait_ready() { :; }
  export -f kapply::wait_ready

  # FIXTURE domain: a kustomization.yaml composing two real targets.
  DOMAIN="fixture.lok8s.dev"
  DOMAIN_DIR="${BATS_TEST_TMPDIR}/clusters/${DOMAIN}"
  mkdir -p "${DOMAIN_DIR}/targets/networking" "${DOMAIN_DIR}/targets/platform"
  cp "${FIXTURES_DIR}/targets/networking/kustomization.yaml" "${DOMAIN_DIR}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/networking/namespace.yaml"     "${DOMAIN_DIR}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml"   "${DOMAIN_DIR}/targets/platform/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml"      "${DOMAIN_DIR}/targets/platform/"
  cat > "${DOMAIN_DIR}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./targets/networking
  - ./targets/platform
YAML
}

teardown() {
  teardown_tmpdir
}

# (a) One artifact, composed from all targets.
@test "lo build composes the domain kustomization into ONE artifacts.yaml" {
  build::artifacts "${DOMAIN}"

  [ -f "${DOMAIN_DIR}/artifacts.yaml" ]
  # No per-target artifact tree.
  [ ! -d "${DOMAIN_DIR}/artifacts" ]
  # Both composed targets landed in the one file.
  run yq -r '.kind' "${DOMAIN_DIR}/artifacts.yaml"
  assert_output --partial "Namespace"
  assert_output --partial "Deployment"
}

# (b) Missing domain kustomization → clear, actionable error.
@test "lo build errors when the domain has no kustomization.yaml" {
  rm -f "${DOMAIN_DIR}/kustomization.yaml"
  run build::artifacts "${DOMAIN}"
  assert_failure
  assert_output --partial "has no kustomization.yaml"
  assert_output --partial "resources:"
}

# (c) Selective deploy filters the single artifact by label.
@test "lo deploy -l <key=value> applies only the matching subset" {
  build::artifacts "${DOMAIN}"

  # Record which kinds reach kubectl apply.
  kubectl() {
    case "$1" in
      apply) local m; m=$(cat)
             grep -q 'kind: Namespace'  <<<"${m}" && echo "applied:Namespace"
             grep -q 'kind: Deployment' <<<"${m}" && echo "applied:Deployment"
             return 0 ;;
      *) return 0 ;;
    esac
  }
  export -f kubectl

  run deploy::apply_filtered "${DOMAIN}" "lok8s.dev/type" "system"
  assert_success
  assert_output --partial "applied:Namespace"
  refute_output --partial "applied:Deployment"
}
