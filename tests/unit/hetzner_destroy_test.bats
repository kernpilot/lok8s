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
    [[ "$1" != "${LIST_FAIL:-}" ]] || { echo "hcloud: server error (service_error)" >&2; return 1; }
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

# One name resolution: the descriptor's cluster_name, which `lo destroy`
# also checks against the other domains. A name from the provision dump or a
# server name is never used.
@test "provider::destroy_plan and provider::destroy refuse a descriptor without cluster_name" {
  printf '{"server": [{"name": "x-cp-0-a", "type": "cx43"}]}\n' > "${DESC}"
  printf '{"cluster_name": "from-dump"}\n' > "${WORK}/hetzner.dump.json"
  run provider::destroy_plan "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: the descriptor ${DESC} has no cluster_name. The destroy deletes every resource that carries lok8s.dev/cluster=<cluster_name>, so it needs the name. Nothing was deleted."
  refute_output --partial "cluster	"
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  run cat "${HCLOUD_LOG}"
  assert_output ""
}

# A failed list must stop the plan: an empty list reads as "no resource".
@test "provider::destroy_plan and provider::destroy stop when a list fails" {
  export LIST_FAIL=volume
  run provider::destroy_plan "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: could not list the volume resources with lok8s.dev/cluster=prod-a. Nothing was deleted."
  refute_output --partial "cluster	prod-a"
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

# The confirmed plan of `lo destroy` (LOK8S_DESTROY_PLAN): delete exactly
# those resources. A resource that is new since the confirmation stays.
@test "provider::destroy deletes exactly the confirmed plan and keeps a new resource" {
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nload-balancer\t11\tapi-lb\nserver\t42\tcp-0\nvolume\t7\tdata-0\nnetwork\t9\tnet'
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output --partial "hetzner: kept server 43 cp-1 (lok8s.dev/cluster=prod-a): it is not in the confirmed plan."
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "load-balancer delete 11
server delete 42
volume delete 7
network delete 9"
}

@test "provider::destroy refuses a confirmed plan of another cluster_name" {
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-b\nserver\t42\tcp-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: the confirmed destroy plan is for cluster_name 'prod-b', but the descriptor has 'prod-a'. Nothing was deleted."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

# ── review round 2 (#299): the confirmed plan comes from the environment ──
# provider::destroy deletes only plan ∩ the live list labelled with the
# cluster, and refuses a plan line lo destroy does not write.

@test "provider::destroy refuses a confirmed plan with an unknown resource type, and deletes nothing" {
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0\ncertificate\t5\tapi-cert'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: the confirmed destroy plan has a line that lo destroy does not write: 'certificate 5 api-cert' (the type must be one of load-balancer server placement-group firewall volume floating-ip ssh-key network, the id a number). Nothing was deleted."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy refuses a confirmed plan with an id that is not a number, and deletes nothing" {
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0\nvolume\tdata-0\tdata-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "the confirmed destroy plan has a line that lo destroy does not write: 'volume data-0 data-0'"
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy skips a plan id that is not in the labelled list, and deletes the rest" {
  # 77 is a server without the label (or of another cluster): never deleted.
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0\nserver\t77\tforeign-0\nvolume\t7\tdata-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output --partial "hetzner: skipped server 77 of the confirmed plan: no server with this id carries lok8s.dev/cluster=prod-a now. It is gone, or it is not of this cluster."
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42
volume delete 7"
}

@test "provider::destroy with a confirmed plan stops when the list fails, and deletes nothing" {
  export LIST_FAIL=network
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: could not list the network resources with lok8s.dev/cluster=prod-a. Nothing was deleted."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy refuses a confirmed plan without its cluster line" {
  export LOK8S_DESTROY_PLAN=$'server\t42\tcp-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: the confirmed destroy plan does not start with its cluster line (it starts with 'server 42 cp-0'). Nothing was deleted."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

# ── review round 3 (#299): edge cases of the confirmed plan ──────────────────

@test "provider::destroy refuses a plan line whose type only contains an allowed type, and deletes nothing" {
  # The type is one tab-separated field: "load-balancer server" is no type.
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nload-balancer server\t42\tcp-0\nvolume\t7\tdata-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "the confirmed destroy plan has a line that lo destroy does not write: 'load-balancer server 42 cp-0'"
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy refuses an empty plan" {
  export LOK8S_DESTROY_PLAN=$'\n\n'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: the confirmed destroy plan is empty. Nothing was deleted. Run lo destroy again."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy reads a plan with CRLF line ends" {
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\r\nserver\t42\tcp-0\r\n'
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

@test "provider::destroy refuses a double tab, a leading zero and a second cluster line" {
  local plan
  for plan in $'cluster\tprod-a\nvolume\t\t7' $'cluster\tprod-a\nserver\t042\tcp-0' $'cluster\tprod-a\ncluster\tprod-b\nserver\t42'; do
    : > "${HCLOUD_LOG}"
    export LOK8S_DESTROY_PLAN="${plan}"
    run provider::destroy "${DESC}" "${WORK}"
    assert_failure
    assert_output --partial "the confirmed destroy plan has a line that lo destroy does not write"
    run grep -c ' delete ' "${HCLOUD_LOG}"
    assert_output "0"
  done
}

@test "provider::destroy keeps every live resource outside the plan, with a warning each" {
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0\nvolume\t7\tdata-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output --partial "kept load-balancer 11 api-lb"
  assert_output --partial "kept server 43 cp-1"
  assert_output --partial "kept network 9 net"
  refute_output --partial "kept server 42"
  refute_output --partial "kept volume 7"
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42
volume delete 7"
}

@test "provider::destroy stops when hcloud answers a list with no JSON" {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then echo 'not json'; else echo '[]'; fi
  }
  export -f hcloud
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: could not list the server resources with lok8s.dev/cluster=prod-a. Nothing was deleted."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}
