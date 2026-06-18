#!/usr/bin/env bats
# use_test.bats — unit tests for `lo use` active-domain setter
#
# Covers use::_set_active, the pure validate-and-persist behind `lo use
# <domain>`. Regression guard: `lo use` could neither
# take a domain (positional → "too many arguments") nor write clusters/.active.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  # Sourcing libs/use defines use::_set_active; imports are stubbed and the
  # bottom guard does not fire (ARGSH_SOURCE is unset by test_helper).
  source "${_PROJECT_ROOT}/.lok8s/libs/use"

  mkdir -p "${PATH_CLUSTERS}"
  # A cluster domain and a deploy domain.
  mkdir -p "${PATH_CLUSTERS}/cluster-dom" "${PATH_CLUSTERS}/deploy-dom"
  : > "${PATH_CLUSTERS}/cluster-dom/cluster.lok8s.yaml"
  : > "${PATH_CLUSTERS}/deploy-dom/deploy.lok8s.yaml"
}

teardown() { teardown_tmpdir; }

@test "use::_set_active writes .active for a cluster domain" {
  run use::_set_active "cluster-dom"
  [ "$status" -eq 0 ]
  [[ "$output" == *"Active domain: cluster-dom"* ]]
  [ "$(cat "${PATH_CLUSTERS}/.active")" = "cluster-dom" ]
}

@test "use::_set_active accepts a deploy domain too" {
  run use::_set_active "deploy-dom"
  [ "$status" -eq 0 ]
  [ "$(cat "${PATH_CLUSTERS}/.active")" = "deploy-dom" ]
}

@test "use::_set_active overwrites a previous active domain" {
  echo "cluster-dom" > "${PATH_CLUSTERS}/.active"
  run use::_set_active "deploy-dom"
  [ "$status" -eq 0 ]
  [ "$(cat "${PATH_CLUSTERS}/.active")" = "deploy-dom" ]
}

@test "use::_set_active rejects a non-existent domain and leaves .active untouched" {
  echo "cluster-dom" > "${PATH_CLUSTERS}/.active"
  run use::_set_active "nope"
  [ "$status" -ne 0 ]
  [[ "$output" == *"domain not found"* ]]
  # .active must be unchanged
  [ "$(cat "${PATH_CLUSTERS}/.active")" = "cluster-dom" ]
}

@test "use::_set_active rejects path traversal" {
  run use::_set_active "../etc"
  [ "$status" -ne 0 ]
  [[ "$output" == *"invalid domain name"* ]]
  [ ! -f "${PATH_CLUSTERS}/.active" ]
}

@test "use::_set_active rejects a name with a slash" {
  run use::_set_active "foo/bar"
  [ "$status" -ne 0 ]
  [[ "$output" == *"invalid domain name"* ]]
}

@test "use::_set_active rejects an empty domain dir with no spec" {
  mkdir -p "${PATH_CLUSTERS}/bare"
  run use::_set_active "bare"
  [ "$status" -ne 0 ]
  [[ "$output" == *"domain not found"* ]]
}
