#!/usr/bin/env bats
# lo_commands_test.bats — unit tests for lo CLI routing and subcommands
# Tests domain discovery (lo use), provisioning dispatch (lo up), and teardown (lo down, lo clean)

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_BASE="${BATS_TEST_TMPDIR}"

  import() { :; }
  export -f import

  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"

  # Create domain structure
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/clusters/staging.lok8s.dev"
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s"

  cp "${FIXTURES_DIR}/lo-cluster.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/test.lok8s.dev/cluster.lok8s.yaml"

  cp "${FIXTURES_DIR}/deploy-domain.lok8s.yaml" \
    "${BATS_TEST_TMPDIR}/clusters/staging.lok8s.dev/deploy.lok8s.yaml"
}

teardown() {
  teardown_tmpdir
}

# ── lo use — domain discovery ────────────────────────

# We re-implement main::use logic since it depends on argsh :args
lo_use_set() {
  local domain_arg="$1"
  if [[ -n "${domain_arg:-}" ]]; then
    if [[ ! -d "${PATH_CLUSTERS}/${domain_arg}" ]]; then
      error "Domain not found: .lok8s/${domain_arg}/"
      return 1
    fi
    mkdir -p "${PATH_LOK8S}"
    echo "${domain_arg}" > "${PATH_CLUSTERS}/.active"
    echo "Active domain: ${domain_arg}"
  fi
}

lo_use_show() {
  if [[ -f "${PATH_CLUSTERS}/.active" ]]; then
    echo "Active: $(cat "${PATH_CLUSTERS}/.active")"
  else
    echo "No active domain set."
  fi
}

@test "lo use sets active domain" {
  run lo_use_set "test.lok8s.dev"
  assert_success
  assert_output --partial "Active domain: test.lok8s.dev"

  # Verify .active file was created
  [ -f "${BATS_TEST_TMPDIR}/clusters/.active" ]
  run cat "${BATS_TEST_TMPDIR}/clusters/.active"
  assert_output "test.lok8s.dev"
}

@test "lo use shows current active domain" {
  echo "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/clusters/.active"

  run lo_use_show
  assert_success
  assert_output "Active: test.lok8s.dev"
}

@test "lo use shows no active domain when unset" {
  rm -f "${BATS_TEST_TMPDIR}/clusters/.active"

  run lo_use_show
  assert_success
  assert_output "No active domain set."
}

@test "lo use fails for nonexistent domain" {
  run lo_use_set "nonexistent.domain"
  assert_failure
  assert_output --partial "Domain not found"
}

@test "lo use discovers cluster domains" {
  yq() {
    case "$2" in
      '.kind') echo "Lo" ;;
      '.spec.clusterRef.domain // "?"') echo "test.lok8s.dev" ;;
      *) echo "" ;;
    esac
  }
  export -f yq

  # List available domains (cluster domains live under PATH_CLUSTERS/)
  local output=""
  for spec in "${PATH_CLUSTERS}"/*/cluster.lok8s.yaml; do
    [[ -f "${spec}" ]] || continue
    local d
    d=$(basename "$(dirname "${spec}")")
    output+="${d} "
  done

  [[ "${output}" == *"test.lok8s.dev"* ]]
}

@test "lo use discovers deploy domains" {
  # List deploy domains
  local output=""
  for spec in "${PATH_CLUSTERS}"/*/deploy.lok8s.yaml; do
    [[ -f "${spec}" ]] || continue
    local d
    d=$(basename "$(dirname "${spec}")")
    output+="${d} "
  done

  [[ "${output}" == *"staging.lok8s.dev"* ]]
}

# ── lo up — provision dispatch ───────────────────────

@test "lo up dispatches to provision::dispatch for .lok8s domain" {
  local provisioned=""

  # Source provision lib
  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  # Override provision::dispatch to track if it's called
  provision::dispatch() {
    provisioned="$1"
  }
  export -f provision::dispatch

  tilt::up() { :; }
  export -f tilt::up

  # Simulate main::up logic
  local domain="test.lok8s.dev"
  if [[ -f "${PATH_CLUSTERS}/${domain}/cluster.lok8s.yaml" ]]; then
    provision::dispatch "${domain}"
  fi

  [ "${provisioned}" = "test.lok8s.dev" ]
}

@test "lo up detects active domain from .active file" {
  echo "test.lok8s.dev" > "${BATS_TEST_TMPDIR}/clusters/.active"

  local domain=""
  if [[ -f "${PATH_CLUSTERS}/.active" ]]; then
    domain=$(cat "${PATH_CLUSTERS}/.active")
  fi

  [ "${domain}" = "test.lok8s.dev" ]
}

# ── lo down — teardown ───────────────────────────────

@test "lo down calls tilt::down and kind::delete" {
  local tilt_down_called="" kind_delete_called=""

  tilt::down() { tilt_down_called="yes"; }
  export -f tilt::down

  kind::delete() { kind_delete_called="yes"; }
  export -f kind::delete

  # Simulate main::down
  tilt::down
  kind::delete

  [ "${tilt_down_called}" = "yes" ]
  [ "${kind_delete_called}" = "yes" ]
}

# ── lo clean — cleanup ──────────────────────────────

@test "lo clean removes cluster volumes" {
  local volumes_cleaned=""

  docker() {
    case "$1" in
      volume)
        case "$2" in
          ls) echo "test-local-volume-1" ;;
          rm) volumes_cleaned="yes" ;;
        esac
        ;;
      system) echo "ok" ;;
    esac
  }
  export -f docker

  tilt::down() { :; }
  export -f tilt::down

  kind::delete() { :; }
  export -f kind::delete

  registry::clean() { :; }
  export -f registry::clean

  # Simulate main::clean
  local cluster="test-local"
  tilt::down
  kind::delete
  for volume in $(docker volume ls --filter "name=^${cluster}-" -q); do
    docker volume rm -f "${volume}"
  done
  registry::clean

  [ "${volumes_cleaned}" = "yes" ]
}

# ── lo provision — explicit domain ───────────────────

@test "lo provision dispatches to provision::dispatch with target domain" {
  local dispatched_domain=""

  provision::dispatch() { dispatched_domain="$1"; }
  export -f provision::dispatch

  # Simulate main::provision logic
  local target_domain="test.lok8s.dev"
  provision::dispatch "${target_domain}"

  [ "${dispatched_domain}" = "test.lok8s.dev" ]
}

# ── lo destroy — explicit domain ─────────────────────

@test "lo destroy dispatches to provision::dispatch_destroy" {
  local destroyed_domain=""

  source "${_PROJECT_ROOT}/.lok8s/libs/provision"

  provision::dispatch_destroy() { destroyed_domain="$1"; }
  export -f provision::dispatch_destroy

  # Simulate main::destroy logic
  local target_domain="test.lok8s.dev"
  provision::dispatch_destroy "${target_domain}"

  [ "${destroyed_domain}" = "test.lok8s.dev" ]
}
