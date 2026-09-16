#!/usr/bin/env bash
# tests/e2e/lib/helpers.bash — shared e2e test helpers
#
# Usage from a scenario's test.bats:
#
#   setup_file() {
#     load "${BATS_TEST_DIRNAME}/../lib/helpers"
#     e2e::require_e2e_enabled
#     e2e::require_tools docker kind kustomize yq tilt dig
#     e2e::require_dns 126.lok8s.dev
#     e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
#     e2e::snapshot_world
#     e2e::provision
#   }
#
#   teardown_file() {
#     load "${BATS_TEST_DIRNAME}/../lib/helpers"
#     e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
#     e2e::destroy
#     e2e::assert_torn_down destroy
#     e2e::sweep
#   }
#
#   setup() {
#     load "${BATS_TEST_DIRNAME}/../lib/helpers"
#     e2e::init "${BATS_TEST_DIRNAME}" 126.lok8s.dev
#   }
#
# Key design points:
#
#  - Each scenario lives in tests/e2e/<name>/. That directory IS the
#    PATH_BASE: `lo` reads services.yaml, clusters/, .kubeconfig/ etc.
#    from there. It is also the PROJECT: the scenario's lok8s.yaml
#    selects the implementation for the leg under test.
#
#  - ONE binary runs every scenario on every leg: E2E_LO_BIN (bin/lo by
#    default). The implementation is NOT chosen by calling a different
#    program. Since v0.3.x it comes from committed configuration
#    (spec.implementation in lok8s.yaml), read by that same binary, and
#    that is how a user chooses it. E2E_LO_IMPL writes the block:
#
#      go      every command native
#      bash    the whole tree routed to the frozen bash implementation
#      mixed   E2E_LO_ROUTED routed, the rest native
#
#    Routing needs the tree INSIDE the project (internal/cli/routing.go
#    refuses the binary's cache extract and an entrypoint that resolves
#    outside), so the bash and mixed legs get their own copy of it: the
#    same explicit, in-project tree `lo assets eject bash` gives a user.
#
#  - PATH_CLUSTERS is set to the scenario's local clusters/ dir so
#    cluster definitions live next to the test that uses them.
#
#  - Each scenario spins up and tears down a real kind cluster through
#    `lo provision` / `lo down` / `lo destroy`, and then asserts that the
#    machine is clean (e2e::assert_torn_down). Scenarios that don't need
#    a cluster (validator-only) should NOT call e2e::provision.
#
#  - Tests skip automatically on machines without docker/kind/etc.
#
# SAFETY. A developer machine carries live kind clusters, live registry
# containers and a live Tilt. Nothing here may touch them:
#
#  - every cluster, network and volume name a scenario creates starts
#    with `e2e-`, and e2e::guard_name refuses anything else before a
#    destructive verb runs;
#  - `kind get clusters` is snapshotted before the first cluster comes
#    up and asserted unchanged after every teardown;
#  - every docker filter is anchored (`^name$` or `^prefix-`), so a
#    substring can never widen the blast radius;
#  - nothing here runs `docker system prune`, `lo clean --all`, or any
#    verb against an object the scenario did not create.

# Resolve the project root from this file's location.
# tests/e2e/lib/helpers.bash → project root is ../../..
_E2E_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_E2E_DIR="$(cd "${_E2E_LIB_DIR}/.." && pwd)"
_PROJECT_ROOT="$(cd "${_E2E_DIR}/../.." && pwd)"

# Load bats assertion libraries via the shared loader (worktree-aware
# resolution lives there; the unit helper uses the same one so the suites
# can't drift). Deliberately NOT sourcing tests/test_helper.bash here — its
# load-time side effects (PATH_BASE export, unset ARGSH_SOURCE, verbose.sh,
# fixtures) belong to the unit plane; e2e sets its environment in e2e::init.
source "${_PROJECT_ROOT}/tests/lib/bats_libs.bash"
_load_bats_libs

# Default timeouts. Scenarios can override by setting these before
# calling helpers.
#
# These have to ADD UP under the CI job's own cap, or a slow leg dies at
# the cap with no message instead of at the operation that hung, and the
# failure diagnostics never run. The worst leg is `lifecycle`: four
# provisions, three downs and two destroys. 4x300 + 5x120 = 1800s = 30
# minutes of budget, and .github/workflows/e2e.yml caps the job at 45 to
# leave room for the ~6 minutes of toolchain setup. A healthy leg is
# ~3 minutes; these are the ceilings, not the expectation.
: "${E2E_PROVISION_TIMEOUT:=300}"   # 5 min to provision a kind cluster
: "${E2E_DESTROY_TIMEOUT:=120}"     # 2 min to tear down
: "${E2E_TILT_CI_TIMEOUT:=600}"     # 10 min for tilt ci to reach steady state

# The binary under test. ONE binary runs every leg: bin/lo by default,
# bin/lo-full or a released `lo` when the caller says so. It selects
# WHICH BINARY, never which implementation — that is lok8s.yaml's job
# (E2E_LO_IMPL below). Absolute: every call runs from inside the
# scenario project, where a relative path would not resolve.
: "${E2E_LO_BIN:=${_PROJECT_ROOT}/bin/lo}"

# The implementation leg: go | bash | mixed. `mixed` routes the commands
# in E2E_LO_ROUTED (space-separated top-level names) to the bash tree and
# runs the rest natively — the shape a project adopts while it migrates.
: "${E2E_LO_IMPL:=go}"
: "${E2E_LO_ROUTED:=}"

