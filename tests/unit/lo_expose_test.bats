#!/usr/bin/env bats
# lo_expose_test.bats: the image of the `lo expose` proxy. lo::expose
# (.lok8s/drivers/lo/utils/expose.sh) starts the proxy container from
# LO_EXPOSE_IMAGE (.lok8s/drivers/lo/utils/defaults.sh), which is pinned by
# tag and digest. Go twin: TestExposeRunsThePinnedImage in
# internal/driver/lo/services_test.go.
#
# No test may reach a real tool. Setup puts failing stubs of the tools first
# on PATH (stub_tools in tests/test_helper.bash), and teardown fails the test
# when one of them ran. docker, yq and envsubst are shell functions in the
# test: a function comes before a program on PATH.

setup() {
  load "../test_helper"
  setup_tmpdir
  stub_tools docker kind kubectl helm ssh scp rsync curl yq envsubst
  import() { :; }
  export -f import
  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/main"
}

# Every test ends with the check that no tool on PATH ran.
teardown() {
  local rc=0
  assert_no_tool_ran || rc=$?
  teardown_tmpdir
  return "${rc}"
}

# The literal is the value of LO_EXPOSE_IMAGE and of ExposeImage (the Go
# twin): the tag of one version and its digest, never a floating tag.
@test "expose: the proxy runs the pinned nginx image, by tag and digest" {
  local log="${BATS_TEST_TMPDIR}/docker.log"
  docker() { printf '%s\n' "$*" >> "${log}"; }
  yq() { echo "test.lok8s.dev"; }
  envsubst() { cat; }
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s LOK8S_LB_POOL="10.125.50.125-10.125.50.150"
  run lo::expose test-dns "${BATS_TEST_TMPDIR}/cluster.lok8s.yaml"
  [ "${status}" -eq 0 ] || { echo "${output}"; return 1; }
  local want="run -d --restart=always --name test-dns-proxy --network lok8s -p 80:80 -p 443:443 nginx:1.30.7-alpine3.24@sha256:caf4e1e032a20f9c3c5e28fc7f25ffd768ad8677b4975c54193075997b3a97e3"
  [ "$(grep -c '^run ' "${log}")" -eq 1 ] || { cat "${log}"; return 1; }
  grep -qxF "${want}" "${log}" || { cat "${log}"; return 1; }
}
