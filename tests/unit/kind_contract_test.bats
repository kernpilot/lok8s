#!/usr/bin/env bats
# kind_contract_test.bats — unit tests for kind contract implementations
# Tests .lok8s/drivers/lo/main and .lok8s/drivers/capi/main contract functions with mocked externals

setup() {
  load "../test_helper"
  setup_tmpdir
  export LOK8S_NONINTERACTIVE=1   # kapply::run → direct exec (no progress UI in tests)

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/credentials.sh"

  # argsh `:args` builtin — minimal positional-binding stub.
  :args() {
    shift  # drop description
    local -a _pos_names=()
    local i
    for ((i=0; i<${#args[@]}; i+=2)); do
      [[ "${args[i]}" == *"|"* ]] && continue
      _pos_names+=("${args[i]}")
    done
    local name
    for name in "${_pos_names[@]}"; do
      if (( $# > 0 )); then
        printf -v "${name}" '%s' "$1"
        shift
      fi
    done
  }
  export -f :args

  # kubehz hooks — stub as no-ops
  kubehz::read_config() { :; }
  kubehz::validate_config() { :; }
  export -f kubehz::read_config kubehz::validate_config

  # Create domain structure
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/driver"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/utils"

  # drivers/lo/main sources ${PATH_LOK8S}/utils/{ip,oidc,kapply}.sh
  cp "${_PROJECT_ROOT}/.lok8s/utils/ip.sh" \
    "${BATS_TEST_TMPDIR}/.lok8s/utils/ip.sh"
  cp "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh" \
    "${BATS_TEST_TMPDIR}/.lok8s/utils/oidc.sh"
  cp "${_PROJECT_ROOT}/.lok8s/utils/kapply.sh" \
    "${BATS_TEST_TMPDIR}/.lok8s/utils/kapply.sh"

  # Copy Lo fixture
  cp "${FIXTURES_DIR}/lo-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
}

teardown() {
  teardown_tmpdir
}

# ── .lok8s/drivers/lo/main ─────────────────────────────────────

@test "lo.sh driver::provision calls kind create cluster" {
  # Use the real lo-cluster-shared.lok8s.yaml fixture (slot 125, 10.125.x
  # layout) and real yq. Mocks only the externals that actually run code
  # we don't want to hit (docker, kind, kubectl, helm, mkcert).
  command -v yq >/dev/null 2>&1 || skip "yq not in PATH"

  cp "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  docker() {
    case "$1" in
      network) echo "ok" ;;
      inspect) echo "false" ;;
      volume) echo "ok" ;;
      run) echo "ok" ;;
      rm) echo "ok" ;;
      exec) echo "ok" ;;
      *) echo "docker $*" ;;
    esac
  }
  export -f docker

  kind() {
    case "$1" in
      get)
        case "$2" in
          clusters) echo "" ;;  # No existing clusters
          kubeconfig) echo "apiVersion: v1" ;;
          nodes) echo "test-local-control-plane" ;;
        esac
        ;;
      create) echo "Creating cluster" ;;
      *) echo "kind $*" ;;
    esac
  }
  export -f kind

  envsubst() { cat; }
  export -f envsubst
  kubectl() { echo "ok"; }
  export -f kubectl
  helm() { echo "ok"; }
  export -f helm
  mkcert() { touch "${BATS_TEST_TMPDIR}/.secrets/tls/tls.crt" "${BATS_TEST_TMPDIR}/.secrets/tls/tls.key"; }
  export -f mkcert

  # Make ip.sh + the registry config files available under PATH_BASE so
  # the provider can source them via PATH_SCRIPTS lookups.
  export PATH_SCRIPTS="${_PROJECT_ROOT}/.lok8s"
  echo 'kind: Cluster' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/config.yaml"
  echo '{}' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns/corefile.yaml"
  echo '{}' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns/expose.yaml"
  echo '[]' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns/patch.json"
  for r in build cache io-docker io-quay io-k8s io-ghcr mirror; do
    echo "version: 0.1" > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/${r}.yaml"
  done

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"

  run driver::provision "test.lok8s.dev"
  assert_success
}

@test "lo.sh driver::destroy deletes kind cluster" {
  local deleted=""

  yq() {
    case "$2" in
      '.metadata.name') echo "test-local" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kind() {
    case "$1" in
      delete) deleted="yes" ;;
      *) echo "kind $*" ;;
    esac
  }
  export -f kind

  docker() { echo "ok"; }
  export -f docker

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"

  run driver::destroy "test.lok8s.dev"
  assert_success
}

