#!/usr/bin/env bats
# lint_apex_test.bats — unit tests for lint::apex (one-cluster-per-plane topology)
#
# lint::apex flags a cluster.lok8s.yaml that is a SUBDOMAIN of another
# cluster.lok8s.yaml (e.g. kkp.kubehz.dev alongside kubehz.dev). One cluster per
# plane — subdomains are routing/targets, not separate cluster specs. This is
# the mechanical guard against the recurring "clusters/<sub>.<apex>/" mistake.

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  mkdir -p "${PATH_CLUSTERS}"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/lint"
}

_mk_cluster() { # <domain>
  mkdir -p "${PATH_CLUSTERS}/$1"
  printf 'apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: %s\nspec:\n  cluster:\n    domain: %s\n' \
    "$1" "$1" > "${PATH_CLUSTERS}/$1/cluster.lok8s.yaml"
}

_mk_deploy() { # <domain>
  mkdir -p "${PATH_CLUSTERS}/$1"
  printf 'apiVersion: deploy.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: %s\n' \
    "$1" > "${PATH_CLUSTERS}/$1/deploy.lok8s.yaml"
}

@test "lint::apex passes for distinct apex clusters (dev + prod)" {
  _mk_cluster example.dev
  _mk_cluster example.com
  run lint::apex
  assert_success
}

@test "lint::apex passes for a single cluster" {
  _mk_cluster kubehz.dev
  run lint::apex
  assert_success
}

@test "lint::apex flags a subdomain cluster (kkp.kubehz.dev under kubehz.dev)" {
  _mk_cluster kubehz.dev
  _mk_cluster kkp.kubehz.dev
  run lint::apex
  assert_failure
  assert_output --partial "kkp.kubehz.dev"
  assert_output --partial "subdomain of cluster 'kubehz.dev'"
}

@test "lint::apex flags every poc subdomain under the apex" {
  _mk_cluster kubehz.dev
  _mk_cluster poc.kubehz.dev
  _mk_cluster poc-hcloud.kubehz.dev
  run lint::apex
  assert_failure
  assert_output --partial "poc.kubehz.dev"
  assert_output --partial "poc-hcloud.kubehz.dev"
}

@test "lint::apex exempts deploy.lok8s.yaml domains (deploy targets, not clusters)" {
  _mk_cluster kubehz.dev
  _mk_deploy app.kubehz.dev
  run lint::apex
  assert_success
}
