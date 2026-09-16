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

# Resolve the bats runner. A host bats comes FIRST, and `argsh test` is
# the fallback: argsh runs bats on the host when it finds one, but
# forwards to its container when it does not — and a cluster scenario in
# a container without the docker socket fails in a way that says nothing
# about the code under test.
if command -v bats &>/dev/null; then
  _run_bats() { bats "$@"; }
elif [[ -x "${_ROOT}/.bin/bin/bats" ]]; then
  _run_bats() { "${_ROOT}/.bin/bin/bats" "$@"; }
elif command -v argsh &>/dev/null; then
  _run_bats() { argsh test "$@"; }
else
  echo "error: no bats in PATH and none under .bin/bin" >&2
  echo "       run: ./.bin/b install" >&2
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
