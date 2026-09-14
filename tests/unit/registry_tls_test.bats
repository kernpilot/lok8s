#!/usr/bin/env bats
# registry_tls_test.bats — unit tests for TLS registries (spec.registries.tls,
# default true). Covers config parsing, the .registries.json tls/port fields,
# query helpers, registry config http-block rendering, containerd certs.d output,
# the Secret-plugin-driven registry cert (SAN list + extraction), the untrusted-CA
# nudge, and image-lib TLS detection. No real docker/openssl calls — those are
# stubbed; the Secret plugin is stubbed with a fake exec.

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
  vendor_lo_utils

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

@test "tls: defaults to true when spec.registries.tls is absent" {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${FIXTURES_DIR}/lo-cluster-shared.lok8s.yaml"
  [ "${LOK8S_REGISTRY_TLS}" = "true" ]
  [ "${LOK8S_REGISTRY_PORT}" = "443" ]
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

  # write_certs_d resolves the dev CA from CAROOT directly (binary-free).
  local fake_caroot="${BATS_TEST_TMPDIR}/caroot"
  mkdir -p "${fake_caroot}"
  echo "FAKE-CA" > "${fake_caroot}/rootCA.pem"
  export CAROOT="${fake_caroot}"

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

# ── registry cert via the Secret plugin ──────────────────

# Write a fake Secret-plugin exec into a plugin home and export KUSTOMIZE_PLUGIN_HOME.
# It captures the manifest on stdin (to assert cert.hosts) and emits a k8s Secret
# with dummy base64 tls.crt/tls.key — what lo::registries_tls_cert extracts.
_stub_secret_plugin() {
  local plugin_home="${BATS_TEST_TMPDIR}/.kustomize"
  mkdir -p "${plugin_home}/secrets.lok8s.dev/v1/secret"
  cat > "${plugin_home}/secrets.lok8s.dev/v1/secret/Secret" <<STUB
#!/usr/bin/env bash
cat > "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: %s\n  tls.key: %s\n' "\$(printf -- '-----BEGIN CERTIFICATE-----\n%s\n-----END CERTIFICATE-----\n' "\$(printf FAKECRT | base64)" | base64 -w0)" "\$(printf -- '-----BEGIN PRIVATE KEY-----\n%s\n-----END PRIVATE KEY-----\n' "\$(printf FAKEKEY | base64)" | base64 -w0)"
STUB
  chmod +x "${plugin_home}/secrets.lok8s.dev/v1/secret/Secret"
  export KUSTOMIZE_PLUGIN_HOME="${plugin_home}"
  # No PATH_SECRETS: the mint hands the plugin a scratch store of its own.
  unset PATH_SECRETS
}

# A file-backed fake docker for the cert volume: volumes are directories
# under ${FAKE_VOL}, `container create` binds a container to a volume,
# `cp` moves files in (host → container) and out (container → tar stream on
# stdout), `inspect` answers the Mounts template, `restart` succeeds. Every
# argv is logged to ${DOCKER_LOG}.
_stub_docker() {
  export FAKE_VOL="${BATS_TEST_TMPDIR}/fake-docker"
  export DOCKER_LOG="${BATS_TEST_TMPDIR}/docker.log"
  mkdir -p "${FAKE_VOL}/volumes" "${FAKE_VOL}/containers"
  : > "${DOCKER_LOG}"
  docker() {
    echo "docker ${*}" >> "${DOCKER_LOG}"
    local cmd="${1}"
    shift
    case "${cmd}" in
      volume)
        case "${1}" in
          inspect) [[ -d "${FAKE_VOL}/volumes/${!#}" ]] ;;
          create)  mkdir -p "${FAKE_VOL}/volumes/${!#}" ;;
          rm)      rm -rf "${FAKE_VOL}/volumes/${!#}" ;;
        esac
        ;;
      container)
        # container create --name N --volume V:/etc/registry/certs IMAGE
        local -a argv=("${@}")
        local i name="" vol=""
        for (( i = 0; i < ${#argv[@]}; i++ )); do
          case "${argv[${i}]}" in
            --name)   name="${argv[$(( i + 1 ))]}" ;;
            --volume) vol="${argv[$(( i + 1 ))]%%:*}" ;;
          esac
        done
        mkdir -p "${FAKE_VOL}/volumes/${vol}"
        printf 'created\n\nvolume|%s\n' "${vol}" > "${FAKE_VOL}/containers/${name}"
        ;;
      cp)
        [[ "${1}" == "-L" ]] && shift   # the fake holds no symlinks; -L is argv only
        local src="${1}" dst="${2}" ctr file vol
        if [[ "${dst}" == "-" ]]; then
          ctr="${src%%:*}"; file="${src##*/}"
          vol=$(sed -n 3p "${FAKE_VOL}/containers/${ctr}" 2>/dev/null); vol="${vol#volume|}"
          [[ -f "${FAKE_VOL}/volumes/${vol}/${file}" ]] || return 1
          tar -cf - -C "${FAKE_VOL}/volumes/${vol}" "${file}"
        else
          ctr="${dst%%:*}"; file="${dst##*/}"
          vol=$(sed -n 3p "${FAKE_VOL}/containers/${ctr}" 2>/dev/null); vol="${vol#volume|}"
          cp "${src}" "${FAKE_VOL}/volumes/${vol}/${file}"
        fi
        ;;
      rm)      rm -f "${FAKE_VOL}/containers/${!#}" ;;
      restart) [[ -f "${FAKE_VOL}/containers/${!#}" ]] ;;
      inspect)
        local fmt="" target
        if [[ "${1}" == "-f" ]]; then fmt="${2}"; shift 2; fi
        target="${1}"
        [[ -f "${FAKE_VOL}/containers/${target}" ]] || return 1
        if [[ "${fmt}" == *"State.Status"* ]]; then
          sed -n 1p "${FAKE_VOL}/containers/${target}"
        elif [[ "${fmt}" == *"config-hash"* ]]; then
          sed -n 2p "${FAKE_VOL}/containers/${target}"
        elif [[ "${fmt}" == *".Mounts"* ]]; then
          local mount; mount=$(sed -n 3p "${FAKE_VOL}/containers/${target}")
          case "${mount}" in
            "volume|"*) echo "/etc/registry/certs|volume|${mount#volume|}|/var/lib/docker/volumes/${mount#volume|}/_data" ;;
            "bind|"*)   echo "/etc/registry/certs|bind||${mount#bind|}" ;;
          esac
        fi
        ;;
      run)
        local -a argv=("${@}")
        local i name="" mount="" hash=""
        for (( i = 0; i < ${#argv[@]}; i++ )); do
          case "${argv[${i}]}" in
            --name)   name="${argv[$(( i + 1 ))]}" ;;
            --label)  hash="${argv[$(( i + 1 ))]#lok8s.dev/config-hash=}" ;;
            --volume) [[ "${argv[$(( i + 1 ))]}" == *:/etc/registry/certs:* ]] && mount="volume|${argv[$(( i + 1 ))]%%:*}" ;;
          esac
        done
        printf 'running\n%s\n%s\n' "${hash}" "${mount}" > "${FAKE_VOL}/containers/${name}"
        ;;
    esac
  }
  export -f docker
}

