#!/usr/bin/env bats
# image_clean_test.bats — `lo image clean` has the same door as `lo image
# cache`: the cache registry is a Lo-driver feature. A non-Lo domain is
# refused with the driver named, before any docker command; an explicit
# LOK8S_REGISTRY_IP_CACHE skips the gate. The Go port carries the same
# gate (internal/cli/cmd_image.go); hack/parity-loop.sh diffs the bytes.

setup() {
  load "../test_helper"
  setup_tmpdir

  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/verbose.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/domain.sh"
  source "${_PROJECT_ROOT}/.lok8s/libs/image"

  mkdir -p "${PATH_CLUSTERS}/beta.cloud" "${PATH_CLUSTERS}/alpha.dev"
  printf 'kind: KubeOne\nmetadata:\n  name: beta\n' > "${PATH_CLUSTERS}/beta.cloud/cluster.lok8s.yaml"
  printf 'kind: Lo\nmetadata:\n  name: alpha\n' > "${PATH_CLUSTERS}/alpha.dev/cluster.lok8s.yaml"
  DOCKER_CALLS="${BATS_TEST_TMPDIR}/docker.calls"
  export DOCKER_CALLS
  docker() { echo "docker $*" >> "${DOCKER_CALLS}"; return 1; }
  export -f docker
  unset LOK8S_REGISTRY_IP_CACHE
}

teardown() { teardown_tmpdir; }

@test "image::clean refuses a non-Lo domain before any docker command" {
  domain=beta.cloud run image::clean
  [ "$status" -eq 1 ]
  [[ "$output" == *"error: domain 'beta.cloud' uses the 'kubeone' driver — the image cache is a 'lo'-driver (local cluster) feature."* ]]
  [ ! -e "${DOCKER_CALLS}" ]
}

@test "image::clean runs on a Lo domain" {
  domain=alpha.dev run image::clean
  [ "$status" -eq 0 ]
  [[ "$output" == *":: dropping cache registry volume (lok8s-registry-cache)"* ]]
  grep -q "docker rm -f lok8s-registry-cache" "${DOCKER_CALLS}"
}

@test "image::clean skips the gate with LOK8S_REGISTRY_IP_CACHE set" {
  LOK8S_REGISTRY_IP_CACHE=10.0.0.2 domain=beta.cloud run image::clean
  [ "$status" -eq 0 ]
  grep -q "docker rm -f lok8s-registry-cache" "${DOCKER_CALLS}"
}
