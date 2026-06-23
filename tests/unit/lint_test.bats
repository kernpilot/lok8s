#!/usr/bin/env bats
# lint_test.bats — unit tests for libs/lint

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import
  :usage() { :; };  export -f :usage
  :args()  { shift; }; export -f :args

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/lint"
  # lint::secrets calls secrets::* helpers (check_unencrypted / check_flat_shadows),
  # which the `lo` entrypoint loads in production — source them here too.
  source "${_PROJECT_ROOT}/.lok8s/libs/secrets"

  # Create a minimal domain structure
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/test-domain"
  mkdir -p "${domain_dir}/targets/crds"
  mkdir -p "${domain_dir}/targets/networking"
  mkdir -p "${domain_dir}/targets/platform"

  cat > "${domain_dir}/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test
spec:
  bootstrap: []
YAML

  # Add kustomization.yaml + resource files to each target
  cat > "${domain_dir}/targets/crds/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - test-crd.yaml
YAML
  cat > "${domain_dir}/targets/crds/test-crd.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.test.lok8s.dev
  labels:
    lok8s.dev/type: system
spec:
  group: test.lok8s.dev
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
YAML

  cat > "${domain_dir}/targets/networking/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - namespace.yaml
YAML
  cat > "${domain_dir}/targets/networking/namespace.yaml" <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: networking
  labels:
    lok8s.dev/type: system
YAML

  cat > "${domain_dir}/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
YAML
  cat > "${domain_dir}/targets/platform/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-app
  namespace: default
  labels:
    lok8s.dev/type: platform
spec:
  replicas: 1
  selector:
    matchLabels:
      app: test-app
  template:
    metadata:
      labels:
        app: test-app
    spec:
      containers:
        - name: app
          image: nginx:latest
YAML
}

teardown() {
  teardown_tmpdir
}

# Helper: create a yq mock that dispatches on the query argument.
# yq is called as: yq -r '<query>' <file>
# So $1=-r, $2=<query>, $3=<file>
_mock_yq_valid() {
  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
          *platform*) echo "deployment.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length')
        echo "1"
        ;;
      *) echo "" ;;
    esac
  }
  export -f yq
}

# --- lint tests: spec existence ---

@test "lint catches missing cluster spec" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/bad-domain"

  run lint::all "bad-domain"
  assert_failure
  assert_output --partial "Missing cluster.lok8s.yaml or deploy.lok8s.yaml"
}

# Note: syncWave-validation tests were removed post-refactor. Bootstrap
# validation (lint::bootstrap) resolves entries against the real
# provider addons tree; tests mock .spec.bootstrap[]? to empty to skip.

@test "lint passes for valid domain" {
  _mock_yq_valid

  run lint::all "test-domain"
  assert_success
  assert_output --partial "OK"
}

@test "lint handles domain with deploy spec" {
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy-domain"
  mkdir -p "${domain_dir}/targets/crds"
  mkdir -p "${domain_dir}/targets/platform"

  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" \
    "${domain_dir}/deploy.lok8s.yaml"

  cat > "${domain_dir}/targets/crds/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
YAML
  cat > "${domain_dir}/targets/crds/crd.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: test
  labels:
    lok8s.dev/type: system
YAML

  cat > "${domain_dir}/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - app.yaml
YAML
  cat > "${domain_dir}/targets/platform/app.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels:
    lok8s.dev/type: platform
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Deploy" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "staging-apps" ;;
      '.spec.clusterRef // ""') echo "domain: test.lok8s.dev" ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "crd.yaml" ;;
          *platform*) echo "app.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "deploy-domain"
  assert_success
  assert_output --partial "OK"
}

# --- lint tests: schema validation ---

@test "lint catches missing apiVersion" {
  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
          *platform*) echo "deployment.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_failure
  assert_output --partial "Missing required field: apiVersion"
}

@test "lint catches missing kind" {
  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
          *platform*) echo "deployment.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_failure
  assert_output --partial "Missing required field: kind"
}

@test "lint catches missing metadata.name" {
  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
          *platform*) echo "deployment.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_failure
  assert_output --partial "Missing required field: metadata.name"
}

@test "lint catches missing clusterRef in deploy spec" {
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy-no-ref"
  mkdir -p "${domain_dir}/targets/platform"

  cat > "${domain_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: bad-deploy
spec:
YAML

  cat > "${domain_dir}/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - app.yaml
YAML
  cat > "${domain_dir}/targets/platform/app.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels:
    lok8s.dev/type: platform
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Deploy" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "bad-deploy" ;;
      '.spec.clusterRef // ""') echo "" ;;
      '.resources[]?') echo "app.yaml" ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "deploy-no-ref"
  assert_failure
  assert_output --partial "Missing required field: spec.clusterRef"
}

