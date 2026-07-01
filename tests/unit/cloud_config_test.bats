#!/usr/bin/env bats
# cloud_config_test.bats — the Hetzner cloud-config generator's module search
# path (cloud-config::paths / CLOUD_PATH_LIB).
#
# A cluster with a custom cloudInit.path must be able to reference framework-
# shipped cloud.d modules (e.g. ceph-osd) WITHOUT copying them into its own
# tree — the drift trap. Guards:
#   1. a framework cloud.d module resolves from CLOUD_PATH_LIB as a fallback.
#   2. the cluster's OWN module + root config still win / apply.
#   3. the library's ROOT config is NEVER mixed in — a custom path fully owns
#      its base (so e.g. a KubeOne node doesn't inherit the Lo docker base).
#   4. when there is no custom path (CLOUD_PATH == CLOUD_PATH_LIB) each module
#      is emitted exactly once (no double-emit).

setup() {
  load "../test_helper"
  setup_tmpdir

  # the real framework module library (ships cloud.d/ceph-osd)
  LIB="${_PROJECT_ROOT}/.lok8s/providers/hetzner/cloud-init"

  # a cluster cloud-init: its OWN `node` module + root packages, but NO ceph-osd
  CL="${BATS_TEST_TMPDIR}/cluster"
  mkdir -p "${CL}/cloud.d/node/write_files/etc/lok8s"
  echo "cluster-node-marker" >"${CL}/cloud.d/node/write_files/etc/lok8s/node.conf"
  printf 'cluster-only-pkg\n' >"${CL}/packages"

  PUB="${BATS_TEST_TMPDIR}/nopub"; mkdir -p "${PUB}"

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/cloud-config"
}

@test "custom cloudInit.path resolves a framework cloud.d module (ceph-osd) via CLOUD_PATH_LIB" {
  export CLOUD_PATH="${CL}" CLOUD_PATH_LIB="${LIB}" CLOUD_PATHD="node:ceph-osd" \
    CLOUD_USER=root CLOUD_PATH_PUB="${PUB}"
  run cloud-config::generate
  assert_success
  assert_output --partial "ceph-osd-partition.sh"   # framework library module
  assert_output --partial "/etc/lok8s/node.conf"    # cluster's own module
  assert_output --partial "cluster-only-pkg"        # cluster root packages
  assert_output --partial 'growpart'                # ceph-osd ⇒ growpart: off
}

@test "the framework library ROOT config is NOT mixed into a custom path" {
  export CLOUD_PATH="${CL}" CLOUD_PATH_LIB="${LIB}" CLOUD_PATHD="node:ceph-osd" \
    CLOUD_USER=root CLOUD_PATH_PUB="${PUB}"
  run cloud-config::generate
  assert_success
  # daemon.json / docker packages live in the framework ROOT — a custom path
  # owns its base, so they must NOT appear.
  refute_output --partial "/etc/docker/daemon.json"
}

@test "no custom path (CLOUD_PATH == CLOUD_PATH_LIB) emits each module exactly once" {
  export CLOUD_PATH="${LIB}" CLOUD_PATH_LIB="${LIB}" CLOUD_PATHD="ceph-osd" \
    CLOUD_USER=root CLOUD_PATH_PUB="${PUB}"
  run cloud-config::generate
  assert_success
  local n
  n=$(grep -c 'ceph-osd-partition.sh"' <<<"${output}")
  [ "${n}" -eq 1 ]
}

@test "empty CLOUD_PATH_LIB keeps the legacy single-path behaviour" {
  export CLOUD_PATH="${CL}" CLOUD_PATH_LIB="" CLOUD_PATHD="node:ceph-osd" \
    CLOUD_USER=root CLOUD_PATH_PUB="${PUB}"
  run cloud-config::generate
  assert_success
  assert_output --partial "/etc/lok8s/node.conf"        # cluster module still applies
  refute_output --partial "ceph-osd-partition.sh"       # no library ⇒ no fallback
}