# ── Logging ──────────────────────────────────────────────

# e2e::log <line...> — one line into the test log. bats keeps fd 3 open
# for output that must show whatever the test does; outside bats the
# line goes to stderr.
e2e::log() {
  if { true >&3; } 2>/dev/null; then
    echo "# $*" >&3
  else
    echo "# $*" >&2
  fi
}

# ── Skip conditions ──────────────────────────────────────

# e2e::_unmet <reason> — a precondition this machine does not meet.
# It SKIPS by default, so the suite stays usable on a laptop without
# kind or without the DNS slots. Under E2E_STRICT=1 it FAILS instead:
# on a CI runner every precondition is installed by the job, so a skip
# there is a green check that proved nothing.
e2e::_unmet() {
  [[ "${E2E_STRICT:-}" != "1" ]] || fail "$1 (E2E_STRICT=1: this is a CI leg, the job installs its own prerequisites)"
  skip "$1"
}

# e2e::require_tools — call from setup() to skip when prereqs are missing.
# Usage: e2e::require_tools docker kind kustomize yq tilt
#
# A routing leg needs the bash tree's own toolchain on top of whatever
# the scenario asks for: the frozen libs read specs with yq and registry
# state with jq. They are added here rather than in each scenario,
# because the requirement follows the LEG, not the subject.
e2e::require_tools() {
  local -a tools=("$@")
  [[ "${E2E_LO_IMPL}" == "go" ]] || tools+=(yq jq)
  for tool in "${tools[@]}"; do
    command -v "${tool}" >/dev/null 2>&1 || {
      e2e::_unmet "e2e: '${tool}' not in PATH"
    }
  done
  unset tool
  if command -v docker >/dev/null 2>&1; then
    docker info >/dev/null 2>&1 || e2e::_unmet "e2e: docker daemon not running"
  fi
}

# e2e::require_e2e_enabled — opt-in gate so `bats tests/unit/...` doesn't
# pull in cluster lifecycle by accident. Set E2E=1 to run.
e2e::require_e2e_enabled() {
  [[ "${E2E:-}" == "1" ]] || skip "e2e: set E2E=1 to run cluster-backed tests"
}

# e2e::require_binary — the binary under test must be there. This FAILS,
# it does not skip: a missing bin/lo is an unbuilt checkout, not a
# machine without docker, and a skip would read as a green suite that
# proved nothing.
e2e::require_binary() {
  [[ -x "${E2E_LO_BIN}" ]] || {
    fail "e2e: ${E2E_LO_BIN} is not executable. Run: make build (or set E2E_LO_BIN)"
  }
}

# e2e::require_go_impl <what> — skip one test whose subject has no bash
# twin. Today that is the confirmation prompts and nothing else:
# `lo registry tls` DOES exist in the frozen tree (registry::tls, the
# same volume model), so the registry-tls scenario runs on every leg.
# The caller names the subject, so the skip line says what this leg does
# not cover.
e2e::require_go_impl() {
  [[ "${E2E_LO_IMPL}" == "go" ]] || skip "e2e: ${1} runs on the go leg only (this leg: ${E2E_LO_IMPL})"
}

# e2e::require_dns <domain>
# Skip the scenario if the wildcard DNS for its slot doesn't resolve.
# Each slot N relies on `*.N.lok8s.dev` pointing at `10.125.N.x`.
# Without that, mkcert certs and any in-cluster service references
# fail in confusing ways late in the run.
e2e::require_dns() {
  local domain="$1"
  command -v dig >/dev/null 2>&1 || {
    e2e::_unmet "e2e: dig not in PATH (install bind-utils/dnsutils to enable the DNS preflight)"
  }
  local probe="probe.${domain}"
  local resolved
  resolved=$(dig +short "${probe}" 2>/dev/null | head -1)
  [[ -n "${resolved}" ]] || {
    e2e::_unmet "e2e: ${probe} does not resolve — DNS slot ${domain} unprovisioned (see tests/e2e/SUBNETS.md)"
  }
}

# ── Safety ───────────────────────────────────────────────

# e2e::guard_name <kind> <name> — refuse a name a scenario may not own.
# Every cluster, network and volume an e2e run creates is `e2e-…`;
# anything else belongs to the machine, not to the test. Called before
# each destructive verb and before each docker filter is built.
e2e::guard_name() {
  local kind="$1" name="$2"
  [[ -n "${name}" ]] || fail "e2e: refusing an empty ${kind} name"
  case "${name}" in
    e2e-*) ;;
    *) fail "e2e: refusing to act on ${kind} '${name}' — e2e owns 'e2e-*' names only" ;;
  esac
}

# ── Scenario lifecycle ───────────────────────────────────

