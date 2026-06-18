#!/usr/bin/env bash
# test_helper.bash — shared setup for all bats tests
# Loads bats-support and bats-assert, sets PATH_BASE, sources verbose helpers.

_TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_PROJECT_ROOT="$(cd "${_TESTS_DIR}/.." && pwd)"

# Load bats assertion libraries. No vendored submodules — use
# `argsh test` which provides bats + bats-support + bats-assert.
_load_bats_libs() {
  # Ensure BATS_LIB_PATH includes standard locations where argsh's
  # Docker image (and system installs) place bats-support/bats-assert.
  local d
  for d in /usr/lib /usr/local/lib "${HOME}/.local/lib" /opt/homebrew/lib "${_PROJECT_ROOT}/.bin/lib"; do
    [[ -d "${d}/bats-support" ]] || continue
    [[ ":${BATS_LIB_PATH:-}:" == *":${d}:"* ]] || BATS_LIB_PATH="${BATS_LIB_PATH:+${BATS_LIB_PATH}:}${d}"
  done
  export BATS_LIB_PATH

  # bats_load_library (bats >= 1.5)
  if declare -F bats_load_library &>/dev/null; then
    bats_load_library bats-support
    bats_load_library bats-assert
    return 0
  fi
  # Direct load fallback (bats < 1.5)
  for d in /usr/lib /usr/local/lib "${HOME}/.local/lib" /opt/homebrew/lib; do
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