# _vol_file <vol> <file> — the content of a fake volume entry.
_vol_file() { cat "${FAKE_VOL}/volumes/${1}/${2}"; }

# _pem <kind> <body> — the PEM block the stubs emit (the read-out accepts
# only a PEM CERTIFICATE / PEM block pair).
_pem() { printf -- '-----BEGIN %s-----\n%s\n-----END %s-----\n' "${1}" "$(printf '%s' "${2}" | base64)" "${1}"; }

@test "registries_tls_cert: mints into the volume through the io container, scratch store gone" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker

  run lo::registries_tls_cert test.lok8s.dev
  assert_success

  # The pair and the SAN set landed in the volume, nothing under .secrets.
  run _vol_file lok8s-registry-tls tls.crt
  assert_output "$(_pem CERTIFICATE FAKECRT)"
  run _vol_file lok8s-registry-tls tls.key
  assert_output "$(_pem "PRIVATE KEY" FAKEKEY)"
  run _vol_file lok8s-registry-tls .sans
  assert_output --partial "lok8s.local"
  [ ! -e "${BATS_TEST_TMPDIR}/.secrets" ]

  # SANs handed to the plugin as cert.hosts: framework hostnames, mirror domain, IPs.
  run cat "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
  assert_output --partial "lok8s.local"
  assert_output --partial "lok8s.cache"
  assert_output --partial "docker.io"
  assert_output --partial "10.125.50.101"
  assert_output --partial "10.125.50.102"

  # The docker sequence, in order: inspect (absent), create the volume,
  # populate it through the throwaway container, remove that container.
  run cat "${DOCKER_LOG}"
  assert_line --index 0 "docker volume inspect -f {{.Name}} lok8s-registry-tls"
  assert_line --index 1 "docker volume create lok8s-registry-tls"
  assert_line --index 2 "docker rm -f lok8s-registry-tls-io"
  assert_line --index 3 "docker container create --name lok8s-registry-tls-io --volume lok8s-registry-tls:/etc/registry/certs registry:2.8.3"
  assert_line --index 4 --regexp "^docker cp ${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/.registry-tls-tmp.[^/]+/tls.crt lok8s-registry-tls-io:/etc/registry/certs/tls.crt$"
  assert_line --index 5 --regexp "^docker cp .*/tls.key lok8s-registry-tls-io:/etc/registry/certs/tls.key$"
  assert_line --index 6 --regexp "^docker cp .*/.sans lok8s-registry-tls-io:/etc/registry/certs/.sans$"
  assert_line --index 7 "docker rm -f lok8s-registry-tls-io"
  # The scratch store under the domain dir is gone.
  run bash -c "ls -d '${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev'/.registry-tls-tmp.* 2>/dev/null"
  assert_output ""
}

