#!/usr/bin/env bats
# shared_registry_test.bats — unit tests for shared/non-shared registry
# resolution under the 10.125.x slot layout.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"

  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.kubeconfig"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/coredns"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/utils"

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

# ── Shared registry mode ─────────────────────────────────
#
# Slot 125 (default cluster) with shared:true. Pull-through mirrors live
# on the shared registry network 10.125.200.0/24, framework-private
# registries (build/cache) live on the project /24 at .101/.102.

@test "shared:true sets LOK8S_REGISTRY_SHARED=true" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"
  [ "${LOK8S_REGISTRY_SHARED}" = "true" ]
}

@test "shared:true sets registry network name and subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"
  [ "${LOK8S_REGISTRY_NETWORK}" = "lok8s-registries" ]
  [ "${LOK8S_REGISTRY_NETWORK_CIDR}" = "10.125.200.0/24" ]
}

@test "shared:true assigns pull-through mirror IPs from shared subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  # Pull-through mirrors with url get sequential IPs from the shared
  # registry network, starting at .2 (docker bridge gateway takes .1).
  # Mirror env var names: io-docker → IO_DOCKER (hyphens → underscores).
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.200.2" ]
  [ "${LOK8S_REGISTRY_IP_IO_QUAY}"   = "10.125.200.3" ]
  [ "${LOK8S_REGISTRY_IP_IO_K8S}"    = "10.125.200.4" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.200.5" ]
}

@test "shared:true keeps build+cache on project subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  # Framework-private registries always live on the project /24 at fixed
  # offsets, even in shared mode.
  [ "${LOK8S_REGISTRY_IP_BUILD}" = "10.125.125.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}" = "10.125.125.102" ]
}

@test "shared:true exports individual IP env vars" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  [ "${LOK8S_REGISTRY_IP_BUILD}"     = "10.125.125.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}"     = "10.125.125.102" ]
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.200.2" ]
  [ "${LOK8S_REGISTRY_IP_IO_QUAY}"   = "10.125.200.3" ]
  [ "${LOK8S_REGISTRY_IP_IO_K8S}"    = "10.125.200.4" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.200.5" ]
}

# ── Non-shared registry mode ─────────────────────────────
#
# Slot 125 with shared.enabled: false. All registries (private +
# pull-through) live on the project /24:
#   .101 build, .102 cache, .103 io-docker, .104 io-quay, .105 io-k8s, .106 io-ghcr

@test "shared.enabled: false sets LOK8S_REGISTRY_SHARED=false" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-no-shared.lok8s.yaml"
  [ "${LOK8S_REGISTRY_SHARED}" = "false" ]
}

@test "shared.enabled: false assigns all IPs from project subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-no-shared.lok8s.yaml"

  [ "${LOK8S_REGISTRY_IP_BUILD}"     = "10.125.125.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}"     = "10.125.125.102" ]
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.125.103" ]
  [ "${LOK8S_REGISTRY_IP_IO_QUAY}"   = "10.125.125.104" ]
  [ "${LOK8S_REGISTRY_IP_IO_K8S}"    = "10.125.125.105" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.125.106" ]
}

@test "shared.enabled: false exports IPs from project subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-no-shared.lok8s.yaml"

  [ "${LOK8S_REGISTRY_IP_BUILD}"     = "10.125.125.101" ]
  [ "${LOK8S_REGISTRY_IP_CACHE}"     = "10.125.125.102" ]
  [ "${LOK8S_REGISTRY_IP_IO_DOCKER}" = "10.125.125.103" ]
  [ "${LOK8S_REGISTRY_IP_IO_QUAY}"   = "10.125.125.104" ]
  [ "${LOK8S_REGISTRY_IP_IO_K8S}"    = "10.125.125.105" ]
  [ "${LOK8S_REGISTRY_IP_IO_GHCR}"   = "10.125.125.106" ]
}

# ── No-back-compat: bare specs error out ─────────────────

@test "absent spec.registries: errors out" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${FIXTURES_DIR}/lo-cluster.lok8s.yaml"
  assert_failure
}

# ── IP validation ────────────────────────────────────────

@test "validate_ips: shared mirrors validated against registry subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  # Project subnet for slot 125 is 10.125.125.0/24. Mirrors live in
  # 10.125.200.0/24 (the shared registry subnet), build/cache live in
  # the project subnet. Both are valid for their respective subnets.
  run lo::validate_ips "10.125.125.0/24"
  assert_success
}

@test "validate_ips: shared mirror outside registry subnet fails" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  # Registry state lives in LOK8S_REGISTRY_JSON now — mutate the JSON
  # to put io-docker outside the shared subnet.
  local tmp
  tmp=$(jq '(.registries[] | select(.name == "io-docker") | .ip) |= "10.0.0.99"' \
    "${LOK8S_REGISTRY_JSON}")
  echo "${tmp}" > "${LOK8S_REGISTRY_JSON}"

  run lo::validate_ips "10.125.125.0/24"
  assert_failure
  assert_output --partial "outside subnet"
}

@test "validate_ips: build IP validated against project subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  local tmp
  tmp=$(jq '(.registries[] | select(.name == "build") | .ip) |= "10.0.0.99"' \
    "${LOK8S_REGISTRY_JSON}")
  echo "${tmp}" > "${LOK8S_REGISTRY_JSON}"

  run lo::validate_ips "10.125.125.0/24"
  assert_failure
  assert_output --partial "outside subnet"
}

@test "validate_ips: MetalLB pool validated against project subnet" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"

  # Pool inside project /24 should pass.
  run lo::validate_ips "10.125.125.0/24" "10.125.125.125-10.125.125.150"
  assert_success
}
