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
  vendor_lo_utils

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

  # The `docker` stub above answers every `network` subcommand with "ok", so
  # lo::registry_network's subnet verification compares "ok" against the
  # fixture's 10.125.200.0/24 and legitimately refuses ("run 'lo registry clean
  # --shared'"). That refusal used to be SWALLOWED — the call was unguarded, so
  # provision walked past it into `kind create` and this test passed on an error.
  # Now that it is guarded (lo_provision_guards_test.bats), the driver correctly
  # aborts. Stub the function so this test exercises the contract it names —
  # that provision reaches `kind create cluster` — rather than the subnet check,
  # which it was never feeding real input.
  lo::registry_network() { return 0; }

  # Same reasoning, second swallowed error — and this one is INVISIBLE on a
  # developer box. lo::registries_tls_cert mints the registry TLS Secret through
  # the secrets.lok8s.dev kustomize plugin, which is a Go binary built into
  # .kustomize/. A workstation that has run `lo kustomize build` has it, so the
  # call succeeds and the test passes; CI never builds it, so the call fails with
  # "the Secret plugin is not built at …". Unguarded, that error was swallowed in
  # BOTH environments and nobody could tell the difference. Guarded, the driver
  # correctly aborts — and the test only fails where the plugin is absent, which
  # is exactly the local-green/CI-red split that hid it (AUDIT.md r834).
  #
  # Reproduce the CI condition locally with:
  #   KUSTOMIZE_PLUGIN_HOME=$(mktemp -d) ./.bin/argsh test tests/unit/kind_contract_test.bats
  # The stub MINTS the cert rather than just returning 0: lo::registries verifies
  # ${PATH_BASE}/.secrets/tls/registries/{tls.crt,tls.key} and refuses without
  # them, so a bare `return 0` only moves the failure one line down. Mirroring the
  # real function's observable effect keeps the rest of the path honest.
  lo::registries_tls_cert() {
    mkdir -p "${PATH_BASE}/.secrets/tls/registries"
    printf 'test-cert\n' > "${PATH_BASE}/.secrets/tls/registries/tls.crt"
    printf 'test-key\n'  > "${PATH_BASE}/.secrets/tls/registries/tls.key"
  }

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

  # capi::detect_provider lives outside drivers/capi/main, so it is NOT defined
  # by the source above. This test used to reach its assertion anyway: the call
  # exited 127 ("command not found"), the assignment was unguarded, and execution
  # simply continued to the kubeconfig check. Once that call is guarded
  # (capi_provision_guards_test.bats), the driver correctly refuses at the
  # missing helper and never prints the message this test is about. Stubbing it
  # keeps the test exercising the scenario it names — a missing management
  # cluster kubeconfig — rather than a missing function.
  capi::detect_provider() { echo "hetzner"; }

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
  # driver::kubeconfig resolves the path from the cluster's metadata.name —
  # the name under which the framework + driver write the workload kubeconfig.
  yq() { [[ "$2" == ".metadata.name" ]] && echo "test-cluster" || echo ""; }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/capi/main"

  run driver::kubeconfig "test.lok8s.dev"
  assert_success
  assert_output "${PATH_BASE}/.kubeconfig/test-cluster.yaml"
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

  # The management kubeconfig must exist for the delete to run at all: a
  # missing one with a remote management cluster is now a FAILED destroy
  # (kkp_capi_destroy_guards_test.bats pins that), and this test's old green
  # came precisely from the delete being silently skipped.
  mkdir -p "${PATH_BASE}/.kubeconfig"
  printf 'apiVersion: v1\nkind: Config\n' > "${PATH_BASE}/.kubeconfig/mgmt.lok8s.dev.yaml"

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
