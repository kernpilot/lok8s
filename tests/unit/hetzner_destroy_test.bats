#!/usr/bin/env bats
# hetzner_destroy_test.bats — provider::destroy and the READ-ONLY
# provider::destroy_plan of the hetzner provider. Both find the resources
# the same way (hetzner::_destroy_cluster_name, hetzner::_destroy_resources):
# every hcloud resource in the token's project that carries
# lok8s.dev/cluster=<cluster_name> and the owner mark of this cluster.
# `lo destroy` shows the plan and guards on its cluster_name
# (destroy_guard_test.bats). hcloud is a recording fake that answers
# `<resource> list -o json` in the API shape (id, name, labels); each part
# has the mark. The parts without it are in hetzner_owner_test.bats.

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
  # The domain lo exports (provider::write_config) and the owner mark of
  # prod-a in it: sha256("<domain>\n<cluster_name>\n"), the first 32 hex chars.
  export PROVIDER_DOMAIN="prod.cloud"
  OWN="{\"lok8s.dev/cluster\":\"prod-a\",\"lok8s.dev/owner\":\"$(printf '%s\n%s\n' "${PROVIDER_DOMAIN}" prod-a | sha256sum | cut -c1-32)\"}"
  export OWN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    [[ "$1" != "${LIST_FAIL:-}" ]] || { echo "hcloud: server error (service_error)" >&2; return 1; }
    case "$1" in
      load-balancer) echo "[{\"id\":11,\"name\":\"api-lb\",\"labels\":${OWN}}]" ;;
      server)        echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":43,\"name\":\"cp-1\",\"labels\":${OWN}}]" ;;
      volume)        echo "[{\"id\":7,\"name\":\"data-0\",\"labels\":${OWN}}]" ;;
      network)       echo "[{\"id\":9,\"name\":\"net\",\"labels\":${OWN}}]" ;;
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
  # Each part has the mark: the destroy kept none, so no summary line.
  refute_output --partial "lo destroy: kept"
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
  assert_output --partial "hetzner: the descriptor ${DESC} has no cluster_name. The destroy finds its resources by lok8s.dev/cluster=<cluster_name> and the owner mark, so it needs the name. The hetzner provider deleted no resource in this step."
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
  assert_output --partial "hetzner: could not list the volume resources with lok8s.dev/cluster=prod-a. The hetzner provider deleted no resource in this step."
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
  assert_output --partial "hetzner: the confirmed destroy plan is for cluster_name 'prod-b', but the descriptor has 'prod-a'. The hetzner provider deleted no resource in this step."
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
  assert_output --partial "hetzner: the confirmed destroy plan has a line that lo destroy does not write: 'certificate 5 api-cert' (the type must be one of load-balancer server placement-group firewall volume floating-ip ssh-key network, the id a number). The hetzner provider deleted no resource in this step."
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
  assert_output --partial "hetzner: skipped server 77 of the confirmed plan: no server with this id carries lok8s.dev/cluster=prod-a and the owner mark of this cluster now. It is gone, or it is not of this cluster."
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42
volume delete 7"
}