# e2e::init <scenario-dir> [domain]
# Point PATH_BASE at the scenario dir, leave PATH_BIN at the project,
# set PATH_CLUSTERS to the scenario's local clusters/, and set
# DOMAIN_NAME + LOK8S_CLUSTER_NAME to unique-per-scenario values so
# parallel runs in the future don't collide. Writes the scenario's
# lok8s.yaml for the leg under test (E2E_LO_IMPL) and materialises the
# bash tree when the leg routes to it.
e2e::init() {
  local scenario_dir="$1"
  local domain="${2:-lok8s.dev}"
  [[ -d "${scenario_dir}" ]] || {
    echo "e2e::init: scenario dir not found: ${scenario_dir}" >&2
    return 1
  }

  export PATH_BASE="${scenario_dir}"
  export PATH_BIN="${_PROJECT_ROOT}/.bin"
  export PATH_CLUSTERS="${scenario_dir}/clusters"
  export PATH_SECRETS="${scenario_dir}/.secrets"
  export DOMAIN_NAME="${domain}"

  # Cluster name derives from the scenario directory so docker bridge
  # interface names stay under the 15-char Linux limit. e.g.
  # "no-services" -> "e2e-noop", and longer ones like
  # "single-local-build" get a short abbreviation. The project docker
  # network carries the same name (spec.network.name in every scenario
  # spec), so one value is the scenario's whole ownership prefix.
  local scenario_name
  scenario_name="$(basename "${scenario_dir}")"
  case "${scenario_name}" in
    single-local-build) export LOK8S_CLUSTER_NAME="e2e-slb" ;;
    cache-mode)         export LOK8S_CLUSTER_NAME="e2e-cache" ;;
    no-services)        export LOK8S_CLUSTER_NAME="e2e-noop" ;;
    registry-tls)       export LOK8S_CLUSTER_NAME="e2e-rtls" ;;
    lifecycle)          export LOK8S_CLUSTER_NAME="e2e-life" ;;
    shared-registries)
      # Two domains in one project: the slot picks the cluster.
      case "${domain}" in
        132.*) export LOK8S_CLUSTER_NAME="e2e-shr-a" ;;
        *)     export LOK8S_CLUSTER_NAME="e2e-shr-b" ;;
      esac
      ;;
    *)                  export LOK8S_CLUSTER_NAME="e2e-${scenario_name:0:10}" ;;
  esac
  e2e::guard_name cluster "${LOK8S_CLUSTER_NAME}"

  # The project docker network is the cluster name. Registry containers
  # and data volumes are `<network>-registry-<name>`, the certificate
  # volume `<network>-registry-tls`.
  export E2E_NETWORK="${LOK8S_CLUSTER_NAME}"

  # Per-scenario scratch that outlives one bats process: the world
  # snapshot, the pty typescripts, and the `lo` shim on PATH.
  export E2E_STATE="${scenario_dir}/.e2e-state"
  mkdir -p "${E2E_STATE}/bin"

  # The rendered registry configs are durable state (a container
  # bind-mounts its file and the docker daemon re-binds it on restart),
  # and default to the user's XDG state dir. Keep a test run out of it:
  # the scenario's own scratch holds them, so nothing of a run outlives
  # the scenario directory.
  export LO_REGISTRY_STATE_DIR="${E2E_STATE}/registries"

  # Kustomize plugin discovery (khelm + secrets plugin under .kustomize/)
  export KUSTOMIZE_PLUGIN_HOME="${_PROJECT_ROOT}/.kustomize"

  e2e::_select_implementation "${scenario_dir}"

  # Expose the binary under test and the project's .bin to subprocesses
  # spawned by Tilt (docker_build, local(), etc.). Without this the
  # Tiltfile's `local('lo env …')` calls fail with "lo: not found",
  # because Tilt's subshell does not inherit the bats harness PATH. The
  # shim resolves to E2E_LO_BIN, so a child runs the same binary the test
  # runs — what the e2e-lo-up CI job asserts about its own PATH too.
  ln -sfn "${E2E_LO_BIN}" "${E2E_STATE}/bin/lo"
  # Prepended ONCE. e2e::init runs in every setup(), and an unguarded
  # prepend grows PATH by two entries per test until it is unreadable in
  # a diagnostic dump.
  case ":${PATH}:" in
    *":${E2E_STATE}/bin:"*) ;;
    *) export PATH="${E2E_STATE}/bin:${PATH_BIN}:${PATH}" ;;
  esac

  # Tilt port: derive from slot to avoid collisions with a dev tilt
  # running at the default 10350. Slot 126 -> 10426, etc.
  local slot
  slot="$(echo "${domain}" | grep -oE '^[0-9]+' || echo "")"
  if [[ -n "${slot}" ]]; then
    export TILT_PORT="$(( 10300 + slot ))"
  fi

  # Default kubeconfig path under the scenario dir.
  export KUBECONFIG="${scenario_dir}/.kubeconfig/${LOK8S_CLUSTER_NAME}.yaml"
  mkdir -p "${scenario_dir}/.kubeconfig"

  # Suppress kind's host-port binding for e2e clusters — they live on
  # isolated bridges and shouldn't fight with the dev cluster (or with
  # each other) over 80/443/8080.
  export LOK8S_HOST_PORTS=false
}

