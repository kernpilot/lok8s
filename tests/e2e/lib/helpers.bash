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
#     e2e::provision
#   }
#
#   teardown_file() {
#     load "${BATS_TEST_DIRNAME}/../lib/helpers"
#     e2e::destroy
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
#    PATH_BASE: `lo` reads services.yaml, clusters/, .kubeconfig/,
#    etc. from there.
#
#  - PATH_LOK8S and PATH_BIN are left pointing at the real project
#    root. `lo` imports its bash libs and argsh binary from there.
#    Framework changes are picked up immediately by running e2e
#    tests — no scenario-side sync needed.
#
#  - PATH_CLUSTERS is set to the scenario's local clusters/ dir so
#    cluster definitions live next to the test that uses them.
#
#  - Each scenario spins up and tears down a real kind cluster via
#    `lo provision` / `lo destroy`. Scenarios that don't need a
#    cluster (validator-only) should NOT call e2e::provision.
#
#  - Tests skip automatically on machines without docker/kind/etc.

# Resolve the project root from this file's location.
# tests/e2e/lib/helpers.bash → project root is ../../..
_E2E_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_E2E_DIR="$(cd "${_E2E_LIB_DIR}/.." && pwd)"
_PROJECT_ROOT="$(cd "${_E2E_DIR}/../.." && pwd)"

# Load bats assertion libraries. No vendored submodules — use
# `argsh test` which provides bats + bats-support + bats-assert.
_load_bats_libs() {
  local d
  for d in /usr/lib /usr/local/lib "${HOME}/.local/lib" /opt/homebrew/lib "${_PROJECT_ROOT}/.bin/lib"; do
    [[ -d "${d}/bats-support" ]] || continue
    [[ ":${BATS_LIB_PATH:-}:" == *":${d}:"* ]] || BATS_LIB_PATH="${BATS_LIB_PATH:+${BATS_LIB_PATH}:}${d}"
  done
  export BATS_LIB_PATH

  if declare -F bats_load_library &>/dev/null; then
    bats_load_library bats-support
    bats_load_library bats-assert
    return 0
  fi
  for d in /usr/lib /usr/local/lib "${HOME}/.local/lib" /opt/homebrew/lib "${_PROJECT_ROOT}/.bin/lib"; do
    if [[ -f "${d}/bats-support/load.bash" ]] && [[ -f "${d}/bats-assert/load.bash" ]]; then
      load "${d}/bats-support/load.bash"
      load "${d}/bats-assert/load.bash"
      return 0
    fi
  done
  echo "error: bats-support/bats-assert not found. Run tests via: argsh test" >&2
  return 1
}
_load_bats_libs

# Default timeouts. Scenarios can override by setting these before
# calling helpers.
: "${E2E_PROVISION_TIMEOUT:=600}"   # 10 min to provision a kind cluster
: "${E2E_DESTROY_TIMEOUT:=180}"     # 3 min to tear down
: "${E2E_TILT_CI_TIMEOUT:=600}"     # 10 min for tilt ci to reach steady state

# ── Skip conditions ──────────────────────────────────────

# e2e::require_tools — call from setup() to skip when prereqs are missing.
# Usage: e2e::require_tools docker kind kustomize yq tilt
e2e::require_tools() {
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 || {
      skip "e2e: '${tool}' not in PATH"
    }
  done
  if command -v docker >/dev/null 2>&1; then
    docker info >/dev/null 2>&1 || skip "e2e: docker daemon not running"
  fi
}

# e2e::require_e2e_enabled — opt-in gate so `bats tests/unit/...` doesn't
# pull in cluster lifecycle by accident. Set E2E=1 to run.
e2e::require_e2e_enabled() {
  [[ "${E2E:-}" == "1" ]] || skip "e2e: set E2E=1 to run cluster-backed tests"
}

# e2e::require_dns <domain>
# Skip the scenario if the wildcard DNS for its slot doesn't resolve.
# Each slot N relies on `*.N.lok8s.dev` pointing at `10.125.N.x`.
# Without that, mkcert certs and any in-cluster service references
# fail in confusing ways late in the run.
e2e::require_dns() {
  local domain="$1"
  command -v dig >/dev/null 2>&1 || {
    skip "e2e: dig not in PATH (install bind-utils/dnsutils to enable DNS preflight)"
  }
  local probe="probe.${domain}"
  local resolved
  resolved=$(dig +short "${probe}" 2>/dev/null | head -1)
  [[ -n "${resolved}" ]] || {
    skip "e2e: ${probe} does not resolve — DNS slot ${domain} unprovisioned (see tests/e2e/SUBNETS.md)"
  }
}

