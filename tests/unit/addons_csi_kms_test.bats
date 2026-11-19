#!/usr/bin/env bats
# addons_csi_kms_test.bats — the bash half of the rook-ceph-csi-kms options
# (issue #265).
#
# The addon is a local chart: its template turns the `kms:` list into the
# roles, and internal/addons/rookceph_kms_test.go renders it with the Go
# builds. These cases prove that the frozen tree gives the chart the same
# values: the entry's `values: {kms: [...]}` replaces the empty default of
# values.yaml, and an entry without it keeps the empty list, which the
# template refuses. kustomize is a stub that keeps the staged build dir.

setup() {
  load "../test_helper"
  setup_tmpdir
  command -v yq &>/dev/null || skip "yq not available"

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/oidc.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/addons"
  source "${_PROJECT_ROOT}/.lok8s/libs/bootstrap"

  KMS_SRC="${_PROJECT_ROOT}/.lok8s/addons/rook-ceph-csi-kms"
  mkdir -p "${PATH_LOK8S}/addons"
  cp -R "${KMS_SRC}" "${PATH_LOK8S}/addons/"
  KMS_ADDON="${PATH_LOK8S}/addons/rook-ceph-csi-kms"

  export SEEN="${BATS_TEST_TMPDIR}/seen"
  mkdir -p "${SEEN}"
  # The build dir is the last argument. Copy what the chart reads out of it
  # before addons::render removes it.
  kustomize() {
    local build_dir="${*: -1}"
    cp "${build_dir}/values.merged.yaml" "${build_dir}/chart.yaml" "${SEEN}/" || return 1
    cp -R "${build_dir}/chart" "${SEEN}/chart" || return 1
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stub\n'
  }
  export -f kustomize
  envsubst() { [[ "${1:-}" != --version ]] || return 0; cat; }
  export -f envsubst
}

teardown() {
  teardown_tmpdir
}

_kms_cluster_yaml() {
  # $1 = the rook-ceph-csi-kms entry (indented list item).
  local path="${PATH_CLUSTERS}/test.lok8s.dev/cluster.lok8s.yaml"
  mkdir -p "${path%/*}"
  printf 'apiVersion: lok8s.dev/v1alpha1\nkind: Cluster\nmetadata:\n  name: test\nspec:\n  bootstrap:\n    - rook-ceph\n%s\n' "${1}" > "${path}"
  printf '%s' "${path}"
}

@test "rook-ceph-csi-kms: the entry's kms list replaces the empty default in the chart values" {
  run addons::render "${KMS_ADDON}" lo "" 'kms: [metadata, rbd-nbd]'
  [ "${status}" -eq 0 ]
  [ "$(yq -o=json -I=0 '.kms' "${SEEN}/values.merged.yaml")" = '["metadata","rbd-nbd"]' ]
  [ "$(yq -r '.chart' "${SEEN}/chart.yaml")" = "./chart" ]
  [ "$(yq -o=json -I=0 '.valueFiles' "${SEEN}/chart.yaml")" = '["values.merged.yaml"]' ]
  cmp "${SEEN}/chart/templates/rbac.yaml" "${KMS_SRC}/chart/templates/rbac.yaml"
  cmp "${SEEN}/chart/templates/_helpers.tpl" "${KMS_SRC}/chart/templates/_helpers.tpl"
}

@test "rook-ceph-csi-kms: kms: [all] reaches the chart as the one option all" {
  run addons::render "${KMS_ADDON}" lo "" 'kms: [all]'
  [ "${status}" -eq 0 ]
  [ "$(yq -o=json -I=0 '.kms' "${SEEN}/values.merged.yaml")" = '["all"]' ]
}

@test "rook-ceph-csi-kms: no kms value keeps the empty list, which the chart refuses" {
  run addons::render "${KMS_ADDON}" lo "" ""
  [ "${status}" -eq 0 ]
  [ "$(yq -o=json -I=0 '.kms' "${SEEN}/values.merged.yaml")" = '[]' ]
  # The stub renders; the real chart stops at this check (the Go render
  # test proves the error text).
  grep -qF '{{- if not $kms }}' "${SEEN}/chart/templates/rbac.yaml"
  grep -qF 'rook-ceph-csi-kms: no kms option is set.' "${SEEN}/chart/templates/rbac.yaml"
}

@test "rook-ceph-csi-kms: bootstrap::inline_values reads kms under values:" {
  local cy; cy=$(_kms_cluster_yaml '    - rook-ceph-csi-kms:
        dependsOn: [rook-ceph]
        values:
          kms: [vaulttenantsa, block-volume-stats]')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" rook-ceph-csi-kms
  [ "${status}" -eq 0 ]
  [ "$(yq -o=json -I=0 '.kms' <<<"${output}")" = '["vaulttenantsa","block-volume-stats"]' ]
}

@test "rook-ceph-csi-kms: an entry of an earlier release (dependsOn only) gives no values" {
  local cy; cy=$(_kms_cluster_yaml '    - rook-ceph-csi-kms:
        dependsOn: [rook-ceph]')
  run bootstrap::inline_values "test.lok8s.dev" "${cy}" rook-ceph-csi-kms
  [ "${status}" -eq 0 ]
  [ -z "${output}" ]
}