@test "lo.sh driver::status returns Running for existing cluster" {
  yq() {
    case "$2" in
      '.metadata.name') echo "test-local" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kind() {
    case "$1" in
      get)
        if [[ "$2" == "clusters" ]]; then
          echo "test-local"
        fi
        ;;
    esac
  }
  export -f kind

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"

  run driver::status "test.lok8s.dev"
  assert_success
  assert_output "Running"
}

@test "lo.sh driver::status returns NotFound for missing cluster" {
  yq() {
    case "$2" in
      '.metadata.name') echo "test-local" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kind() {
    case "$1" in
      get)
        if [[ "$2" == "clusters" ]]; then
          echo "other-cluster"
        fi
        ;;
    esac
  }
  export -f kind

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"

  run driver::status "test.lok8s.dev"
  assert_success
  assert_output "NotFound"
}

@test "lo.sh driver::kubeconfig extracts kubeconfig" {
  yq() {
    echo "test-local"
  }
  export -f yq

  kind() {
    if [[ "$1" == "get" && "$2" == "kubeconfig" ]]; then
      echo "apiVersion: v1"
      echo "kind: Config"
    fi
  }
  export -f kind

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"

  run driver::kubeconfig "test.lok8s.dev"
  assert_success
  # Should output the kubeconfig path
  assert_output --partial ".kubeconfig/test-local.yaml"

  # Verify file was created
  [ -f "${BATS_TEST_TMPDIR}/.kubeconfig/test-local.yaml" ]
}

@test "lo.sh driver::provision fails for unsupported runtime" {
  yq() {
    case "$2" in
      '.spec.runtime // "kind"') echo "k3d" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"

  run driver::provision "test.lok8s.dev"
  assert_failure
  assert_output --partial "unsupported Lo runtime"
}

# ── .lok8s/drivers/capi/main ───────────────────────────────────

@test "capi.sh driver::provision fails without management cluster kubeconfig" {
  cp "${FIXTURES_DIR}/capi-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  yq() {
    case "$1" in
      -r)
        case "$2" in
          '.spec.managementCluster.domain // ""') echo "mgmt.lok8s.dev" ;;
          *) echo "" ;;
        esac
        ;;
      -e) return 0 ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::provision "test.lok8s.dev"
  assert_failure
  assert_output --partial "management cluster kubeconfig not found"
}

@test "capi.sh driver::provision fails for SaaS mode (not yet implemented)" {
  # Create a spec without managementCluster
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: test-saas
spec:
  kubernetes:
    version: "v1.31.10"
YAML

  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::provision "test.lok8s.dev"
  assert_failure
  assert_output --partial "spec.managementCluster.domain is required"
}

@test "capi.sh driver::status returns NotFound when kubectl fails" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "mgmt.lok8s.dev" ;;
      '.metadata.name') echo "test-prod" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubectl() { return 1; }
  export -f kubectl

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::status "test.lok8s.dev"
  assert_success
  assert_output "NotFound"
}

@test "capi.sh driver::status returns Unknown for SaaS mode" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::status "test.lok8s.dev"
  assert_success
  assert_output "Unknown"
}

@test "capi.sh driver::destroy fails for SaaS mode" {
  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "" ;;
      '.metadata.name') echo "test-saas" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::destroy "test.lok8s.dev"
  assert_failure
  assert_output --partial "spec.managementCluster.domain is required"
}

@test "capi.sh driver::kubeconfig returns expected path" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::kubeconfig "test.lok8s.dev"
  assert_success
  assert_output "${PATH_BASE}/.kubeconfig/test.lok8s.dev.yaml"
}

@test "capi.sh driver::ensure_credentials sets up hetzner secret" {
  yq() {
    case "$2" in
      '.spec.credentials.secretName // (.metadata.name + "-credentials")') echo "test-creds" ;;
      '.spec.cluster.namespace // "default"') echo "default" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  kubectl() { echo "kubectl $*"; }
  export -f kubectl

  export HCLOUD_TOKEN="test-token"

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/generate"
  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::ensure_credentials \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" \
    "hetzner" \
    "/tmp/kubeconfig.yaml"
  assert_success
}

@test "capi.sh driver::destroy deletes cluster on management cluster" {
  cp "${FIXTURES_DIR}/capi-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  yq() {
    case "$2" in
      '.spec.managementCluster.domain // ""') echo "mgmt.lok8s.dev" ;;
      '.metadata.name') echo "test-production" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  local deleted_cluster=""
  kubectl() {
    if [[ "$1" == "delete" && "$2" == "cluster" ]]; then
      deleted_cluster="$3"
    fi
  }
  export -f kubectl

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::destroy "test.lok8s.dev"
  assert_success
}