# --- lint tests: kustomization.yaml reference validation ---

@test "lint catches missing kustomization.yaml in target" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/test-domain/targets/platform/kustomization.yaml"

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
          *) echo "" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_success
  assert_output --partial "Target platform/ missing kustomization.yaml"
}

@test "lint catches kustomization.yaml referencing missing file" {
  cat > "${BATS_TEST_TMPDIR}/clusters/test-domain/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - nonexistent.yaml
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *platform*)
            echo "deployment.yaml"
            echo "nonexistent.yaml"
            ;;
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_failure
  assert_output --partial "Target platform/: kustomization.yaml references missing path: nonexistent.yaml"
}

@test "lint accepts directory and URL resource references" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/.targets/shared-base"
  cat > "${BATS_TEST_TMPDIR}/clusters/test-domain/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../../../.targets/shared-base
  - https://github.com/org/repo//manifests?ref=v1.0.0
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *platform*)
            echo "../../../.targets/shared-base"
            echo "https://github.com/org/repo//manifests?ref=v1.0.0"
            ;;
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_success
}

# --- lint tests: label convention ---

@test "lint warns about missing lok8s.dev labels" {
  cat > "${BATS_TEST_TMPDIR}/clusters/test-domain/targets/platform/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: unlabeled-app
  namespace: default
spec:
  replicas: 1
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Lo" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "test" ;;
      '.spec.kind // .kind // ""') echo "Lo" ;;
      '.spec.bootstrap[]?') ;;
      '.resources[]?')
        case "${file}" in
          *crds*) echo "test-crd.yaml" ;;
          *networking*) echo "namespace.yaml" ;;
          *platform*) echo "deployment.yaml" ;;
        esac
        ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length')
        case "${file}" in
          *platform/deployment*) echo "0" ;;
          *) echo "1" ;;
        esac
        ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "test-domain"
  assert_success
  assert_output --partial "platform/deployment.yaml: missing lok8s.dev/* label"
}

# --- lint tests: unencrypted secrets ---

@test "lint warns about unencrypted secrets" {
  local secrets_dir="${BATS_TEST_TMPDIR}/clusters/test-domain/secrets"
  mkdir -p "${secrets_dir}"

  cat > "${secrets_dir}/db-password.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: db-password
type: Opaque
data:
  password: dGVzdA==
YAML

  _mock_yq_valid

  run lint::all "test-domain"
  assert_success
  assert_output --partial "secrets/db-password.yaml: appears unencrypted (contains data/stringData)"
}

@test "lint skips encrypted secret files" {
  local secrets_dir="${BATS_TEST_TMPDIR}/clusters/test-domain/secrets"
  mkdir -p "${secrets_dir}"

  echo "ENCRYPTED_CONTENT" > "${secrets_dir}/db-password.yaml.enc"
  echo "ENCRYPTED_CONTENT" > "${secrets_dir}/api-key.yaml.age"

  _mock_yq_valid

  run lint::all "test-domain"
  assert_success
  refute_output --partial "appears unencrypted"
}

@test "lint warns about stringData secrets" {
  local secrets_dir="${BATS_TEST_TMPDIR}/clusters/test-domain/secrets"
  mkdir -p "${secrets_dir}"

  cat > "${secrets_dir}/api-key.yaml" <<'YAML'
apiVersion: v1
kind: Secret
metadata:
  name: api-key
type: Opaque
stringData:
  key: super-secret-value
YAML

  _mock_yq_valid

  run lint::all "test-domain"
  assert_success
  assert_output --partial "secrets/api-key.yaml: appears unencrypted (contains data/stringData)"
}

# --- lint tests: clusterRef domain validation ---

@test "lint catches clusterRef pointing to nonexistent domain" {
  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy-bad-ref"
  mkdir -p "${domain_dir}/targets/platform"

  cat > "${domain_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: bad-ref
spec:
  clusterRef:
    domain: nonexistent.lok8s.dev
YAML

  cat > "${domain_dir}/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - app.yaml
YAML
  cat > "${domain_dir}/targets/platform/app.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels:
    lok8s.dev/type: platform
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Deploy" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "bad-ref" ;;
      '.spec.clusterRef // ""') echo "domain: nonexistent.lok8s.dev" ;;
      '.spec.clusterRef.domain // ""') echo "nonexistent.lok8s.dev" ;;
      '.resources[]?') echo "app.yaml" ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "deploy-bad-ref"
  assert_failure
  assert_output --partial "clusterRef.domain 'nonexistent.lok8s.dev' not found"
}