@test "provider::destroy with a confirmed plan stops when the list fails, and deletes nothing" {
  export LIST_FAIL=network
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: could not list the network resources with lok8s.dev/cluster=prod-a. The hetzner provider deleted no resource in this step."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

@test "provider::destroy refuses a confirmed plan without its cluster line" {
  export LOK8S_DESTROY_PLAN=$'server\t42\tcp-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_failure
  assert_output --partial "hetzner: the confirmed destroy plan does not start with its cluster line (it starts with 'server 42 cp-0'). The hetzner provider deleted no resource in this step."
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
  assert_output --partial "hetzner: the confirmed destroy plan is empty. The hetzner provider deleted no resource in this step. Run lo destroy again."
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
  assert_output --partial "hetzner: could not list the server resources with lok8s.dev/cluster=prod-a. The hetzner provider deleted no resource in this step."
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

# The list query selects lok8s.dev/cluster=<cluster_name>, but the destroy
# does not trust it alone: each part's cluster label is checked again. This
# fake gives two foreign parts in the server list (a list that does not
# apply its selector): one with another cluster label and one with none,
# both with the owner mark of this cluster. Neither is in the plan or
# deleted, also when a confirmed plan names them. A warning names each, and
# so do the last lines of the destroy, with the reason. The confirmed plan
# below is made by hand (no label changes): with a plan that names them,
# the reason says only what the provider knows, that the plan named them,
# and the warning that the list gave them comes again (the plan did not
# show it for a part that it names).
@test "destroy: a listed part without the cluster label of this cluster is never deleted, also with the owner mark" {
  local mark
  mark="$(printf '%s\n%s\n' "${PROVIDER_DOMAIN}" prod-a | sha256sum | cut -c1-32)"
  export FOREIGN="[{\"id\":44,\"name\":\"other-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-b\",\"lok8s.dev/owner\":\"${mark}\"}},{\"id\":45,\"name\":\"bare-0\",\"labels\":{\"lok8s.dev/owner\":\"${mark}\"}}]"
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      jq -c --argjson f "${FOREIGN}" '. + $f' <<<"[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}}]"
    else
      echo '[]'
    fi
  }
  export -f hcloud

  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/plan.err")"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t42\tcp-0')" ]
  run cat "${BATS_TEST_TMPDIR}/plan.err"
  assert_output --partial "hetzner: skipped server 44 other-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b. lok8s does not delete a part of another cluster."
  assert_output --partial "hetzner: skipped server 45 bare-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=<none>."
  # The part of this cluster is not a foreign part.
  refute_output --partial "skipped server 42"

  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output --partial "hetzner: skipped server 44 other-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b."
  assert_output --partial "hetzner: skipped server 45 bare-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=<none>."
  refute_output --partial "skipped server 42"
  assert_output --partial "[warn] lo destroy: skipped server 44 other-0: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it."
  assert_output --partial "[warn] lo destroy: skipped server 45 bare-0: it has lok8s.dev/cluster=<none>, not lok8s.dev/cluster=prod-a. lok8s did not delete it."
  # Not a kept part of this cluster: the summary does not count it.
  refute_output --partial "lo destroy: kept"
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"

  : > "${HCLOUD_LOG}"
  export LOK8S_DESTROY_PLAN=$'cluster\tprod-a\nserver\t42\tcp-0\nserver\t44\tother-0\nserver\t45\tbare-0'
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_line "[warn] lo destroy: skipped server 44 other-0: the plan named it, and now it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it."
  assert_line "[warn] lo destroy: skipped server 45 bare-0: the plan named it, and now it has lok8s.dev/cluster=<none>, not lok8s.dev/cluster=prod-a. lok8s did not delete it."
  # The plan named them, so it showed no warning for them: the list warns
  # once for each that it gave a part outside its selector.
  [ "$(grep -c 'hetzner: skipped server 44 other-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b.' <<<"${output}")" -eq 1 ]
  [ "$(grep -c 'hetzner: skipped server 45 bare-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=<none>.' <<<"${output}")" -eq 1 ]
  # Not also the line of a plan row that is not live.
  refute_output --partial "skipped server 44 of the confirmed plan"
  refute_output --partial "skipped server 45 of the confirmed plan"
  refute_output --partial "skipped server 42"
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# The plan names part 46 with the cluster label of this cluster, and at the
# destroy the list gives it with another one. This fake does not apply the
# selector (a real hcloud list leaves such a part out: see the next test).
# The destroy does not delete it, and the last lines name it with the
# reason, before the line that counts and names the kept parts (47 has no
# owner mark), which stays the last line.
@test "destroy: a part of the plan that the list gives with another cluster label is named in the last lines, and the kept line stays last" {
  unset LOK8S_DESTROY_PLAN
  MARK="$(printf '%s\n%s\n' "${PROVIDER_DOMAIN}" prod-a | sha256sum | cut -c1-32)"
  export MARK
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    local label=prod-a
    [[ -z "${LOK8S_DESTROY_PLAN:-}" ]] || label=prod-b
    if [[ "$1" == server ]]; then
      echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":46,\"name\":\"moved-0\",\"labels\":{\"lok8s.dev/cluster\":\"${label}\",\"lok8s.dev/owner\":\"${MARK}\"}},{\"id\":47,\"name\":\"old-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}]"
    else
      echo '[]'
    fi
  }
  export -f hcloud

  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}" 2>/dev/null)"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t42\tcp-0\nserver\t46\tmoved-0')" ]

  : > "${HCLOUD_LOG}"
  export LOK8S_DESTROY_PLAN="${plan}"
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  [ "${lines[${#lines[@]}-2]}" = "[warn] lo destroy: skipped server 46 moved-0: the plan named it, and now it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it." ]
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod-a that does not have the owner mark of this cluster (1 server): server 47 'old-0'. Delete it by hand only if it is yours and you want it gone." ]
  # Two lines name part 46: the warning that the list gave it, and the
  # skipped line. Not the vague line of a plan row that is not live.
  [ "$(grep -c 'server 46' <<<"${output}")" -eq 2 ]
  [ "$(grep -c 'hetzner: skipped server 46 moved-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b.' <<<"${output}")" -eq 1 ]
  refute_output --partial "skipped server 46 of the confirmed plan"
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# The common case: the list applies its selector, as the Hetzner API does.
# The plan names part 46, and at the destroy 46 has another cluster label,
# so the list does not give it at all. The destroy does not delete it; the
# warning of a plan row that is not live names it, and no skipped line
# does (the label check sees only the parts of the list).
@test "destroy: a part of the plan whose label changed is left out by a list that applies its selector" {
  unset LOK8S_DESTROY_PLAN
  MARK="$(printf '%s\n%s\n' "${PROVIDER_DOMAIN}" prod-a | sha256sum | cut -c1-32)"
  export MARK
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    # The selector is the last argument: -l lok8s.dev/cluster=<name>.
    local label=prod-a sel="${*: -1}"
    [[ -z "${LOK8S_DESTROY_PLAN:-}" ]] || label=prod-b
    if [[ "$1" == server ]]; then
      jq -c --arg c "${sel#lok8s.dev/cluster=}" '[.[] | select(.labels["lok8s.dev/cluster"] == $c)]' \
        <<<"[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":46,\"name\":\"moved-0\",\"labels\":{\"lok8s.dev/cluster\":\"${label}\",\"lok8s.dev/owner\":\"${MARK}\"}}]"
    else
      echo '[]'
    fi
  }
  export -f hcloud

  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}" 2>/dev/null)"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t42\tcp-0\nserver\t46\tmoved-0')" ]

  : > "${HCLOUD_LOG}"
  export LOK8S_DESTROY_PLAN="${plan}"
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output "[warn] hetzner: skipped server 46 of the confirmed plan: no server with this id carries lok8s.dev/cluster=prod-a and the owner mark of this cluster now. It is gone, or it is not of this cluster."
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# A part without a name: its skipped line has the kind, the id and the
# reason, and no empty field shifts the reason into the name.
@test "destroy: the skipped line of a part without a name keeps its reason" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":48,\"name\":\"\",\"labels\":{\"lok8s.dev/cluster\":\"prod-b\"}}]"
    else
      echo '[]'
    fi
  }
  export -f hcloud

  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: skipped server 48: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it." ]
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# Two destroys in one shell: each starts with empty lists, so the second
# names each skipped part once, as the first does, and counts the same
# kept parts.
@test "destroy: a second destroy in the same shell does not repeat the skipped parts of the first" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":44,\"name\":\"other-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-b\"}},{\"id\":47,\"name\":\"old-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}]"
    else
      echo '[]'
    fi
  }
  export -f hcloud

  # Not `run`: a subshell would drop the globals of the first destroy.
  provider::destroy "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/first.err"
  provider::destroy "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/second.err"
  run tail -n 2 "${BATS_TEST_TMPDIR}/second.err"
  assert_output "[warn] lo destroy: skipped server 44 other-0: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it.
