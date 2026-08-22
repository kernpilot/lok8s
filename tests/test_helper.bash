#!/usr/bin/env bash
# test_helper.bash — shared setup for all bats tests
# Loads bats-support and bats-assert, sets PATH_BASE, sources verbose helpers.

_TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_PROJECT_ROOT="$(cd "${_TESTS_DIR}/.." && pwd)"

# Load bats assertion libraries via the shared loader (worktree-aware
# resolution lives there; e2e uses the same one so the suites can't drift).
source "${_TESTS_DIR}/lib/bats_libs.bash"
_load_bats_libs

# Project root used by all library scripts
export PATH_BASE="${_PROJECT_ROOT}"

# Unset ARGSH_SOURCE so the standalone guards at the bottom of each lib
# don't fire when tests `source` a lib file. The guard condition
# `[[ "$0" != "${BASH_SOURCE[0]}" && -z "${ARGSH_SOURCE:-}" ]]`
# succeeds (skips main::*) only when ARGSH_SOURCE is empty.
# Inside the argsh docker container, ARGSH_SOURCE=argsh by default —
# without unsetting it, every `source .lok8s/libs/*` would trigger
# main::* at parse time.
unset ARGSH_SOURCE

# Source verbose helpers (debug, error, warn) — these are used by all libs
source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"

# Source utils/spec.sh — the shared spec.workers reader that kkp, kubeone and
# capi all `import`. Sourced here for the same reason verbose.sh is: a test that
# sources a driver stubs `import` first, so the driver's own `import ^utils/spec`
# is a no-op and the helpers would be undefined in every driver test. `import` is
# stubbed only for the length of this source and then removed, so a test that
# does NOT stub it still fails loudly on its own missing imports.
import() { :; }
source "${_PROJECT_ROOT}/.lok8s/utils/spec.sh"
unset -f import

# Fixture directory
export FIXTURES_DIR="${_TESTS_DIR}/fixtures"

# Create a temporary directory per test for scratch files.
# Also exports PATH_BASE / PATH_LOK8S / PATH_SCRIPTS pointed at the
# tmpdir so that library code reading those vars resolves under the
# per-test sandbox.
setup_tmpdir() {
  BATS_TEST_TMPDIR="$(mktemp -d)"
  export BATS_TEST_TMPDIR
  export PATH_BASE="${BATS_TEST_TMPDIR}"
  export PATH_LOK8S="${BATS_TEST_TMPDIR}/.lok8s"
  export PATH_SCRIPTS="${PATH_LOK8S}"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
}

teardown_tmpdir() {
  [[ -d "${BATS_TEST_TMPDIR:-}" ]] && rm -rf "${BATS_TEST_TMPDIR}"
}

# Vendor the framework utils that drivers/lo/main sources unconditionally
# (ip, oidc, kapply) into the per-test PATH_LOK8S tree, so a sandbox can source
# the real lo driver. Single source of truth: when the driver gains a new
# framework-util dependency, add it here — not to every sandbox's setup().
# Requires setup_tmpdir (BATS_TEST_TMPDIR) + _PROJECT_ROOT.
vendor_lo_utils() {
  mkdir -p "${BATS_TEST_TMPDIR}/.lok8s/utils"
  local util
  for util in ip oidc kapply; do
    cp "${_PROJECT_ROOT}/.lok8s/utils/${util}.sh" "${BATS_TEST_TMPDIR}/.lok8s/utils/${util}.sh"
  done
}

# Mock a command by creating a bash function that overrides it.
# Usage: mock_command <name> [exit_code] [stdout_output]
mock_command() {
  local name="$1" exit_code="${2:-0}" stdout="${3:-}"
  eval "${name}() { echo '${stdout}'; return ${exit_code}; }"
  export -f "${name}"
}

# Mock yq to return a specific value for any call.
# Usage: mock_yq_value <value>
mock_yq_value() {
  local value="$1"
  yq() { echo "${value}"; }
  export -f yq
}
