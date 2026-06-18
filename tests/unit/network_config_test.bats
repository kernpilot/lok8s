#!/usr/bin/env bats
# network_config_test.bats — unit tests for spec.network + spec.registries
# resolution under the 10.125.x slot layout.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/utils"

  # Copy ip + oidc utilities so drivers/lo/main can source them
  cp "${_PROJECT_ROOT}/.lok8s/utils/ip.sh" "${BATS_TEST_TMPDIR}/.lok8s/utils/ip.sh"
  cp "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh" "${BATS_TEST_TMPDIR}/.lok8s/utils/oidc.sh"

  echo 'kind: Cluster' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/config.yaml"
  echo '{}' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns/corefile.yaml"
  echo '{}' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns/expose.yaml"
  echo '[]' > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns/patch.json"

  for r in build cache io-docker io-quay io-k8s io-ghcr mirror; do
    echo "version: 0.1" > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/${r}.yaml"
  done
}

teardown() {
  teardown_tmpdir
}

# ── lo::read_network_config ──────────────────────────────

@test "read_network_config: requires explicit cidr" {
  yq() {
    case "$2" in
      '.spec.network.name // ""') echo "lok8s" ;;
      '.spec.network.cidr // ""') echo "" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.network.cidr is required"
}

@test "read_network_config: requires explicit name" {
  yq() {
    case "$2" in
      '.spec.network.name // ""') echo "" ;;
      '.spec.network.cidr // ""') echo "10.125.50.0/24" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.network.name is required"
}

@test "read_network_config: derives base IP from cidr (slot 50)" {
  # Use the real fixture (slot 50) so we exercise the actual yq parser
  cp "${FIXTURES_DIR}/lo-cluster-network.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  [ "${KIND_EXPERIMENTAL_DOCKER_NETWORK}" = "lok8s-test" ]
  [ "${LOK8S_NETWORK_CIDR}" = "10.125.50.0/24" ]
  [ "${LOK8S_NETWORK_BASE_IP}" = "10.125.50.0" ]
  # Private registries always live on the project subnet at .101/.102
  [ "${LOK8S_REGISTRY_IP_BUILD}" = "10.125.50.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}" = "10.125.50.102" ]
  # Non-shared mode: pull-throughs follow on the project subnet at .103+
  # (mirror names are uppercased with - → _: io-docker → IO_DOCKER)
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.50.103" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.50.106" ]
}

@test "read_network_config: exports individual registry IP env vars" {
  cp "${FIXTURES_DIR}/lo-cluster-network.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  [ "${LOK8S_REGISTRY_IP_BUILD}"     = "10.125.50.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}"     = "10.125.50.102" ]
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.50.103" ]
  [ "${LOK8S_REGISTRY_IP_IO_QUAY}"   = "10.125.50.104" ]
  [ "${LOK8S_REGISTRY_IP_IO_K8S}"    = "10.125.50.105" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.50.106" ]
}

@test "read_network_config: back-compat alias LOK8S_NETWORK_SUBNET mirrors LOK8S_NETWORK_CIDR" {
  cp "${FIXTURES_DIR}/lo-cluster-network.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  [ "${LOK8S_NETWORK_SUBNET}" = "${LOK8S_NETWORK_CIDR}" ]
  [ "${LOK8S_NETWORK_SUBNET}" = "10.125.50.0/24" ]
}

# ── Schema validation: build/cache in mirrors list is rejected ──

@test "read_registry_config: rejects 'build' in spec.registries.mirrors" {
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-reject-build
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.125.0/24"
  registries:
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: build
      - name: io-docker
        url: https://registry-1.docker.io
  runtime: kind
  bootstrap: []
YAML

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "'build' is reserved"
}

@test "read_registry_config: rejects 'cache' in spec.registries.mirrors" {
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-reject-cache
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.125.0/24"
  registries:
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: cache
      - name: io-docker
        url: https://registry-1.docker.io
  runtime: kind
  bootstrap: []
YAML

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "'cache' is reserved"
}

@test "read_registry_config: mirror missing 'url' errors out" {
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-missing-url
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.125.0/24"
  registries:
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
  runtime: kind
  bootstrap: []
YAML

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "url is required"
}

# ── *.lok8s.dev defaults ─────────────────────────────────

@test "lo::slot_from_domain: lok8s.dev returns 125" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/slot125"
  cat > "${BATS_TEST_TMPDIR}/clusters/slot125/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
YAML
  run lo::slot_from_domain "${BATS_TEST_TMPDIR}/clusters/slot125/cluster.lok8s.yaml"
  assert_success
  assert_output "125"
}

@test "lo::slot_from_domain: 126.lok8s.dev returns 126" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-noop
spec:
  cluster:
    domain: 126.lok8s.dev
YAML
  run lo::slot_from_domain "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output "126"
}

@test "lo::slot_from_domain: non-lok8s.dev returns empty" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/prod.example.com"
  cat > "${BATS_TEST_TMPDIR}/clusters/prod.example.com/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: prod