[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod-a that does not have the owner mark of this cluster (1 server): server 47 'old-0'. Delete it by hand only if it is yours and you want it gone."
  run cat "${BATS_TEST_TMPDIR}/second.err"
  assert_output "$(cat "${BATS_TEST_TMPDIR}/first.err")"
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "2"
}

# The list gives a foreign part (another cluster label: a list that does
# not apply its selector) at the plan and at the destroy. The plan warns
# for it and does not name it. The destroy of that plan does not repeat
# the warning: its only line for the part is the skipped line.
@test "destroy: a confirmed plan that leaves out a foreign part gets no second warning for it, only the skipped line" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":44,\"name\":\"other-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-b\"}}]"
    else
      echo '[]'
    fi
  }
  export -f hcloud

  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/plan.err")"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t42\tcp-0')" ]
  run cat "${BATS_TEST_TMPDIR}/plan.err"
  assert_output "[warn] hetzner: skipped server 44 other-0: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b. lok8s does not delete a part of another cluster."

  : > "${HCLOUD_LOG}"
  export LOK8S_DESTROY_PLAN="${plan}"
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output "[warn] lo destroy: skipped server 44 other-0: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it."
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# The plan sees part 42 of this cluster and the kept part 47 (no owner
# mark). After the plan, a foreign part 49, a kept part 50, a kept network
# 51 without a name and a kept load balancer 53 appear (the list does not
# apply its selector, so it gives 49). None of them gets a 'hetzner:
# skipped' warning: the plan did not see them, and the destroy does not
# warn for a part that the plan does not name. The last lines name them all
# the same: 49 in a skipped line, the others in the kept line, in the
# delete order. The load balancer has the highest id and comes first: a
# sort by id would put it last.
@test "destroy: a foreign part and kept parts that appear after the plan are named in the last lines" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    local now=""
    [[ -z "${LOK8S_DESTROY_PLAN:-}" ]] || now=1
    case "$1" in
      server)
        if [[ -z "${now}" ]]; then
          echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":47,\"name\":\"old-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}]"
        else
          echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":47,\"name\":\"old-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}},{\"id\":49,\"name\":\"new-foreign\",\"labels\":{\"lok8s.dev/cluster\":\"prod-b\"}},{\"id\":50,\"name\":\"new-kept\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}]"
        fi ;;
      network)
        if [[ -z "${now}" ]]; then echo '[]'; else echo '[{"id":51,"name":"","labels":{"lok8s.dev/cluster":"prod-a"}}]'; fi ;;
      load-balancer)
        if [[ -z "${now}" ]]; then echo '[]'; else echo '[{"id":53,"name":"new-lb","labels":{"lok8s.dev/cluster":"prod-a"}}]'; fi ;;
      *) echo '[]' ;;
    esac
  }
  export -f hcloud

  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/plan.err")"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t42\tcp-0')" ]
  run cat "${BATS_TEST_TMPDIR}/plan.err"
  assert_output --partial "hetzner: skipped server 47 old-0: it has lok8s.dev/cluster=prod-a, but not the owner mark of this cluster"
  refute_output --partial " 49 "
  refute_output --partial " 50 "
  refute_output --partial " 51"
  refute_output --partial " 53 "

  : > "${HCLOUD_LOG}"
  export LOK8S_DESTROY_PLAN="${plan}"
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output "[warn] lo destroy: skipped server 49 new-foreign: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it.
[warn] lo destroy: kept 4 resources with lok8s.dev/cluster=prod-a that do not have the owner mark of this cluster (1 load-balancer, 2 servers, 1 network): load-balancer 53 'new-lb', server 47 'old-0', server 50 'new-kept', network 51. Delete them by hand only if they are yours and you want them gone."
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# Two destroys in one shell with different lists. Each destroy starts with
# empty lists, so the last lines of the second name only the parts of the
# second list. No part of the first list comes back.
@test "destroy: a second destroy in the same shell with another list names only the parts of its own list" {
  unset LOK8S_DESTROY_PLAN
  export LIST_RUN=first
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" != server ]]; then
      echo '[]'
    elif [[ "${LIST_RUN}" == first ]]; then
      echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":44,\"name\":\"other-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-b\"}},{\"id\":47,\"name\":\"old-0\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}]"
    else
      echo "[{\"id\":48,\"name\":\"other-1\",\"labels\":{\"lok8s.dev/cluster\":\"prod-c\"}},{\"id\":52,\"name\":\"old-1\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}]"
    fi
  }
  export -f hcloud

  # Not `run`: a subshell would drop the globals of the first destroy.
  provider::destroy "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/first.err"
  run tail -n 2 "${BATS_TEST_TMPDIR}/first.err"
  assert_output "[warn] lo destroy: skipped server 44 other-0: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it.
