#!/usr/bin/env bats
# registry_lifecycle_test.bats — unit tests for the idempotent registry
# container lifecycle (lo::registries): desired-state reconciliation via the
# config-hash label, durable config files under LO_REGISTRY_STATE_DIR, the
# squatted-IP eviction/re-attach heal, portable ${REMOTE_URL} rendering (no
# envsubst), and the reserved dynamic range for the shared network.
#
# Docker is stubbed with a file-backed fake: containers/<name> holds
# "status\nhash", networks/<net> holds "ip/prefix name" lines — enough state
# for the reconcile matrix to be exercised end-to-end without a daemon.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state"

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

  for r in build cache; do
    echo "version: 0.1" > "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/${r}.yaml"
  done
  # Real mirror template (no per-mirror io-docker.yaml, matching the shipped
  # config dir) so the ${REMOTE_URL} substitution is exercised via fallback.
  cp "${_PROJECT_ROOT}/.lok8s/drivers/lo/cluster/registry/mirror.yaml" \
     "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/mirror.yaml"

  # File-backed fake docker state.
  FAKE_DOCKER="${BATS_TEST_TMPDIR}/fake-docker"
  DOCKER_LOG="${FAKE_DOCKER}/log"
  mkdir -p "${FAKE_DOCKER}/containers" "${FAKE_DOCKER}/networks"
  : > "${DOCKER_LOG}"
  : > "${FAKE_DOCKER}/networks/lok8s"
  : > "${FAKE_DOCKER}/networks/lok8s-registries"
  export FAKE_DOCKER DOCKER_LOG
}

teardown() {
  teardown_tmpdir
}

# Plain-HTTP spec (tls: false skips the cert mount plumbing) with a single
# shared pull-through mirror.
_write_spec() {
  cat > "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" <<'YAML'
apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-lifecycle
spec:
  cluster:
    domain: test.lok8s.dev
  network:
    name: lok8s
    cidr: "10.125.50.0/24"
  registries:
    tls: false
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

_load_driver() {
  _write_spec
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
  lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
}

# The fake docker. Containers: containers/<name> = "status\nhash".
# Networks: networks/<net> = "ip/prefix name" lines. `docker run` fails with
# the daemon's address-in-use error when the requested static IP is taken.
docker() {
  echo "docker ${*}" >> "${DOCKER_LOG}"
  local cmd="${1}"
  shift
  case "${cmd}" in
    inspect)
      local fmt=""
      if [[ "${1}" == "-f" ]]; then fmt="${2}"; shift 2; fi
      local target="${1}"
      [[ -f "${FAKE_DOCKER}/containers/${target}" ]] || return 1
      if [[ "${fmt}" == *"State.Status"* ]]; then
        sed -n 1p "${FAKE_DOCKER}/containers/${target}"
      elif [[ "${fmt}" == *"config-hash"* ]]; then
        sed -n 2p "${FAKE_DOCKER}/containers/${target}"
      fi
      ;;
    volume) ;;
    rm)
      local target="${!#}" nf
      rm -f "${FAKE_DOCKER}/containers/${target}"
      # docker rm -f releases the container's endpoints — mirror that.
      for nf in "${FAKE_DOCKER}/networks/"*; do
        [[ -f "${nf}" ]] && sed -i "\| ${target}\$|d" "${nf}"
      done
      ;;
    run)
      local -a argv=("${@}")
      local i name="" ip="" net="" hash=""
      for (( i = 0; i < ${#argv[@]}; i++ )); do
        case "${argv[${i}]}" in
          --name)  name="${argv[$(( i + 1 ))]}" ;;
          --ip)    ip="${argv[$(( i + 1 ))]}" ;;
          --net=*) net="${argv[${i}]#--net=}" ;;
          --label) hash="${argv[$(( i + 1 ))]#lok8s.dev/config-hash=}" ;;
        esac
      done
      if grep -q "^${ip}/" "${FAKE_DOCKER}/networks/${net}" 2>/dev/null; then
        # Real docker leaves the container behind in Created state.
        printf 'created\n%s\n' "${hash}" > "${FAKE_DOCKER}/containers/${name}"
        echo "docker: failed to set up container networking: Address already in use" >&2
        return 125
      fi
      printf 'running\n%s\n' "${hash}" > "${FAKE_DOCKER}/containers/${name}"
      echo "${ip}/24 ${name}" >> "${FAKE_DOCKER}/networks/${net}"
      echo "cid-${name}"
      ;;
    network)
      local sub="${1}"
      shift
      case "${sub}" in
        inspect)
          [[ -f "${FAKE_DOCKER}/networks/${1}" ]] || return 1
          cat "${FAKE_DOCKER}/networks/${1}"
          ;;
        disconnect)
          [[ "${1}" == "-f" ]] && shift
          sed -i "\| ${2}\$|d" "${FAKE_DOCKER}/networks/${1}"
          ;;
        connect)
          echo "10.125.200.240/24 ${2}" >> "${FAKE_DOCKER}/networks/${1}"
          ;;
      esac
      ;;
  esac
}

