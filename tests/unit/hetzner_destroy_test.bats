#!/usr/bin/env bats
# hetzner_destroy_test.bats — provider::destroy and the READ-ONLY
# provider::destroy_plan of the hetzner provider. Both find the resources
# the same way (hetzner::_destroy_cluster_name, hetzner::_destroy_resources):
# every hcloud resource in the token's project that carries
# lok8s.dev/cluster=<cluster_name>. `lo destroy` shows the plan and guards on
# its cluster_name (destroy_guard_test.bats). hcloud is a recording fake that
# answers `<resource> list -o json` in the API shape (id, name).

setup() {
  load "../test_helper"
  setup_tmpdir

  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  export HCLOUD_LOG="${BATS_TEST_TMPDIR}/hcloud.log"
  : > "${HCLOUD_LOG}"
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    case "$1" in
      load-balancer) echo '[{"id":11,"name":"api-lb","labels":{"lok8s.dev/cluster":"prod-a"}}]' ;;
      server)        echo '[{"id":42,"name":"cp-0"},{"id":43,"name":"cp-1"}]' ;;
      volume)        echo '[{"id":7,"name":"data-0"}]' ;;
      network)       echo '[{"id":9,"name":"net"}]' ;;
      *)             echo '[]' ;;
    esac
  }
  export -f hcloud

  DESC="${BATS_TEST_TMPDIR}/clusters/prod.cloud/hetzner.json"
  WORK="${BATS_TEST_TMPDIR}/clusters/prod.cloud/.kubeone"
  mkdir -p "${DESC%/*}" "${WORK}"
  printf '{"cluster_name": "prod-a", "server": [{"name": "cp-0", "type": "cx43"}]}\n' > "${DESC}"

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
}

teardown() { teardown_tmpdir; }

@test "provider::destroy_plan lists every labelled resource and deletes nothing" {
  run provider::destroy_plan "${DESC}" "${WORK}"
  assert_success
  assert_output "$(printf 'cluster\tprod-a\nload-balancer\t11\tapi-lb\nserver\t42\tcp-0\nserver\t43\tcp-1\nvolume\t7\tdata-0\nnetwork\t9\tnet')"
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
  # Every list is scoped to the cluster label.
  run grep -vc -- '-l lok8s.dev/cluster=prod-a$' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy deletes exactly the resources of the plan, in its order" {
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "load-balancer delete 11
server delete 42
server delete 43
volume delete 7
network delete 9"
}

@test "provider::destroy_plan takes the cluster name from the provision dump, as destroy does" {
  printf '{"server": [{"name": "x-cp-0-a", "type": "cx43"}]}\n' > "${DESC}"
  printf '{"cluster_name": "from-dump"}\n' > "${WORK}/hetzner.dump.json"
  run provider::destroy_plan "${DESC}" "${WORK}"
  assert_success
  assert_line --index 0 "$(printf 'cluster\tfrom-dump')"
  run grep -c -- '-l lok8s.dev/cluster=from-dump$' "${HCLOUD_LOG}"
  assert_output "8"
}

@test "provider::destroy_plan fails without a cluster name, and lists nothing" {
  printf '{}\n' > "${DESC}"
  run provider::destroy_plan "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "cannot determine cluster name for destroy"
  refute_output --partial "cluster	"
  run cat "${HCLOUD_LOG}"
  assert_output ""
}
