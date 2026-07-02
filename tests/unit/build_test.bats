#!/usr/bin/env bats
# build_test.bats — unit tests for .lok8s/libs/build
#
# Domain-based build: `lo build` renders the DOMAIN kustomization
# (clusters/<domain>/kustomization.yaml, which composes the targets) into ONE
# clusters/<domain>/artifacts.yaml. There is no per-target loop and no
# artifacts/<target>/ output.

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

  # build::artifacts calls template::envsubst_whitelist for its envsubst pass. In
  # production it arrives via argsh `import` (stubbed to a no-op above), so stub it
  # here too — otherwise the build path hits "command not found" (status 127).
  template::envsubst_whitelist() { echo ""; }
  export -f template::envsubst_whitelist

  # Create the domain directory: targets + a domain kustomization composing them.
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

  cat > "${domain_dir}/kustomization.yaml" <<'YAML'
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

# --- build::artifacts ---

@test "build::artifacts writes ONE artifacts.yaml from the domain kustomization" {
  kustomize() {
    echo "apiVersion: v1"
    echo "kind: ConfigMap"
    echo "metadata:"
    echo "  name: test-cm"
  }
  export -f kustomize

  build::artifacts "test.lok8s.dev"

  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  # ONE artifact at the domain root — not per-target.
  [ -f "${domain_dir}/artifacts.yaml" ]
  [ ! -d "${domain_dir}/artifacts/networking" ]
  [ ! -d "${domain_dir}/artifacts/platform" ]
  run cat "${domain_dir}/artifacts.yaml"
  assert_output --partial "kind: ConfigMap"
}

@test "build::artifacts defaults to \$DOMAIN_NAME when no domain arg is given" {
  export DOMAIN_NAME="test.lok8s.dev"
  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize

  build::artifacts

  [ -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/artifacts.yaml" ]
}

@test "build::artifacts errors when the domain has no kustomization.yaml" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/kustomization.yaml"

  run build::artifacts "test.lok8s.dev"
  assert_failure
  assert_output --partial "has no kustomization.yaml"
}

@test "build::artifacts errors for a nonexistent domain" {
  run build::artifacts "nope.lok8s.dev"
  assert_failure
  assert_output --partial "has no kustomization.yaml"
}

@test "build::artifacts cleans stale per-target artifact dirs from the pre-domain era" {
  # A leftover artifacts/<target>/artifacts.yaml from the old per-target build
  # must be removed so status/hooks don't read phantom output.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${domain_dir}/artifacts/stale"
  echo "kind: ConfigMap" > "${domain_dir}/artifacts/stale/artifacts.yaml"
  # A non-target file directly under artifacts/ must be PRESERVED.
  echo "queued" > "${domain_dir}/artifacts/.cache-queue"

  kustomize() { printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n'; }
  export -f kustomize

  build::artifacts "test.lok8s.dev"

  [ ! -d "${domain_dir}/artifacts/stale" ]
  [ -f "${domain_dir}/artifacts/.cache-queue" ]
}

@test "build::artifacts survives a deploy domain (no cluster.lok8s.yaml) under set -e" {
  # Regression: a DEPLOY domain (deploy.lok8s.yaml + clusterRef) has NO
  # cluster.lok8s.yaml, and its KUBECONFIG resolves to a secret kubeconfig that
  # may not be on disk yet. build::_resolve_api must guard the missing-file read
  # so it does not abort the whole build under `set -euo pipefail`. bats does not
  # run under errexit, so this case runs the build inside an explicit
  # `set -euo pipefail` subshell to mirror the real runtime.
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy.lok8s.dev"
  mkdir -p "${domain_dir}/targets/app"
  cp "${FIXTURES_DIR}/targets/platform/kustomization.yaml" "${domain_dir}/targets/app/"
  cp "${FIXTURES_DIR}/targets/platform/deployment.yaml" "${domain_dir}/targets/app/"
  cat > "${domain_dir}/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./targets/app
YAML
  # Deliberately NO cluster.lok8s.yaml here — that is what makes it a deploy domain.

  run bash -c '
    set -euo pipefail
    import() { :; }
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/verbose.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/utils/targets.sh"
    source "'"${_PROJECT_ROOT}"'/.lok8s/libs/build"
    export PATH_BASE="'"${BATS_TEST_TMPDIR}"'"
    export PATH_CLUSTERS="'"${BATS_TEST_TMPDIR}"'/clusters"
    # KUBECONFIG points at a not-yet-fetched secret kubeconfig (does not exist).
    export KUBECONFIG="'"${BATS_TEST_TMPDIR}"'/.kubeconfig/secret.deploy.lok8s.dev.yaml"
    # Keep the test focused on the kubeconfig-resolution crash, not envsubst.
    template::envsubst_whitelist() { echo ""; }
    kustomize() { printf "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"; }
    export -f template::envsubst_whitelist kustomize
    build::artifacts "deploy.lok8s.dev"
  '
  assert_success
  [ -f "${domain_dir}/artifacts.yaml" ]
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