@test "registries_tls_cert: re-mint is skipped when the SAN set is unchanged; the key is never read out" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  rm -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"   # detector: did the plugin run again?
  : > "${DOCKER_LOG}"

  run lo::registries_tls_cert test.lok8s.dev         # second call, same SANs
  assert_success
  [ ! -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml" ] # plugin NOT re-invoked
  run cat "${DOCKER_LOG}"
  refute_output --partial "docker volume create"
  assert_output --partial "docker cp lok8s-registry-tls-io:/etc/registry/certs/tls.crt -"
  assert_output --partial "docker cp lok8s-registry-tls-io:/etc/registry/certs/tls.key -"   # presence only
  assert_output --partial "docker cp lok8s-registry-tls-io:/etc/registry/certs/.sans -"
  [[ "${LO_REGISTRY_TLS_READ_CRT:-}" != *FAKEKEY* ]]
}

@test "registries_tls_cert: a volume with tls.crt but no tls.key re-mints; registries refuse it" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"

  lo::registries_tls_cert test.lok8s.dev
  rm -f "${FAKE_VOL}/volumes/lok8s-registry-tls/tls.key"
  LO_REGISTRY_TLS_CRT=""
  run lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "holds no complete tls.crt + tls.key pair"

  rm -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
  : > "${DOCKER_LOG}"
  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  [ -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml" ]          # re-minted
  [ -f "${FAKE_VOL}/volumes/lok8s-registry-tls/tls.key" ]
  run cat "${DOCKER_LOG}"
  refute_output --partial "docker volume create"
}