[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod-a that does not have the owner mark of this cluster (1 server): server 47 'old-0'. Delete it by hand only if it is yours and you want it gone."

  LIST_RUN=second
  provider::destroy "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/second.err"
  run tail -n 2 "${BATS_TEST_TMPDIR}/second.err"
  assert_output "[warn] lo destroy: skipped server 48 other-1: it has lok8s.dev/cluster=prod-c, not lok8s.dev/cluster=prod-a. lok8s did not delete it.
[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod-a that does not have the owner mark of this cluster (1 server): server 52 'old-1'. Delete it by hand only if it is yours and you want it gone."
  run cat "${BATS_TEST_TMPDIR}/second.err"
  [ "$(grep -c 'lo destroy: skipped' <<<"${output}")" -eq 1 ]
  refute_output --partial "server 44"
  refute_output --partial "server 47"
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "1"
}

# A name can hold any character. When warn printed with `echo -e`, a
# newline in a name split the line, an ESC reached the terminal, and a
# backslash sequence became a control character. Each control character
# and each backslash of a name now shows as '?': each message is one line.
# Part 60 has a newline in its name, part 61 a backslash and an 'n', part
# 62 an ESC, a CR and a tab.
@test "destroy: a control character or a backslash in a name shows as '?', one line for each message" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      printf '%s\n' "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},"'{"id":60,"name":"a\nb","labels":{"lok8s.dev/cluster":"prod-a"}},{"id":61,"name":"a\\nb","labels":{"lok8s.dev/cluster":"prod-a"}},{"id":62,"name":"x\u001b[2Ky\r\tz","labels":{"lok8s.dev/cluster":"prod-b"}}]'
    else
      echo '[]'
    fi
  }
  export -f hcloud

  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  # Five messages, five lines: three warnings of the list, the skipped
  # line and the kept line. No control character is in the output.
  [ "$(printf '%s\n' "${output}" | wc -l)" -eq 5 ]
  [ "$(grep -c '^\[warn\] ' <<<"${output}")" -eq 5 ]
  [ -z "$(tr -d '[:print:]\n' <<<"${output}")" ]
  assert_line "[warn] hetzner: skipped server 62 x?[2Ky??z: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b. lok8s does not delete a part of another cluster."
  assert_line --partial "[warn] hetzner: skipped server 60 a?b: it has lok8s.dev/cluster=prod-a, but not the owner mark of this cluster"
  assert_line --partial "[warn] hetzner: skipped server 61 a?nb: it has lok8s.dev/cluster=prod-a, but not the owner mark of this cluster"
  [ "${lines[3]}" = "[warn] lo destroy: skipped server 62 x?[2Ky??z: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it." ]
  [ "${lines[4]}" = "[warn] lo destroy: kept 2 resources with lok8s.dev/cluster=prod-a that do not have the owner mark of this cluster (2 servers): server 60 'a?b', server 61 'a?nb'. Delete them by hand only if they are yours and you want them gone." ]
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}

