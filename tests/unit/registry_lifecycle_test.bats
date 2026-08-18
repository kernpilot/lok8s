#!/usr/bin/env bats
# registry_lifecycle_test.bats — unit tests for the idempotent registry
# container lifecycle (lo::registries): desired-state reconciliation via the
# config-hash label, durable config files under LO_REGISTRY_STATE_DIR, the
# loud squatted-IP failure, portable ${REMOTE_URL} rendering (no envsubst),
# and the reserved dynamic range for the shared network — including the
# recreate of a legacy network that predates the reservation.
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
          # --format (either side of the name): IPAM metadata comes from
          # networks/<net>.meta ("subnet\nrange"). Bare/-f inspect keeps the
          # historical cat of the member lines (the awk in registry_ip_holder
          # feeds on it).
          local nfmt="" nnet="" narg
          for narg in "${@}"; do
            case "${narg}" in
              --format|-f) ;;
              '{{'*) nfmt="${narg}" ;;
              *) [[ -z "${nnet}" ]] && nnet="${narg}" ;;
            esac
          done
          [[ -f "${FAKE_DOCKER}/networks/${nnet}" ]] || return 1
          if [[ "${nfmt}" == *"Subnet"* ]]; then
            sed -n 1p "${FAKE_DOCKER}/networks/${nnet}.meta" 2>/dev/null
          elif [[ "${nfmt}" == *"IPRange"* ]]; then
            sed -n 2p "${FAKE_DOCKER}/networks/${nnet}.meta" 2>/dev/null
          elif [[ "${nfmt}" == *"IPv4Address"* ]]; then
            # ORDER MATTERS: registry_ip_holder's template contains
            # IPv4Address AND Containers/Name — this branch must win or the
            # holder lookup gets names-only lines and every match fails.
            cat "${FAKE_DOCKER}/networks/${nnet}"
          elif [[ "${nfmt}" == *"Containers"*"Name"* ]]; then
            awk '{print $2}' "${FAKE_DOCKER}/networks/${nnet}"
          else
            cat "${FAKE_DOCKER}/networks/${nnet}"
          fi
          ;;
        disconnect)
          [[ "${1}" == "-f" ]] && shift
          sed -i "\| ${2}\$|d" "${FAKE_DOCKER}/networks/${1}"
          ;;
        connect)
          echo "10.125.200.240/24 ${2}" >> "${FAKE_DOCKER}/networks/${1}"
          ;;
        create)
          local -a nargv=("${@}")
          local j nname="${nargv[-1]}" nsubnet="" nrange=""
          for (( j = 0; j < ${#nargv[@]}; j++ )); do
            case "${nargv[${j}]}" in
              --subnet)   nsubnet="${nargv[$(( j + 1 ))]}" ;;
              --ip-range) nrange="${nargv[$(( j + 1 ))]}" ;;
            esac
          done
          : > "${FAKE_DOCKER}/networks/${nname}"
          printf '%s\n%s\n' "${nsubnet}" "${nrange}" > "${FAKE_DOCKER}/networks/${nname}.meta"
          ;;
        rm)
          # Real docker: rm REFUSES a network with active endpoints; -f only
          # suppresses the not-found error (verified live 2026-08-18).
          [[ "${1}" == "-f" ]] && shift
          if [[ -s "${FAKE_DOCKER}/networks/${1}" ]]; then
            echo "Error response from daemon: error while removing network: network ${1} has active endpoints" >&2
            return 1
          fi
          rm -f "${FAKE_DOCKER}/networks/${1}" "${FAKE_DOCKER}/networks/${1}.meta"
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

@test "network_dynamic_range: /24 reserves the upper /26 for kind nodes" {
  _load_driver
  run lo::network_dynamic_range "10.125.125.0/24"
  assert_success
  # .192+ — ABOVE the registries (.101-.110) and the default MetalLB pool
  # (.125-.150): a dynamically-attached node can collide with neither.
  assert_output "10.125.125.192/26"
}

@test "network: fresh project network reserves the node range" {
  _load_driver
  rm -f "${FAKE_DOCKER}/networks/lok8s"

  run lo::network
  assert_success
  grep -q -- "--ip-range 10.125.50.0/24" "${DOCKER_LOG}" && return 1
  grep -q -- "--ip-range 10.125.50.192/26" "${DOCKER_LOG}" || {
    echo "the project network was created WITHOUT its reserved node range —" >&2
    echo "a rebooting node can squat build/cache (.101/.102) or a MetalLB" >&2
    echo "pool address again." >&2
    return 1
  }
}

@test "registry_network: fresh create reserves the dynamic range" {
  _load_driver
  rm -f "${FAKE_DOCKER}/networks/lok8s-registries"

  run lo::registry_network
  assert_success
  grep -q -- "--ip-range 10.125.200.128/25" "${DOCKER_LOG}" || {
    echo "network created WITHOUT the reserved range — dynamic attachers can" >&2
    echo "squat the mirrors' static IPs again." >&2
    return 1
  }
  [ "$(sed -n 2p "${FAKE_DOCKER}/networks/lok8s-registries.meta")" = "10.125.200.128/25" ]
}