# e2e::_select_implementation <scenario-dir> — write the project file
# that picks the leg, and give a routing leg the tree it needs.
#
# The mechanism is hack/lib/parity.sh's parity::implementation: the
# committed lok8s.yaml, and nothing else. No environment variable
# selects code, on either side.
e2e::_select_implementation() {
  local dir="$1"
  local -a routed=()
  case "${E2E_LO_IMPL}" in
    go|bash) ;;
    mixed)
      # shellcheck disable=SC2206  # E2E_LO_ROUTED is a word list
      routed=(${E2E_LO_ROUTED})
      (( ${#routed[@]} )) || fail "e2e: E2E_LO_IMPL=mixed needs E2E_LO_ROUTED"
      ;;
    *) fail "e2e: E2E_LO_IMPL must be go, bash or mixed (got '${E2E_LO_IMPL}')" ;;
  esac

  local default="go"
  [[ "${E2E_LO_IMPL}" != "bash" ]] || default="bash"
  {
    printf 'apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: e2e\nspec:\n  implementation:\n    default: %s\n' "${default}"
    if (( ${#routed[@]} )); then
      printf '    bash:\n      commands: [%s]\n' "$(IFS=,; echo "${routed[*]}")"
    fi
  } > "${dir}/lok8s.yaml"

  if [[ "${E2E_LO_IMPL}" == "go" ]]; then
    # Nothing is routed, so the data units (addons, the driver cluster
    # templates, the inventory CRD mirror) come from the repo tree and
    # a framework change shows up in the next run without a copy.
    export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  else
    e2e::_bash_tree "${dir}"
    export PATH_LOK8S="${dir}/.lok8s"
  fi
  export PATH_SCRIPTS="${PATH_LOK8S}"
}

# e2e::_bash_tree <dir> — put the frozen bash tree inside the project.
#
# A routed command always runs `<project>/<tree>/lo`: routing.go refuses
# the binary's cache extract, and refuses an entrypoint that resolves
# outside the project, so a symlink to the repo tree will not do. The
# copy is what `lo assets eject bash` gives a user, taken from the
# checkout so a framework change reaches the next run. The stamp keeps
# the copy to once per state of the source tree.
e2e::_bash_tree() {
  local dir="$1"
  local stamp="${dir}/.lok8s/.e2e-stamp"
  local want
  # Content, not mtimes: `find -printf` is GNU-only, and on a BSD find it
  # prints nothing, which froze the stamp and left the leg re-testing a
  # stale copy of the tree forever. `-exec cksum {} +` and `cksum` are
  # POSIX, and a checksum over every path and its bytes also skips the
  # copy when a touched file did not actually change.
  want="$(cd "${_PROJECT_ROOT}" && find .lok8s -type f -exec cksum {} + 2>/dev/null | LC_ALL=C sort | cksum)"
  # An empty tree would hash to a constant and match forever; a checkout
  # without .lok8s is a broken run, not a cached one.
  [[ -n "${want}" ]] || fail "e2e: no bash tree at ${_PROJECT_ROOT}/.lok8s to copy into the project"
  if [[ -f "${stamp}" && "$(cat "${stamp}")" == "${want}" ]]; then
    return 0
  fi
  rm -rf "${dir}/.lok8s"
  cp -R "${_PROJECT_ROOT}/.lok8s" "${dir}/.lok8s"
  printf '%s' "${want}" > "${stamp}"
  # The routed exec sources `${PATH_BIN}/argsh`, and derives PATH_BIN
  # from the project root when the caller sets none: give the project its
  # own entry so the toolchain resolves either way.
  ln -sfn "${_PROJECT_ROOT}/.bin" "${dir}/.bin"
}

# e2e::banner — name what this run tests. Called from setup_file, so a
# failure in the log says which binary and which leg produced it.
e2e::banner() {
  e2e::log "e2e: binary   ${E2E_LO_BIN}"
  e2e::log "e2e: version  $("${E2E_LO_BIN}" version 2>/dev/null | head -1)"
  e2e::log "e2e: leg      ${E2E_LO_IMPL}${E2E_LO_ROUTED:+ (routed: ${E2E_LO_ROUTED})}"
  e2e::log "e2e: project  ${PATH_BASE}"
  e2e::log "e2e: domain   ${DOMAIN_NAME} · cluster ${LOK8S_CLUSTER_NAME} · network ${E2E_NETWORK}"
}

# e2e::assert_provenance — prove the leg ran the implementation it says
# it runs. Called once per scenario, from setup_file.
#
# Without it a routing regression is invisible: if `routing.routed()`
# ever returned false where it should return true, the bash and mixed
# legs would silently re-run the Go code and stay green, and the suite
# would claim to cover two implementations while covering one.
#
# The signal is the one `hack/lib/parity.sh` uses for the same self-check
# (parity::init): `lo version` prints a `bash <version>` row only when
# the frozen entrypoint produced the output. The Go implementation has no
# such row, because it is not running under bash.
#
#   go     no bash row — nothing was routed behind our back
#   bash   a bash row — the whole tree really is routed
#   mixed  a bash row from the routed `version`, AND `lo doctor` (which
#          is NOT routed) printing its `implementation:` line, which only
#          the Go doctor writes. One command from each half.
e2e::assert_provenance() {
  local version doctor
  version="$("${E2E_LO_BIN}" version </dev/null 2>&1)"
  case "${E2E_LO_IMPL}" in
    go)
      ! grep -q '^bash ' <<<"${version}" \
        || fail "e2e: the go leg ran the bash tree (lo version printed a bash row):
${version}"
      ;;
    bash|mixed)
      grep -q '^bash ' <<<"${version}" \
        || fail "e2e: the ${E2E_LO_IMPL} leg did NOT run the bash tree — lo version printed no bash row, so this leg is re-testing Go:
${version}"
      ;;
  esac
  if [[ "${E2E_LO_IMPL}" == "mixed" ]]; then
    doctor="$("${E2E_LO_BIN}" doctor </dev/null 2>&1 || true)"
    grep -q 'implementation: go; bash for ' <<<"${doctor}" \
      || fail "e2e: the mixed leg has no native half — lo doctor printed no Go implementation line, so the whole tree is routed:
${doctor}"
  fi
  e2e::log "e2e: provenance ok — the ${E2E_LO_IMPL} leg runs what it says"
}

# e2e::lo <args...> — invoke the binary under test with the scenario's
# environment. The implementation comes from the project file the leg
# wrote, so this one call covers every leg.
#
# stdin is closed. A destructive command asks on a terminal since v0.5.0,
# and a developer running bats from a terminal would otherwise hand a
# test the question and wait forever. The prompt has its own tests, which
# give it a real pty (e2e::pty_run) instead of this.
e2e::lo() {
  "${E2E_LO_BIN}" "$@" </dev/null
}

# e2e::provision — run lo provision for the active scenario domain.
# Times out after E2E_PROVISION_TIMEOUT.
# When E2E_REMOTE=1, passes --remote to activate provider + remote flow.
e2e::provision() {
  e2e::require_binary
  e2e::guard_name cluster "${LOK8S_CLUSTER_NAME}"
  local -a _args=(provision --domain "${DOMAIN_NAME}")
  [[ "${E2E_REMOTE:-}" == "1" ]] && _args+=(--remote)
  # The marker tells teardown_file that there is something to tear down.
  # Without it a scenario that skipped every test would "fail" its
  # teardown assertions over objects that were never created. One marker
  # per cluster: a scenario with two domains tears down only what it
  # actually brought up.
  : > "${E2E_STATE}/provisioned.${LOK8S_CLUSTER_NAME}"
  timeout "${E2E_PROVISION_TIMEOUT}" \
    "${E2E_LO_BIN}" "${_args[@]}" \
    || {
      echo "e2e::provision failed for ${DOMAIN_NAME}" >&2
      return 1
    }
}

# e2e::down — `lo down`: stop Tilt, delete the cluster, remove the
# registry containers. The data volumes and the project network stay by
# design; e2e::assert_torn_down down checks exactly that.
e2e::down() {
  e2e::require_binary
  e2e::guard_name cluster "${LOK8S_CLUSTER_NAME}"
  timeout "${E2E_DESTROY_TIMEOUT}" \
    "${E2E_LO_BIN}" down --domain "${DOMAIN_NAME}" </dev/null
}

# e2e::destroy — tear the cluster down for good: the cluster, the
# registry containers AND their volumes, the certificate volume and the
# proxy container. The error is NOT swallowed: a failing destroy is the
# bug this suite exists to catch. The belt-and-braces removal lives in
# e2e::assert_torn_down, which fails the test when it has work to do.
e2e::destroy() {
  e2e::require_binary
  e2e::guard_name cluster "${LOK8S_CLUSTER_NAME}"
  local -a _args=(destroy --domain "${DOMAIN_NAME}" --force)
  [[ "${E2E_REMOTE:-}" == "1" ]] && _args+=(--remote)
  timeout "${E2E_DESTROY_TIMEOUT}" \
    "${E2E_LO_BIN}" "${_args[@]}" </dev/null
}

# e2e::destroy_best_effort — `lo destroy`, error logged and swallowed.
# For the remote scenarios ONLY: their teardown removes billed Hetzner
# resources after this call, and a non-zero exit here would stop the
# teardown before it got there. A local scenario uses e2e::destroy and
# lets the failure be the result.
e2e::destroy_best_effort() {
  e2e::destroy || e2e::log "e2e: lo destroy returned non-zero for ${DOMAIN_NAME} (teardown continues)"
}

# e2e::tilt_ci — run tilt ci against the scenario's Tiltfile with a
# scenario-specific port and timeout.
#
# The port comes from TILT_PORT, which e2e::init exports from the slot.
# It used to be repeated as a command prefix on the same line that
# expanded it — an assignment the expansion cannot see (SC2097/SC2098).
# It read as the source of the value and never was; the export is.
e2e::tilt_ci() {
  cd "${PATH_BASE}" || return 1
  timeout "${E2E_TILT_CI_TIMEOUT}" \
    tilt ci --port "${TILT_PORT:-10350}" \
    --file "${PATH_BASE}/Tiltfile"
}

# ── The world: snapshot and teardown assertions ──────────

# e2e::snapshot_world — record the kind clusters that were here before
# the scenario ran. Called once from setup_file, BEFORE the first
# provision. Every teardown assertion compares against this file.
e2e::snapshot_world() {
  mkdir -p "${E2E_STATE}"
  kind get clusters 2>/dev/null | sort > "${E2E_STATE}/clusters.before"
  : > "${E2E_STATE}/pty-transcript.log"
  if grep -qx "${LOK8S_CLUSTER_NAME}" "${E2E_STATE}/clusters.before"; then
    fail "e2e: kind cluster ${LOK8S_CLUSTER_NAME} is already there — an earlier run left it behind"
  fi
  e2e::log "e2e: kind get clusters (before): $(tr '\n' ' ' < "${E2E_STATE}/clusters.before")"
}

# e2e::_world_problems — what is wrong with the kind clusters right now,
# one line per problem, empty when nothing is. Printed, not failed, so
# e2e::assert_torn_down can report it together with whatever else the
# teardown left behind instead of stopping at the first of the two.
#
# Not a plain equality against the snapshot: a scenario with two domains
# still has its second cluster up while the first one is torn down, and
# that is the shape it is testing. An addition is allowed only when it
# carries the `e2e-` prefix the harness owns; anything else is a foreign
# cluster this run has no business creating.
e2e::_world_problems() {
  local snapshot="${E2E_STATE}/clusters.before"
  [[ -f "${snapshot}" ]] || {
    echo "no world snapshot — call e2e::snapshot_world in setup_file"
    return 0
  }
  local after missing extra
  after="$(kind get clusters 2>/dev/null | sort)"
  e2e::log "e2e: kind get clusters (after):  $(tr '\n' ' ' <<<"${after}")"

  missing="$(comm -23 "${snapshot}" <(printf '%s\n' "${after}"))"
  [[ -z "${missing}" ]] || echo "pre-existing kind clusters disappeared: $(tr '\n' ' ' <<<"${missing}") (before: $(tr '\n' ' ' < "${snapshot}"))"

  extra="$(comm -13 "${snapshot}" <(printf '%s\n' "${after}") | grep -v '^e2e-' || true)"
  [[ -z "${extra}" ]] || echo "a kind cluster this run does not own appeared: $(tr '\n' ' ' <<<"${extra}")"
}

# e2e::assert_world_unchanged — e2e::_world_problems, as an assertion.
e2e::assert_world_unchanged() {
  local problems
  problems="$(e2e::_world_problems)"
  [[ -z "${problems}" ]] || fail "e2e: ${problems}"
}

# e2e::_docker_names <what> <filter> — the names of matching docker
# objects, one per line. <what> is ps | volume | network.
e2e::_docker_names() {
  case "$1" in
    ps)      docker ps -a --filter "name=$2" --format '{{.Names}}' 2>/dev/null | sort ;;
    volume)  docker volume ls --filter "name=$2" --format '{{.Name}}' 2>/dev/null | sort ;;
    network) docker network ls --filter "name=$2" --format '{{.Name}}' 2>/dev/null | sort ;;
  esac
}