@test "registries_tls_cert: an empty, truncated or non-PEM entry in the volume counts as missing and re-mints" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"
  local vol="${FAKE_VOL}/volumes/lok8s-registry-tls" case
  for case in "tls.crt:" "tls.crt:garbage, not a certificate" "tls.crt:$(_pem 'PRIVATE KEY' X)" "tls.key:" "tls.key:garbage"; do
    lo::registries_tls_cert test.lok8s.dev
    printf '%s' "${case#*:}" > "${vol}/${case%%:*}"
    LO_REGISTRY_TLS_CRT=""
    run lo::registry_tls_read lok8s-registry-tls
    [ "${status}" -eq 1 ] || { echo "case ${case%%:*}: read-out accepted the damaged pair (rc ${status})"; false; }
    run lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
    assert_failure
    assert_output --partial "holds no complete tls.crt + tls.key pair"
    rm -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
    run lo::registries_tls_cert test.lok8s.dev
    assert_success
    [ -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml" ] || { echo "case ${case%%:*}: no re-mint"; false; }
    run _vol_file lok8s-registry-tls tls.crt
    assert_output "$(_pem CERTIFICATE FAKECRT)"
  done
}

@test "registries_tls_cert: a docker failure on the read-out is surfaced, not reported as 'no cert'" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"
  lo::registries_tls_cert test.lok8s.dev
  LO_REGISTRY_TLS_CRT=""
  _real_docker=$(declare -f docker)
  docker() {
    if [[ "${1}" == "container" && "${2}" == "create" ]]; then
      echo "Error response from daemon: no such image: registry:2.8.3" >&2
      return 1
    fi
    eval "${_real_docker/docker ()/_inner_docker ()}"
    _inner_docker "$@"
  }

  run lo::registries_tls_cert test.lok8s.dev
  assert_failure
  assert_output --partial "error: docker container create lok8s-registry-tls-io (volume lok8s-registry-tls) failed: Error response from daemon: no such image"
  run lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "docker container create"
  refute_output --partial "holds no complete"
}

@test "registries_tls_cert: stale .registry-tls-tmp.* dirs are swept before the mint" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  local stale="${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/.registry-tls-tmp.abandoned"
  mkdir -p "${stale}"
  printf 'OLDKEY' > "${stale}/tls.key"

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  [ ! -e "${stale}" ]
  run bash -c "ls -d '${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev'/.registry-tls-tmp.* 2>/dev/null"
  assert_output ""
}

@test "registries_tls_cert: a changed SAN set re-mints without recreating the volume" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  echo "lok8s.local" > "${FAKE_VOL}/volumes/lok8s-registry-tls/.sans"   # minted for another set
  rm -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
  : > "${DOCKER_LOG}"

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  [ -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml" ]
  run cat "${DOCKER_LOG}"
  refute_output --partial "docker volume create"
}

@test "registries_tls_cert: no-op when TLS disabled" {
  _write_tls_spec false
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_docker

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  run cat "${DOCKER_LOG}"
  assert_output ""
}

@test "registries_tls_cert: fails fast when the Secret plugin is missing" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_docker
  export KUSTOMIZE_PLUGIN_HOME="${BATS_TEST_TMPDIR}/.kustomize-empty"

  run lo::registries_tls_cert test.lok8s.dev
  assert_failure
  assert_output --partial "Secret plugin is not built"
  [ ! -d "${FAKE_VOL}/volumes/lok8s-registry-tls" ]
}

@test "registries_tls_cert: imports the legacy .secrets/tls/registries pair once, files kept, one [warn]" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  local legacy="${BATS_TEST_TMPDIR}/.secrets/tls/registries"
  mkdir -p "${legacy}"
  _pem CERTIFICATE LEGACYCRT > "${legacy}/tls.crt"
  _pem "PRIVATE KEY" LEGACYKEY > "${legacy}/tls.key"
  lo::registry_tls_sans > "${legacy}/.sans"

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  [ ! -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml" ]   # nothing minted
  run _vol_file lok8s-registry-tls tls.crt
  assert_output "$(_pem CERTIFICATE LEGACYCRT)"
  run _vol_file lok8s-registry-tls tls.key
  assert_output "$(_pem "PRIVATE KEY" LEGACYKEY)"
  [ -f "${legacy}/tls.key" ]                              # the files stay
  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  # Exactly one [warn] over both runs, naming the directory and the next step.
  run cat "${DOCKER_LOG}"
  # -L: a symlinked legacy file copies its target, not the link.
  assert_output --partial "docker cp -L ${legacy}/tls.key lok8s-registry-tls-io:/etc/registry/certs/tls.key"
  [ "$(grep -c 'docker volume create lok8s-registry-tls' "${DOCKER_LOG}")" -eq 1 ]
  [ "$(grep -c "^docker cp ${legacy}" "${DOCKER_LOG}")" -eq 0 ]
}

