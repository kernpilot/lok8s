#!/usr/bin/env bats
# env_test.bats — unit tests for .lok8s/libs/env

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  # Stub import and argsh builtins
  import() { :; }
  export -f import
  :usage() { :; }
  export -f :usage
  :args() { shift; }
  export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/env"

  # Copy base services fixture
  cp "${FIXTURES_DIR}/services.yaml" "${BATS_TEST_TMPDIR}/services.yaml"
}

teardown() {
  teardown_tmpdir
}

# --- env::services ---

@test "env::services reads base services.yaml" {
  # Mock yq and envsubst
  yq() {
    if [[ "$1" == "eval-all" ]]; then
      cat
    else
      cat
    fi
  }
  export -f yq

  envsubst() { cat; }
  export -f envsubst

  run env::services
  assert_success
  assert_output --partial "registry:"
  assert_output --partial "services:"
}

@test "env::services merges config-specific override" {
  cp "${FIXTURES_DIR}/services.local.yaml" "${BATS_TEST_TMPDIR}/services.local.yaml"
  export LOK8S_SERVICE_CONFIG="local"

  yq() {
    if [[ "$1" == "eval-all" ]]; then
      cat
    else
      cat
    fi
  }
  export -f yq

  envsubst() { cat; }
  export -f envsubst

  run env::services
  assert_success
  # Should include both base and override content
  assert_output --partial "services:"
  assert_output --partial "---"
}

@test "env::services falls back to services.base.yaml" {
  rm -f "${BATS_TEST_TMPDIR}/services.yaml"
  cp "${FIXTURES_DIR}/services.yaml" "${BATS_TEST_TMPDIR}/services.base.yaml"

  yq() {
    if [[ "$1" == "eval-all" ]]; then
      cat
    else
      cat
    fi
  }
  export -f yq

  envsubst() { cat; }
  export -f envsubst

  run env::services
  assert_success
  assert_output --partial "services:"
}

@test "env::services returns empty config when no services file exists" {
  # env::services treats absent services.yaml as a valid "no services"
  # state — returns {} and exits 0. Projects without services (pure
  # infra clusters) need this path.
  rm -f "${BATS_TEST_TMPDIR}/services.yaml"
  rm -f "${BATS_TEST_TMPDIR}/services.base.yaml"

  run env::services
  assert_success
  assert_output --partial "{}"
}

@test "env::services merges services.default.yaml if present" {
  cat > "${BATS_TEST_TMPDIR}/services.default.yaml" <<'YAML'
services:
  default-svc:
    enabled: true
    build: false
YAML

  yq() {
    if [[ "$1" == "eval-all" ]]; then
      cat
    else
      cat
    fi
  }
  export -f yq

  envsubst() { cat; }
  export -f envsubst

  run env::services
  assert_success
  # Output should contain the separator and default content
  assert_output --partial "---"
}

# --- env::kustomization ---

@test "env::kustomization writes to .lok8s/<domain>/artifacts/kustomization.yaml" {
  # Mock all dependencies
  build::artifacts() { :; }
  export -f build::artifacts

  # Real yq is required — the function builds structured queries at runtime
  # that are hard to enumerate in a stub. If the host doesn't have yq,
  # the test simply skips.
  if ! command -v yq &>/dev/null; then
    skip "yq not available"
  fi

  export DOMAIN_NAME="test.example"

  # Seed a per-target artifacts.yaml so env::kustomization has
  # something to reference (discover loop scans */artifacts.yaml).
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.example/artifacts/platform"
  echo "# placeholder" > "${BATS_TEST_TMPDIR}/clusters/test.example/artifacts/platform/artifacts.yaml"

  # Merged services env: one service builds locally, one uses registry, one
  # pins an explicit image.
  env::services() {
    cat <<'YAML'
registry:
  endpoint: ghcr.io/myorg
  branch: test-project
  tag: latest
  prefix: lok8s.local
defaults:
  build: false
services:
  api:
    build: true
  worker:
    build: false
  pinned:
    image: ghcr.io/external/pinned:v1.2.3
YAML
  }
  export -f env::services

  envsubst() { cat; }
  export -f envsubst

  env::kustomization --no-build

  local out="${BATS_TEST_TMPDIR}/clusters/test.example/artifacts/kustomization.yaml"
  [ -f "${out}" ] || { echo "expected output at ${out}"; return 1; }

  run cat "${out}"
  assert_output --partial "apiVersion: kustomize.config.k8s.io/v1beta1"
  assert_output --partial "kind: Kustomization"
  # per-target artifact reference emitted as a resource
  assert_output --partial "platform/artifacts.yaml"

  # worker: build=false → registry swap goes through the on-cluster
  # cache (lok8s.cache), not directly to the remote registry. The
  # remote ref is queued for `lo image cache` to pre-populate.
  assert_output --partial "name: lok8s.local/worker"
  assert_output --partial "newName: lok8s.cache/test-project/worker"
  assert_output --partial 'newTag: "latest"'

  # pinned: explicit image bypasses the cache
  assert_output --partial "name: lok8s.local/pinned"
  assert_output --partial "newName: ghcr.io/external/pinned"
  assert_output --partial 'newTag: "v1.2.3"'

  # api: build=true → must NOT appear (it builds locally, canonical ref is correct)
  refute_output --partial "name: lok8s.local/api"
}

@test "env::kustomization emits empty images block when no swaps needed" {
  build::artifacts() { :; }
  export -f build::artifacts

  if ! command -v yq &>/dev/null; then
    skip "yq not available"
  fi

  export DOMAIN_NAME="lok8s.dev"

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/lok8s.dev/artifacts/apps"
  echo "# placeholder" > "${BATS_TEST_TMPDIR}/clusters/lok8s.dev/artifacts/apps/artifacts.yaml"

  # All services build locally — no swaps should be emitted
  env::services() {
    cat <<'YAML'
registry:
  endpoint: ghcr.io/myorg
  branch: test-project
  tag: latest
  prefix: lok8s.local
defaults:
  build: true
services:
  api: {}
  web: {}
YAML
  }
  export -f env::services

  envsubst() { cat; }
  export -f envsubst

  env::kustomization --no-build

  local out="${BATS_TEST_TMPDIR}/clusters/lok8s.dev/artifacts/kustomization.yaml"
  [ -f "${out}" ] || { echo "expected output at ${out}"; return 1; }

  run cat "${out}"
  assert_output --partial "resources:"
  assert_output --partial "apps/artifacts.yaml"
  refute_output --partial "name: lok8s.local"
}
