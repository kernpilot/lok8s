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

# The fake keeps the parts of each kind in ${STORE}/<kind>.json. `list`
# prints the store, `describe` prints the part with that id. `create` reads
# its flags as hcloud does ("--flag value" and "--flag=value"), writes them
# to ${STORE}/creates.jsonl ({"kind": ..., "flags": [[flag, value], ...]})
# and adds a part with the labels of its --label flags. A server create
# models two parts of hcloud:
# - With no --ssh-key flag at all, the server gets the keys of
#   FAKE_DEFAULT_SSH_KEYS (comma-separated ids: the option default-ssh-keys
#   of the hcloud configuration). "--ssh-key=" sets the flag with no key.
# - The image is found as hcloud-go finds it: a value that strconv.ParseInt
#   reads is an id ("+4711" and "04711" are 4711); any other value is the
#   name of an image in the store.
# The flags are read as pflag reads them (_fake_is_bool): "--quiet
# --network=999" is quiet=true and network=999.
# The writes add-target, remove-target, add-subnet, attach-to-network,
# add-service, update-service and assign change the part. Other writes
# succeed. Switches:
#   HCLOUD_FAIL=<regex>   a command that matches fails (nothing is done)
#   HCLOUD_FAIL_AFTER=<regex>  HCLOUD_FAIL applies only after a command that
#                         matches this one
#   HCLOUD_LOST=<regex>   a command that matches is done, but fails (a lost answer)
#   HCLOUD_ERR=<text>     the error text of a command that fails
#   LIST_FAILS_AFTER_CREATE=<kind>  after a create of <kind>, its list fails
#   RELABEL_AFTER_LIST="<kind> <id> <value> [<n>]"  after the n-th (default:
#                         first) list of <kind>, the owner label of that part
#                         changes (another actor)
#   RELABEL_AFTER_CALL="<kind> <id> <value> <regex>"  after the first command
#                         that matches <regex> (done or failed), the owner
#                         label of that part changes (another actor)
_fake_hcloud() {
  hcloud() {
    local rc=0 rk rid rv rre
    printf '%s\n' "$*" >> "${HCLOUD_LOG}"
    _fake_hcloud_call "$@" || rc=$?
    if [[ -n "${HCLOUD_FAIL_AFTER:-}" && "$*" =~ ${HCLOUD_FAIL_AFTER} ]]; then
      : > "${STORE}/fail-armed"
    fi
    read -r rk rid rv rre <<<"${RELABEL_AFTER_CALL:-}"
    if [[ -n "${rre}" && ! -f "${STORE}/relabel-done" && "$*" =~ ${rre} ]]; then
      : > "${STORE}/relabel-done"
      _fake_set "${rk}" "${rid}" '.labels["lok8s.dev/owner"] = $v' --arg v "${rv}"
    fi
    return "${rc}"
  }
  # _fake_set <kind> <id> <jq-update> [jq args...]: changes the part <id>.
  _fake_set() {
    local f="${STORE}/${1}.json" id="${2}" upd="${3}"
    shift 3
    [[ -f "${f}" ]] || echo '[]' > "${f}"
    jq -c --arg id "${id}" "$@" "map(if (.id | tostring) == \$id then ${upd} else . end)" "${f}" > "${f}.new"
    mv "${f}.new" "${f}"
  }
  _fake_hcloud_call() {
    if [[ -n "${HCLOUD_FAIL:-}" && "$*" =~ ${HCLOUD_FAIL} ]] &&
       [[ -z "${HCLOUD_FAIL_AFTER:-}" || -f "${STORE}/fail-armed" ]]; then
      echo "${HCLOUD_ERR:-hcloud: ${1} ${2} failed (server_error)}" >&2
      return 1
    fi
    local kind="${1}" verb="${2}" lost=0
    [[ -z "${HCLOUD_LOST:-}" || ! "$*" =~ ${HCLOUD_LOST} ]] || lost=1
    shift 2
    local f="${STORE}/${kind}.json"
    [[ -f "${f}" ]] || echo '[]' > "${f}"
    case "${verb}" in
      list)
        if [[ -f "${STORE}/list-fails-${kind}" ]]; then
          echo "hcloud: ${kind} list failed (server_error)" >&2
          return 1
        fi
        # -l <selector>: only the parts that have each "<key>=<value>" term
        # (a comma is AND). A term of another form fails the list, so a
        # broken selector fails the test that runs it.
        local sel=""
        while (( $# )); do
          case "${1}" in
            -l|--selector) sel="${2-}"; shift; (( $# )) && shift ;;
            --selector=*) sel="${1#*=}"; shift ;;
            *) shift ;;
          esac
        done
        jq -c --arg sel "${sel}" '
          [ $sel | select(. != "") | split(",")[]
            | if test("^[^=!,() ]+=[^=!,() ]*$") then {k: .[:index("=")], v: .[index("=") + 1:]}
              else error("fake hcloud: unsupported selector term: \(.)") end ] as $terms
          | map(select(. as $p | all($terms[]; (($p.labels // {})[.k] // null) == .v)))' "${f}" || return 1
        echo "${kind}" >> "${STORE}/lists"
        local rk rid rv rn
        read -r rk rid rv rn <<<"${RELABEL_AFTER_LIST:-}"
        if [[ "${rk}" == "${kind}" && "$(grep -cx -- "${kind}" "${STORE}/lists")" == "${rn:-1}" ]]; then
          _fake_set "${kind}" "${rid}" '.labels["lok8s.dev/owner"] = $v' --arg v "${rv}"
        fi ;;
      create)
        local -a fl=()
        local k v id name labels extra='{}' keys image
        # As pflag reads them: "--flag=value"; a bool flag without "=" is
        # true and reads no value; any other flag takes the next token as
        # its value, also one that starts with "-".
        while (( $# )); do
          case "${1}" in
            --*=*) k="${1%%=*}"; k="${k#--}"; v="${1#*=}"; shift ;;
            --*)
              k="${1#--}"
              shift
              if _fake_is_bool "${kind}" "${k}"; then
                v=true
              elif (( $# )); then
                v="${1}"
                shift
              else
                echo "hcloud: flag needs an argument: --${k}" >&2
                return 1
              fi ;;
            *)
              echo "hcloud: ${kind} create takes no argument: ${1}" >&2
              return 1 ;;
          esac
          fl+=("${k}" "${v}")
        done
        # NUL-separated on stdin: after --args, jq reads a value that
        # starts with "-" as an option of its own and records nothing.
        printf '%s\0' "${fl[@]}" | jq -cRs --arg kind "${kind}" '(split("\u0000") | .[:-1]) as $a
          | {kind: $kind, flags: [range(0; $a | length; 2) as $j | [$a[$j], $a[$j + 1]]]}' \
          >> "${STORE}/creates.jsonl" || return 1
        name="$(tail -n1 "${STORE}/creates.jsonl" | jq -r '[.flags[] | select(.[0] == "name") | .[1]] | last // ""')"
        labels="$(tail -n1 "${STORE}/creates.jsonl" | jq -c '[.flags[] | select(.[0] == "label") | .[1]
          | split(",")[] | split("=") | {(.[0]): (.[1:] | join("="))}] | add // {}')"
        if [[ "${kind}" == "server" ]]; then
          keys="$(tail -n1 "${STORE}/creates.jsonl" | jq -c --arg d "${FAKE_DEFAULT_SSH_KEYS:-}" '
            [.flags[] | select(.[0] == "ssh-key") | .[1]] as $s
            | if ($s | length) == 0 then ($d | split(",") | map(select(. != "")))
              else [$s[] | split(",")[] | select(. != "")] end')"
          image="$(tail -n1 "${STORE}/creates.jsonl" | jq -r '[.flags[] | select(.[0] == "image") | .[1]] | last // ""')"
          if [[ "${image}" =~ ^[+-]?[0-9]+$ ]]; then
            v="${image#[+-]}"
            v=$(( 10#${v} ))
            [[ "${image}" != -* ]] || v=$(( -v ))
          else
            v="$(jq -r --arg n "${image}" 'first(.[] | select(.name == $n) | .id) // "null"' "${STORE}/image.json" 2>/dev/null)" || v=null
            [[ -n "${v}" ]] || v=null
          fi
          extra="$(jq -cn --argjson k "${keys}" --argjson img "${v}" '{ssh_keys: $k, image: {id: $img}}')"
        fi
        id=$(( 100 + $(cat "${STORE}"/*.json | jq -s 'map(length) | add') ))
        _part "${kind}" "${id}" "${name}" "${labels}" "${extra}"
        [[ "${LIST_FAILS_AFTER_CREATE:-}" != "${kind}" ]] || : > "${STORE}/list-fails-${kind}" ;;
      describe)
        jq -c --arg r "${1}" 'first(.[] | select((.id | tostring) == $r)) // error("not found")' "${f}" 2>/dev/null ||
          { echo "hcloud: ${kind} not found: ${1}" >&2; return 1; } ;;
      add-target|remove-target)
        local lb="${1}" t=""
        shift
        while (( $# )); do
          case "${1}" in
            --label-selector) t="$(jq -cn --arg s "${2}" '{type: "label_selector", label_selector: {selector: $s}}')"; shift 2 ;;
            --server) t="$(jq -cn --argjson s "${2}" '{type: "server", server: {id: $s}}')"; shift 2 ;;
            *) shift ;;
          esac
        done
        if [[ "${verb}" == "add-target" ]]; then
          _fake_set "${kind}" "${lb}" '.targets = ((.targets // []) + [$t + {use_private_ip: false}])' --argjson t "${t}"
        else
          _fake_set "${kind}" "${lb}" '.targets = [ (.targets // [])[] | select((del(.use_private_ip)) != $t) ]' --argjson t "${t}"
        fi ;;
      add-subnet)
        local net="${1}" r=""
        while (( $# )); do
          [[ "${1}" != "--ip-range" ]] || r="${2}"
          shift
        done
        _fake_set "${kind}" "${net}" '.subnets = ((.subnets // []) + [{ip_range: $r}])' --arg r "${r}" ;;
      attach-to-network)
        local lb="${1}" n=""
        while (( $# )); do
          [[ "${1}" != "--network" ]] || n="${2}"
          shift
        done
        if jq -e --arg lb "${lb}" --arg n "${n}" 'any(.[]; (.id | tostring) == $lb
             and any((.private_net // [])[]; (.network | tostring) == $n))' "${f}" >/dev/null; then
          echo "hcloud: load balancer is already attached to the network (load_balancer_already_attached)" >&2
          return 1
        fi
        _fake_set "${kind}" "${lb}" '.private_net = ((.private_net // []) + [{network: ($n | tonumber)}])' --arg n "${n}" ;;
      add-service|update-service)
        local lb="${1}" p="" lp="" dp=""
        while (( $# )); do
          case "${1}" in
            --protocol) p="${2}" ;;
            --listen-port) lp="${2}" ;;
            --destination-port) dp="${2}" ;;
          esac
          shift
        done
        _fake_set "${kind}" "${lb}" '.services = ([ (.services // [])[] | select((.listen_port | tostring) != $lp) ]
          + [{protocol: $p, listen_port: ($lp | tonumber), destination_port: ($dp | tonumber)}])' \
          --arg p "${p}" --arg lp "${lp}" --arg dp "${dp}" ;;
      assign)
        _fake_set "${kind}" "${1}" '.server = ($s | tonumber)' --arg s "${2}" ;;
      *) : ;;
    esac
    if (( lost )); then
      echo "${HCLOUD_ERR:-hcloud: timeout awaiting response headers}" >&2
      return 1
    fi
    return 0
  }
  # _fake_is_bool <kind> <flag>: a bool flag of 'hcloud <kind> create'
  # (hcloud v1.70.2): the bool options of the root command
  # (internal/state/config/options.go: debug, no-experimental-warnings,
  # quiet), the help flag of cobra and the Bool flags of
  # internal/cmd/<kind>/create.go.
  _fake_is_bool() {
    local -A b=(
      [server]="start-after-create automount allow-deprecated-image without-ipv4 without-ipv6 enable-backup"
      [volume]="automount"
      [network]="expose-routes-to-vswitch"
    )
    [[ " debug quiet no-experimental-warnings help ${b[${1}]:-} " == *" ${2} "* ]]
  }
  export -f hcloud _fake_set _fake_hcloud_call _fake_is_bool
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

# _provision: the steps of provider::provision (hetzner::_run_steps), without
# the cloud-init setup.
_provision() {
  CLOUD_HETZNER_JSON="$(template::descriptor_json "${DESC}")" || return 1
  hetzner::_run_steps
}

# _writes: the hcloud calls that change something (all but list/describe).
_writes() {
  grep -vE '^[a-z-]+ (list|describe)( |$)' "${HCLOUD_LOG}" || true
}

# _created <kind> <flag>: the values of <flag> in each create of <kind>, as
# the fake read them (one line per value).
_created() {
  [[ -f "${STORE}/creates.jsonl" ]] || return 0
  jq -r --arg k "${1}" --arg f "${2}" 'select(.kind == $k) | .flags[] | select(.[0] == $f) | .[1]' \
    "${STORE}/creates.jsonl"
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
  # The lookup found the part by its name only: the user checks the id first.
  assert_output --partial "first make sure that id ${2} is yours ('hcloud ${1} describe ${2}')"
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
  assert_output --partial "network create --name=prod-net --ip-range=10.0.0.0/16 --label=lok8s.dev/owner=${MARK}"
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
    assert_output --regexp "--label=lok8s.dev/owner=${MARK}( |$)"
  done
  # The mark comes last: hcloud keeps the last value of a key.
  run grep -E '^network create ' "${HCLOUD_LOG}"
  assert_output --partial "--label=lok8s.dev/owner=someone-else --label=lok8s.dev/owner=${MARK}"
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
  # main imports drivers/kubeone/config (import is stubbed here): the edits
  # of kubeone.yaml run through its kubeone::_yq_replace.
  source "${_PROJECT_ROOT}/.lok8s/drivers/kubeone/config"
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

@test "owner mark: '.' and '..' are not a domain" {
  PROVIDER_DOMAIN=".." run hetzner::owner_mark prod
  assert_failure
  assert_output --partial "the domain of the cluster is '..', which is not a domain name"
  PROVIDER_DOMAIN="." run hetzner::owner_mark prod
  assert_failure
  assert_output --partial "the domain of the cluster is '.', which is not a domain name"
}

# ── references at create: only verified ids of marked parts ─

# _foreign_parts: one part of each kind that a create can refer to, without
# the owner mark (another person's parts in the same project).
_foreign_parts() {
  _part ssh-key 71 their-key '{}'
  _part network 72 their-net '{}'
  _part volume 73 their-vol '{}'
  _part firewall 74 their-fw '{}'
  _part placement-group 75 their-pg '{}'
  _part primary-ip 76 their-ip4 '{}'
  _part primary-ip 77 their-ip6 '{}'
  _part server 78 their-cp '{"lok8s.dev/cluster":"prod"}'
}

# _refused_refs: the provision stops in the refs step: no write at all
# (checked first), then the summary.
_refused_refs() {
  run _provision
  local out="${output}" rc="${status}"
  run _writes
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "refused key(s) or reference(s). Nothing was created or changed."
}

@test "refs: every server reference to a foreign part, by name, by id or by id string, is refused before any create" {
  _foreign_parts
  yq -i -o json '.server[0] += {"ssh-key": ["their-key"], "network": "72", "volume": ["their-vol"],
    "firewall": ["their-fw"], "placement-group": "75", "primary-ipv4": 76, "primary-ipv6": "their-ip6"}' "${DESC}"
  _refused_refs
  assert_output --partial "the descriptor has 7 refused key(s) or reference(s)."
  assert_output --partial "refused server 'cp-0': ssh-key: ssh-key 'their-key' (id 71) does not have the owner mark of this cluster (lok8s.dev/owner=${MARK})"
  assert_output --partial "first make sure that id 71 is yours ('hcloud ssh-key describe 71'), then mark it one time: hcloud ssh-key add-label 71 lok8s.dev/owner=${MARK}"
  assert_output --partial "refused server 'cp-0': network: network 'their-net' (id 72) does not have the owner mark"
  assert_output --partial "refused server 'cp-0': volume: volume 'their-vol' (id 73) does not have the owner mark"
  assert_output --partial "refused server 'cp-0': firewall: firewall 'their-fw' (id 74) does not have the owner mark"
  assert_output --partial "refused server 'cp-0': placement-group: placement-group 'their-pg' (id 75) does not have the owner mark"
  assert_output --partial "refused server 'cp-0': primary-ipv4: primary-ip 'their-ip4' (id 76) does not have the owner mark"
  assert_output --partial "hcloud primary-ip add-label 76 lok8s.dev/owner=${MARK}"
  assert_output --partial "refused server 'cp-0': primary-ipv6: primary-ip 'their-ip6' (id 77) does not have the owner mark"
}

@test "refs: the volume server, floating-ip server and load-balancer network by name are refused when foreign" {
  _foreign_parts
  yq -i -o json '.volume[0].server = "their-cp" | .["floating-ip"][0].server = "their-cp"
    | .["load-balancer"][0].network = "their-net" | .["load-balancer"][0]["primary-ipv4"] = "their-ip4"' "${DESC}"
  _refused_refs
  assert_output --partial "refused volume 'prod-data': server: server 'their-cp' (id 78) does not have the owner mark"
  assert_output --partial "refused floating-ip 'prod-ip': server: server 'their-cp' (id 78) does not have the owner mark"
  assert_output --partial "refused load-balancer 'prod-api': network: network 'their-net' (id 72) does not have the owner mark"
  assert_output --partial "refused load-balancer 'prod-api': primary-ipv4: primary-ip 'their-ip4' (id 76) does not have the owner mark"
}

@test "refs: a key with a #suffix goes through the same check, as a string and as a number" {
  _foreign_parts
  _part ssh-key 999 also-theirs '{}'
  yq -i -o json 'del(.volume[0].server) | .volume[0]["server#ref"] = "78"
    | .["floating-ip"][0]["server#x"] = "their-cp"
    | .["load-balancer"][0]["network#x"] = "their-net"
    | .server[0]["ssh-key#2"] = ["their-key"] | .server[0]["ssh-key#3"] = [999]' "${DESC}"
  _refused_refs
  assert_output --partial "refused volume 'prod-data': server: server 'their-cp' (id 78) does not have the owner mark"
  # A number on a reference key is an index into the descriptor, also with a suffix.
  assert_output --partial "refused server 'cp-0': ssh-key: the descriptor has no ssh-key with the index 999."
  assert_output --partial "refused floating-ip 'prod-ip': server: server 'their-cp' (id 78)"
  assert_output --partial "refused load-balancer 'prod-api': network: network 'their-net' (id 72)"
  assert_output --partial "refused server 'cp-0': ssh-key: ssh-key 'their-key' (id 71)"
}

@test "refs: a reference to a part with the mark of another cluster is refused" {
  _part firewall 74 their-fw "{\"lok8s.dev/owner\":\"${OTHER}\"}"
  yq -i -o json '.server[0].firewall = ["their-fw"]' "${DESC}"
  _refused_refs
  assert_output --partial "refused server 'cp-0': firewall: firewall 'their-fw' (id 74) has the owner mark of another cluster (lok8s.dev/owner=${OTHER}; this cluster: ${MARK})"
}

@test "refs: a value that is no index, id or name is refused (true, null, an empty list, a list on a one-part flag)" {
  _part server 81 a "{\"lok8s.dev/owner\":\"${MARK}\"}"
  _part server 82 b "{\"lok8s.dev/owner\":\"${MARK}\"}"
  yq -i -o json '.server[0].network = true | .server[0].firewall = null | .server[0]["ssh-key"] = []
    | .volume[0].server = ["a", "b"] | .["floating-ip"][0].server = "a,b"' "${DESC}"
  _refused_refs
  assert_output --partial "refused server 'cp-0': network: true is a boolean."
  assert_output --partial "refused server 'cp-0': firewall: null is a null."
  assert_output --partial "refused server 'cp-0': ssh-key is empty."
  assert_output --partial "refused volume 'prod-data': server: hcloud volume create takes one server, not a list."
  assert_output --partial "refused floating-ip 'prod-ip': server: 'a,b' is not one id or name."
}

@test "refs: a reference to a part that lok8s creates in a later step is refused before any create" {
  yq -i -o json '.["floating-ip"][0].server = 0' "${DESC}"
  _refused_refs
  assert_output --partial "refused floating-ip 'prod-ip': server: server 'cp-0' of the descriptor does not exist yet, and lok8s creates the server parts after the floating-ip parts."
}

@test "refs: a name of a descriptor part, and a marked part outside the descriptor (also on a #suffix key), reach hcloud as verified ids" {
  _part firewall 74 our-fw "{\"lok8s.dev/owner\":\"${MARK}\"}"
  _part primary-ip 76 our-ip4 "{\"lok8s.dev/owner\":\"${MARK}\"}"
  _part primary-ip 77 our-ip6 "{\"lok8s.dev/owner\":\"${MARK}\"}"
  yq -i -o json '.server[0]["ssh-key"] = ["prod-key"] | .server[0].network = "prod-net"
    | .server[0].firewall = ["our-fw"] | .server[0]["primary-ipv4"] = 76
    | .server[0]["primary-ipv6#v6"] = "our-ip6"' "${DESC}"
  run _provision
  assert_success
  # ids of the fake: ssh-key 103, network 105 (three parts exist before the run).
  run grep -E '^server create ' "${HCLOUD_LOG}"
  assert_output --regexp ' --ssh-key=103 '
  assert_output --regexp ' --network=105 '
  assert_output --regexp ' --firewall=74 '
  assert_output --regexp ' --primary-ipv4=76 '
  assert_output --regexp ' --primary-ipv6=77 '
  refute_output --regexp '[ =](prod-key|prod-net|our-fw|our-ip6)( |$)'
}

# The conversion of the descriptor reads a reference key without its
# "#..." suffix too: a number is the id of that descriptor part. (The
# bare-metal hooks get these values.)
@test "conversion: a number on a reference key with a #suffix is the id of that descriptor part, as without the suffix" {
  CLOUD_HETZNER_JSON='{"server":[{"name":"a","id":41},{"name":"b","id":42}],"volume":[{"name":"v","server":1,"server#ref":1,"size":10}]}'
  # shellcheck disable=SC2016
  run hetzner::json -rc --argjson refs "${_HETZNER_REF_KEYS}" "${_HETZNER_JQ_VALUE}"' . as $root
    | .volume[0] | to_entries[] | "--" + .key, hcloud_value($root; $refs)'
  assert_success
  assert_output "--name
v
--server
42
--server#ref
42
--size
10"
}

@test "refs: the create resolves a reference again right before it, and stops when the part changed" {
  _part firewall 74 our-fw "{\"lok8s.dev/owner\":\"${MARK}\"}"
  yq -i -o json '.server[0].firewall = ["our-fw"]' "${DESC}"
  # The refs step lists the firewalls (the first list); then another actor
  # gives the firewall another owner mark.
  export RELABEL_AFTER_LIST="firewall 74 ${OTHER}"
  run _provision
  assert_failure
  assert_output --partial "refused server 'cp-0': firewall: firewall 'our-fw' (id 74) has the owner mark of another cluster"
  assert_output --partial "did not create server 'cp-0'"
  run grep -E '^server create ' "${HCLOUD_LOG}"
  assert_output ""
}

@test "refs: a snapshot image id needs the owner mark; a system image id and an image name pass" {
  _part image 4711 "" '{}' '{"type":"snapshot"}'
  _part image 5 ubuntu-24.04 '{}' '{"type":"system"}'
  yq -i -o json '.server[0].image = 4711' "${DESC}"
  _refused_refs
  assert_output --partial "refused server 'cp-0': image: image 4711 (a snapshot) does not have the owner mark of this cluster"
  assert_output --partial "hcloud image add-label 4711 lok8s.dev/owner=${MARK}"
  yq -i -o json '.server[0].image = 5' "${DESC}"
  : > "${HCLOUD_LOG}"
  run _provision
  assert_success
  run grep -E '^server create ' "${HCLOUD_LOG}"
  assert_output --regexp ' --image=5 '
}

# ── the form of the keys and values ──────────────────────

@test "keys: a key that is not a flag name (network=999, image=4711) is refused, and no create runs" {
  _part network 999 their-net '{}'
  _part image 4711 "" '{}' '{"type":"snapshot"}'
  yq -i -o json '.server[0]["network=999"] = "unused" | .server[0]["image=4711"] = "ubuntu-24.04"' "${DESC}"
  run _provision
  local out="${output}" rc="${status}"
  # hcloud reads "--network=999" as the network 999 and "--image=4711" as
  # the image 4711: no create gets them.
  run _created server network
  refute_output --partial "999"
  run _created server image
  refute_output --partial "4711"
  run _writes
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "refused server 'cp-0': the key \"network=999\" is not the name of an hcloud flag."
  assert_output --partial "refused server 'cp-0': the key \"image=4711\" is not the name of an hcloud flag."
  assert_output --partial "the descriptor has 2 refused key(s) or reference(s). Nothing was created or changed."
}

# _desc_set <jq-filter>: changes the descriptor with jq (a JSON escape such
# as "\t" in the filter is the control character).
_desc_set() {
  jq "${1}" "${DESC}" > "${DESC}.new" && mv "${DESC}.new" "${DESC}"
}

@test "keys: a flag of the hcloud command (quiet, debug) is refused, and its value never becomes a flag" {
  _part network 999 their-net '{}'
  _part ssh-key 777 their-key '{}'
  _desc_set '.server[0].quiet = "--network=999" | .server[0].debug = "--ssh-key=777"'
  run _provision
  local out="${output}" rc="${status}"
  # pflag reads --quiet and --debug as switches with no value: then
  # "--network=999" and "--ssh-key=777" are flags of their own.
  run _created server network
  refute_output --partial "999"
  run _created server ssh-key
  refute_output --partial "777"
  run _writes
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "refused server 'cp-0': the key \"quiet\" is not a flag of 'hcloud server create' (hcloud v1.70.2)."
  assert_output --partial "refused server 'cp-0': the key \"debug\" is not a flag of 'hcloud server create' (hcloud v1.70.2)."
  assert_output --partial "the descriptor has 2 refused key(s) or reference(s). Nothing was created or changed."
}

@test "fake: hcloud create reads the flags as pflag does (--quiet --network=999 is quiet=true and network=999)" {
  run hcloud server create --name=fx --quiet --network=999 --debug --ssh-key=777 --type -x --without-ipv6
  run _created server quiet
  assert_output "true"
  run _created server network
  assert_output "999"
  run _created server debug
  assert_output "true"
  run _created server ssh-key
  assert_output "777"
  run _created server type
  assert_output "-x"
  run _created server without-ipv6
  assert_output "true"
}

@test "keys: a connection flag (context, config, endpoint) is refused on each kind, and no create runs" {
  _desc_set '.server[0].context = "other-project" | .volume[0].config = "/tmp/other-cli.toml"
    | .["load-balancer"][0].endpoint = "https://api.example.test/v1"'
  run _provision
  local out="${output}" rc="${status}"
  run _created server context
  assert_output ""
  run _created volume config
  assert_output ""
  run _created load-balancer endpoint
  assert_output ""
  run _writes
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "refused server 'cp-0': the key \"context\" is not a flag of 'hcloud server create'"
  assert_output --partial "refused volume 'prod-data': the key \"config\" is not a flag of 'hcloud volume create'"
  assert_output --partial "refused load-balancer 'prod-api': the key \"endpoint\" is not a flag of 'hcloud load-balancer create'"
}

@test "values: a control character in a value or a hook note, and a name with a space at an end, are refused before any create" {
  _desc_set '.network[0].name = " prod-net" | .server[0].location = "fsn1\n"
    | .["load-balancer"][0]["#targets"][1].value = "lok8s.dev/cluster=prod\tx"'
  run _provision
  local out="${output}" rc="${status}"
  run _writes
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "refused network ' prod-net': name: \" prod-net\" has a space at the start or the end."
  assert_output --partial "refused server 'cp-0': location: \"fsn1\\n\" has a control character (U+0000 to U+001F or U+007F)."
  assert_output --partial "refused load-balancer 'prod-api': #targets: "
  assert_output --partial "lok8s.dev/cluster=prod\\tx"
  assert_output --partial "the descriptor has 3 refused key(s) or reference(s)."
}

@test "image: a control character or a space at an end (\"4711\\t\", \"4711 \", \"\\t4711\", \"47\\t11\") is refused before any create" {
  _part image 4711 "" '{}' '{"type":"snapshot"}'
  local v out rc
  for v in '"4711\t"' '"4711 "' '"\t4711"' '"47\t11"'; do
    _desc_set ".server[0].image = ${v}"
    run _provision
    out="${output}" rc="${status}"
    run _writes
    assert_output ""
    output="${out}" status="${rc}"
    assert_failure
    assert_output --partial "refused server 'cp-0': image: ${v} has a"
  done
}

@test "image: a signed or zero-padded id of a foreign snapshot (+4711, 04711) is refused before any create" {
  _part image 4711 "" '{}' '{"type":"snapshot"}'
  local v out rc
  for v in "+4711" "04711"; do
    yq -i -o json ".server[0].image = \"${v}\"" "${DESC}"
    run _provision
    out="${output}" rc="${status}"
    # hcloud reads both as the image id 4711 (strconv.ParseInt).
    run jq -c '[.[] | .image.id // empty]' "${STORE}/server.json"
    assert_output "[]"
    run _writes
    assert_output ""
    output="${out}" status="${rc}"
    assert_failure
    assert_output --partial "refused server 'cp-0': image: '${v}' is an image id for hcloud (it has a sign or a leading zero). Write the id as plain digits, for example 4711."
  done
}

@test "ssh-key: a server without ssh-key gets no default key of the hcloud configuration" {
  _part ssh-key 71 their-key '{}'
  # The option default-ssh-keys of the hcloud configuration names the key
  # 71 (no owner mark): hcloud adds it when the create has no --ssh-key.
  export FAKE_DEFAULT_SSH_KEYS=71
  yq -i -o json 'del(.server[0]["ssh-key"])' "${DESC}"
  run _provision
  local out="${output}" rc="${status}"
  run jq -c '[.[] | select(.name == "cp-0") | .ssh_keys]' "${STORE}/server.json"
  assert_output '[[]]'
  run jq -c 'select(.kind == "server") | [.flags[] | select(.[0] == "ssh-key")]' "${STORE}/creates.jsonl"
  assert_output '[["ssh-key",""]]'
  output="${out}" status="${rc}"
  assert_success
}

@test "refs: a list on a one-part flag, an image list and a bare-metal server are refused before any create, also when the part comes later" {
  _part image 4711 "" '{}' '{"type":"snapshot"}'
  # cp-0 and cp-1 are made in this run, before the volumes; metal-0 is a
  # bare-metal server (no Cloud part).
  yq -i -o json '.server += [{"name": "cp-1", "type": "cx23", "image": "ubuntu-24.04", "location": "fsn1", "ssh-key": [0]},
      {"name": "metal-0", "#cloud.root": "true"}]
    | .volume[0].server = [0, 1]
    | .volume += [{"name": "metal-data", "size": 10, "server": 2}]
    | .server[0].image = [4711]' "${DESC}"
  _refused_refs
  assert_output --partial "refused volume 'prod-data': server: hcloud volume create takes one server, not a list. Write one index, id or name."
  assert_output --partial "refused server 'cp-0': image: [4711] is not one image. Write one image name or id."
  assert_output --partial "refused volume 'metal-data': server: server 'metal-0' of the descriptor is a bare-metal server (#cloud.root)."
  assert_output --partial "the descriptor has 3 refused key(s) or reference(s)."
}

# ── a part that changes between the check and the write ──

@test "recheck: a network that loses the mark after the lookup gets no add-subnet" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export RELABEL_AFTER_LIST="network 13 ${OTHER}"
  run _provision
  assert_failure
  assert_output --partial "did not change network 13: it does not have the owner mark of this cluster now (lok8s.dev/owner=${OTHER}; this cluster: ${MARK})"
  run _writes
  refute_output --partial "add-subnet"
}

@test "recheck: a floating IP that loses the mark after the lookup is not assigned" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # The second list is the lookup at the end of the floating-ip step; the
  # assign comes in the server step after it.
  export RELABEL_AFTER_LIST="floating-ip 12 ${OTHER} 2"
  run _provision
  assert_failure
  assert_output --partial "did not change floating-ip 12: it does not have the owner mark of this cluster now"
  run _writes
  refute_output --partial "floating-ip assign"
}

@test "recheck: a server that loses the mark after the lookup gets no floating IP" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # No load balancer: its server target would check the server first.
  yq -i -o json 'del(.["load-balancer"])' "${DESC}"
  export RELABEL_AFTER_LIST="server 15 ${OTHER}"
  run _provision
  assert_failure
  local out="${output}"
  run _writes
  refute_output --partial "floating-ip assign"
  output="${out}"
  assert_output --partial "did not change server 15: it does not have the owner mark of this cluster now"
}

@test "recheck: a network that loses the mark before the LB step: no attach and no target" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # The second network list is the lookup at the end of the network step.
  export RELABEL_AFTER_LIST="network 13 ${OTHER} 2"
  run _provision
  assert_failure
  assert_output --partial "did not change network 13: it does not have the owner mark of this cluster now"
  run _writes
  assert_output --partial "network add-subnet 13 "
  refute_output --partial "attach-to-network"
  refute_output --partial "add-target"
}

@test "recheck: a load balancer that loses the mark after the lookup gets no change" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export RELABEL_AFTER_LIST="load-balancer 14 ${OTHER}"
  run _provision
  assert_failure
  assert_output --partial "did not change load-balancer 14: it does not have the owner mark of this cluster now"
  run _writes
  refute_output --partial "load-balancer"
}

@test "recheck: a load balancer with no network to attach that loses the mark after the lookup gets no target and no service" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # No descriptor network: no attach (and no check before it), so the read
  # before the targets is the first check of the load balancer.
  yq -i -o json 'del(.network)' "${DESC}"
  export RELABEL_AFTER_LIST="load-balancer 14 ${OTHER}"
  run _provision
  assert_failure
  local out="${output}"
  run _writes
  refute_output --partial "load-balancer"
  output="${out}"
  assert_output --partial "did not change load-balancer 14: it does not have the owner mark of this cluster now"
}

@test "recheck: a server that loses the mark after the lookup is not added as an LB target" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export RELABEL_AFTER_LIST="server 15 ${OTHER}"
  run _provision
  assert_failure
  assert_output --partial "did not change server 15: it does not have the owner mark of this cluster now"
  run _writes
  refute_output --partial "add-target 14 --server"
}

# ── load balancer targets: the audit ─────────────────────

# _lb_targets <targets-json>: the LB 14 (with the mark) has these targets.
_lb_targets() {
  jq -c --argjson t "${1}" '.[0] += {private_net: [{network: 13, ip: "10.0.0.2"}], targets: $t}' \
    "${STORE}/load-balancer.json" > "${STORE}/lb.new"
  mv "${STORE}/lb.new" "${STORE}/load-balancer.json"
}

@test "lb: a failed add of the selector with the mark fails the run, and the old selector goes all the same" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  local old="lok8s.dev/cluster=prod,lok8s.dev/role=control-plane"
  _lb_targets "[{\"type\":\"label_selector\",\"label_selector\":{\"selector\":\"${old}\"},\"use_private_ip\":false}]"
  export HCLOUD_FAIL='^load-balancer add-target 14 --label-selector'
  run _provision
  assert_failure
  assert_output --partial "lb prod-api: could not add target label-selector '${old},lok8s.dev/owner=${MARK}'"
  assert_output --partial "hetzner: the load balancer does not match the descriptor"
  run grep -E '^load-balancer remove-target 14 ' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target 14 --label-selector ${old}"
  # Left: the descriptor's server target (cp-0 has the mark). No selector.
  run jq -c '[.[0].targets[] | .label_selector.selector // .server.id]' "${STORE}/load-balancer.json"
  assert_output "[15]"
}

@test "lb: the audit removes a dropped selector without the mark and a foreign server target, and keeps the rest" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  _part server 77 their-cp '{"lok8s.dev/cluster":"prod","lok8s.dev/role":"control-plane"}'
  local dropped="lok8s.dev/cluster=prod,lok8s.dev/role=old-role"
  local owned_old="lok8s.dev/role=worker,lok8s.dev/owner=${MARK}"
  _lb_targets "[{\"type\":\"label_selector\",\"label_selector\":{\"selector\":\"${dropped}\"},\"use_private_ip\":false},
    {\"type\":\"server\",\"server\":{\"id\":77},\"use_private_ip\":false},
    {\"type\":\"label_selector\",\"label_selector\":{\"selector\":\"${owned_old}\"},\"use_private_ip\":false},
    {\"type\":\"ip\",\"ip\":{\"ip\":\"203.0.113.9\"},\"use_private_ip\":false}]"
  run _provision
  assert_success
  run grep -E '^load-balancer remove-target 14 ' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target 14 --label-selector ${dropped}
load-balancer remove-target 14 --server 77"
  run jq -c '[.[0].targets[] | .label_selector.selector // .server.id // .ip.ip]' "${STORE}/load-balancer.json"
  assert_output "[\"${owned_old}\",\"203.0.113.9\",15,\"lok8s.dev/cluster=prod,lok8s.dev/role=control-plane,lok8s.dev/owner=${MARK}\"]"
}

@test "lb: a failed removal of an unsafe target fails the run and names the target" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  local old="lok8s.dev/cluster=prod,lok8s.dev/role=control-plane"
  _lb_targets "[{\"type\":\"label_selector\",\"label_selector\":{\"selector\":\"${old}\"},\"use_private_ip\":false}]"
  export HCLOUD_FAIL='^load-balancer remove-target'
  run _provision
  assert_failure
  assert_output --partial "lb prod-api: could not remove the target label_selector '${old}'"
  assert_output --partial "It can route to a server of another owner. Remove it with 'hcloud load-balancer remove-target prod-api --label-selector ${old}'"
}

# ── load balancer: a failed check stops the hook ─────────

@test "lb: the audit read reports another owner: no service write after it" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # Another actor gives the LB another owner mark right after the selector
  # add: the next read is the read of the audit.
  export RELABEL_AFTER_CALL="load-balancer 14 ${OTHER} ^load-balancer add-target 14 --label-selector"
  run _provision
  local out="${output}" rc="${status}"
  run grep -E '^load-balancer (add|update)-service ' "${HCLOUD_LOG}"
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "did not change load-balancer 14: it does not have the owner mark of this cluster now"
}

@test "lb: a failed server list of the audit: no service write after it" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export HCLOUD_FAIL='^server list -o json$' HCLOUD_FAIL_AFTER='^load-balancer add-target 14 --label-selector'
  run _provision
  local out="${output}" rc="${status}"
  run grep -E '^load-balancer (add|update)-service ' "${HCLOUD_LOG}"
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "lb prod-api: could not list the servers ('hcloud server list')"
}

@test "lb: each service write reads the load balancer first: a changed owner stops the next one" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  yq -i -o json '.["load-balancer"][0]["#services"] += [{"protocol": "tcp", "listen-port": "80", "destination-port": "80"}]' "${DESC}"
  export RELABEL_AFTER_CALL="load-balancer 14 ${OTHER} ^load-balancer add-service 14"
  run _provision
  local out="${output}" rc="${status}"
  run grep -E '^load-balancer (add|update)-service ' "${HCLOUD_LOG}"
  assert_output "load-balancer add-service 14 --protocol tcp --listen-port 6443 --destination-port 6443"
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "did not change load-balancer 14: it does not have the owner mark of this cluster now"
}

@test "lb: the selector add reads the load balancer first: a changed owner stops it" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export RELABEL_AFTER_CALL="load-balancer 14 ${OTHER} ^load-balancer add-target 14 --server"
  run _provision
  local out="${output}" rc="${status}"
  run grep -E '^load-balancer (add-target|remove-target|add-service|update-service) ' "${HCLOUD_LOG}"
  assert_output "load-balancer add-target 14 --server 15"
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "did not change load-balancer 14: it does not have the owner mark of this cluster now"
}

@test "lb: each removal of the audit reads the load balancer first: a changed owner stops the next one" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  _part server 77 their-cp '{"lok8s.dev/cluster":"prod","lok8s.dev/role":"control-plane"}'
  local dropped="lok8s.dev/cluster=prod,lok8s.dev/role=old-role"
  _lb_targets "[{\"type\":\"label_selector\",\"label_selector\":{\"selector\":\"${dropped}\"},\"use_private_ip\":false},
    {\"type\":\"server\",\"server\":{\"id\":77},\"use_private_ip\":false}]"
  export RELABEL_AFTER_CALL="load-balancer 14 ${OTHER} ^load-balancer remove-target 14"
  run _provision
  local out="${output}" rc="${status}"
  run grep -E '^load-balancer (remove-target|add-service|update-service) ' "${HCLOUD_LOG}"
  assert_output "load-balancer remove-target 14 --label-selector ${dropped}"
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "did not change load-balancer 14: it does not have the owner mark of this cluster now"
}

@test "lb: each retry of a target add reads the load balancer first: a changed owner stops the retries" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # The live target 15 has use-private-ip true; the descriptor wants false:
  # remove, then add with retries. Each add fails; after the first one the
  # LB gets another owner mark.
  _lb_targets '[{"type":"server","server":{"id":15},"use_private_ip":true}]'
  export HETZNER_LB_ADD_DELAY=0
  export HCLOUD_FAIL='^load-balancer add-target 14 --server'
  export RELABEL_AFTER_CALL="load-balancer 14 ${OTHER} ^load-balancer add-target 14 --server"
  run _provision
  local out="${output}" rc="${status}"
  run grep -cE '^load-balancer add-target 14 --server' "${HCLOUD_LOG}"
  assert_output "1"
  run grep -E '^load-balancer (add-target 14 --label-selector|add-service|update-service)' "${HCLOUD_LOG}"
  assert_output ""
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "removed target server '15', and did not add it again: the load balancer or the server changed"
}

@test "lb: each retry of a target add reads the server too: a server that loses the mark stops the retries" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # The live target 15 has use-private-ip true; the descriptor wants false:
  # remove, then add with retries. Each add fails; after the first one the
  # server 15 gets another owner mark.
  _lb_targets '[{"type":"server","server":{"id":15},"use_private_ip":true}]'
  export HETZNER_LB_ADD_DELAY=0
  export HCLOUD_FAIL='^load-balancer add-target 14 --server'
  export RELABEL_AFTER_CALL="server 15 ${OTHER} ^load-balancer add-target 14 --server"
  run _provision
  local out="${output}" rc="${status}"
  run grep -cE '^load-balancer add-target 14 --server' "${HCLOUD_LOG}"
  assert_output "1"
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "did not change server 15: it does not have the owner mark of this cluster now"
  assert_output --partial "removed target server '15', and did not add it again: the load balancer or the server changed"
}

# ── the outcome after a failure ──────────────────────────

# _provision_real: provider::provision itself (it writes the state dump).
_provision_real() {
  provider::provision "${DESC}" "${BATS_TEST_TMPDIR}/work"
}

@test "outcome: a failed list after a network create reports the partial result, not 'Nothing was created or changed'" {
  export LIST_FAILS_AFTER_CREATE=network
  run _provision_real
  assert_failure
  assert_output --partial "hetzner: could not list the network parts ('hcloud network list'). This run made changes before it stopped"
  refute_output --partial "Nothing was created or changed"
  assert_output --partial "the provision stopped after it made changes, so the result is partial"
  # ids of the fake: ssh-key 100, floating-ip 101, network 102.
  assert_output --partial "created ssh-key prod-key (id 100)"
  assert_output --partial "created floating-ip prod-ip (id 101)"
  assert_output --partial "created network prod-net (id not known yet)"
  assert_output --partial "the ids that this run knows are in ${BATS_TEST_TMPDIR}/work/hetzner.dump.json"
  run jq -c '[.["ssh-key"][0].id, .["floating-ip"][0].id]' "${BATS_TEST_TMPDIR}/work/hetzner.dump.json"
  assert_output "[100,101]"
}

@test "outcome: a create whose answer is lost is reported as not confirmed, and the next run adopts the part" {
  export HCLOUD_LOST='^network create'
  run _provision_real
  assert_failure
  assert_output --partial "could not create network 'prod-net', or Hetzner did not confirm it"
  assert_output --partial "not confirmed: created network prod-net? hcloud exit 1"
  refute_output --partial "Nothing was created or changed"
  [[ -f "${BATS_TEST_TMPDIR}/work/hetzner.dump.json" ]]
  # The network exists with the mark: the next run adopts it and adds the subnet.
  unset HCLOUD_LOST
  : > "${HCLOUD_LOG}"
  run _provision_real
  assert_success
  run grep -cE '^network create ' "${HCLOUD_LOG}"
  assert_output "0"
  run grep -E '^network add-subnet ' "${HCLOUD_LOG}"
  assert_output --regexp '^network add-subnet 102 '
}

# ── a failed write fails its step ────────────────────────

@test "write: a failed add-subnet fails the network step, and the changes are listed" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export HCLOUD_FAIL='^network add-subnet'
  run _provision
  assert_failure
  assert_output --partial "hetzner: network prod-net: could not add the subnet 10.0.1.0/24: hcloud: network add-subnet failed (server_error)"
  assert_output --partial "hetzner network failed"
  assert_output --partial "the provision stopped after it made changes, so the result is partial"
  assert_output --partial "not confirmed (hcloud exit 1; it can be done or not): hcloud network add-subnet 13 "
  run _writes
  refute_output --partial "load-balancer"
}

@test "write: a failed floating-IP assign fails the server step, and the changes are listed" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export HCLOUD_FAIL='^floating-ip assign'
  run _provision
  assert_failure
  assert_output --partial "hetzner: server cp-0: could not assign floating-ip 12: hcloud: floating-ip assign failed (server_error)"
  assert_output --partial "hetzner server failed"
  assert_output --partial "not confirmed (hcloud exit 1; it can be done or not): hcloud floating-ip assign 12 15"
}

@test "write: a failed LB attach fails the load-balancer step before any target, and the changes are listed" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export HCLOUD_FAIL='^load-balancer attach-to-network'
  run _provision
  local out="${output}" rc="${status}"
  run _writes
  refute_output --partial "add-target"
  refute_output --partial "add-service"
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "lb prod-api: could not attach the load balancer to network 'prod-net' (id 13)"
  assert_output --partial "hetzner load-balancer failed"
  assert_output --partial "not confirmed (hcloud exit 1; it can be done or not): hcloud load-balancer attach-to-network 14 --network 13"
}

@test "write: a lost answer of the LB attach fails the step too: only an answer that it exists is checked by a new read" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  # The attach is done, but hcloud fails (no answer): a new read shows the
  # network, and the step fails all the same.
  export HCLOUD_LOST='^load-balancer attach-to-network'
  run _provision
  local out="${output}" rc="${status}"
  run _writes
  refute_output --partial "add-target"
  refute_output --partial "add-service"
  output="${out}" status="${rc}"
  assert_failure
  assert_output --partial "lb prod-api: could not attach the load balancer to network 'prod-net' (id 13): hcloud: timeout awaiting response headers"
  refute_output --partial "A new read does not show the network"
}

@test "write: a failed add-service fails the load-balancer step, and the changes are listed" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export HCLOUD_FAIL='^load-balancer add-service'
  run _provision
  assert_failure
  assert_output --partial "lb prod-api: could not add service tcp:6443 -> 6443: hcloud: load-balancer add-service failed (server_error)"
  assert_output --partial "hetzner load-balancer failed"
  assert_output --partial "not confirmed (hcloud exit 1; it can be done or not): hcloud load-balancer add-service 14 "
}

@test "write: an answer that the subnet exists is accepted only when a new read shows the subnet" {
  _all_parts "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  export HCLOUD_ERR='hcloud: the subnet already exists (conflict)'
  # The add fails with "already", and the network has no such subnet.
  export HCLOUD_FAIL='^network add-subnet'
  run _provision
  assert_failure
  assert_output --partial "hetzner: network prod-net: could not add the subnet 10.0.1.0/24: hcloud: the subnet already exists (conflict)"
  # The add is done, the answer says "already": a new read shows the subnet.
  unset HCLOUD_FAIL
  export HCLOUD_LOST='^network add-subnet'
  : > "${HCLOUD_LOG}"
  run _provision
  assert_success
  assert_output --partial "10.0.1.0/24"$'\033[0m (it exists)'
  run jq -c '.[0].subnets' "${STORE}/network.json"
  assert_output '[{"ip_range":"10.0.1.0/24"}]'
}

# ── the fake's label selector ────────────────────────────

@test "fake: hcloud list takes only the parts with each term of -l (a comma is AND), and fails on another form" {
  _part server 31 a '{"lok8s.dev/cluster":"prod","lok8s.dev/role":"control-plane"}'
  _part server 32 b '{"lok8s.dev/cluster":"prod"}'
  _part server 33 c '{"lok8s.dev/cluster":"other","lok8s.dev/role":"control-plane"}'
  run hcloud server list -o json
  assert_output --partial '"id":33'
  run hcloud server list -o json -l lok8s.dev/cluster=prod
  assert_success
  assert_output "$(jq -c '[.[] | select(.id == 31 or .id == 32)]' "${STORE}/server.json")"
  run hcloud server list -o json -l lok8s.dev/cluster=prod,lok8s.dev/role=control-plane
  assert_success
  assert_output "$(jq -c '[.[] | select(.id == 31)]' "${STORE}/server.json")"
  run hcloud server list -o json -l 'lok8s.dev/cluster!=prod'
  assert_failure
  assert_output --partial "fake hcloud: unsupported selector term"
}

# ── destroy ──────────────────────────────────────────────

# _destroy_parts: two parts with the cluster label and this mark, one with
# the cluster label and the mark of the same cluster_name in another domain,
# one with the cluster label and no mark, and one with this mark and another
# cluster label (only a broken selector lists it).
_destroy_parts() {
  _part server 21 cp-0 "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  _part server 22 cp-0 "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${OTHER}\"}"
  _part server 23 old-0 '{"lok8s.dev/cluster":"prod"}'
  _part volume 24 prod-data "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  _part volume 25 other-data "{\"lok8s.dev/cluster\":\"other\",\"lok8s.dev/owner\":\"${MARK}\"}"
}

@test "destroy: the plan and the destroy take only the parts with the cluster label and this mark; the others are skipped with the reason" {
  _destroy_parts
  local plan
  plan="$(provider::destroy_plan "${DESC}" "${BATS_TEST_TMPDIR}" 2>"${BATS_TEST_TMPDIR}/plan.err")"
  [ "${plan}" = "$(printf 'cluster\tprod\nserver\t21\tcp-0\nvolume\t24\tprod-data')" ]
  run cat "${BATS_TEST_TMPDIR}/plan.err"
  assert_output --partial "hetzner: skipped server 22 cp-0: it has lok8s.dev/cluster=prod, but the owner mark of another cluster (lok8s.dev/owner=${OTHER}; this cluster: ${MARK}). lok8s does not delete it."
  assert_output --partial "hcloud server add-label --overwrite 22 lok8s.dev/owner=${MARK}"
  assert_output --partial "hetzner: skipped server 23 old-0: it has lok8s.dev/cluster=prod, but not the owner mark of this cluster (lok8s.dev/owner=${MARK}). lok8s does not delete a part that can belong to another person or cluster."
  assert_output --partial "('hcloud server describe 23'), then mark it one time: hcloud server add-label 23 lok8s.dev/owner=${MARK}."
  refute_output --partial "skipped server 21"
  refute_output --partial "skipped volume 24"
  refute_output --partial " 25 "
  # Every list is scoped to the cluster label; the plan deletes nothing.
  run grep -vc -- ' list -o json -l lok8s.dev/cluster=prod$' "${HCLOUD_LOG}"
  assert_output "0"

  : > "${HCLOUD_LOG}"
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  assert_output --partial "hetzner: skipped server 22 cp-0"
  assert_output --partial "hetzner: skipped server 23 old-0"
  run _writes
  assert_output "server delete 21
volume delete 24"
}

# The warning for a part with the owner mark of another cluster shows its
# name through shown_name, as the other warnings do: the ESC, the BEL and
# the U+202E (right-to-left override) of the name are '?', so the name
# cannot set the terminal title or turn the rest of the line around. The
# kept line shows the same name.
@test "destroy: the warning for a part with the mark of another cluster shows its name safely" {
  _part server 21 cp-0 "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}"
  _part server 22 $'cp\e]0;x\a-\xe2\x80\xae0' "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${OTHER}\"}"
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  assert_line --partial "[warn] hetzner: skipped server 22 cp?]0;x?-?0: it has lok8s.dev/cluster=prod, but the owner mark of another cluster (lok8s.dev/owner=${OTHER}; this cluster: ${MARK}). lok8s does not delete it."
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod that does not have the owner mark of this cluster (1 server): server 22 'cp?]0;x?-?0'. Delete it by hand only if it is yours and you want it gone." ]
  [ -z "$(tr -d '[:print:]\n' <<<"${output}")" ]
  run _writes
  assert_output "server delete 21"
}

@test "destroy: a confirmed plan that names a part of another domain or of another cluster deletes only the marked part" {
  _destroy_parts
  export LOK8S_DESTROY_PLAN=$'cluster\tprod\nserver\t21\tcp-0\nserver\t22\tcp-0\nvolume\t25\tother-data'
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  # The plan names 22 as a part to delete, but 22 has the mark of another
  # domain: it is kept, and one warning names it with the reason.
  assert_output --partial "hetzner: skipped server 22 cp-0: it has lok8s.dev/cluster=prod, but the owner mark of another cluster (lok8s.dev/owner=${OTHER}; this cluster: ${MARK}). lok8s does not delete it."
  [ "$(grep -c 'skipped server 22' <<<"${output}")" -eq 1 ]
  assert_output --partial "hetzner: skipped volume 25 of the confirmed plan: no volume with this id carries lok8s.dev/cluster=prod and the owner mark of this cluster now."
  assert_output --partial "hetzner: kept volume 24 prod-data (lok8s.dev/cluster=prod): it is not in the confirmed plan."
  # The plan showed a warning for each part without the mark: the destroy
  # of the confirmed plan does not repeat it for a part that the plan did
  # not name (23), and its last line counts them.
  refute_output --partial "skipped server 23"
  assert_output --partial "[warn] lo destroy: kept 2 resources with lok8s.dev/cluster=prod that do not have the owner mark of this cluster (2 servers): server 22 'cp-0', server 23 'old-0'."
  run _writes
  assert_output "server delete 21"
}

# A part that lost its mark after the plan was shown in the plan as a part
# to delete, not as a kept part: the destroy keeps it, a warning names it
# with the reason, and the last line names it with the other kept parts.
@test "destroy: a part of the confirmed plan that lost its mark after the plan is kept, and a warning names it" {
  _destroy_parts
  local plan
  plan="$(provider::destroy_plan "${DESC}" "${BATS_TEST_TMPDIR}" 2>/dev/null)"
  [ "${plan}" = "$(printf 'cluster\tprod\nserver\t21\tcp-0\nvolume\t24\tprod-data')" ]
  _fake_set server 21 '.labels |= del(."lok8s.dev/owner")'
  export LOK8S_DESTROY_PLAN="${plan}"
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  assert_output --partial "hetzner: skipped server 21 cp-0: it has lok8s.dev/cluster=prod, but not the owner mark of this cluster (lok8s.dev/owner=${MARK})."
  [ "$(grep -c 'skipped server 21' <<<"${output}")" -eq 1 ]
  # The plan showed 22 and 23 as kept parts: no repeat.
  refute_output --partial "skipped server 22"
  refute_output --partial "skipped server 23"
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 3 resources with lok8s.dev/cluster=prod that do not have the owner mark of this cluster (3 servers): server 21 'cp-0', server 22 'cp-0', server 23 'old-0'. Delete them by hand only if they are yours and you want them gone." ]
  run _writes
  assert_output "volume delete 24"
}

# A destroy that keeps parts (the cluster label, no mark or the mark of
# another cluster) succeeds: the skip is the safety feature. Its last line
# says how many parts it kept and of which kinds, and names each part, in
# the delete order.
@test "destroy: the last line counts the kept parts by kind and names each one, and the destroy exits 0" {
  _destroy_parts
  _part network 26 old-net '{"lok8s.dev/cluster":"prod"}'
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  [ "${lines[${#lines[@]}-1]}" = "[warn] lo destroy: kept 3 resources with lok8s.dev/cluster=prod that do not have the owner mark of this cluster (2 servers, 1 network): server 22 'cp-0', server 23 'old-0', network 26 'old-net'. Delete them by hand only if they are yours and you want them gone." ]
  [ "$(grep -c 'lo destroy: kept' <<<"${output}")" -eq 1 ]
  run _writes
  assert_output "server delete 21
volume delete 24"

  # One kept part: the singular. The plan prints no summary (it deletes
  # nothing).
  echo '[]' > "${STORE}/server.json"
  echo '[]' > "${STORE}/volume.json"
  run provider::destroy_plan "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  refute_output --partial "lo destroy: kept"
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_success
  assert_output --partial "[warn] lo destroy: kept 1 resource with lok8s.dev/cluster=prod that does not have the owner mark of this cluster (1 network): network 26 'old-net'. Delete it by hand only if it is yours and you want it gone."
}

@test "destroy: no domain for the mark stops the plan and the destroy before any hcloud call" {
  _destroy_parts
  unset PROVIDER_DOMAIN
  run provider::destroy_plan "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_failure
  assert_output --partial "PROVIDER_DOMAIN is empty"
  refute_output --partial "cluster	prod"
  run provider::destroy "${DESC}" "${BATS_TEST_TMPDIR}"
  assert_failure
  run cat "${HCLOUD_LOG}"
  assert_output ""
}

# provider::status counts the servers with the cluster label, and says how
# many of them do not have the owner mark of this cluster: lo destroy keeps
# those, and the inventory leaves them out.
@test "status: the status says how many servers do not have the owner mark of this cluster" {
  _part server 21 cp-0 "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${MARK}\"}" '{"status":"running"}'
  run provider::status "${DESC}"
  assert_success
  assert_output "Running"

  # One server without the mark: the singular.
  _fake_set server 21 '.labels |= del(."lok8s.dev/owner")'
  run provider::status "${DESC}"
  assert_output "Running (1 of 1 server without the owner mark of this cluster)"
  _fake_set server 21 ".labels.\"lok8s.dev/owner\" = \"${MARK}\""

  _part server 22 cp-1 "{\"lok8s.dev/cluster\":\"prod\",\"lok8s.dev/owner\":\"${OTHER}\"}" '{"status":"running"}'
  _part server 23 old-0 '{"lok8s.dev/cluster":"prod"}' '{"status":"off"}'
  run provider::status "${DESC}"
  assert_success
  assert_output "Partial (2/3 running; 2 of 3 servers without the owner mark of this cluster)"

  _fake_set server 23 '.status = "running"'
  run provider::status "${DESC}"
  assert_output "Running (2 of 3 servers without the owner mark of this cluster)"

  # No domain: the mark is not known, and the status says so.
  unset PROVIDER_DOMAIN
  run provider::status "${DESC}"
  assert_success
  assert_output "Running (the owner mark of this cluster is not known)"

  # A list that does not apply its selector gives a server of another
  # cluster too: the status counts only the servers with the cluster label
  # of this cluster.
  export PROVIDER_DOMAIN="prod.example.test"
  hcloud() {
    [[ "$1 $2" == "server list" ]] || return 1
    printf '%s\n' "[{\"id\":23,\"name\":\"old-0\",\"status\":\"running\",\"labels\":{\"lok8s.dev/cluster\":\"prod\"}},{\"id\":31,\"name\":\"other-0\",\"status\":\"off\",\"labels\":{\"lok8s.dev/cluster\":\"other\",\"lok8s.dev/owner\":\"${MARK}\"}}]"
  }
  run provider::status "${DESC}"
  assert_success
  assert_output "Running (1 of 1 server without the owner mark of this cluster)"
}

# ── the standalone hetzner command ───────────────────────

# The command runs through the real argsh parser, with its flags: a stub of
# :args would set the variables itself, so it would test none of the flag
# table of main::hetzner (an earlier table could not parse --domain). The
# --domain flag has the type domain, so the domain needs its cluster spec.
# provider::validate and provider::provision are stubs; hcloud must not run.
@test "standalone: the hetzner command takes the domain of the mark from --domain, and refuses --hetzner-json alone" {
  [ -x "${_PROJECT_ROOT}/.bin/argsh" ] ||
    skip "needs the argsh binary at .bin/argsh (install it with b); this test does not download it"
  export PATH_CLUSTERS="${BATS_TEST_TMPDIR}/clusters"
  printf 'kind: KubeOne\nmetadata:\n  name: prod\n' > "${PATH_CLUSTERS}/prod.example.test/cluster.lok8s.yaml"
  local calls="${BATS_TEST_TMPDIR}/calls" script='
    source "${PATH_BIN}/argsh"
    source "${PATH_LOK8S}/providers/hetzner/main"
    hcloud() { echo "hcloud must not run: $*" >&2; return 1; }
    provider::validate() { return 0; }
    provider::provision() { printf "provision %s %s\n" "${PROVIDER_DOMAIN:-}" "${1}" >> "${CALLS}"; }
    main::hetzner "$@"'

  run env -u PROVIDER_DOMAIN PATH_BIN="${_PROJECT_ROOT}/.bin" CALLS="${calls}" \
    bash -c "${script}" hetzner --hetzner-json "${DESC}"
  assert_failure
  assert_output --partial "hetzner: --domain is missing. lok8s needs the domain of the cluster for the owner mark lok8s.dev/owner of its parts, and --hetzner-json does not give it."
  [ ! -e "${calls}" ]

  run env -u PROVIDER_DOMAIN PATH_BIN="${_PROJECT_ROOT}/.bin" CALLS="${calls}" \
    bash -c "${script}" hetzner --domain prod.example.test
  assert_success
  run cat "${calls}"
  assert_output "provision prod.example.test ${DESC}"

  # A flag that the table does not have is refused by the parser.
  rm -f "${calls}"
  run env -u PROVIDER_DOMAIN PATH_BIN="${_PROJECT_ROOT}/.bin" CALLS="${calls}" \
    bash -c "${script}" hetzner --domian prod.example.test
  assert_failure
  [ ! -e "${calls}" ]
}