@test "registries_tls_cert: an incomplete legacy pair is warned about once and a fresh cert minted" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  local legacy="${BATS_TEST_TMPDIR}/.secrets/tls/registries"
  mkdir -p "${legacy}"
  _pem CERTIFICATE LEGACYCRT > "${legacy}/tls.crt"

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  assert_output --partial "registry TLS: the legacy directory ${legacy} holds an incomplete pair (tls.crt or tls.key is missing). Minting a fresh certificate."
  [ "$(grep -c '\[warn\]' <<<"${output}")" -eq 1 ]
  [ -f "${BATS_TEST_TMPDIR}/plugin-manifest.yaml" ]
  run _vol_file lok8s-registry-tls tls.crt
  assert_output "$(_pem CERTIFICATE FAKECRT)"
  run grep -c '^docker cp -L ' "${DOCKER_LOG}"
  assert_output "0"
}

@test "registries_tls_cert: the import warns once with the legacy dir and the recreate command" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  local legacy="${BATS_TEST_TMPDIR}/.secrets/tls/registries"
  mkdir -p "${legacy}"
  _pem CERTIFICATE LEGACYCRT > "${legacy}/tls.crt"
  _pem "PRIVATE KEY" LEGACYKEY > "${legacy}/tls.key"
  lo::registry_tls_sans > "${legacy}/.sans"

  run lo::registries_tls_cert test.lok8s.dev
  assert_success
  assert_output --partial "[warn]"
  assert_output --partial "registry TLS cert imported from ${legacy} into volume lok8s-registry-tls. The running registry containers still mount ${legacy}. Next: lo registry down && lo registry up"
  [ "$(grep -c '\[warn\]' <<<"${output}")" -eq 1 ]
}

@test "registries: every container mounts the volume read-only; a new cert recreates them" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"
  export LOK8S_NONINTERACTIVE=1

  lo::registries_tls_cert test.lok8s.dev
  run lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  run grep -c -- '^docker run .* --volume lok8s-registry-tls:/etc/registry/certs:ro ' "${DOCKER_LOG}"
  assert_output "3"
  run grep -c -- '^docker run .*\.secrets' "${DOCKER_LOG}"
  assert_output "0"

  # A different cert changes the config hash: the running containers are recreated.
  local plugin="${KUSTOMIZE_PLUGIN_HOME}/secrets.lok8s.dev/v1/secret/Secret"
  sed -i 's/FAKECRT/FAKECRT2/' "${plugin}"
  echo stale > "${FAKE_VOL}/volumes/lok8s-registry-tls/.sans"
  lo::registries_tls_cert test.lok8s.dev
  run lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-build configured"
}

@test "registries: fails without a cert in the volume, starts nothing" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_docker
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"

  run lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "volume lok8s-registry-tls holds no complete tls.crt + tls.key pair. Next: lo up"
  run grep -c '^docker run ' "${DOCKER_LOG}"
  assert_output "0"
}

@test "cleanup_registries: removes the cert volume with the data volumes" {
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  _stub_secret_plugin
  _stub_docker
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"

  lo::registries_tls_cert test.lok8s.dev
  [ -d "${FAKE_VOL}/volumes/lok8s-registry-tls" ]
  lo::cleanup_registries test-tls
  [ ! -d "${FAKE_VOL}/volumes/lok8s-registry-tls" ]
}

# ── lo registry tls status | renew ───────────────────────