# e2e::_remove_report <what> <names> — remove the named docker objects
# (<what> is ps | volume) and describe the OUTCOME, not the attempt:
# which ones the safety net removed and which ones survived it. A report
# written from the list gathered before the removal reads as a clean-up
# even when nothing could be cleaned up.
e2e::_remove_report() {
  local what="$1" names="$2" removed="" survived="" n
  while IFS= read -r n; do
    [[ -n "${n}" ]] || continue
    case "${what}" in
      ps)     docker rm -f "${n}" >/dev/null 2>&1 || true ;;
      volume) docker volume rm -f "${n}" >/dev/null 2>&1 || true ;;
    esac
    if [[ -n "$(e2e::_docker_names "${what}" "^${n}$")" ]]; then
      survived+="${n} "
    else
      removed+="${n} "
    fi
  done <<<"${names}"
  [[ -z "${removed}" ]] || printf '%s(removed by the safety net) ' "${removed}"
  [[ -z "${survived}" ]] || printf 'STILL THERE: %s' "${survived}"
}

# e2e::assert_torn_down <phase>
#
# The teardown IS the assertion. <phase> is what just ran:
#
#   down     `lo down`: the kind cluster, its node containers and the
#            proxy are gone. So are the registry containers — UNLESS the
#            set is shared, where `lo down` leaves them up on purpose
#            (the mirrors are reused by the other clusters and build and
#            cache stay warm; `lo registry down` is what removes them).
#            The registry data volumes and the project docker network
#            stay either way, so this asserts they are still there
#            instead of ignoring them.
#   destroy  `lo destroy`: all of the above, and the project's registry
#            containers, their data volumes and the certificate volume
#            are gone too. The shared mirrors are not the project's and
#            survive by design; `lo registry clean --shared` removes
#            them. The project network still stays; e2e::sweep removes
#            it at the end of the scenario.
#
# Anything the scenario owns that survives is removed here, so the
# machine is left clean whatever the result, and then the test FAILS
# naming what `lo <phase>` should have removed. A silent belt-and-braces
# cleanup is how a broken teardown passes for months.
e2e::assert_torn_down() {
  local phase="${1:?e2e::assert_torn_down needs a phase (down|destroy)}"
  local cluster="${LOK8S_CLUSTER_NAME}" net="${E2E_NETWORK}"
  e2e::guard_name cluster "${cluster}"
  e2e::guard_name network "${net}"

  local -a leftovers=()
  local names regs

  # Shared or not comes from the registry file the run itself wrote, not
  # from what the test believes the spec says.
  local shared=0
  if [[ -f "${PATH_CLUSTERS}/${DOMAIN_NAME}/.registries.json" ]] \
    && grep -q '"shared": true' "${PATH_CLUSTERS}/${DOMAIN_NAME}/.registries.json"; then
    shared=1
  fi

  # 1. The kind cluster. The belt-and-braces delete stays, and it reports
  #    what is TRUE after it ran, not what it attempted. `kind delete` can
  #    fail too, and a failure message that claims the cluster was cleaned
  #    up while it is still running is the one lie this suite cannot
  #    afford: the next scenario would then trip over it and blame itself.
  if kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
    kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
    if kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
      leftovers+=("kind cluster ${cluster} — lo ${phase} left it behind AND the safety net could not delete it. It is STILL RUNNING. Remove it by hand: kind delete cluster --name ${cluster}")
    else
      leftovers+=("kind cluster ${cluster} — lo ${phase} left it behind; the safety net deleted it")
    fi
  fi

  # 2. The kind nodes and the proxy: never a documented keep. Same rule
  #    as the cluster — say which ones actually went.
  names="$(e2e::_docker_names ps "^${cluster}-" | grep -v "^${net}-registry-" || true)"
  if [[ -n "${names}" ]]; then
    leftovers+=("containers after lo ${phase}: $(e2e::_remove_report ps "${names}")")
  fi

  # 3. The project's registry containers. Gone, except after a `down` on
  #    a shared set, where the framework says it keeps them.
  regs="$(e2e::_docker_names ps "^${net}-registry-")"
  if [[ -n "${regs}" ]]; then
    if (( shared )) && [[ "${phase}" == "down" ]]; then
      e2e::log "e2e: shared set — lo down keeps $(tr '\n' ' ' <<<"${regs}")"
    else
      leftovers+=("registry containers after lo ${phase}: $(e2e::_remove_report ps "${regs}")")
    fi
  fi

  # 4. Volumes. After `down` the registry data volumes are a documented
  #    keep; anything else with the prefix is not. After `destroy`
  #    nothing with the prefix may survive.
  names="$(e2e::_docker_names volume "^${net}-")"
  if [[ "${phase}" == "down" ]]; then
    local unexpected="" v
    while IFS= read -r v; do
      [[ -n "${v}" ]] || continue
      case "${v}" in
        "${net}"-registry-*) ;;   # kept by design until `lo destroy`
        *) unexpected+="${v} " ;;
      esac
    done <<<"${names}"
    [[ -z "${unexpected}" ]] || leftovers+=("unexpected volumes after lo down: ${unexpected}")
  elif [[ -n "${names}" ]]; then
    leftovers+=("volumes after lo destroy: $(e2e::_remove_report volume "${names}")")
  fi

  # 5. The project docker network persists BY DESIGN across down and
  #    destroy (`lo up` reuses it). State that, rather than ignoring the
  #    one object that is allowed to survive.
  if ! docker network inspect "${net}" >/dev/null 2>&1; then
    leftovers+=("the project network ${net} is gone — the framework keeps it across lo ${phase}")
  fi

  # 6. The kind clusters. Collected, not failed on the spot: a teardown
  #    that both changed the world AND left objects behind has to report
  #    both, or the second finding waits for the next run.
  local problem
  while IFS= read -r problem; do
    [[ -z "${problem}" ]] || leftovers+=("${problem}")
  done < <(e2e::_world_problems)

  if (( ${#leftovers[@]} )); then
    local report="" item
    for item in "${leftovers[@]}"; do report+="  - ${item}"$'\n'; done
    fail "e2e: lo ${phase} did not leave the machine clean:
${report}"
  fi
  e2e::log "e2e: lo ${phase} left the machine clean (cluster ${cluster}, network ${net})"
}

# e2e::assert_registry_containers <state> <short-name...>
# <state> is `up` (the container is there) or `gone` (it is not). The
# names are registry short names (build, cache, io-docker …); the
# container name is derived the way the framework derives it.
e2e::assert_registry_containers() {
  local state="$1"; shift
  local short running
  for short in "$@"; do
    running="$(e2e::_docker_names ps "^${E2E_NETWORK}-registry-${short}$")"
    case "${state}" in
      up)   [[ -n "${running}" ]] || fail "e2e: registry container ${E2E_NETWORK}-registry-${short} is not there" ;;
      gone) [[ -z "${running}" ]] || fail "e2e: registry container ${E2E_NETWORK}-registry-${short} survived" ;;
      *)    fail "e2e::assert_registry_containers: state must be up or gone" ;;
    esac
  done
}