# ── Rendering: no envsubst dependency ────────────────────

@test "render: substitutes \${REMOTE_URL} without calling envsubst" {
  _load_driver
  # A poisoned envsubst proves the render path never shells out to it — the
  # b-managed Go envsubst rejects gettext's SHELL-FORMAT argument outright.
  envsubst() { echo "ERROR: Unknown flag: \${REMOTE_URL}" >&2; return 64; }

  local out
  out=$(lo::render_registry_config \
    "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/mirror.yaml" \
    "https://registry-1.docker.io")
  echo "${out}" | grep -q "remoteurl: https://registry-1.docker.io"
  # No unexpanded placeholder left (the template comment still names it).
  ! echo "${out}" | grep -q '\${REMOTE_URL}'
}

# ── Shared-network dynamic range ─────────────────────────

@test "registry_dynamic_range: /24 reserves the upper /25 for dynamic attach" {
  _load_driver
  run lo::registry_dynamic_range "10.125.200.0/24"
  assert_success
  assert_output "10.125.200.128/25"
}

@test "registry_dynamic_range: /16 reserves the upper /17" {
  _load_driver
  run lo::registry_dynamic_range "10.60.0.0/16"
  assert_success
  assert_output "10.60.128.0/17"
}

@test "registry_dynamic_range: /31 has no room to split" {
  _load_driver
  run lo::registry_dynamic_range "10.0.0.0/31"
  assert_failure
}

# ── IP holder lookup ─────────────────────────────────────

@test "registry_ip_holder: exact prefix match, .2 never matches .20" {
  _load_driver
  echo "10.125.200.20/24 other-node" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  run lo::registry_ip_holder "lok8s-registries" "10.125.200.2"
  assert_success
  assert_output ""

  run lo::registry_ip_holder "lok8s-registries" "10.125.200.20"
  assert_output "other-node"
}

# ── Reconcile matrix ─────────────────────────────────────

@test "registries: fresh start creates every container + durable config" {
  _load_driver
  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-build created"
  assert_output --partial "registry/lok8s-registry-cache created"
  assert_output --partial "registry/lok8s-registry-io-docker created"

  # Durable config, not a mktemp: the daemon re-binds it on every restart.
  [ -f "${LO_REGISTRY_STATE_DIR}/lok8s-registry-io-docker.yaml" ]
  grep -q "remoteurl: https://registry-1.docker.io" \
    "${LO_REGISTRY_STATE_DIR}/lok8s-registry-io-docker.yaml"
  # Mirror landed on its static shared-network IP.
  grep -q "^10.125.200.2/24 lok8s-registry-io-docker$" \
    "${FAKE_DOCKER}/networks/lok8s-registries"
}