# The registry lib on top of the driver utils, with the domain the leaves
# inherit from main::registry.
_source_registry_lib() {
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  domain::require_driver() { :; }
  # libs/registry sources its deps from ${PATH_LOK8S}: the real tree.
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/libs/registry"
  export DOMAIN_NAME=test.lok8s.dev
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"
  export LOK8S_NONINTERACTIVE=1
}

@test "tls status: no volume yet" {
  _write_tls_spec true
  _source_registry_lib
  _stub_docker

  run tls::status
  assert_success
  assert_line --index 0 "registry TLS  on"
  assert_line --index 1 "volume        lok8s-registry-tls"
  assert_line --index 2 "certificate   none. The volume does not exist. Next: lo up"
  assert_line --index 3 "containers"
  assert_line --index 4 "  lok8s-registry-build         absent"
}

@test "tls status: plain mode" {
  _write_tls_spec false
  _source_registry_lib
  _stub_docker

  run tls::status
  assert_success
  assert_output "registry TLS  off (spec.registries.tls: false in test.lok8s.dev)"
}

@test "tls status: a real leaf prints its dates, SANs and every container's mount" {
  command -v openssl >/dev/null 2>&1 || skip "openssl not installed"
  _write_tls_spec true
  _source_registry_lib
  _stub_secret_plugin
  _stub_docker
  # A self-signed leaf with the registry SANs stands in for the plugin's.
  local leaf="${BATS_TEST_TMPDIR}/leaf"
  openssl req -x509 -newkey rsa:2048 -nodes -keyout "${leaf}.key" -out "${leaf}.crt" -days 2 \
    -subj "/CN=lok8s.local" \
    -addext "subjectAltName=DNS:lok8s.local,DNS:lok8s.cache,IP:10.125.50.101" >/dev/null 2>&1
  local plugin="${KUSTOMIZE_PLUGIN_HOME}/secrets.lok8s.dev/v1/secret/Secret"
  cat > "${plugin}" <<STUB
#!/usr/bin/env bash
cat > "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: %s\n  tls.key: %s\n' "\$(base64 -w0 '${leaf}.crt')" "\$(base64 -w0 '${leaf}.key')"
STUB
  lo::registries_tls_cert test.lok8s.dev
  lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" >/dev/null
  # One container of the set still runs with the pre-v0.4.0 bind mount.
  printf 'running\n\nbind|/home/me/proj/.secrets/tls/registries\n' > "${FAKE_VOL}/containers/lok8s-registry-cache"

  run tls::status
  assert_success
  assert_line --regexp '^not before    20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$'
  assert_line --regexp '^not after     20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$'
  assert_line "sans          lok8s.local lok8s.cache 10.125.50.101"
  assert_line "  lok8s-registry-build         volume lok8s-registry-tls"
  assert_line "  lok8s-registry-cache         bind /home/me/proj/.secrets/tls/registries (legacy). Next: lo registry down && lo registry up"
  assert_line "  lok8s-registry-io-docker     volume lok8s-registry-tls"
  refute_output --partial "san set       stale"

  echo "lok8s.local" > "${FAKE_VOL}/volumes/lok8s-registry-tls/.sans"
  run tls::status
  assert_line "san set       stale. The registries changed since the mint. Next: lo registry tls renew"
}