# ── Scenario lifecycle ───────────────────────────────────

# e2e::init <scenario-dir> [domain]
# Point PATH_BASE at the scenario dir, leave PATH_LOK8S/PATH_BIN at
# the project, set PATH_CLUSTERS to the scenario's local clusters/,
# and set DOMAIN_NAME + LOK8S_CLUSTER_NAME to unique-per-scenario
# values so parallel runs in the future don't collide.
e2e::init() {
  local scenario_dir="$1"
  local domain="${2:-lok8s.dev}"
  [[ -d "${scenario_dir}" ]] || {
    echo "e2e::init: scenario dir not found: ${scenario_dir}" >&2
    return 1
  }

  export PATH_BASE="${scenario_dir}"
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export PATH_SCRIPTS="${PATH_LOK8S}"
  export PATH_BIN="${_PROJECT_ROOT}/.bin"
  export PATH_CLUSTERS="${scenario_dir}/clusters"
  export PATH_SECRETS="${scenario_dir}/.secrets"
  export DOMAIN_NAME="${domain}"

  # Cluster name derives from the scenario directory so docker bridge
  # interface names stay under the 15-char Linux limit. e.g.
  # "no-services" -> "e2e-no-services" (15 chars exactly), and longer
  # ones like "single-local-build" get a 3-char abbreviation.
  local scenario_name
  scenario_name="$(basename "${scenario_dir}")"
  case "${scenario_name}" in
    single-local-build) export LOK8S_CLUSTER_NAME="e2e-slb" ;;
    cache-mode)         export LOK8S_CLUSTER_NAME="e2e-cache" ;;
    no-services)        export LOK8S_CLUSTER_NAME="e2e-noop" ;;
    *)                  export LOK8S_CLUSTER_NAME="e2e-${scenario_name:0:10}" ;;
  esac

  # Kustomize plugin discovery (khelm + secrets plugin under .kustomize/)
  export KUSTOMIZE_PLUGIN_HOME="${_PROJECT_ROOT}/.kustomize"

  # Expose the framework lo CLI and the project's .bin to subprocesses
  # spawned by Tilt (docker_build, local(), etc.). Without this, the
  # Tiltfile's `local('lo env ...')` calls fail with "lo: not found"
  # because Tilt's subshell doesn't inherit the bats test harness PATH.
  export PATH="${_PROJECT_ROOT}/.lok8s:${_PROJECT_ROOT}/.bin:${PATH}"

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

# e2e::lo <args...> — invoke the framework lo CLI with the scenario's
# environment. Just a thin wrapper for clarity in test bodies.
e2e::lo() {
  "${_PROJECT_ROOT}/.lok8s/lo" "$@"
}

# e2e::provision — run lo provision for the active scenario domain.
# Times out after E2E_PROVISION_TIMEOUT.
# When E2E_REMOTE=1, passes --remote to activate provider + remote flow.
e2e::provision() {
  local -a _args=(provision --domain "${DOMAIN_NAME}")
  [[ "${E2E_REMOTE:-}" == "1" ]] && _args+=(--remote)
  timeout "${E2E_PROVISION_TIMEOUT}" \
    "${_PROJECT_ROOT}/.lok8s/lo" "${_args[@]}" \
    || {
      echo "e2e::provision failed for ${DOMAIN_NAME}" >&2
      return 1
    }
}

# e2e::destroy — tear down the cluster. Best-effort: errors are
# logged but the test continues so teardown can clean other state.
e2e::destroy() {
  local -a _args=(destroy --domain "${DOMAIN_NAME}" --force)
  [[ "${E2E_REMOTE:-}" == "1" ]] && _args+=(--remote)
  timeout "${E2E_DESTROY_TIMEOUT}" \
    "${_PROJECT_ROOT}/.lok8s/lo" "${_args[@]}" \
    2>/dev/null || {
      echo "e2e::destroy: best-effort failure for ${DOMAIN_NAME}" >&2
    }
  # Belt-and-braces: kill the named kind cluster directly in case
  # `lo destroy` left state behind.
  kind delete cluster --name "${LOK8S_CLUSTER_NAME}" 2>/dev/null || true
}

# e2e::tilt_ci — run tilt ci against the scenario's Tiltfile with a
# scenario-specific port and timeout.
e2e::tilt_ci() {
  cd "${PATH_BASE}"
  TILT_PORT="${TILT_PORT:-10350}" \
    timeout "${E2E_TILT_CI_TIMEOUT}" \
    tilt ci --port "${TILT_PORT:-10350}" \
    --file "${PATH_BASE}/Tiltfile"
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