spec:
  cluster:
    domain: prod.example.com
YAML
  run lo::slot_from_domain "${BATS_TEST_TMPDIR}/clusters/prod.example.com/cluster.lok8s.yaml"
  assert_success
  assert_output ""
}

@test "lo::slot_from_domain: rejects reserved slot 200" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/200.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/200.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: invalid
spec:
  cluster:
    domain: 200.lok8s.dev
YAML
  run lo::slot_from_domain "${BATS_TEST_TMPDIR}/clusters/200.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output ""
}

@test "read_network_config: minimal *.lok8s.dev spec gets full defaults" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-noop
spec:
  cluster:
    domain: 126.lok8s.dev
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml"

  # Network derived from slot + metadata.name.
  [ "${KIND_EXPERIMENTAL_DOCKER_NETWORK}" = "e2e-noop" ]
  [ "${LOK8S_NETWORK_CIDR}" = "10.125.126.0/24" ]
  [ "${LOK8S_NETWORK_BASE_IP}" = "10.125.126.0" ]

  # Registries defaulted to shared:enabled + standard mirrors.
  [ "${LOK8S_REGISTRY_SHARED}" = "true" ]
  [ "${LOK8S_REGISTRY_NETWORK_CIDR}" = "10.125.200.0/24" ]
  [ "${LOK8S_REGISTRY_IP_BUILD}"     = "10.125.126.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}"     = "10.125.126.102" ]
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.200.2" ]
  [ "${LOK8S_REGISTRY_IP_IO_QUAY}"   = "10.125.200.3" ]
  [ "${LOK8S_REGISTRY_IP_IO_K8S}"    = "10.125.200.4" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.200.5" ]
}

@test "read_node_config: bare lok8s.dev defaults hostPorts to true" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: local
spec:
  cluster:
    domain: lok8s.dev
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_node_config "${BATS_TEST_TMPDIR}/clusters/lok8s.dev/cluster.lok8s.yaml"
  [ "${LOK8S_CP_COUNT}" = "1" ]
  [ "${LOK8S_WORKER_COUNT}" = "0" ]
  [ "${LOK8S_HOST_PORTS}" = "true" ]
}

@test "read_node_config: numbered slot defaults hostPorts to false" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-noop
spec:
  cluster:
    domain: 126.lok8s.dev
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_node_config "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml"
  [ "${LOK8S_HOST_PORTS}" = "false" ]
}

@test "read_lb_config: *.lok8s.dev slot-derives a default pool" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev"
  cat > "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: e2e-noop
spec:
  cluster:
    domain: 126.lok8s.dev
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_lb_config "${BATS_TEST_TMPDIR}/clusters/126.lok8s.dev/cluster.lok8s.yaml"
  [ "${LOK8S_LB_POOL}" = "10.125.126.125-10.125.126.150" ]
}

@test "read_lb_config: non-lok8s.dev has no default pool" {
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/prod.example.com"
  cat > "${BATS_TEST_TMPDIR}/clusters/prod.example.com/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: prod
spec:
  cluster:
    domain: prod.example.com
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_lb_config "${BATS_TEST_TMPDIR}/clusters/prod.example.com/cluster.lok8s.yaml"
  [ -z "${LOK8S_LB_POOL}" ]
}

@test "read_registry_config: non-lok8s.dev domain gets default registries+mirrors" {
  # Domain-independent defaults (shared registries, io-* mirror set)
  # apply to ANY domain — including non-lok8s.dev — as long as
  # spec.network.{name,cidr} is explicit. Only slot-derived fields
  # (network.cidr, loadBalancer.pool, hostPorts) are *.lok8s.dev-only.
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/prod.example.com"
  cat > "${BATS_TEST_TMPDIR}/clusters/prod.example.com/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: prod
spec:
  cluster:
    domain: prod.example.com
  network:
    name: prod
    cidr: "192.168.1.0/24"
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/prod.example.com/cluster.lok8s.yaml"

  # Registries defaulted even without *.lok8s.dev.
  [ "${LOK8S_REGISTRY_SHARED}" = "true" ]
  [ "${LOK8S_REGISTRY_NETWORK_CIDR}" = "10.125.200.0/24" ]
  [ "${LOK8S_REGISTRY_IP_BUILD}"     = "192.168.1.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}"     = "192.168.1.102" ]
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.200.2" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.200.5" ]
}

# ── No-back-compat: legacy bare specs error out ──────────

@test "no spec.network on non-lok8s.dev: errors out" {
  # lo-cluster.lok8s.yaml has no spec.network, spec.registries, or
  # spec.bootstrap and its domain (test.lok8s.dev) is NOT a slot-
  # parseable *.lok8s.dev (test != digits). Non-slot domains must
  # supply network.name + network.cidr explicitly.
  cp "${FIXTURES_DIR}/lo-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.network"
}