@test "tls status: the dates print the same through the BSD date fallback (no -d, -j -f)" {
  command -v openssl >/dev/null 2>&1 || skip "openssl not installed"
  _write_tls_spec true
  _source_registry_lib
  _stub_secret_plugin
  _stub_docker
  local leaf="${BATS_TEST_TMPDIR}/leaf"
  openssl req -x509 -newkey rsa:2048 -nodes -keyout "${leaf}.key" -out "${leaf}.crt" -days 2 \
    -subj "/CN=lok8s.local" -addext "subjectAltName=DNS:lok8s.local" >/dev/null 2>&1
  local plugin="${KUSTOMIZE_PLUGIN_HOME}/secrets.lok8s.dev/v1/secret/Secret"
  cat > "${plugin}" <<STUB
#!/usr/bin/env bash
cat > "${BATS_TEST_TMPDIR}/plugin-manifest.yaml"
printf 'apiVersion: v1\nkind: Secret\nmetadata:\n  name: registries-tls\n  namespace: lok8s-system\ntype: kubernetes.io/tls\ndata:\n  tls.crt: %s\n  tls.key: %s\n' "\$(base64 -w0 '${leaf}.crt')" "\$(base64 -w0 '${leaf}.key')"
STUB
  lo::registries_tls_cert test.lok8s.dev

  run tls::status
  assert_success
  local gnu_before gnu_after
  gnu_before=$(grep '^not before' <<<"${output}")
  gnu_after=$(grep '^not after' <<<"${output}")
  [[ "${gnu_before}" =~ ^not\ before\ +20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z$ ]]

  # A BSD-shaped date: `-d`/`-ud` fail, `-j -f FMT INPUT +OUT` succeeds
  # (answered by the real GNU date on the same input, so the stub proves the
  # fallback branch is taken and prints the same string).
  date() {
    case "${1}" in
      -d|-ud|-u) return 1 ;;
      -j)
        # The fallback must hand BSD date openssl's exact shape and the
        # matching format string; anything else fails loudly.
        if [[ "${2}" != "-f" || "${3}" != '%b %e %H:%M:%S %Y %Z' ]]; then
          echo "date stub: unexpected -j arguments: ${*}" >&2
          return 99
        fi
        if [[ ! "${4}" =~ ^[A-Z][a-z]{2}\ [\ 0-9][0-9]\ [0-9]{2}:[0-9]{2}:[0-9]{2}\ [0-9]{4}\ GMT$ ]]; then
          echo "date stub: input is not openssl's 'Mon [ d]d HH:MM:SS YYYY GMT' shape: '${4}'" >&2
          return 99
        fi
        command date -ud "${4}" "${5}"
        ;;
      *) command date "$@" ;;
    esac
  }
  export -f date
  run tls::status
  assert_success
  assert_line "${gnu_before}"
  assert_line "${gnu_after}"
  # A single-digit day is space-padded by openssl (`Sep  4 …`); the same
  # stub takes it through the fallback branch.
  run _tls_rfc3339 "Sep  4 15:00:00 2026 GMT"
  assert_success
  assert_output "2026-09-04T15:00:00Z"
  run _tls_rfc3339 "2026-09-04"   # not openssl's shape: the stub refuses, the helper echoes the input
  assert_output "2026-09-04"
  unset -f date
}

@test "tls renew: re-mints into the volume and restarts only the existing containers" {
  _write_tls_spec true
  _source_registry_lib
  _stub_secret_plugin
  _stub_docker

  lo::registries_tls_cert test.lok8s.dev
  lo::registries test.lok8s.dev "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" >/dev/null
  rm -f "${FAKE_VOL}/containers/lok8s-registry-io-docker"   # one container of the set is absent
  sed -i 's/FAKECRT/RENEWED/' "${KUSTOMIZE_PLUGIN_HOME}/secrets.lok8s.dev/v1/secret/Secret"
  : > "${DOCKER_LOG}"

  run tls::renew
  assert_success
  assert_line --index 0 "registry TLS renewed: volume lok8s-registry-tls, 6 SANs"
  assert_line --index 1 "restarted lok8s-registry-build"
  assert_line --index 2 "restarted lok8s-registry-cache"
  assert_line --index 3 "Next: run 'lo up' in each cluster that uses the set. It picks the certificate up there."
  refute_output --partial "restarted lok8s-registry-io-docker"
  run _vol_file lok8s-registry-tls tls.crt
  assert_output "$(_pem CERTIFICATE RENEWED)"
  run cat "${DOCKER_LOG}"
  refute_output --partial "docker volume create"
  assert_output --partial "docker restart lok8s-registry-build"
  refute_output --partial "docker restart lok8s-registry-io-docker"
}