# The kept line names at most 10 parts, in the delete order, and then
# counts the others. The count by kind includes all of them.
@test "destroy: the kept line names at most 10 parts, then the number of the others" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    local i json=""
    if [[ "$1" == server ]]; then
      for (( i = 0; i < KEPT_N; i++ )); do
        json+="${json:+,}{\"id\":$(( 70 + i )),\"name\":\"old-${i}\",\"labels\":{\"lok8s.dev/cluster\":\"prod-a\"}}"
      done
    fi
    printf '[%s]\n' "${json}"
  }
  export -f hcloud

  export KEPT_N=10
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 10 resources with lok8s.dev/cluster=prod-a that do not have the owner mark of this cluster (10 servers): server 70 'old-0', server 71 'old-1', server 72 'old-2', server 73 'old-3', server 74 'old-4', server 75 'old-5', server 76 'old-6', server 77 'old-7', server 78 'old-8', server 79 'old-9'. Delete them by hand only if they are yours and you want them gone." ]

  export KEPT_N=12
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 12 resources with lok8s.dev/cluster=prod-a that do not have the owner mark of this cluster (12 servers): server 70 'old-0', server 71 'old-1', server 72 'old-2', server 73 'old-3', server 74 'old-4', server 75 'old-5', server 76 'old-6', server 77 'old-7', server 78 'old-8', server 79 'old-9', and 2 more. Delete them by hand only if they are yours and you want them gone." ]
  run grep -c ' delete ' "${HCLOUD_LOG}"
  assert_output "0"
}