@test "registry_network: a WRONG existing range is recreated, not accepted" {
  _load_driver
  # A range that differs from the derived one (older tooling, hand-made
  # network) can still let dynamic allocation overlap the statics — mere
  # non-emptiness must not pass for "reserved".
  printf '10.125.200.0/24\n10.125.200.64/26\n' > "${FAKE_DOCKER}/networks/lok8s-registries.meta"

  run lo::registry_network
  assert_success
  [ "$(sed -n 2p "${FAKE_DOCKER}/networks/lok8s-registries.meta")" = "10.125.200.128/25" ] || {
    echo "a mismatched --ip-range (10.125.200.64/26) was accepted as reserved" >&2
    return 1
  }
}

@test "registry_network: a range-reserved network is left untouched" {
  _load_driver
  printf '10.125.200.0/24\n10.125.200.128/25\n' > "${FAKE_DOCKER}/networks/lok8s-registries.meta"
  : > "${DOCKER_LOG}"

  run lo::registry_network
  assert_success
  ! grep -qE "^docker network (rm|create)" "${DOCKER_LOG}" || {
    echo "a correctly-configured network was churned:" >&2
    cat "${DOCKER_LOG}" >&2
    return 1
  }
}

@test "registry_network: a legacy no-range network is recreated, mirrors removed" {
  _load_driver
  # Pre-reservation network: subnet only, a mirror + a kind node attached.
  printf '10.125.200.0/24\n\n' > "${FAKE_DOCKER}/networks/lok8s-registries.meta"
  printf 'running\nsome-hash\n' > "${FAKE_DOCKER}/containers/lok8s-registry-io-docker"
  printf 'running\n\n' > "${FAKE_DOCKER}/containers/test-node"
  echo "10.125.200.2/24 lok8s-registry-io-docker" >> "${FAKE_DOCKER}/networks/lok8s-registries"
  echo "10.125.200.7/24 test-node" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  run lo::registry_network
  assert_success

  # Recreated with the range.
  [ "$(sed -n 2p "${FAKE_DOCKER}/networks/lok8s-registries.meta")" = "10.125.200.128/25" ]
  # The mirror container was REMOVED — a running mirror with a matching
  # config-hash would otherwise reconcile "unchanged" while detached from
  # the new network, silently breaking every pull.
  [ ! -f "${FAKE_DOCKER}/containers/lok8s-registry-io-docker" ] || {
    echo "the mirror survived the recreate — lo::registries will report it" >&2
    echo "unchanged and never re-attach it." >&2
    return 1
  }
  # The kind node is detached but NOT removed — it re-attaches via
  # lo::connect_nodes_to_registry_network on its cluster's next lo up.
  [ -f "${FAKE_DOCKER}/containers/test-node" ]
  ! grep -q "^docker rm -f test-node$" "${DOCKER_LOG}"
}

