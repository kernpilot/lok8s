#!/usr/bin/env bats
# registry_tls_test.bats — unit tests for mkcert-signed TLS registries
# (spec.registries.tls). Covers config parsing, the .registries.json
# tls/port fields, query helpers, registry config http-block rendering,
# containerd certs.d output, the mkcert SAN list, and image-lib TLS
# detection. No real mkcert/docker calls — those are stubbed.

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

  # Copy the REAL registry config files so render tests exercise the
  # shipped http: blocks, not stubs.
  cp "${_PROJECT_ROOT}/.lok8s/drivers/lo/cluster/registry/build.yaml" \
     "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/build.yaml"
  cp "${_PROJECT_ROOT}/.lok8s/drivers/lo/cluster/registry/cache.yaml" \
     "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/cache.yaml"
  cp "${_PROJECT_ROOT}/.lok8s/drivers/lo/cluster/registry/mirror.yaml" \
     "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/mirror.yaml"
  for r in io-docker io-quay io-k8s io-ghcr; do
    echo "version: 0.1" > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/${r}.yaml"
  done
}

teardown() {
  teardown_tmpdir
}

# A slot-50 TLS-enabled spec written into the per-test clusters dir.
_write_tls_spec() {
  local tls="${1:-true}"
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<YAML
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-tls
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.50.0/24"
  registries:
    tls: ${tls}
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
  runtime: kind
  bootstrap: []
YAML
}

# ── Config knob: spec.registries.tls ─────────────────────

@test "tls: defaults to false when spec.registries.tls is absent" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"
  [ "${LOK8S_REGISTRY_TLS}" = "false" ]
  [ "${LOK8S_REGISTRY_PORT}" = "80" ]
}

@test "tls: true is parsed and sets port 443" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  [ "${LOK8S_REGISTRY_TLS}" = "true" ]
  [ "${LOK8S_REGISTRY_PORT}" = "443" ]
}

@test "tls: false keeps port 80" {
  _write_tls_spec false
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  [ "${LOK8S_REGISTRY_TLS}" = "false" ]
  [ "${LOK8S_REGISTRY_PORT}" = "80" ]
}

@test "tls: non-boolean value is rejected" {
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-bad-tls
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.50.0/24"
  registries:
    tls: "maybe"
    shared:
      enabled: true
      network:
        name: lok8s-registries
        cidr: "10.125.200.0/24"
    mirrors:
      - name: io-docker
        url: https://registry-1.docker.io
  runtime: kind
  bootstrap: []
YAML
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  run lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "spec.registries.tls must be true or false"
}

# ── .registries.json fields + query helpers ──────────────

@test "json: tls and port recorded in .registries.json" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run jq -r '.tls' "${LOK8S_REGISTRY_JSON}"
  assert_output "true"
  run jq -r '.port' "${LOK8S_REGISTRY_JSON}"
  assert_output "443"
}

@test "registry::is_tls reflects the JSON" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run registry::is_tls
  assert_success

  _write_tls_spec false
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run registry::is_tls
  assert_failure
}

@test "registry::port returns the configured port" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run registry::port
  assert_output "443"
}

@test "registry::url uses https in TLS mode, http otherwise" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run registry::url "10.125.50.101"
  assert_output "https://10.125.50.101"

  _write_tls_spec false
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run registry::url "10.125.50.101"
  assert_output "http://10.125.50.101"
}

# ── Registry config http-block rendering ─────────────────

@test "render_registry_config: TLS mode emits :443 + tls block" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  local out
  out=$(lo::render_registry_config \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/build.yaml" "")
  echo "${out}" | grep -q "addr: :443"
  echo "${out}" | grep -q "certificate: /etc/registry/certs/tls.crt"
  echo "${out}" | grep -q "key: /etc/registry/certs/tls.key"
  # The original :80 listener must be gone.
  ! echo "${out}" | grep -q "addr: :80"
}