@test "lint catches clusterRef pointing to domain without cluster spec" {
  # Create a deploy-only domain as the target
  local ref_dir="${BATS_TEST_TMPDIR}/clusters/other.lok8s.dev"
  mkdir -p "${ref_dir}"
  cat > "${ref_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: other
YAML

  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy-wrong-ref"
  mkdir -p "${domain_dir}/targets/platform"

  cat > "${domain_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: wrong-ref
spec:
  clusterRef:
    domain: other.lok8s.dev
YAML

  cat > "${domain_dir}/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - app.yaml
YAML
  cat > "${domain_dir}/targets/platform/app.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels:
    lok8s.dev/type: platform
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Deploy" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "wrong-ref" ;;
      '.spec.clusterRef // ""') echo "domain: other.lok8s.dev" ;;
      '.spec.clusterRef.domain // ""') echo "other.lok8s.dev" ;;
      '.resources[]?') echo "app.yaml" ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "deploy-wrong-ref"
  assert_failure
  assert_output --partial "clusterRef.domain 'other.lok8s.dev' has no cluster.lok8s.yaml"
}

@test "lint passes for deploy domain with valid clusterRef" {
  # Create a valid cluster domain as the reference target
  local ref_dir="${BATS_TEST_TMPDIR}/clusters/prod.lok8s.dev"
  mkdir -p "${ref_dir}"
  cat > "${ref_dir}/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: prod
YAML

  local domain_dir="${BATS_TEST_TMPDIR}/clusters/deploy-valid-ref"
  mkdir -p "${domain_dir}/targets/platform"

  cat > "${domain_dir}/deploy.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: valid-ref
spec:
  clusterRef:
    domain: prod.lok8s.dev
YAML

  cat > "${domain_dir}/targets/platform/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - app.yaml
YAML
  cat > "${domain_dir}/targets/platform/app.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels:
    lok8s.dev/type: platform
YAML

  yq() {
    local query="$2"
    local file="$3"
    case "${query}" in
      '.kind // ""') echo "Deploy" ;;
      '.apiVersion // ""') echo "cluster.lok8s.dev/v1beta1" ;;
      '.metadata.name // ""') echo "valid-ref" ;;
      '.spec.clusterRef // ""') echo "domain: prod.lok8s.dev" ;;
      '.spec.clusterRef.domain // ""') echo "prod.lok8s.dev" ;;
      '.resources[]?') echo "app.yaml" ;;
      '.metadata.labels | keys | map(select(test("^lok8s\\.dev/"))) | length') echo "1" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  run lint::all "deploy-valid-ref"
  assert_success
  assert_output --partial "OK"
}

# --- lint tests: deprecated flat-store shadows ---

@test "lint warns about a flat-store shadow of a per-domain secret" {
  local secrets_dir="${BATS_TEST_TMPDIR}/clusters/test-domain/secrets"
  local flat="${BATS_TEST_TMPDIR}/.secrets"
  mkdir -p "${secrets_dir}" "${flat}"
  # Same Secret.* in BOTH the per-domain store and the flat store = a shadow.
  printf 'same' > "${secrets_dir}/Secret.app.default.TOKEN"
  printf 'same' > "${flat}/Secret.app.default.TOKEN"

  _mock_yq_valid

  # Warnings-only: lint still succeeds, but the shadow is reported.
  run lint::all "test-domain"
  assert_success
  assert_output --partial "Flat-store shadow: Secret.app.default.TOKEN"
}

@test "lint::secrets stays zero-exit under set -euo pipefail when a finding exists" {
  # lo runs `set -euo pipefail`; the warnings-only check pipes (check_unencrypted
  # / check_flat_shadows) must not let a check's `return 1` (on a finding) abort
  # the lint via pipefail+errexit. Regression for the `|| true` on those pipes.
  local dom="${BATS_TEST_TMPDIR}/clusters/test-domain"
  local flat="${BATS_TEST_TMPDIR}/.secrets"
  mkdir -p "${dom}/secrets" "${flat}"
  printf 'same' > "${dom}/secrets/Secret.app.default.TOKEN"
  printf 'same' > "${flat}/Secret.app.default.TOKEN"
  # domain="" lets check_unencrypted's secrets::path resolve the flat store
  # without needing PATH_CLUSTERS; both checks then fire (a finding -> return 1).
  _ls() ( set -euo pipefail; domain=""; lint::secrets "$1" )
  run _ls "${dom}"
  assert_success
}
