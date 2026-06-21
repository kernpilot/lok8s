#!/usr/bin/env bats
# build_test.bats — unit tests for .lok8s/libs/build

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  # Stub import
  import() { :; }
  export -f import

  # Source build.sh (and its deps that argsh `import` pulls in at runtime)
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/targets.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/build"

  # build::targets calls template::envsubst_whitelist for its envsubst pass. In
  # production it arrives via argsh `import` (stubbed to a no-op above), so stub it
  # here too — otherwise the build path hits "command not found" (status 127).
  template::envsubst_whitelist() { echo ""; }
  export -f template::envsubst_whitelist

  # Create domain directory with targets
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${domain_dir}/targets/networking"
  mkdir -p "${domain_dir}/targets/platform"

  cp "${FIXTURES_DIR}/targets/networking/kustomization.yaml" \
    "${domain_dir}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/networking/namespace.yaml" \
    "${domain_dir}/targets/networking/"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml" \
    "${domain_dir}/targets/platform/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml" \
    "${domain_dir}/targets/platform/"
}

teardown() {
  teardown_tmpdir
}

# --- build::targets ---

@test "build::targets discovers all targets in domain directory" {
  # Mock kustomize to produce some output
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: Namespace"
    echo "metadata:"
    echo "  name: test"
  }
  export -f kustomize

  run build::targets "test.lok8s.dev"
  assert_success
}

@test "build::targets writes artifacts.yaml per target" {
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: test-cm"
  }
  export -f kustomize

  build::targets "test.lok8s.dev"

  local artifacts_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts"
  [ -f "${artifacts_dir}/networking/artifacts.yaml" ]
  [ -f "${artifacts_dir}/platform/artifacts.yaml" ]
}

@test "build::targets generates kustomization.yaml per target" {
  kustomize() { echo "---"; }
  export -f kustomize

  build::targets "test.lok8s.dev"

  local artifacts_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts"
  [ -f "${artifacts_dir}/networking/kustomization.yaml" ]

  # Verify kustomization.yaml references artifacts.yaml
  run grep "artifacts.yaml" "${artifacts_dir}/networking/kustomization.yaml"
  assert_success
}

@test "build::targets builds only specified targets" {
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: Namespace"
    echo "metadata:"
    echo "  name: net"
  }
  export -f kustomize

  build::targets "test.lok8s.dev" "networking"

  local artifacts_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts"
  [ -f "${artifacts_dir}/networking/artifacts.yaml" ]
  [ ! -d "${artifacts_dir}/platform" ]
}

@test "build::targets fails for missing targets directory" {
  rm -rf "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/targets"

  run build::targets "test.lok8s.dev"
  assert_failure
  assert_output --partial "No targets directory"
}

@test "build::targets fails for nonexistent requested target" {
  kustomize() { echo "---"; }
  export -f kustomize

  run build::targets "test.lok8s.dev" "nonexistent"
  assert_failure
  assert_output --partial "Target not found"
}

@test "build::targets handles empty targets directory gracefully" {
  rm -rf "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/targets/"*

  run build::targets "test.lok8s.dev"
  assert_success
  assert_output --partial "No targets found"
}

@test "build::targets survives a deploy domain (no cluster.lok8s.yaml) under set -e" {
  # Regression: a DEPLOY domain (deploy.lok8s.yaml + clusterRef) has NO
  # cluster.lok8s.yaml, and its KUBECONFIG resolves to a secret kubeconfig that
  # may not be on disk yet. The old kubeconfig-introspection block did a bare
  #   _cn=$(yq … "${domain_dir}/cluster.lok8s.yaml" 2>/dev/null)
  # which exits non-zero on the missing file and — under the `lo` runtime's
  # `set -euo pipefail` — aborted the WHOLE build silently (stderr suppressed)
  # before any artifact was written. This broke `lo build`/`lo deploy` for every
  # deploy-domain target. The suite's other tests miss it because bats does not
  # run under errexit, so this case runs the build inside an explicit
  # `set -euo pipefail` subshell to mirror the real runtime.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy.lok8s.dev"
  mkdir -p "${domain_dir}/targets/app"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml" "${domain_dir}/targets/app/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml" "${domain_dir}/targets/app/"
  # Deliberately NO cluster.lok8s.yaml here — that is what makes it a deploy domain.

  run bash -c '
    set -euo pipefail
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/targets.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/libs/build"
    export PATH_BASE="'"${BATS_TEST_TMPDIR}"'"
    # KUBECONFIG points at a not-yet-fetched secret kubeconfig (does not exist).
    export KUBECONFIG="'"${BATS_TEST_TMPDIR}"'/.kubeconfig/secret.deploy.lok8s.dev.yaml"
    # Keep the test focused on the kubeconfig-resolution crash, not envsubst.
    template::envsubst_whitelist() { echo ""; }
    kustomize() { printf "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"; }
    export -f template::envsubst_whitelist kustomize
    build::targets "deploy.lok8s.dev"
  '
  assert_success
  [ -f "${domain_dir}/artifacts/app/artifacts.yaml" ]
}

# --- build::targets_split ---

@test "build::targets_split produces individual files per resource" {
  # Mock kustomize + yq for the split path
  kustomize() {
    cat <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: test-ns
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
  namespace: default
YAML
  }
  export -f kustomize

  # yq -s splits multi-doc YAML; mock to output each doc on its own line
  yq() {
    if [[ "$1" == "-s" ]]; then
      # Read from stdin and split by ---
      local input
      input=$(cat)
      # Output doc 1
      echo "apiVersion: v1
kind: Namespace
metadata:
  name: test-ns"
      echo "---"
      echo "apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
  namespace: default"
    elif [[ "$1" == "-r" ]]; then
      case "$2" in
        '.kind // empty')
          local input
          input=$(cat)
          if echo "${input}" | grep -q "Namespace"; then
            echo "Namespace"
          elif echo "${input}" | grep -q "Deployment"; then
            echo "Deployment"
          fi
          ;;
        '.metadata.namespace // empty')
          local input
          input=$(cat)
          if echo "${input}" | grep -q "namespace: default"; then
            echo "default"
          else
            echo ""
          fi
          ;;
        '.metadata.name // empty')
          local input
          input=$(cat)
          if echo "${input}" | grep -q "test-ns"; then
            echo "test-ns"
          elif echo "${input}" | grep -q "test-deploy"; then
            echo "test-deploy"
          fi
          ;;
        *) cat ;;
      esac
    else
      cat
    fi
  }
  export -f yq

  build::targets_split "test.lok8s.dev" "networking"

  local artifacts_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts/networking"
  [ -f "${artifacts_dir}/kustomization.yaml" ]
}

# --- build::_export_secrets_path (per-instance secret isolation) ---

@test "build::_export_secrets_path redirects PATH_SECRETS to a domain's own store" {
  local dd="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"

  # No per-domain store: must be a clean no-op AND exit 0 — it's the helper's
  # last command, so a non-zero would abort the build under set -e.
  run build::_export_secrets_path "${dd}"
  assert_success

  # Store present: the secrets plugin is pointed at it (never the flat store).
  export PATH_SECRETS="${BATS_TEST_TMPDIR}/.secrets"
  mkdir -p "${dd}/secrets"
  build::_export_secrets_path "${dd}"
  [ "${PATH_SECRETS}" = "${dd}/secrets" ]
}
