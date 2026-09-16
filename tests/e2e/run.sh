#!/usr/bin/env bash
# tests/e2e/run.sh — discover and run e2e scenario tests.
#
# Usage:
#   tests/e2e/run.sh                    # run all scenarios
#   tests/e2e/run.sh <scenario>         # run one scenario by name
#   tests/e2e/run.sh <scenario> [...]   # run multiple by name
#   E2E=1 tests/e2e/run.sh ...          # actually spin up clusters
#                                       # (tests skip otherwise)
#
# Runs bats. `b install` puts one under .bin/bin, together with the
# bats-support and bats-assert the helpers load from .bin/lib.

set -euo pipefail

_HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_ROOT="$(cd "${_HERE}/../.." && pwd)"

# _have_bats_libs — can the helpers load bats-support and bats-assert?
#
# A bats binary alone is not enough: every scenario loads those two
# through tests/lib/bats_libs.bash, and without them the run dies in
# setup() with "Could not find library 'bats-support'" — which reads as a
# broken branch, not a missing install. The candidate list mirrors that
# loader (it is the authority; keep the two in step), including the main
# working tree's .bin/lib so a linked git worktree resolves.
_have_bats_libs() {
  local d common main_root=""
  if common="$(git -C "${_ROOT}" rev-parse --git-common-dir 2>/dev/null)"; then
    [[ "${common}" == /* ]] || common="${_ROOT}/${common}"
    main_root="$(cd "${common}/.." 2>/dev/null && pwd)" || main_root=""
  fi
  # A pre-set BATS_LIB_PATH comes first: a host that already resolves the
  # libraries through it works, and answering "not installed" there would
  # send a perfectly good machine down the fallback.
  local -a dirs=()
  [[ -z "${BATS_LIB_PATH:-}" ]] || IFS=: read -r -a dirs <<<"${BATS_LIB_PATH}"
  dirs+=(/usr/lib /usr/local/lib "${HOME}/.local/lib" /opt/homebrew/lib
    "${_ROOT}/.bin/lib" ${main_root:+"${main_root}/.bin/lib"})
  for d in "${dirs[@]}"; do
    [[ -n "${d}" ]] || continue
    [[ -d "${d}/bats-support" && -d "${d}/bats-assert" ]] && return 0
  done
  return 1
}

# Resolve the bats runner. A host bats comes FIRST, and `argsh test` is
# the fallback: argsh runs bats on the host when it finds one, but
# forwards to its container when it does not — and a cluster scenario in
# a container without the docker socket fails in a way that says nothing
# about the code under test.
if _have_bats_libs && command -v bats &>/dev/null; then
  _run_bats() { bats "$@"; }
elif _have_bats_libs && [[ -x "${_ROOT}/.bin/bin/bats" ]]; then
  _run_bats() { "${_ROOT}/.bin/bin/bats" "$@"; }
elif command -v argsh &>/dev/null; then
  if ! _have_bats_libs; then
    echo "note: bats-support/bats-assert are not installed, so this run goes" >&2
    echo "      through 'argsh test'. If argsh has no host bats either it" >&2
    echo "      forwards to its container, which has no docker socket and" >&2
    echo "      cannot stand a cluster up. Install them with: ./.bin/b install" >&2
  fi
  _run_bats() { argsh test "$@"; }
else
  echo "error: no usable bats runner." >&2
  echo "       run: ./.bin/b install   (installs bats under .bin/bin and" >&2
  echo "       bats-support + bats-assert under .bin/lib)" >&2
  exit 1
fi

# Collect scenarios
declare -a scenarios=()
if (( $# == 0 )); then
  # All top-level dirs under tests/e2e/ that contain a test.bats
  while IFS= read -r bats_file; do
    scenarios+=("$(basename "$(dirname "${bats_file}")")")
  done < <(find "${_HERE}" -mindepth 2 -maxdepth 2 -name 'test.bats' | sort)
else
  scenarios=("$@")
fi

if (( ${#scenarios[@]} == 0 )); then
  echo "no e2e scenarios found under ${_HERE}" >&2
  exit 1
fi

fail_count=0
for scenario in "${scenarios[@]}"; do
  test_file="${_HERE}/${scenario}/test.bats"
  # Use path relative to repo root for argsh test Docker compatibility
  test_file_rel="tests/e2e/${scenario}/test.bats"
  if [[ ! -f "${test_file}" ]]; then
    echo "!! ${scenario}: no test.bats at ${test_file}" >&2
    fail_count=$(( fail_count + 1 ))
    continue
  fi
  echo "== ${scenario} =="
  if ! _run_bats "${test_file_rel}"; then
    fail_count=$(( fail_count + 1 ))
  fi
  echo
done

if (( fail_count > 0 )); then
  echo "${fail_count} scenario(s) failed" >&2
  exit 1
fi
echo "all scenarios passed"