# The plan and each message of the destroy show a name through shown_name
# (utils/verbose.sh): each control character (C0, DEL, C1), each backslash
# and each bidi or zero-width character is '?', and a multibyte UTF-8 name
# stays. Parts 70 to 74 have this cluster's mark (the plan lists them);
# parts 80 to 82 have the cluster label and no mark (kept); part 83 has
# another cluster label (skipped). The deletes go by id.
@test "destroy: the plan and each message show a name safely, and a multibyte name stays" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      printf '%s\n' "[{\"id\":70,\"name\":\"ö-測試\",\"labels\":${OWN}},{\"id\":71,\"name\":\"a\\u007fb\\u0085c\",\"labels\":${OWN}},{\"id\":72,\"name\":\"\\\\\",\"labels\":${OWN}},{\"id\":73,\"name\":\"x\\u202ey\\u200bz\",\"labels\":${OWN}},{\"id\":74,\"name\":\"e\\u001b[31mvil\",\"labels\":${OWN}},"'{"id":80,"name":"ö-測試","labels":{"lok8s.dev/cluster":"prod-a"}},{"id":81,"name":"\\","labels":{"lok8s.dev/cluster":"prod-a"}},{"id":82,"name":"a\u007fb\u0085c","labels":{"lok8s.dev/cluster":"prod-a"}},{"id":83,"name":"x\u202ey\u200bz","labels":{"lok8s.dev/cluster":"prod-b"}}]'
    else
      echo '[]'
    fi
  }
  export -f hcloud

  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}" 2>"${BATS_TEST_TMPDIR}/plan.err")"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t70\tö-測試\nserver\t71\ta?b?c\nserver\t72\t?\nserver\t73\tx?y?z\nserver\t74\te?[31mvil')" ]
  run cat "${BATS_TEST_TMPDIR}/plan.err"
  [ "${#lines[@]}" -eq 4 ]
  assert_line "[warn] hetzner: skipped server 83 x?y?z: the list of the parts with lok8s.dev/cluster=prod-a gave it, but it has lok8s.dev/cluster=prod-b. lok8s does not delete a part of another cluster."
  assert_line --partial "[warn] hetzner: skipped server 80 ö-測試: it has lok8s.dev/cluster=prod-a, but not the owner mark of this cluster"
  assert_line --partial "[warn] hetzner: skipped server 81 ?: it has lok8s.dev/cluster=prod-a, but not the owner mark of this cluster"
  assert_line --partial "[warn] hetzner: skipped server 82 a?b?c: it has lok8s.dev/cluster=prod-a, but not the owner mark of this cluster"

  : > "${HCLOUD_LOG}"
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  [ "${lines[${#lines[@]}-2]}" = "[warn] lo destroy: skipped server 83 x?y?z: it has lok8s.dev/cluster=prod-b, not lok8s.dev/cluster=prod-a. lok8s did not delete it." ]
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 3 resources with lok8s.dev/cluster=prod-a that do not have the owner mark of this cluster (3 servers): server 80 'ö-測試', server 81 '?', server 82 'a?b?c'. Delete them by hand only if they are yours and you want them gone." ]
  refute_output --partial $'\xe2\x80\xae'
  refute_output --partial $'\xe2\x80\x8b'
  refute_output --partial $'\x7f'
  refute_output --partial $'\xc2\x85'
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 70
server delete 71
server delete 72
server delete 73
server delete 74"
}

# The plan saw part 42. After it, part 90 of this cluster (with the mark)
# appears, with an ESC sequence and a newline in its name. The destroy with
# the confirmed plan keeps it and warns: one line, the ESC and the newline
# as '?'. Before shown_name, @tsv gave the newline as '\n', and the
# `echo -e` of warn turned it back into a newline.
@test "destroy: with a confirmed plan, the warning for a kept part that is not in the plan shows its name on one line" {
  unset LOK8S_DESTROY_PLAN
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    [[ "$2" == "list" ]] || return 0
    if [[ "$1" == server ]]; then
      local now=""
      [[ ! -f "${BATS_TEST_TMPDIR}/after-plan" ]] || now=1
      if [[ -z "${now}" ]]; then
        echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}}]"
      else
        echo "[{\"id\":42,\"name\":\"cp-0\",\"labels\":${OWN}},{\"id\":90,\"name\":\"e\\u001b[31mvil\\nX\",\"labels\":${OWN}}]"
      fi
    else
      echo '[]'
    fi
  }
  export -f hcloud
  local plan
  plan="$(provider::destroy_plan "${DESC}" "${WORK}")"
  [ "${plan}" = "$(printf 'cluster\tprod-a\nserver\t42\tcp-0')" ]

  touch "${BATS_TEST_TMPDIR}/after-plan"
  : > "${HCLOUD_LOG}"
  export LOK8S_DESTROY_PLAN="${plan}"
  run provider::destroy "${DESC}" "${WORK}"
  assert_success
  assert_output "[warn] hetzner: kept server 90 e?[31mvil?X (lok8s.dev/cluster=prod-a): it is not in the confirmed plan. Delete it by hand, or run lo destroy again."
  [ -z "$(tr -d '[:print:]\n' <<<"${output}")" ]
  run grep ' delete ' "${HCLOUD_LOG}"
  assert_output "server delete 42"
}
