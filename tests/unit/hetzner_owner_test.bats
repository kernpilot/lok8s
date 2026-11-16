#!/usr/bin/env bats
# hetzner_owner_test.bats — the owner mark of the Hetzner provider.
#
# lok8s puts lok8s.dev/owner=<mark> on every part it creates. It adopts,
# changes or uses an existing part only when the part has the mark of THIS
# cluster. A part with a descriptor name and no mark, or another cluster's
# mark, can belong to another person or cluster in the same Hetzner project:
# the provision refuses it before it creates or changes anything. The
# inventory (API endpoint, private network, hosts) takes only marked parts,
# and every hook calls hcloud with the verified ids, never with a name.
#
# hcloud is a recording fake with a small store per kind: `create` adds the
# part with the labels of its --label flags, `list` prints the store. No
# real hcloud runs (a stub binary that fails is first on PATH in CI too: the
# function shadows any binary).

setup() {
  load "../test_helper"
  setup_tmpdir

  command -v jq &>/dev/null || skip "jq required"
  command -v yq &>/dev/null || skip "yq required"
  command -v envsubst &>/dev/null || skip "envsubst required"

  export PATH_LOK8S="${_PROJECT_ROOT}/.lok8s"
  export CLOUD_LOG_FILE="${BATS_TEST_TMPDIR}/hetzner.log"
  import() { :; }
  export -f import
  source "${_PROJECT_ROOT}/.lok8s/utils/template.sh"
  source "${_PROJECT_ROOT}/.lok8s/utils/http.sh"

  export HCLOUD_LOG="${BATS_TEST_TMPDIR}/hcloud.log"
  : > "${HCLOUD_LOG}"
  export STORE="${BATS_TEST_TMPDIR}/store"
  mkdir -p "${STORE}"
  export HCLOUD_TOKEN="tok"

  # The domain lo exports (provider::write_config) and the cluster name of
  # the descriptor below. The mark is calculated here, independent of the
  # provider: sha256("<domain>\n<cluster_name>\n"), the first 32 hex chars.
  export PROVIDER_DOMAIN="prod.example.test"
  MARK="$(printf '%s\n%s\n' "${PROVIDER_DOMAIN}" "prod" | sha256sum | cut -c1-32)"
  OTHER="$(printf '%s\n%s\n' "other.example.test" "prod" | sha256sum | cut -c1-32)"
  export MARK OTHER

  _fake_hcloud
  _write_descriptor

  # shellcheck source=/dev/null
  source "${_PROJECT_ROOT}/.lok8s/providers/hetzner/main"
}

teardown() { teardown_tmpdir; }

# ── the fake ─────────────────────────────────────────────

# _part <kind> <id> <name> [<labels-json>] [<extra-json>]: an existing part.
_part() {
  local f="${STORE}/${1}.json"
  [[ -f "${f}" ]] || echo '[]' > "${f}"
  jq -c --argjson id "${2}" --arg n "${3}" --argjson l "${4:-{\}}" --argjson x "${5:-{\}}" \
    '. + [{id: $id, name: $n, labels: $l} + $x]' "${f}" > "${f}.new"
  mv "${f}.new" "${f}"
}