@test "registry_network: legacy recreate then reconcile restores the mirror at its static IP" {
  _load_driver
  # End-to-end: the recreate (above) followed by the normal lo::registries
  # run must land the mirror back on .2 on the NEW network — the property
  # the whole recreate design leans on.
  printf '10.125.200.0/24\n\n' > "${FAKE_DOCKER}/networks/lok8s-registries.meta"
  printf 'running\nsome-hash\n' > "${FAKE_DOCKER}/containers/lok8s-registry-io-docker"
  echo "10.125.200.2/24 lok8s-registry-io-docker" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  lo::registry_network
  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-io-docker created"
  grep -q "^10.125.200.2/24 lok8s-registry-io-docker$" \
    "${FAKE_DOCKER}/networks/lok8s-registries"
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

@test "registries: moving LO_REGISTRY_STATE_DIR recreates containers" {
  _load_driver
  lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml" > /dev/null
  # The config path feeds the hash: containers still bound to the abandoned
  # path must be recreated or the vanished-mount failure returns.
  export LO_REGISTRY_STATE_DIR="${BATS_TEST_TMPDIR}/registry-state-moved"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-build configured"
  [ -f "${LO_REGISTRY_STATE_DIR}/lok8s-registry-build.yaml" ]
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

@test "registries: a persistently squatted static IP fails loudly, holder named" {
  _load_driver
  # A container holds the mirror's .2. Eviction is GONE by design — on a
  # range-reserved network this cannot happen, so when it does (legacy
  # network, concurrent tooling) the fix is recreating the network, not
  # silently detaching someone else's container.
  printf 'running\n\n' > "${FAKE_DOCKER}/containers/test-node"
  echo "10.125.200.2/24 test-node" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "held by 'test-node'"
  assert_output --partial "lo registry clean --shared"

  # The squatter was NOT touched — no eviction, no shuffle.
  grep -q " test-node$" "${FAKE_DOCKER}/networks/lok8s-registries"
  ! grep -q "^docker network disconnect -f lok8s-registries test-node$" "${DOCKER_LOG}"
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

@test "registries: a transiently-held address succeeds on the bounded retry" {
  _load_driver
  # The endpoint of a just-removed container can lag its release: the first
  # `docker run` fails address-in-use with NO visible holder (the entry is
  # already gone by the re-check) — the retry must succeed, not fail loudly.
  eval "_real_docker() $(declare -f docker | tail -n +2)"
  LAG_ONCE="${BATS_TEST_TMPDIR}/lag-once"
  docker() {
    if [[ "${1} ${2:-}" == "run -d" && "${*}" == *lok8s-registry-io-docker* && ! -f "${LAG_ONCE}" ]]; then
      touch "${LAG_ONCE}"
      echo "docker ${*}" >> "${DOCKER_LOG}"
      echo "docker: failed to set up container networking: Address already in use" >&2
      return 125
    fi
    _real_docker "${@}"
  }
  sleep() { :; }

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_success
  assert_output --partial "registry/lok8s-registry-io-docker created"
  grep -q "^10.125.200.2/24 lok8s-registry-io-docker$" \
    "${FAKE_DOCKER}/networks/lok8s-registries"
}

@test "registry_network: legacy recreate survives a lagging endpoint release" {
  _load_driver
  # First `docker network rm` fails (a just-removed mirror's endpoint lags
  # its release — the daemon behavior the codebase documents), the retry
  # succeeds. Without the bounded retry the recreate dies here transiently.
  printf '10.125.200.0/24\n\n' > "${FAKE_DOCKER}/networks/lok8s-registries.meta"
  printf 'running\nsome-hash\n' > "${FAKE_DOCKER}/containers/lok8s-registry-io-docker"
  echo "10.125.200.2/24 lok8s-registry-io-docker" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  eval "_real_docker() $(declare -f docker | tail -n +2)"
  LAGGED="${BATS_TEST_TMPDIR}/net-rm-lagged"
  docker() {
    if [[ "${1} ${2:-} ${3:-}" == "network rm lok8s-registries" && ! -f "${LAGGED}" ]]; then
      touch "${LAGGED}"
      echo "docker network ${*:2}" >> "${DOCKER_LOG}"
      echo "Error response from daemon: error while removing network: network lok8s-registries has active endpoints" >&2
      return 1
    fi
    _real_docker "${@}"
  }
  sleep() { :; }

  run lo::registry_network
  assert_success
  [ "$(sed -n 2p "${FAKE_DOCKER}/networks/lok8s-registries.meta")" = "10.125.200.128/25" ]
}

@test "registries: a project-network squat gets the node-reboot remediation" {
  _load_driver
  # Same persistent-holder failure, but on the PROJECT network (build/cache
  # live there): the shared-net "clean --shared" hint cannot fix it — the
  # error must point at the squatting node instead.
  printf 'running\n\n' > "${FAKE_DOCKER}/containers/test-node"
  echo "10.125.50.101/24 test-node" >> "${FAKE_DOCKER}/networks/lok8s"

  run lo::registries "test.lok8s.dev" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  assert_failure
  assert_output --partial "held by 'test-node'"
  assert_output --partial "docker network disconnect -f lok8s test-node"
  # The shared-net remediation must NOT be offered for a project-net squat.
  ! grep -q "clean --shared && lo up" <<< "${output}" || {
    echo "the error recommends 'lo registry clean --shared' for a PROJECT-" >&2
    echo "network squat — that command touches a different network and" >&2
    echo "cannot fix this." >&2
    return 1
  }
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

@test "registry clean --shared: detaches holders so the network actually goes away" {
  _load_driver
  # libs/registry sources its deps from ${PATH_LOK8S} — point it at the real
  # tree (each @test runs in its own process; nothing leaks).
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/libs/registry"
  # The real init needs the (stubbed-out) domain helpers — re-derive the
  # registry JSON from the spec instead.
  _registry_init() {
    lo::read_network_config "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"
  }
  lo::cleanup_registries() { :; }
  warn() { echo "warn: ${*}"; }

  # A foreign holder is attached — exactly the state the squatted-registry
  # error sends the operator here to fix. The old empty-check kept the
  # network, sending the next lo up straight back into the same error.
  printf '10.125.200.0/24\n\n' > "${FAKE_DOCKER}/networks/lok8s-registries.meta"
  echo "10.125.200.2/24 foreign-node" >> "${FAKE_DOCKER}/networks/lok8s-registries"

  local shared=1 domain="test.lok8s.dev"
  run registry::clean
  assert_success
  assert_output --partial "detaching 'foreign-node'"
  [ ! -f "${FAKE_DOCKER}/networks/lok8s-registries" ] || {
    echo "the network survived clean --shared — the recommended remediation" >&2
    echo "loops back into the same squatted-IP failure forever." >&2
    return 1
  }
}