# e2e::sweep — the end of a scenario. Remove what the framework keeps by
# design (the project docker network) and fail if anything else the
# scenario owns is still there. Call it from teardown_file AFTER the
# last e2e::assert_torn_down.
#
# The kind cluster is checked here too. assert_torn_down deletes one it
# finds, but that delete can fail, and a scenario whose last assertion
# already failed must not leave the next one to discover a running
# cluster and blame itself for it.
e2e::sweep() {
  local net="${E2E_NETWORK}" cluster="${LOK8S_CLUSTER_NAME}"
  e2e::guard_name network "${net}"
  e2e::guard_name cluster "${cluster}"
  docker network rm "${net}" >/dev/null 2>&1 || true

  local -a left=()
  if kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
    kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
    if kind get clusters 2>/dev/null | grep -qx "${cluster}"; then
      left+=("kind cluster ${cluster} (STILL RUNNING — remove it by hand)")
    else
      left+=("kind cluster ${cluster} (removed by the sweep)")
    fi
  fi
  # One list per kind, each on its own line: concatenating three command
  # substitutions runs the last name of one into the first of the next.
  local names
  for names in \
    "$(e2e::_docker_names ps "^${cluster}-")" \
    "$(e2e::_docker_names volume "^${net}-")" \
    "$(e2e::_docker_names network "^${net}$")"; do
    [[ -z "${names}" ]] || left+=("$(tr '\n' ' ' <<<"${names}")")
  done

  rm -rf "${E2E_STATE:?}/bin" "${E2E_STATE:?}/provisioned.${cluster}"
  (( ${#left[@]} == 0 )) || fail "e2e: sweep found leftovers: ${left[*]}"
}

# e2e::final_teardown — teardown_file's one call: destroy whatever is
# left, assert the machine is clean, sweep the network the framework
# keeps. A scenario that never provisioned (every test skipped) has
# nothing to assert about, so it returns early instead of reporting
# objects that were never created.
#
# The destroy here is the safety net, not the subject: the scenario's
# own test asserts that `lo destroy` succeeds. Its error is logged and
# the assertion below is what judges the result.
e2e::final_teardown() {
  if [[ ! -f "${E2E_STATE}/provisioned.${LOK8S_CLUSTER_NAME}" ]]; then
    e2e::log "e2e: nothing was provisioned — no teardown to assert"
    return 0
  fi
  e2e::destroy || e2e::log "e2e: the teardown destroy returned non-zero (the assertion below judges it)"
  e2e::assert_torn_down destroy
  e2e::sweep
}

# ── The registry set's certificate ───────────────────────

# e2e::tls_volume — the set's certificate volume.
e2e::tls_volume() {
  e2e::guard_name network "${E2E_NETWORK}"
  echo "${E2E_NETWORK}-registry-tls"
}

# e2e::tls_volume_read <file> — print one file of that volume (tls.crt,
# tls.key, .sans). Read the way the driver reads it: a throwaway
# container mounts the volume, because `docker cp` needs one and the
# registries may not be running.
#
# The existence check is not politeness: `--volume <name>:…` CREATES a
# missing named volume, so reading a volume that is not there would
# quietly bring it back and make the next "it is gone" assertion lie.
e2e::tls_volume_read() {
  local vol ctr
  vol="$(e2e::tls_volume)"
  docker volume inspect "${vol}" >/dev/null 2>&1 || return 1
  # Named OUTSIDE the `<network>-` prefix that assert_torn_down polices:
  # a reader killed mid-run would otherwise look like a container `lo`
  # failed to remove, and the failure would name the wrong culprit. The
  # `e2e-` prefix still marks it as this suite's, and the removals below
  # bracket every use.
  ctr="e2e-tlsread-${vol}"
  docker rm -f "${ctr}" >/dev/null 2>&1 || true
  docker container create --name "${ctr}" --volume "${vol}:/etc/registry/certs" \
    registry:2.8.3 >/dev/null 2>&1 || return 1
  docker cp "${ctr}:/etc/registry/certs/${1}" - 2>/dev/null | tar -xO
  docker rm -f "${ctr}" >/dev/null 2>&1 || true
}

# e2e::served_cert <ip> — the certificate the registry at <ip> serves on
# :443, as PEM. Empty when it serves none.
e2e::served_cert() {
  </dev/null openssl s_client -connect "${1}:443" 2>/dev/null | openssl x509 2>/dev/null
}

# e2e::cert_serial — the serial of the PEM certificate on stdin.
e2e::cert_serial() {
  openssl x509 -noout -serial 2>/dev/null | cut -d= -f2
}

# ── The confirmation prompts (a real terminal) ───────────

# e2e::pty_run <answer> <argv...>
#
# Run the binary under test on a pseudo-terminal and TYPE <answer> into
# it. `script` owns the pty; the command under test sees a terminal on
# stdin and on stderr, which is the gate its confirmation reads
# (internal/cli/confirm.go). The answer goes through the pty master, so
# it is typed, not piped: a pipe on the command's OWN stdin turns the
# prompt off and the command proceeds, the exact mistake this test
# exists to pin down.
#
# Sets E2E_PTY_OUTPUT (the typescript, CR stripped) and E2E_PTY_RC.
e2e::pty_run() {
  local answer="$1"; shift
  command -v script >/dev/null 2>&1 || e2e::_unmet "e2e: util-linux 'script' not in PATH (the pty tests need it)"
  local log="${E2E_STATE}/pty.log"
  local cmdline
  cmdline="$(printf '%q ' "${E2E_LO_BIN}" "$@")"
  E2E_PTY_RC=0
  printf '%s\n' "${answer}" \
    | script -q -e -c "${cmdline}" "${log}" >/dev/null 2>&1 || E2E_PTY_RC=$?
  E2E_PTY_OUTPUT="$(tr -d '\r' < "${log}")"
  export E2E_PTY_OUTPUT E2E_PTY_RC
  # Every transcript is kept, in order: what the prompt printed and what
  # was answered is the evidence a reviewer reads, and one overwritten
  # file would only hold the last of them.
  {
    printf '=== pty: answer %q · rc %s ===\n%s\n\n' \
      "${answer}" "${E2E_PTY_RC}" "${E2E_PTY_OUTPUT}"
  } >> "${E2E_STATE}/pty-transcript.log"
}

# ── Assertions ───────────────────────────────────────────

# e2e::assert_kustomization_has <pattern>
# Assert the auto-generated artifacts/kustomization.yaml contains a
# regex pattern. Useful for verifying env::kustomization output.
e2e::assert_kustomization_has() {
  local pattern="$1"
  local kustfile="${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/kustomization.yaml"
  if [[ ! -f "${kustfile}" ]]; then
    echo "no kustomization.yaml at ${kustfile}" >&2
    return 1
  fi
  if ! grep -qE "${pattern}" "${kustfile}"; then
    echo "pattern '${pattern}' not found in ${kustfile}" >&2
    echo "actual:" >&2
    cat "${kustfile}" >&2
    return 1
  fi
}

# e2e::assert_kustomization_missing <pattern>
e2e::assert_kustomization_missing() {
  local pattern="$1"
  local kustfile="${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/kustomization.yaml"
  if [[ ! -f "${kustfile}" ]]; then
    return 0
  fi
  if grep -qE "${pattern}" "${kustfile}"; then
    echo "pattern '${pattern}' should NOT appear in ${kustfile} but does" >&2
    return 1
  fi
}

# e2e::assert_queue_empty
# Assert the cache pre-pull queue (.cache-queue) has zero entries.
e2e::assert_queue_empty() {
  local queue="${PATH_CLUSTERS}/${DOMAIN_NAME}/artifacts/.cache-queue"
  if [[ -s "${queue}" ]]; then
    echo "expected empty cache queue but ${queue} has entries:" >&2
    cat "${queue}" >&2
    return 1
  fi
}