@test "tls renew: refuses in plain mode" {
  _write_tls_spec false
  _source_registry_lib
  _stub_docker

  run tls::renew
  assert_failure
  assert_output "error: spec.registries.tls is false in test.lok8s.dev. There is no certificate to renew."
  run cat "${DOCKER_LOG}"
  assert_output ""
}

# ── untrusted-CA nudge ───────────────────────────────────

@test "registries_tls_nudge: warns to run lo trust when the CA is untrusted" {
  command -v openssl >/dev/null || skip "openssl not available"
  _write_tls_spec true
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  export CAROOT="${BATS_TEST_TMPDIR}/empty-caroot"   # no rootCA.pem → untrusted
  mkdir -p "${CAROOT}"

  run lo::registries_tls_nudge
  assert_success   # non-fatal
  assert_output --partial "lo trust"
}

@test "registries_tls_nudge: silent when TLS disabled" {
  _write_tls_spec false
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  run lo::registries_tls_nudge
  assert_success
  [ -z "${output}" ]
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

@test "image::_registry_tls follows --domain, not DOMAIN_NAME (issue #89)" {
  # image::list / image::cache resolve their own `domain` (a `--domain X` sets
  # it), but this helper keyed off DOMAIN_NAME alone. A `lo image list
  # --domain X` therefore paired X's cache IP with the OTHER domain's TLS
  # scheme and every request failed.
  mkdir -p "${PATH_CLUSTERS}/tls.lok8s.dev" "${PATH_CLUSTERS}/plain.lok8s.dev"
  printf '{"tls":true,"registries":[]}\n'  > "${PATH_CLUSTERS}/tls.lok8s.dev/.registries.json"
  printf '{"tls":false,"registries":[]}\n' > "${PATH_CLUSTERS}/plain.lok8s.dev/.registries.json"

  ARGSH_SOURCE="" source "${_PROJECT_ROOT}/.lok8s/libs/image" 2>/dev/null || true
  unset LOK8S_REGISTRY_JSON

  # The two disagree on purpose: env says plain, the resolved domain says TLS.
  export DOMAIN_NAME="plain.lok8s.dev"
  local domain="tls.lok8s.dev"

  run image::_registry_tls
  assert_success

  # And the explicit argument wins over both.
  run image::_registry_tls "plain.lok8s.dev"
  assert_failure

  # With no domain in scope it still falls back to the env, for Tilt's local().
  unset domain
  run image::_registry_tls
  assert_failure
}

@test "image::_cache_one PASSES its domain to the TLS check" {
  # The optional [domain] parameter added for issue #89 was dead at both
  # production call sites: the behaviour rode entirely on a dynamically scoped
  # `domain` that image::_cache_one never declared, and _cache_one runs in a
  # background subshell under `--all`. It takes the domain explicitly now, and
  # this is what says so.
  mkdir -p "${PATH_CLUSTERS}/tls.lok8s.dev" "${PATH_CLUSTERS}/plain.lok8s.dev"
  printf '{"tls":true,"registries":[]}\n'  > "${PATH_CLUSTERS}/tls.lok8s.dev/.registries.json"
  printf '{"tls":false,"registries":[]}\n' > "${PATH_CLUSTERS}/plain.lok8s.dev/.registries.json"

  ARGSH_SOURCE="" source "${_PROJECT_ROOT}/.lok8s/libs/image" 2>/dev/null || true
  unset LOK8S_REGISTRY_JSON
  # Everything ambient says "plain" — only the argument says "tls".
  export DOMAIN_NAME="plain.lok8s.dev"

  local log="${BATS_TEST_TMPDIR}/docker.log"
  docker() { echo "$*" >> "${log}"; return 0; }

  image::_cache_one svc registry.example/svc:1 branch 1 10.0.0.1 0 tls.lok8s.dev
  run cat "${log}"
  assert_output --partial "manifest inspect"
  refute_output --partial "--insecure"

  : > "${log}"
  image::_cache_one svc registry.example/svc:1 branch 1 10.0.0.1 0 plain.lok8s.dev
  run cat "${log}"
  assert_output --partial "--insecure"
}