@test "registries: second run is a pure no-op (unchanged, no docker churn)" {
  _load_driver
  lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" > /dev/null
  : > "${DOCKER_LOG}"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-build unchanged"
  assert_output --partial "registry/lok8s-registry-cache unchanged"
  assert_output --partial "registry/lok8s-registry-io-docker unchanged"
  ! grep -qE "^docker (run|rm)" "${DOCKER_LOG}"
}

@test "registries: config drift on a running container recreates it" {
  _load_driver
  lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" > /dev/null
  # Simulate drift: the stored hash no longer matches the desired state.
  sed -i '2s/.*/stale-hash/' "${FAKE_DOCKER}/containers/lok8s-registry-cache"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-cache configured"
  assert_output --partial "registry/lok8s-registry-build unchanged"
}

@test "registries: a dead container is recreated as restarted" {
  _load_driver
  lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" > /dev/null
  sed -i '1s/.*/exited/' "${FAKE_DOCKER}/containers/lok8s-registry-io-docker"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-io-docker restarted"
}

@test "registries: evicts a node squatting the static IP and re-attaches it" {
  _load_driver
  # A kind node grabbed the mirror's .2 via dynamic allocation (the reboot
  # failure mode). It has no config-hash label — it is not one of ours.
  printf 'running\n\n' > "${FAKE_DOCKER}/containers/test-node"
  echo "10.125.200.2/24 test-node" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-io-docker created"

  # The registry owns its IP; the node was re-attached (dynamically) after.
  grep -q "^10.125.200.2/24 lok8s-registry-io-docker$" \
    "${FAKE_DOCKER}/networks/lok8s-registries"
  grep -q " test-node$" "${FAKE_DOCKER}/networks/lok8s-registries"
  grep -q "^docker network disconnect -f lok8s-registries test-node$" "${DOCKER_LOG}"
  grep -q "^docker network connect lok8s-registries test-node$" "${DOCKER_LOG}"
}

@test "registries: one failure doesn't abort the rest, rc is non-zero" {
  _load_driver
  rm "${BATS_TEST_TMPDIR}/.lok8s/drivers/lo/cluster/registry/build.yaml"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "error: registry/lok8s-registry-build"
  assert_output --partial "registry/lok8s-registry-cache created"
  assert_output --partial "registry/lok8s-registry-io-docker created"
}

@test "registries: losing the start race to a concurrent lo is unchanged" {
  _load_driver
  lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" > /dev/null
  sed -i '2s/.*/stale-hash/' "${FAKE_DOCKER}/containers/lok8s-registry-io-docker"

  # Concurrent-lo emulation: our rm is a no-op (the other run recreated the
  # container immediately) and our run loses the name to it.
  eval "_real_docker() $(declare -f docker | tail -n +2)"
  docker() {
    case "${1} ${2:-}" in
      "rm -f") echo "docker ${*}" >> "${DOCKER_LOG}" ;;
      "run -d")
        echo "docker: Error response from daemon: Conflict. The container name is already in use" >&2
        return 125
        ;;
      *) _real_docker "${@}" ;;
    esac
  }

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-io-docker unchanged"
}

# ── Cleanup ──────────────────────────────────────────────

@test "cleanup: removes project containers + configs, keeps shared mirrors" {
  _load_driver
  lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" > /dev/null

  lo::cleanup_registries "test-lifecycle"

  [ ! -f "${FAKE_DOCKER}/containers/lok8s-registry-build" ]
  [ ! -f "${LO_REGISTRY_STATE_DIR}/lok8s-registry-build.yaml" ]
  [ ! -f "${LO_REGISTRY_STATE_DIR}/lok8s-registry-cache.yaml" ]
  # Shared mirrors persist across project lifecycles.
  [ -f "${FAKE_DOCKER}/containers/lok8s-registry-io-docker" ]
  [ -f "${LO_REGISTRY_STATE_DIR}/lok8s-registry-io-docker.yaml" ]
}
