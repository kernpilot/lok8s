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

# utils/domain.sh — domain::resolve / domain::driver / domain::spec_driver, the
# single domain-resolution and driver-identity point every lib now reads
# through. Same reasoning as above: a lib sourced under a stubbed `import`
# would otherwise find domain::spec_driver undefined.
source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"

# NOTE on the two global sources above: they make it impossible for any test in
# this suite to fail on a lib's MISSING `import ^utils/spec` / `import
# ^utils/domain` — the definitions are already there. That is a real class of
# bug (nine libs called domain::spec_driver with no import of their own), and
# it is NOT guarded here. It is guarded by
# `tests/unit/import_convention_test.bats::every lib that calls a shared util's
# helpers imports it`, which reads the source instead of running it. That gate
# no longer needs updating when a global source is added: it DERIVES the
# namespaces it checks from `.lok8s/utils/*.sh`, so a new util is covered the
# day it lands. (It used to carry a hand-kept list here, which is the same
# class of defect the gate exists to catch.)

# ── shared sweep primitives ──────────────────────────────────────────────────
#
# Several gates sweep the shell tree for a forbidden spelling. Both halves of
# such a gate — WHICH files it reads, and WHETHER its pattern still matches the
# thing it forbids — are silent when broken: an absence assertion over an empty
# file list, or with a stale regex, passes. Both live here so the gates cannot
# derive them independently and drift (they did: three copies in two spellings,
# with differing exclusions).

# shell_sources — every bash/argsh source under .lok8s, one path per line.
#
# `init.d` is excluded by name: it is the scaffolding lok8s writes into a
# user's project, not framework code, and it is TypeScript. The extension
# exclusions drop data files that the header grep would otherwise have to read.
#
# Fails loudly on a near-empty result — a broken predicate must not read as
# "nothing to find".
shell_sources() {
  local -a files=()
  mapfile -t files < <(
    find "${_PROJECT_ROOT}/.lok8s" -type f \
      ! -path '*/init.d/*' \
      ! -name '*.yaml' ! -name '*.yml' ! -name '*.json' ! -name '*.ts' \
      -print0 \
      | xargs -0 grep -lE '^#!/usr/bin/env (argsh|bash)|^# shellcheck shell=bash' 2>/dev/null
  )
  # 67 today. The floor only has to be high enough that a predicate which
  # matches almost nothing cannot pass for a tree that has almost nothing.
  (( ${#files[@]} >= 50 )) || {
    echo "shell_sources found only ${#files[@]} bash/argsh file(s) under .lok8s" >&2
    echo "— the discovery predicate is broken, not the tree." >&2
    return 1
  }
  printf '%s\n' "${files[@]}"
}

# assert_pattern_matches <label> <ere> <sample> — the anti-vacuity half of an
# absence gate.
#
# A gate that asserts "this spelling appears nowhere" is green both when the
# spelling is gone and when the pattern stopped recognising it. Feeding the
# pattern a canary line that MUST match separates the two without needing a
# live specimen of the forbidden form to sit in the tree.
assert_pattern_matches() {
  local label="$1" ere="$2" sample="$3"
  printf '%s\n' "${sample}" | grep -qE "${ere}" && return 0
  echo "${label}: the pattern no longer matches its own canary, so the gate" >&2
  echo "below is measuring nothing. Fix the pattern, not the tree." >&2
  echo "  pattern: ${ere}" >&2
  echo "  canary:  ${sample}" >&2
  return 1
}

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