_fake_hcloud() {
  hcloud() {
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    local kind="${1}" verb="${2}"
    shift 2
    local f="${STORE}/${kind}.json"
    [[ -f "${f}" ]] || echo '[]' > "${f}"
    case "${verb}" in
      list) cat "${f}" ;;
      create)
        local name="" labels='{}' id
        while (( $# )); do
          case "${1}" in
            --name) name="${2}"; shift 2 ;;
            --label)
              labels="$(jq -c --arg kv "${2}" '. + ($kv | split(",")
                | map(split("=") | {(.[0]): (.[1:] | join("="))}) | add)' <<<"${labels}")"
              shift 2 ;;
            *) shift ;;
          esac
        done
        id=$(( 100 + $(cat "${STORE}"/*.json | jq -s 'map(length) | add') ))
        _part "${kind}" "${id}" "${name}" "${labels}" ;;
      describe)
        jq -c --arg r "${1}" 'first(.[] | select((.id | tostring) == $r)) // error("not found")' "${f}" ||
          { echo "hcloud: ${kind} not found: ${1}" >&2; return 1; } ;;
      *) return 0 ;;
    esac
  }
  export -f hcloud
}

# One part of every kind the provision makes. cp-0 uses the key, the
# network and the floating IP; the volume and the LB target refer to cp-0.
_write_descriptor() {
  DESC="${BATS_TEST_TMPDIR}/clusters/prod.example.test/hetzner.json"
  mkdir -p "${DESC%/*}"
  printf 'ssh-ed25519 AAAAtest test\n' > "${BATS_TEST_TMPDIR}/id.pub"
  cat > "${DESC}" <<JSON
{
  "cluster_name": "prod",
  "ssh-key": [ { "name": "prod-key", "public-key-from-file": "${BATS_TEST_TMPDIR}/id.pub" } ],
  "floating-ip": [ { "name": "prod-ip", "type": "ipv4", "home-location": "fsn1" } ],
  "network": [ { "name": "prod-net", "ip-range": "10.0.0.0/16",
    "#subnets": [ { "network-zone": "eu-central", "type": "cloud", "ip-range": "10.0.1.0/24" } ] } ],
  "load-balancer": [ { "name": "prod-api", "type": "lb11", "location": "fsn1",
    "label": "lok8s.dev/cluster=prod",
    "#targets": [ { "type": "server", "value": "0" },
                  { "type": "label-selector", "value": "lok8s.dev/cluster=prod,lok8s.dev/role=control-plane" } ],
    "#services": [ { "protocol": "tcp", "listen-port": "6443", "destination-port": "6443" } ] } ],
  "server": [ { "name": "cp-0", "type": "cx23", "image": "ubuntu-24.04", "location": "fsn1",
    "ssh-key": [0], "network": 0, "#floating-ip": 0,
    "label": "lok8s.dev/cluster=prod,lok8s.dev/role=control-plane" } ],
  "volume": [ { "name": "prod-data", "size": 10, "server": 0 } ]
}
JSON
}

# _provision: the steps of provider::provision, in its order.
_provision() {
  CLOUD_HETZNER_JSON="$(template::descriptor_json "${DESC}")" || return 1
  local step
  for step in lookup ssh-key floating-ip network load-balancer server volume; do
    "hetzner::${step}" || { error "hetzner ${step} failed"; return 1; }
  done
}

# _writes: the hcloud calls that change something (all but list/describe).
_writes() {
  grep -vE '^[a-z-]+ (list|describe)( |$)' "${HCLOUD_LOG}" || true
}

# _all_parts <labels-json>: every descriptor part exists with these labels.
_all_parts() {
  _part ssh-key 11 prod-key "${1}"
  _part floating-ip 12 prod-ip "${1}"
  _part network 13 prod-net "${1}" '{"ip_range":"10.0.0.0/16"}'
  _part load-balancer 14 prod-api "${1}"
  _part server 15 cp-0 "${1}"
  _part volume 16 prod-data "${1}"
}

# ── the mark ─────────────────────────────────────────────

@test "owner mark: the first 32 hex chars of sha256(domain, cluster_name), a valid Hetzner label value" {
  run hetzner::owner_mark prod
  assert_success
  assert_output "${MARK}"
  [[ "${output}" =~ ^[0-9a-f]{32}$ ]]
}

@test "owner mark: another domain or another cluster name gives another mark" {
  run hetzner::owner_mark other
  assert_success
  refute_output "${MARK}"
  PROVIDER_DOMAIN="other.example.test" run hetzner::owner_mark prod
  assert_success
  assert_output "${OTHER}"
}

@test "owner mark: no domain or no cluster_name stops the provision before any hcloud call" {
  unset PROVIDER_DOMAIN
  run _provision
  assert_failure
  assert_output --partial "PROVIDER_DOMAIN is empty"
  export PROVIDER_DOMAIN="prod.example.test"
  yq -i -o json 'del(.cluster_name)' "${DESC}"
  run _provision
  assert_failure
  assert_output --partial "the descriptor has no cluster_name"
  run cat "${HCLOUD_LOG}"
  assert_output ""
}

# ── refused: a part with a descriptor name and no mark ───

# _refused <kind> <id> <name>: only this part exists, with the cluster label
# and no owner mark. The provision stops in the lookup and changes nothing.
_refused() {
  _part "${1}" "${2}" "${3}" '{"lok8s.dev/cluster":"prod"}'
  run _provision
  assert_failure
  assert_output --partial "refused ${1} '${3}' (id ${2}): it exists, but it does not have the owner mark of this cluster (lok8s.dev/owner=${MARK})"
  assert_output --partial "hcloud ${1} add-label ${2} lok8s.dev/owner=${MARK}"
  assert_output --partial "Nothing was created or changed"
  run _writes
  assert_output ""
  # The refused id never enters the descriptor, also for a caller that
  # ignores the status of the lookup.
  CLOUD_HETZNER_JSON="$(template::descriptor_json "${DESC}")"
  hetzner::lookup >/dev/null 2>&1 || true
  run jq -c --arg k "${1}" '[.[$k][] | .id // empty]' <<<"${CLOUD_HETZNER_JSON}"
  assert_output "[]"
}

@test "refused: a server with the descriptor name and no owner mark" {
  _refused server 15 cp-0
}

@test "refused: an ssh-key with the descriptor name and no owner mark" {
  _refused ssh-key 11 prod-key
}

@test "refused: a network with the descriptor name and no owner mark" {
  _refused network 13 prod-net
}

@test "refused: a load-balancer with the descriptor name and no owner mark" {
  _refused load-balancer 14 prod-api
}

@test "refused: a floating-ip with the descriptor name and no owner mark" {
  _refused floating-ip 12 prod-ip
}

@test "refused: a volume with the descriptor name and no owner mark" {
  _refused volume 16 prod-data
}

@test "refused: a part with the owner mark of another cluster" {
  _part network 13 prod-net "{\"lok8s.dev/owner\":\"${OTHER}\"}"
  run _provision
  assert_failure
  assert_output --partial "refused network 'prod-net' (id 13): it has the owner mark of another cluster (lok8s.dev/owner=${OTHER}; this cluster: ${MARK})"
  run _writes
  assert_output ""
}

@test "refused: the lookup reports every unmarked part of all kinds in one run" {
  _all_parts '{}'
  run _provision
  assert_failure
  assert_output --partial "6 existing part(s) do not have the owner mark of this cluster"
  run _writes
  assert_output ""
}

@test "refused: an id that the descriptor itself carries is dropped, not used" {
  _part network 91 their-net '{}'
  yq -i -o json '.network[0].id = 91' "${DESC}"
  run _provision
  assert_success
  run _writes
  refute_output --regexp ' 91( |$)'
  assert_output --partial "network create --name prod-net --ip-range 10.0.0.0/16 --label lok8s.dev/owner=${MARK}"
}

# ── adopted: a re-run over parts with this cluster's mark ─

@test "adopted: parts with this cluster's mark are kept, and every hook calls hcloud with ids" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  run _provision
  assert_success
  run _writes
  refute_output --partial " create "
  # The subnet goes to the network id, the LB is attached and reconciled by
  # its id, the server target and the floating IP use the server id.
  assert_output --partial "network add-subnet 13 --network-zone eu-central --type cloud --ip-range 10.0.1.0/24"
  assert_output --partial "load-balancer attach-to-network 14 --network 13"
  assert_output --partial "load-balancer add-target 14 --server 15"
  assert_output --partial "load-balancer add-target 14 --label-selector lok8s.dev/cluster=prod,lok8s.dev/role=control-plane,lok8s.dev/owner=${MARK}"
  assert_output --partial "load-balancer add-service 14 --protocol tcp --listen-port 6443 --destination-port 6443"
  assert_output --partial "floating-ip assign 12 15"
  # No change names a part.
  refute_output --regexp ' (prod-net|prod-api|cp-0|prod-ip)( |$)'
}

# ── created: every part gets the mark ────────────────────

@test "created: every kind gets the owner mark at create, after the descriptor's own labels" {
  yq -i -o json '.network[0].label = "lok8s.dev/owner=someone-else"' "${DESC}"
  run _provision
  assert_success
  local kind
  for kind in ssh-key floating-ip network load-balancer server volume; do
    run grep -E "^${kind} create " "${HCLOUD_LOG}"
    assert_success
    assert_output --regexp "--label lok8s.dev/owner=${MARK}( |$)"
  done
  # The mark comes last: hcloud keeps the last value of a key.
  run grep -E '^network create ' "${HCLOUD_LOG}"
  assert_output --partial "--label lok8s.dev/owner=someone-else --label lok8s.dev/owner=${MARK}"
  run jq -r '.[0].labels["lok8s.dev/owner"]' "${STORE}/network.json"
  assert_output "${MARK}"
}

@test "created: the hooks of a new part call hcloud with its new id, not its name" {
  run _provision
  assert_success
  # ids of the fake: ssh-key 100, floating-ip 101, network 102, LB 103, server 104.
  run _writes
  assert_output --partial "network add-subnet 102 "
  assert_output --partial "load-balancer attach-to-network 103 --network 102"
  assert_output --partial "load-balancer add-service 103 "
  assert_output --partial "floating-ip assign 101 104"
  # The LB step runs before the server step: the server target waits for the
  # next run and is never added by name.
  refute_output --partial "--server cp-0"
  # Only a create names a part.
  run grep -vE ' create ' <<<"${output}"
  refute_output --regexp ' (prod-net|prod-api|cp-0|prod-ip)( |$)'
}

# ── load balancer targets ────────────────────────────────

# _selector_matches <selector> <labels-json>: equality terms only, as the
# Hetzner API reads "k=v,k=v".
_selector_matches() {
  jq -e --arg s "${1}" '. as $l | all($s | split(",")[]; split("=") as $kv | $l[$kv[0]] == ($kv[1:] | join("=")))' <<<"${2}" >/dev/null
}

@test "lb: a foreign server with the cluster and role labels but no owner mark is not a target" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  local ours foreign
  ours="{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/role\":\"control-plane\",\"lok8s.dev/owner\":\"${MARK}\"}"
  foreign='{"lok8s.dev/cluster":"prod","lok8s.dev/role":"control-plane"}'
  _part server 77 their-cp-0 "${foreign}"
  run _provision
  assert_success
  run grep -E '^load-balancer add-target 14 --label-selector ' "${HCLOUD_LOG}"
  assert_success
  local sel="${output#load-balancer add-target 14 --label-selector }"
  # The selector that lok8s gives Hetzner selects our server, not theirs.
  _selector_matches "${sel}" "${ours}"
  run _selector_matches "${sel}" "${foreign}"
  assert_failure
  run _writes
  refute_output --partial "--server 77"
  refute_output --partial "their-cp-0"
}

@test "lb: the old selector target without the owner mark is replaced by the one with the mark" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  local old="lok8s.dev/cluster=prod,lok8s.dev/role=control-plane"
  jq -c --arg s "${old}" '.[0] += {private_net: [{network: 13, ip: "10.0.0.2"}],
      targets: [{type: "label_selector", label_selector: {selector: $s}, use_private_ip: false}]}' \
    "${STORE}/load-balancer.json" > "${STORE}/lb.new"
  mv "${STORE}/lb.new" "${STORE}/load-balancer.json"
  run _provision
  assert_success
  run grep -E '^load-balancer (add|remove)-target 14 --label-selector' "${HCLOUD_LOG}"
  # Add the target with the mark first, then remove the old one: no gap.
  assert_output "load-balancer add-target 14 --label-selector ${old},lok8s.dev/owner=${MARK}
load-balancer remove-target 14 --label-selector ${old}"
}

@test "lb: a server target that is not a server of the descriptor is refused" {
  _all_parts "{\"lok8s.dev/owner\":\"${MARK}\"}"
  _part server 77 their-cp-0 '{"lok8s.dev/cluster":"prod"}'
  yq -i -o json '.["load-balancer"][0]["#targets"] = [{"type": "server", "value": "their-cp-0"}]' "${DESC}"
  run _provision
  assert_failure
  assert_output --partial "lb prod-api: refused target server 'their-cp-0': it is not a server of the descriptor"
  run _writes
  refute_output --partial "add-target"
}

@test "lb: a selector that names lok8s.dev/owner with another value is refused" {
  _all_parts "{\"lok8s.dev/owner\":\"${MARK}\"}"
  yq -i -o json ".[\"load-balancer\"][0][\"#targets\"] = [{\"type\": \"label-selector\", \"value\": \"lok8s.dev/role=control-plane,lok8s.dev/owner=${OTHER}\"}]" "${DESC}"
  run _provision
  assert_failure
  assert_output --partial "refused target label-selector 'lok8s.dev/role=control-plane,lok8s.dev/owner=${OTHER}'"
  run _writes
  refute_output --partial "add-target"
}

# ── inventory ────────────────────────────────────────────

# _inventory_parts: our LB, network and server have the mark. A foreign LB
# (listed first) and network carry the cluster label only; a foreign server
# carries the cluster label and a declared name of another descriptor.
_inventory_parts() {
  _part load-balancer 90 their-api '{"lok8s.dev/cluster":"prod"}' \
    '{"public_net":{"ipv4":{"ip":"198.51.100.66"}},"private_net":[{"ip":"10.9.0.2"}]}'
  _part load-balancer 14 prod-api "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}" \
    '{"public_net":{"ipv4":{"ip":"203.0.113.14"}},"private_net":[{"ip":"10.0.0.2"}]}'
  _part network 91 their-net '{"lok8s.dev/cluster":"prod"}' '{"ip_range":"10.9.0.0/16"}'
  _part network 13 prod-net "{\"lok8s.dev/owner\":\"${MARK}\"}" '{"ip_range":"10.0.0.0/16"}'
  _part server 15 cp-0 "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/role\":\"control-plane\",\"lok8s.dev/owner\":\"${MARK}\"}" \
    '{"public_net":{"ipv4":{"ip":"203.0.113.15"}},"private_net":[{"ip":"10.0.0.3"}]}'
  _part server 92 cp-1 '{"lok8s.dev/cluster":"prod","lok8s.dev/role":"control-plane"}' \
    '{"public_net":{"ipv4":{"ip":"198.51.100.92"}},"private_net":[{"ip":"10.9.0.3"}]}'
  yq -i -o json '.server += [{"name": "cp-1", "type": "cx23", "label": "lok8s.dev/cluster=prod,lok8s.dev/role=control-plane"}]' "${DESC}"
}

@test "inventory: a foreign LB or network with only the cluster label is ignored" {
  _inventory_parts
  local out
  out="$(provider::output "${DESC}" 2>"${BATS_TEST_TMPDIR}/output.err")"
  run jq -r '.api.endpoint, .api.privateEndpoint, .network.id, .network.name, ([.nodes[].name] | join(","))' <<<"${out}"
  assert_output "203.0.113.14
10.0.0.2
13
prod-net
cp-0"
  # The selectors ask Hetzner for the mark too.
  run grep -E '^(load-balancer|network) list' "${HCLOUD_LOG}"
  assert_output "load-balancer list -o json -l lok8s.dev/cluster=prod,lok8s.dev/owner=${MARK}
network list -o json -l lok8s.dev/owner=${MARK}"
}

@test "inventory: a declared server without the mark is left out with the one-time mark command" {
  _inventory_parts
  run provider::output "${DESC}"
  assert_success
  assert_output --partial "server cp-1 is left out of the inventory"
  assert_output --partial "hcloud server add-label 92 lok8s.dev/owner=${MARK}"
}

@test "inventory: only foreign parts give no endpoint and no network, never a foreign one" {
  _part load-balancer 90 their-api '{"lok8s.dev/cluster":"prod"}' '{"public_net":{"ipv4":{"ip":"198.51.100.66"}}}'
  # The descriptor's network name, without the mark: the old name fallback.
  _part network 91 prod-net '{}' '{"ip_range":"10.9.0.0/16"}'
  run provider::output "${DESC}"
  assert_success
  run jq -r '.api.endpoint + "|" + .network.id' <<<"${output}"
  assert_output "|"
}

@test "inventory: the kubeone hosts, API endpoint and networkID come from the marked parts only" {
  _inventory_parts
  :usage() { :; }
  :args() { shift; }
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"
  : > "${BATS_TEST_TMPDIR}/id_key"
  yq -i -o json ".sshPrivateKey = \"${BATS_TEST_TMPDIR}/id_key\"" "${DESC}"
  local manifest="${BATS_TEST_TMPDIR}/kubeone.yaml"
  printf 'apiVersion: kubeone.k8c.io/v1beta2\nkind: KubeOneCluster\ncloudProvider:\n  hetzner: {}\n' > "${manifest}"
  run _append_inventory "${DESC}" "${manifest}"
  assert_success
  run yq -r '.apiEndpoint.host, .cloudProvider.hetzner.networkID, ([.controlPlane.hosts[].hostname] | join(","))' "${manifest}"
  assert_output "203.0.113.14
13
cp-0"
  run cat "${manifest}"
  refute_output --partial "198.51.100"
}

@test "inventory: a failed list stops the kubeone inventory, it never reads as no part" {
  _inventory_parts
  :usage() { :; }
  :args() { shift; }
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/main"
  : > "${BATS_TEST_TMPDIR}/id_key"
  yq -i -o json ".sshPrivateKey = \"${BATS_TEST_TMPDIR}/id_key\"" "${DESC}"
  local manifest="${BATS_TEST_TMPDIR}/kubeone.yaml"
  printf 'apiVersion: kubeone.k8c.io/v1beta2\nkind: KubeOneCluster\ncloudProvider:\n  hetzner: {}\n' > "${manifest}"
  hcloud() { echo "hcloud: server error (service_error)" >&2; return 1; }
  run _append_inventory "${DESC}" "${manifest}"
  assert_failure
  assert_output --partial "could not list the servers, load balancers or networks of cluster prod"
  assert_output --partial "kubeone inventory: provider::output failed"
  run yq -r '.apiEndpoint // "none"' "${manifest}"
  assert_output "none"
}