@test "render_registry_config: plain mode emits :80, no tls block" {
  _write_tls_spec false
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  local out
  out=$(lo::render_registry_config \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/build.yaml" "")
  echo "${out}" | grep -q "addr: :80"
  ! echo "${out}" | grep -q "tls:"
  ! echo "${out}" | grep -q "addr: :443"
}

@test "render_registry_config: mirror keeps proxy.remoteurl under TLS" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  local out
  out=$(lo::render_registry_config \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/mirror.yaml" \
    "https://registry-1.docker.io")
  echo "${out}" | grep -q "remoteurl: https://registry-1.docker.io"
  echo "${out}" | grep -q "addr: :443"
}

# ── containerd certs.d output ────────────────────────────

@test "write_certs_d: TLS mode emits https + ca reference, copies rootCA" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  # Stub mkcert -CAROOT so lo::registry_ca_path resolves to a real file.
  local fake_caroot="${BATS_TEST_TMPDIR}/caroot"
  mkdir -p "${fake_caroot}"
  echo "FAKE-CA" > "${fake_caroot}/rootCA.pem"
  mkcert() { [[ "$1" == "-CAROOT" ]] && echo "${fake_caroot}"; }
  export -f mkcert

  export DOMAIN_NAME="test.lok8s.dev"
  lo::write_certs_d

  local certs_d="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/.containerd/certs.d"
  # rootCA copied into the certs.d tree
  [ -f "${certs_d}/.ca/rootCA.pem" ]
  run cat "${certs_d}/.ca/rootCA.pem"
  assert_output "FAKE-CA"

  # build registry hostname entry: https + ca, no skip_verify
  run cat "${certs_d}/lok8s.local/hosts.toml"
  assert_output --partial 'server = "https://10.125.50.101"'
  assert_output --partial 'ca = "/etc/containerd/certs.d/.ca/rootCA.pem"'
  refute_output --partial "skip_verify"

  # direct IP entry also https + ca
  run cat "${certs_d}/10.125.50.101/hosts.toml"
  assert_output --partial 'server = "https://10.125.50.101"'
  assert_output --partial "ca ="
}

@test "write_certs_d: plain mode keeps http + skip_verify, no .ca dir" {
  _write_tls_spec false
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  export DOMAIN_NAME="test.lok8s.dev"
  lo::write_certs_d

  local certs_d="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/.containerd/certs.d"
  [ ! -d "${certs_d}/.ca" ]
  run cat "${certs_d}/lok8s.local/hosts.toml"
  assert_output --partial 'server = "http://10.125.50.101"'
  assert_output --partial "skip_verify = true"
  refute_output --partial "ca ="
}

# ── mkcert SAN list ──────────────────────────────────────

@test "mkcert_registries: builds SAN list of hostnames + IPs, requires CA" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  # Stub mkcert: -CAROOT prints a dir with a rootCA; cert generation
  # records the SAN args it was called with.
  local fake_caroot="${BATS_TEST_TMPDIR}/caroot"
  mkdir -p "${fake_caroot}"
  echo "FAKE-CA" > "${fake_caroot}/rootCA.pem"
  local sans_capture="${BATS_TEST_TMPDIR}/mkcert-sans.txt"
  export SANS_CAPTURE="${sans_capture}"
  mkcert() {
    if [[ "$1" == "-CAROOT" ]]; then echo "${fake_caroot}"; return 0; fi
    # Skip the -cert-file FILE -key-file FILE prefix, capture the SANs.
    shift 4
    printf '%s\n' "$@" > "${SANS_CAPTURE}"
    # Emit dummy cert+key files at the requested paths.
    : > "${BATS_TEST_TMPDIR}/.secrets/tls/registries/tls.crt"
    : > "${BATS_TEST_TMPDIR}/.secrets/tls/registries/tls.key"
  }
  export -f mkcert
  command() {
    # `command -v mkcert` must succeed.
    if [[ "$1" == "-v" && "$2" == "mkcert" ]]; then echo "mkcert"; return 0; fi
    builtin command "$@"
  }
  export -f command

  run lo::mkcert_registries
  assert_success

  # SANs must include framework hostnames, mirror domain, and IPs.
  run cat "${sans_capture}"
  assert_output --partial "lok8s.local"
  assert_output --partial "lok8s.cache"
  assert_output --partial "docker.io"
  assert_output --partial "10.125.50.101"
  assert_output --partial "10.125.50.102"
}

@test "mkcert_registries: no-op when TLS disabled" {
  _write_tls_spec false
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  # mkcert must NOT be invoked.
  mkcert() { echo "MKCERT SHOULD NOT RUN" >&2; return 99; }
  export -f mkcert

  run lo::mkcert_registries
  assert_success
  [ ! -f "${BATS_TEST_TMPDIR}/.secrets/tls/registries/tls.crt" ]
}

@test "mkcert_registries: fails fast when TLS on but mkcert missing" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  command() {
    if [[ "$1" == "-v" && "$2" == "mkcert" ]]; then return 1; fi
    builtin command "$@"
  }
  export -f command

  run lo::mkcert_registries
  assert_failure
  assert_output --partial "mkcert is not on PATH"
}

# ── image lib TLS detection ──────────────────────────────

@test "image::_registry_tls reads the active cluster JSON" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  # Source the image lib's helper in isolation.
  ARGSH_SOURCE="" source "${_PROJECT_ROOT}/.lok8s/libs/image" 2>/dev/null || true

  run image::_registry_tls
  assert_success

  _write_tls_spec false
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  run image::_registry_tls
  assert_failure
}
