# shellcheck shell=bash disable=SC2016,SC2034
# SC2016: single-quoted $ here is jq/yq/awk program text or a remote-script literal
# json.sh — Hetzner JSON descriptor engine
# JSON helpers, lookup, and generic resource create loop.
# Operates on the global CLOUD_HETZNER_JSON state.

# Log file for all provider operations (set by provider::provision)
: "${CLOUD_LOG_FILE:="/tmp/lok8s-hetzner.log"}"

# ── Output helpers ─────────────────────────────────────

_hetzner_log() {
  echo "[$(date -Iseconds)] ${*}" >> "${CLOUD_LOG_FILE}"
}

_hetzner_print() {
  _hetzner_log "${*}"
  if [[ -z "${CLOUD_QUIET:-}" ]]; then
    echo -e "${*}"
  fi
}

# ── Dry-run helpers ──────────────────────────────────────

# dry-run::cmd <command...>
# Runs a write (or prints it in a dry run). Every hcloud write that runs is
# added to the file _HETZNER_WRITES ("<rc><TAB><command>"), when it is set:
# after a failed step, hetzner::_outcome lists what this run changed. A file,
# not a variable: many writes run in a $(...) subshell.
dry-run::cmd() {
  _hetzner_log "cmd: ${*}"
  if [[ -n "${CLOUD_DRY_RUN}" ]]; then
    mkdir -p "${CLOUD_DRY_RUN_PATH}"
    echo "${@}" | tee -a "${CLOUD_DRY_RUN_PATH}/dry-run-cmd.log"
  else
    local rc=0
    "${@}" || rc=$?
    [[ -z "${_HETZNER_WRITES:-}" || "${1}" != "hcloud" ]] ||
      printf '%s\t%s\n' "${rc}" "${*}" >> "${_HETZNER_WRITES}"
    return "${rc}"
  fi
}

dry-run::cloud-config() {
  if [[ -n "${CLOUD_DRY_RUN}" ]]; then
    mkdir -p "${CLOUD_DRY_RUN_PATH}/cloud-config"
    echo -n "${2}" > "${CLOUD_DRY_RUN_PATH}/cloud-config/${1}.conf"
  fi
  echo -n "${2}"
}

# ── JSON helpers ─────────────────────────────────────────

# The descriptor keys that refer to another part of the project (an hcloud
# create flag that takes an "ID or name"), and the kind of that part. A key
# is read without its "#..." suffix: the create loop strips the suffix for
# the flag ("server#2" is --server). The create loop gives hcloud only the
# verified id of a part with this cluster's owner mark (hetzner::_ref_ids).
declare -gA _HETZNER_REF_KIND=(
  [ssh-key]=ssh-key [network]=network [server]=server [volume]=volume
  [placement-group]=placement-group [firewall]=firewall
  [primary-ipv4]=primary-ip [primary-ipv6]=primary-ip
)

# Of these, the keys on which a number is an index into the top-level list of
# that kind, and the create loop replaces it with that part's id
# (`"ssh-key": [0]`, `"network": 0`, `"server": 1` on a volume). A number on
# any other key is a literal and passes verbatim (`"size": 100` on a volume,
# `"image": 12345` for a snapshot, `"primary-ipv4": 123` is an id). Before,
# every number was dereferenced, and `"size": 100` reached hcloud as
# `--size null`.
_HETZNER_REF_KEYS='["ssh-key","network","server","volume","placement-group","firewall"]'

# The kinds that the provision creates, in the order of its steps.
_HETZNER_CREATE_KINDS=(ssh-key floating-ip network load-balancer server volume)

# The form of a descriptor key that becomes an hcloud flag: the flag name
# (a-z, 0-9 and "-"), with an optional suffix "#..." that only makes the key
# unique ("server#2" is --server). A key that starts with "#" is a note for
# the hooks, not a flag. The flag name must also be on the list of its kind
# (_HETZNER_FLAGS).
_HETZNER_KEY_RE='^[a-z0-9][a-z0-9-]*(#[A-Za-z0-9._-]*)?$'

# The flags of 'hcloud <kind> create' that a descriptor part can give, per
# kind, as "<flag>:<type>". Any other key is refused before the first create.
# The types (pflag): s string, i int, n IP range, b bool, l list
# (StringSlice: one comma-separated value, repeated flags append), a
# repeated value (StringArray), m labels (StringToString). The lists are
# the create flags of hcloud CLI v1.70.2 (github.com/hetznercloud/cli):
#   ssh-key        internal/cmd/sshkey/create.go:22-29
#   floating-ip    internal/cmd/floatingip/create.go:25-41
#   network        internal/cmd/network/create.go:24-34
#   load-balancer  internal/cmd/loadbalancer/create.go:25-54
#   server         internal/cmd/server/create.go:49-97
#   volume         internal/cmd/volume/create.go:26-45
# Not on any list: the flags of the hcloud command itself. These are the
# persistent flags of the root command (internal/cli/root.go:81 adds each
# option of internal/state/config/options.go:84-188 that has a flag: config,
# context, endpoint, hetzner-endpoint, debug, debug-file,
# no-experimental-warnings, poll-interval, http-timeout, quiet), the output
# flag of each create (internal/cmd/base/create.go:30) and the help flag of
# cobra. They change how hcloud runs and which project it calls, and a
# boolean one (quiet, debug) reads no value: its descriptor value became a
# flag of its own ("quiet": "--network=999" gave --network=999). lok8s does
# not create firewalls, placement groups and primary IPs, so these kinds
# have no list.
declare -gA _HETZNER_FLAGS=(
  [ssh-key]="name:s public-key:s public-key-from-file:s label:m"
  [floating-ip]="type:s description:s name:s home-location:s server:s label:m enable-protection:l"
  [network]="name:s ip-range:n expose-routes-to-vswitch:b label:m enable-protection:l"
  [load-balancer]="name:s type:s algorithm-type:s location:s network-zone:s label:m enable-protection:l network:s primary-ipv4:s primary-ipv6:s"
  [server]="name:s type:s image:s location:s ssh-key:l label:m user-data-from-file:a start-after-create:b volume:l network:l firewall:l automount:b allow-deprecated-image:b placement-group:s primary-ipv4:s primary-ipv6:s without-ipv4:b without-ipv6:b enable-protection:l enable-backup:b"
  [volume]="name:s server:s location:s size:i automount:b format:s label:m enable-protection:l"
)

# hetzner::_flag_type <kind> <flag>: prints the type of <flag> on the list
# of 'hcloud <kind> create' (_HETZNER_FLAGS). rc 1 when it is not on it.
hetzner::_flag_type() {
  local e
  for e in ${_HETZNER_FLAGS[${1}]:-}; do
    if [[ "${e%%:*}" == "${2}" ]]; then
      printf '%s\n' "${e##*:}"
      return 0
    fi
  done
  return 1
}

# hetzner::_flag_names <kind>: the flags on the list of <kind>, comma-separated.
hetzner::_flag_names() {
  local e out=""
  for e in ${_HETZNER_FLAGS[${1}]:-}; do
    out+="${out:+, }${e%%:*}"
  done
  printf '%s\n' "${out}"
}

# jq: true when a JSON value holds a string with a control character
# (U+0000 to U+001F or U+007F). hcloud gets a value as it is, and the
# rebuild plan read a tab as a field separator.
_HETZNER_JQ_CTRL='[.. | strings | explode[] | select(. < 32 or . == 127)] | length > 0'

# jq: true when a JSON string starts or ends with white space.
_HETZNER_JQ_EDGE_SPACE='type == "string" and test("\\A\\s|\\s\\z")'

# The flags of one create, made by hetzner::_part_args from the values that
# its checks passed, and the checked value of each descriptor key.
_HETZNER_ARGS=()
declare -gA _HETZNER_ARGVAL=()

# jq definition: the hcloud value of one descriptor entry (.key, .value).
# $root is the whole descriptor, $refs is _HETZNER_REF_KEYS. The key is read
# without its "#..." suffix. A list on a reference key becomes the
# comma-separated ids; a list on any other key becomes its comma-separated
# values. A number that is no index of the list gives "", any other element
# stays as it is: the conversion never fails on a value. For a cloud create,
# the create loop replaces the value of every reference key with the
# verified ids (hetzner::_ref_ids).
_HETZNER_JQ_VALUE='
  def hcloud_value($root; $refs):
    (.key | sub("#.*$"; "")) as $key
    | def ref: if type == "number" then ((try ($root[$key][.].id) catch null) // "" | tostring) else . end;
    if any($refs[]; . == $key) | not then
        (if (.value | type) == "array" then .value | map(tostring) | join(",") else .value end)
      elif (.value | type) == "array" then
        [ .value[] | ref | tostring ] | join(",")
      else
        .value | ref
      end;'

hetzner::json() {
  jq "${@}" <(echo "${CLOUD_HETZNER_JSON}")
}

hetzner::json::edit() {
  CLOUD_HETZNER_JSON="$(hetzner::json "${@}")"
}

hetzner::json::loop() {
  local what="${1}"
  [[ "${what:0:1}" == "." ]] ||
    what=".[\"${what}\"]"
  local fields="${2:-".id, .name"}"
  hetzner::json -r "${what} | keys[] as \$i | .[\$i] | [\$i, ${fields}] | @csv" | tr -d '"'
}

# ── Owner mark ───────────────────────────────────────────

# The label that marks a part as made by lok8s for ONE cluster. hetzner::create
# sets it on every part it creates. lok8s adopts, changes or uses an existing
# part only when the part has this label with the value of this cluster
# (hetzner::owner_mark). A part with the same name and no mark, or the mark of
# another cluster, can belong to another person or cluster in the same Hetzner
# project: the lookup refuses it.
_HETZNER_OWNER_KEY='lok8s.dev/owner'

# hetzner::_sha256: the hex sha256 of stdin.
hetzner::_sha256() {
  local sum
  if command -v sha256sum >/dev/null 2>&1; then
    sum="$(sha256sum)" || return 1
  elif command -v shasum >/dev/null 2>&1; then
    sum="$(shasum -a 256)" || return 1
  else
    return 1
  fi
  printf '%s\n' "${sum%% *}"
}

# hetzner::owner_mark [<cluster_name>]
# The owner mark value of this cluster: the first 32 hex characters of the
# sha256 of "<domain>\n<cluster_name>\n". The domain (PROVIDER_DOMAIN, set by
# provider::write_config) separates two projects that use the same cluster
# name in one Hetzner project. The cluster name separates two specs of one
# domain. 32 lowercase hex characters are a valid Hetzner label value (at most
# 63 characters, first and last alphanumeric). Without an argument, the
# cluster name is .cluster_name of CLOUD_HETZNER_JSON.
hetzner::owner_mark() {
  local cluster="${1-}" domain="${PROVIDER_DOMAIN:-}" sum
  [[ -n "${cluster}" ]] ||
    cluster="$(hetzner::json -r '.cluster_name // ""' 2>/dev/null)" || cluster=""
  if [[ -z "${domain}" ]]; then
    error "hetzner: the domain of the cluster is not known (PROVIDER_DOMAIN is empty). lok8s needs it for the owner mark ${_HETZNER_OWNER_KEY}. Run the provider through lo (lo provision, lo recover): lo sets it."
    return 1
  fi
  if [[ "${domain}" == "." || "${domain}" == ".." || "${domain}" == "/" ]]; then
    error "hetzner: the domain of the cluster is '${domain}', which is not a domain name (PROVIDER_DOMAIN is the name of the directory of the cluster spec). lok8s needs the domain for the owner mark ${_HETZNER_OWNER_KEY}. Keep the cluster spec at clusters/<domain>/cluster.lok8s.yaml."
    return 1
  fi
  if [[ -z "${cluster}" ]]; then
    error "hetzner: the descriptor has no cluster_name. lok8s needs it for the owner mark ${_HETZNER_OWNER_KEY}. Set cluster_name in the descriptor."
    return 1
  fi
  sum="$(printf '%s\n%s\n' "${domain}" "${cluster}" | hetzner::_sha256)" || sum=""
  if [[ ! "${sum}" =~ ^[0-9a-f]{64}$ ]]; then
    error "hetzner: cannot calculate the owner mark: no sha256sum or shasum found. Install coreutils."
    return 1
  fi
  printf '%s\n' "${sum:0:32}"
}

# ── Lookup ───────────────────────────────────────────────

# hetzner::lookup [<kind>]
# Writes into the descriptor the hcloud id of each existing part that has a
# descriptor name AND the owner mark of this cluster. A part with a descriptor
# name and no mark (or another cluster's mark) is refused: the lookup reports
# every such part of all kinds, with the one-time command that marks it, and
# fails before anything is created or changed. An id that the descriptor
# itself carries is dropped: only a verified id stays.
hetzner::lookup() {
  local resources="${1-}"
  [[ -n "${resources}" ]] ||
    mapfile -t resources < <(hetzner::json -r '[to_entries[] | select(.value | type == "array") | .key] | .[]')

  local mark
  mark="$(hetzner::owner_mark)" || return 1

  local resource remote rows rid rname rmark
  local -a refused=()
  for resource in "${resources[@]}"; do
    _hetzner_print "☁️  fetching \033[3m${resource}\033[0m..."

    # A list that fails must not read as "no part exists".
    if ! remote="$(hcloud "${resource}" list -o json)" ||
       ! jq -e 'type == "array" or type == "null"' >/dev/null 2>&1 <<<"${remote}"; then
      error "hetzner: could not list the ${resource} parts ('hcloud ${resource} list'). $(hetzner::_changes_note) Check the hcloud token and the API, then run the provision again."
      return 1
    fi

    # The parts with a descriptor name and without this cluster's mark. The
    # name is only shown (shown_name: a backslash or a control character
    # becomes '?'); the id names the part.
    # shellcheck disable=2016
    rows="$(hetzner::json -r --argjson remote "${remote}" --arg field "${resource}" \
      --arg key "${_HETZNER_OWNER_KEY}" --arg mark "${mark}" "${_HETZNER_JQ_SHOWN_NAME}"'
      [ .[$field][]? | .name ] as $names
      | ($remote // [])[]
      | select(.name as $n | any($names[]; . == $n))
      | select((.labels // {})[$key] != $mark)
      | [ (.id | tostring), (.name | shown_name), ((.labels // {})[$key] // "") ] | @tsv')" || return 1
    while IFS=$'\t' read -r rid rname rmark; do
      [[ -n "${rid}" ]] || continue
      refused+=("${resource}"$'\t'"${rid}"$'\t'"${rname}"$'\t'"${rmark}")
    done <<<"${rows}"

    # The id of the part with the name and this cluster's mark; no other id.
    # shellcheck disable=2016
    hetzner::json::edit --argjson remote "${remote}" --arg field "${resource}" \
      --arg key "${_HETZNER_OWNER_KEY}" --arg mark "${mark}" '
      .[$field] = [ .[$field][] | .name as $name | del(.id)
        + (first(($remote // [])[]
            | select(.name == $name and (.labels // {})[$key] == $mark) | {id}) // {}) ]' || return 1
  done

  (( ${#refused[@]} > 0 )) || return 0
  local row kind
  for row in "${refused[@]}"; do
    IFS=$'\t' read -r kind rid rname rmark <<<"${row}"
    if [[ -z "${rmark}" ]]; then
      error "hetzner: refused ${kind} '${rname}' (id ${rid}): it exists, but it does not have the owner mark of this cluster (${_HETZNER_OWNER_KEY}=${mark}). lok8s does not use or change a part that can belong to another person or cluster. lok8s found it by its name only. If it is a part of this cluster (an older lok8s made it), first make sure that id ${rid} is yours ('hcloud ${kind} describe ${rid}'), then mark it one time: hcloud ${kind} add-label ${rid} ${_HETZNER_OWNER_KEY}=${mark}. If it is not, give the ${kind} another name in the descriptor."
    else
      error "hetzner: refused ${kind} '${rname}' (id ${rid}): it has the owner mark of another cluster (${_HETZNER_OWNER_KEY}=${rmark}; this cluster: ${mark}). Give the ${kind} another name in the descriptor. Replace the mark only when you know that the mark ${rmark} is yours (for example, the mark of this cluster under its old domain; the guide shows how to calculate it): hcloud ${kind} add-label --overwrite ${rid} ${_HETZNER_OWNER_KEY}=${mark}."
    fi
  done
  error "hetzner: ${#refused[@]} existing part(s) do not have the owner mark of this cluster. $(hetzner::_changes_note) Do the step above for each part, then run the provision again."
  return 1
}

# ── References ───────────────────────────────────────────

# The lists that hetzner::_list read, per kind. Reset before each use.
declare -gA _HETZNER_LISTS=()
# The refusals that hetzner::_ref_ids and hetzner::_image_ok add.
_HETZNER_REFUSED=()

# hetzner::_changes_note
# The end of a message that stops the provision. "Nothing was created or
# changed." is true only while this run made no write (_HETZNER_WRITES).
hetzner::_changes_note() {
  if [[ -n "${_HETZNER_WRITES:-}" && -s "${_HETZNER_WRITES}" ]]; then
    printf '%s' "This run made changes before it stopped: the list of the changes follows."
  else
    printf '%s' "Nothing was created or changed."
  fi
}

# hetzner::_create_order <kind>: the step number of <kind> in the provision.
# rc 1 when the provision does not create that kind.
hetzner::_create_order() {
  local k n=0
  for k in "${_HETZNER_CREATE_KINDS[@]}"; do
    [[ "${k}" != "${1}" ]] || { printf '%s\n' "${n}"; return 0; }
    n=$(( n + 1 ))
  done
  return 1
}

# hetzner::_list <kind>
# REPLY: the parts of <kind> ('hcloud <kind> list -o json'), read one time
# until _HETZNER_LISTS is reset. rc 1 when hcloud fails or its answer is not
# a JSON list. Do not call it in a $(...): the cache is a variable.
hetzner::_list() {
  local kind="${1}" out
  REPLY=""
  if [[ -n "${_HETZNER_LISTS[${kind}]+set}" ]]; then
    REPLY="${_HETZNER_LISTS[${kind}]}"
    return 0
  fi
  out="$(hcloud "${kind}" list -o json)" || return 1
  jq -e 'type == "array" or type == "null"' >/dev/null 2>&1 <<<"${out}" || return 1
  _HETZNER_LISTS["${kind}"]="${out}"
  REPLY="${out}"
}

# hetzner::_ref_match <kind> <value> <list-json> <mark>
# REPLY: the id of the ONE part of the list that <value> names, in the order
# that hcloud uses: an all-digit value is the part with that id, and only
# when no part has that id, the part with that name; another value is the
# part with that name. The part must have the owner mark of this cluster.
# rc 1 with the reason in REPLY.
hetzner::_ref_match() {
  local kind="${1}" value="${2}" list="${3}" mark="${4}" row state rid rname rmark
  row="$(jq -r --arg v "${value}" --arg key "${_HETZNER_OWNER_KEY}" --arg mark "${mark}" '
    (. // []) as $all
    | (if ($v | test("^[0-9]+$")) then [ $all[] | select((.id | tostring) == $v) ] else [] end) as $byid
    | (if ($byid | length) > 0 then $byid else [ $all[] | select(.name == $v) ] end) as $c
    | if ($c | length) == 0 then "none"
      elif ($c | length) > 1 then "many"
      else $c[0] | ((.labels // {})[$key] // "") as $m
        | [ (if $m == $mark then "ok" elif $m == "" then "unmarked" else "other" end),
            (.id | tostring), (.name // ""), $m ] | join("\u001f")
      end' <<<"${list}" 2>/dev/null)" || row=""
  IFS=$'\x1f' read -r state rid rname rmark <<<"${row}"
  case "${state}" in
    ok) REPLY="${rid}"; return 0 ;;
    none) REPLY="no ${kind} has the id or the name '${value}'" ;;
    many) REPLY="more than one ${kind} has the name '${value}'" ;;
    unmarked) REPLY="${kind} '${rname}' (id ${rid}) does not have the owner mark of this cluster (${_HETZNER_OWNER_KEY}=${mark}). lok8s does not use a part that can belong to another person or cluster. If it is yours, first make sure that id ${rid} is yours ('hcloud ${kind} describe ${rid}'), then mark it one time: hcloud ${kind} add-label ${rid} ${_HETZNER_OWNER_KEY}=${mark}" ;;
    other) REPLY="${kind} '${rname}' (id ${rid}) has the owner mark of another cluster (${_HETZNER_OWNER_KEY}=${rmark}; this cluster: ${mark})" ;;
    *) REPLY="could not read the ${kind} list" ;;
  esac
  return 1
}

# hetzner::_ref_ids <what> <i> <key> <raw> <mark> [later]
# REPLY: the comma-separated ids of the parts that the reference <key> (the
# key without its "#..." suffix) of the descriptor part <what>[<i>] names.
# <raw> is its descriptor value as JSON: a number on a key of
# _HETZNER_REF_KEYS is an index into the descriptor list of the kind; a
# string that is the name of a descriptor part of the kind is that part; any
# other string (or a number on another key) is an id or a name; a list is
# each of its elements. Every part must exist with this cluster's owner mark
# (hetzner::_ref_match on a list from hetzner::_list). Each refusal is added
# to _HETZNER_REFUSED; rc 1. With "later": an index to a descriptor part that
# has no id yet, of a kind that the provision creates in an earlier step, is
# not refused and not in REPLY (the create loop resolves it after that create).
hetzner::_ref_ids() {
  local what="${1}" i="${2}" key="${3}" raw="${4}" mark="${5}" later="${6-}"
  local kind="${_HETZNER_REF_KIND[${key}]}" pname where el type value idx tid tname troot row
  local one=1 bad=0 n_what n_kind
  local -a ids=() els=()
  REPLY=""
  pname="$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i].name // ""')"
  where="${what} '${pname}': ${key}"
  # A list flag (_HETZNER_FLAGS type l: the ssh keys, networks, volumes and
  # firewalls of a server) takes a list; every other reference flag takes
  # one part.
  [[ "$(hetzner::_flag_type "${what}" "${key}")" != "l" ]] || one=0
  mapfile -t els < <(jq -c 'if type == "array" then .[] else . end' <<<"${raw}" 2>/dev/null)
  if (( ${#els[@]} == 0 )); then
    _HETZNER_REFUSED+=("${where} is empty. Write an index, an id or a name, or remove the key.")
    return 1
  fi
  # The form comes first, before any part is resolved: also a reference to a
  # part that this run creates later is checked here.
  if (( one )) && [[ "$(jq -r 'type' <<<"${raw}" 2>/dev/null)" == "array" ]]; then
    _HETZNER_REFUSED+=("${where}: hcloud ${what} create takes one ${kind}, not a list. Write one index, id or name.")
    return 1
  fi
  for el in "${els[@]}"; do
    type="$(jq -r 'type' <<<"${el}")"
    idx="" value=""
    case "${type}" in
      number)
        if ! jq -e --arg k "${key}" 'index($k) != null' >/dev/null <<<"${_HETZNER_REF_KEYS}"; then
          value="$(jq -r 'tostring' <<<"${el}")"
        elif [[ "${el}" =~ ^[0-9]+$ ]]; then
          idx="${el}"
        else
          _HETZNER_REFUSED+=("${where}: ${el} is not an index (a whole number from 0) of the ${kind} list of the descriptor.")
          bad=1; continue
        fi ;;
      string)
        value="$(jq -r '.' <<<"${el}")"
        if [[ -z "${value}" || "${value}" == *,* ]]; then
          _HETZNER_REFUSED+=("${where}: '${value}' is not one id or name. Write each part as one element of a JSON list.")
          bad=1; continue
        fi
        # The name of a part of the descriptor is that part: its verified id
        # goes to hcloud, as for an index (also when this run creates it).
        idx="$(hetzner::json -r --arg k "${kind}" --arg v "${value}" \
          '(.[$k] // []) | to_entries | map(select(.value.name == $v)) | .[0].key // empty')" ;;
      *)
        _HETZNER_REFUSED+=("${where}: ${el} is a ${type}. Write an index, an id or a name.")
        bad=1; continue ;;
    esac
    if [[ -n "${idx}" ]]; then
      row="$(hetzner::json -r --arg k "${kind}" --argjson n "${idx}" '
        (.[$k] // [])[$n] // null
        | if . == null then "missing"
          else [ (.id // "" | tostring), (.name // ""), ((.["#cloud.root"] // "") | tostring) ] | join("\u001f") end')"
      if [[ "${row}" == "missing" ]]; then
        _HETZNER_REFUSED+=("${where}: the descriptor has no ${kind} with the index ${idx}.")
        bad=1; continue
      fi
      IFS=$'\x1f' read -r tid tname troot <<<"${row}"
      # A bare-metal server is not a Cloud part: hcloud has no id for it.
      if [[ "${troot}" == "true" ]]; then
        _HETZNER_REFUSED+=("${where}: ${kind} '${tname}' of the descriptor is a bare-metal server (#cloud.root). It is not a Hetzner Cloud part, so hcloud ${what} create cannot use it.")
        bad=1; continue
      fi
      if [[ -z "${tid}" ]]; then
        n_what="$(hetzner::_create_order "${what}")" || n_what=""
        if n_kind="$(hetzner::_create_order "${kind}")"; then
          # A part that this run creates in an earlier step: the refs step
          # checks the form only; the create resolves it after that create.
          if [[ -n "${later}" && -n "${n_what}" ]] && (( n_kind < n_what )); then
            continue
          fi
          # A dry run created nothing: the plan shows the name of the part.
          if [[ -n "${CLOUD_DRY_RUN:-}" && -n "${n_what}" ]] && (( n_kind < n_what )); then
            ids+=("${tname}")
            continue
          fi
          if [[ -n "${n_what}" ]] && (( n_kind < n_what )); then
            _HETZNER_REFUSED+=("${where}: ${kind} '${tname}' of the descriptor has no id with the owner mark of this cluster.")
          else
            _HETZNER_REFUSED+=("${where}: ${kind} '${tname}' of the descriptor does not exist yet, and lok8s creates the ${kind} parts after the ${what} parts. Remove the reference, and give it to the other part (a volume has server, a server has #floating-ip).")
          fi
        else
          _HETZNER_REFUSED+=("${where}: ${kind} '${tname}' of the descriptor does not exist with the owner mark of this cluster, and lok8s does not create a ${kind}. Create it with the mark (hcloud ${kind} create --name ${tname} --label ${_HETZNER_OWNER_KEY}=${mark} ...) or mark your ${kind} (first make sure that it is yours), then run the provision again.")
        fi
        bad=1; continue
      fi
      value="${tid}"
    fi
    if ! hetzner::_list "${kind}"; then
      _HETZNER_REFUSED+=("${where}: could not list the ${kind} parts ('hcloud ${kind} list'). Check the hcloud token and the API.")
      bad=1; continue
    fi
    if ! hetzner::_ref_match "${kind}" "${value}" "${REPLY}" "${mark}"; then
      _HETZNER_REFUSED+=("${where}: ${REPLY}.")
      bad=1; continue
    fi
    ids+=("${REPLY}")
  done
  if (( one && ${#ids[@]} > 1 )); then
    _HETZNER_REFUSED+=("${where}: hcloud ${what} create takes one ${kind}, and the descriptor names ${#ids[@]}.")
    bad=1
  fi
  REPLY=""
  (( bad == 0 )) || return 1
  local IFS=,
  REPLY="${ids[*]}"
}

# hetzner::_image_ok <what> <name> <image-json> <mark>
# REPLY: the image value for hcloud. <image-json> is the descriptor value as
# JSON. hcloud reads an image value as an id when strconv.ParseInt(v, 10, 64)
# accepts it, and only then as a name: hcloud CLI v1.70.2
# internal/cmd/server/create.go:297 and internal/cmd/server/rebuild.go:49 call
# Image().GetForArchitecture; hcloud-go v2.53.0 hcloud/image.go:139 calls
# getByIDOrName, hcloud/client_helper.go:73:
#     if id, err := strconv.ParseInt(idOrName, 10, 64); err == nil {
# ParseInt takes a sign and leading zeros: "+4711" and "04711" are the id
# 4711. So:
# - Digits with a sign or a leading zero are refused: write an id as plain
#   digits.
# - Plain digits are an id. A system or app image passes; a snapshot or a
#   backup is a part of the project and needs this cluster's owner mark.
# - Any other string is a name. hcloud finds a name only among the system and
#   app images (a snapshot has no name), so it is not checked.
# A value that is not one string or number (a list, true, null) is refused,
# and so is a value with a control character or with white space at its
# start or end.
# A refusal is added to _HETZNER_REFUSED; rc 1.
hetzner::_image_ok() {
  local what="${1}" pname="${2}" raw="${3}" mark="${4}" image row type m msg
  REPLY=""
  case "$(jq -r 'type' <<<"${raw}" 2>/dev/null)" in
    string|number) ;;
    *)
      _HETZNER_REFUSED+=("${what} '${pname}': image: ${raw} is not one image. Write one image name or id.")
      return 1 ;;
  esac
  # A control character or a space at an end: "4711\t" is no id, so it
  # passed as a name, and the rebuild plan cut the tab (the id 4711).
  if jq -e "${_HETZNER_JQ_CTRL}" >/dev/null 2>&1 <<<"${raw}"; then
    _HETZNER_REFUSED+=("${what} '${pname}': image: ${raw} has a control character (U+0000 to U+001F or U+007F). Write the image name or id without it.")
    return 1
  fi
  if jq -e "${_HETZNER_JQ_EDGE_SPACE}" >/dev/null 2>&1 <<<"${raw}"; then
    _HETZNER_REFUSED+=("${what} '${pname}': image: ${raw} has a space at the start or the end. Write the image name or id without it.")
    return 1
  fi
  image="$(jq -r 'tostring' <<<"${raw}")"
  if [[ -z "${image}" ]]; then
    _HETZNER_REFUSED+=("${what} '${pname}': image is empty. Write one image name or id.")
    return 1
  fi
  if [[ "${image}" =~ ^[+-]?[0-9]+$ && ! "${image}" =~ ^[1-9][0-9]*$ ]]; then
    _HETZNER_REFUSED+=("${what} '${pname}': image: '${image}' is an image id for hcloud (it has a sign or a leading zero). Write the id as plain digits, for example 4711.")
    return 1
  fi
  if [[ ! "${image}" =~ ^[0-9]+$ ]]; then
    REPLY="${image}"
    return 0
  fi
  if ! hetzner::_list image; then
    _HETZNER_REFUSED+=("${what} '${pname}': image ${image}: could not list the images ('hcloud image list'). Check the hcloud token and the API.")
    return 1
  fi
  row="$(jq -r --arg v "${image}" --arg key "${_HETZNER_OWNER_KEY}" '
    [ (. // [])[] | select((.id | tostring) == $v) ] | first
    | if . == null then "none" else [ (.type // ""), ((.labels // {})[$key] // "") ] | join("\u001f") end' <<<"${REPLY}" 2>/dev/null)" || row=""
  IFS=$'\x1f' read -r type m <<<"${row}"
  case "${type}" in
    system|app) REPLY="${image}"; return 0 ;;
    none) msg="no image has the id ${image}" ;;
    "") msg="could not read the image list" ;;
    *)
      if [[ "${m}" == "${mark}" ]]; then
        REPLY="${image}"
        return 0
      fi
      if [[ -z "${m}" ]]; then
        msg="image ${image} (a ${type}) does not have the owner mark of this cluster (${_HETZNER_OWNER_KEY}=${mark}). If it is yours, first make sure with 'hcloud image describe ${image}', then mark it one time: hcloud image add-label ${image} ${_HETZNER_OWNER_KEY}=${mark}"
      else
        msg="image ${image} (a ${type}) has the owner mark of another cluster (${_HETZNER_OWNER_KEY}=${m}; this cluster: ${mark})"
      fi ;;
  esac
  REPLY=""
  _HETZNER_REFUSED+=("${what} '${pname}': image: ${msg}.")
  return 1
}

# hetzner::_part_args <what> <i> <mark> [later]
# Checks every key and value of the descriptor part <what>[<i>] that becomes
# a flag of 'hcloud <what> create', and puts the flags into _HETZNER_ARGS.
# Each flag is made from the exact key (without its "#..." suffix) and the
# value that the check passed. _HETZNER_ARGVAL[<key>] is the checked value
# of each key (the server hook exports them). Each refusal is added to
# _HETZNER_REFUSED; rc 1. A key that starts with "#" is a note for the hooks,
# not a flag; "id" is the verified id that the lookup wrote.
# - A key must have the form _HETZNER_KEY_RE, and its flag must be on the
#   list of the kind (_HETZNER_FLAGS). The keys are read NUL-separated: a
#   key can hold any character.
# - No value has a control character, and a name has no white space at its
#   start or end.
# - A flag that takes one value (s, i, n, b) can be in a part one time only:
#   hcloud takes the last of two.
# - A reference key: hetzner::_ref_ids (the ids of parts with the mark only).
# - image: hetzner::_image_ok.
# - A bool flag takes JSON true or false only: "--flag" or "--flag=false".
# - Each value is one "--flag=value" token: pflag binds it to its flag, also
#   a value that starts with "-". A list (l) is one comma-separated value; a
#   list on an "a" or "m" flag gives one token per element. A list on a flag
#   that takes one value, true, false, null and an object are refused.
# A server create gets "--ssh-key=" when the part has no ssh-key: then hcloud
# adds no SSH key from its own configuration (the option default-ssh-keys,
# which lok8s cannot check). hcloud CLI v1.70.2
# internal/cmd/server/create.go:364 adds the default keys only when the flag
# is not set:
#     if !flags.Changed("ssh-key") && config.OptionDefaultSSHKeys.Changed(s.Config()) {
# An empty value sets the flag with no key (pflag v1.0.10 string_slice.go:23
# reads "" as an empty list).
# With "later" (the refs step), a reference to a part that this run creates
# in an earlier step is checked for its form only (hetzner::_ref_ids).
hetzner::_part_args() {
  local what="${1}" i="${2}" mark="${3}" later="${4-}"
  local pname key nk raw type ft val k has_key=0 n0="${#_HETZNER_REFUSED[@]}"
  local -A seen=()
  local -a vals=()
  _HETZNER_ARGS=()
  _HETZNER_ARGVAL=()
  pname="$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i].name // ""')"
  # The keys first: the form, then the list of the kind. A refused key is
  # shown as JSON (it can hold any character); no other check runs on the
  # part.
  while IFS= read -r -d '' key; do
    k="$(jq -cn --arg k "${key}" '$k')"
    if [[ ! "${key}" =~ ${_HETZNER_KEY_RE} ]]; then
      _HETZNER_REFUSED+=("${what} '${pname}': the key ${k} is not the name of an hcloud flag. A key is a flag name (a-z, 0-9 and -), with an optional suffix (#...). Write the value as the value of the key.")
    elif ! hetzner::_flag_type "${what}" "${key%%#*}" >/dev/null; then
      _HETZNER_REFUSED+=("${what} '${pname}': the key ${k} is not a flag of 'hcloud ${what} create' (hcloud v1.70.2). The flags of a ${what}: $(hetzner::_flag_names "${what}"). lok8s never takes a flag of the hcloud command itself (such as context, config, endpoint, quiet or debug) from the descriptor.")
    fi
  done < <(hetzner::json -j --arg w "${what}" --argjson i "${i}" '
    .[$w][$i] | keys_unsorted[] | select(startswith("#") | not) | select(. != "id") | . + "\u0000"')
  (( ${#_HETZNER_REFUSED[@]} == n0 )) || return 1
  while IFS= read -r -d '' key; do
    nk="${key%%#*}"
    ft="$(hetzner::_flag_type "${what}" "${nk}")"
    raw="$(hetzner::json -c --arg w "${what}" --argjson i "${i}" --arg k "${key}" '.[$w][$i][$k]')"
    type="$(jq -r 'type' <<<"${raw}")"
    [[ "${nk}" != "ssh-key" ]] || has_key=1
    # A flag that takes one value: one key only (hcloud takes the last of
    # two, so two keys are a list too).
    if [[ "${ft}" != [lam] ]]; then
      if [[ -n "${seen[${nk}]:-}" ]]; then
        _HETZNER_REFUSED+=("${what} '${pname}': ${nk}: the keys ${seen[${nk}]} and ${key} both give it, and hcloud ${what} create takes one. Keep one key.")
        continue
      fi
      seen["${nk}"]="${key}"
    fi
    # hcloud gets each value as it is. (_image_ok checks the image.)
    if [[ "${nk}" != "image" ]] && jq -e "${_HETZNER_JQ_CTRL}" >/dev/null 2>&1 <<<"${raw}"; then
      _HETZNER_REFUSED+=("${what} '${pname}': ${key}: ${raw} has a control character (U+0000 to U+001F or U+007F). Write the value without it.")
      continue
    fi
    if [[ "${nk}" == "name" ]] && jq -e "${_HETZNER_JQ_EDGE_SPACE}" >/dev/null 2>&1 <<<"${raw}"; then
      _HETZNER_REFUSED+=("${what} '${pname}': name: ${raw} has a space at the start or the end. Write the name without it.")
      continue
    fi
    vals=()
    if [[ -n "${_HETZNER_REF_KIND[${nk}]:-}" ]]; then
      hetzner::_ref_ids "${what}" "${i}" "${nk}" "${raw}" "${mark}" "${later}" || continue
      # Only a reference that the refs step checks later has no id yet.
      [[ -n "${REPLY}" ]] || continue
      vals=("${REPLY}")
    elif [[ "${nk}" == "image" ]]; then
      hetzner::_image_ok "${what}" "${pname}" "${raw}" "${mark}" || continue
      vals=("${REPLY}")
    elif [[ "${ft}" == "b" ]]; then
      if [[ "${type}" != "boolean" ]]; then
        _HETZNER_REFUSED+=("${what} '${pname}': ${key}: ${raw} is not true or false. --${nk} is a switch of hcloud ${what} create.")
        continue
      fi
      if [[ "${raw}" == "true" ]]; then
        _HETZNER_ARGS+=("--${nk}")
      else
        _HETZNER_ARGS+=("--${nk}=false")
      fi
      _HETZNER_ARGVAL["${key}"]="${raw}"
      continue
    else
      case "${type}:${ft}" in
        string:*|number:*) vals=("$(jq -r 'tostring' <<<"${raw}")") ;;
        array:l)
          if ! val="$(jq -er 'select(length > 0 and all(.[]; type == "string" or type == "number"))
              | map(tostring) | join(",")' <<<"${raw}")"; then
            _HETZNER_REFUSED+=("${what} '${pname}': ${key}: ${raw} is not a list of values. Write a value or a list of strings and numbers.")
            continue
          fi
          vals=("${val}") ;;
        array:a|array:m)
          if ! jq -e 'length > 0 and all(.[]; type == "string" or type == "number")' >/dev/null 2>&1 <<<"${raw}"; then
            _HETZNER_REFUSED+=("${what} '${pname}': ${key}: ${raw} is not a list of values. Write a value or a list of strings and numbers.")
            continue
          fi
          mapfile -t vals < <(jq -r '.[] | tostring' <<<"${raw}") ;;
        array:*)
          _HETZNER_REFUSED+=("${what} '${pname}': ${key}: ${raw} is a list, and --${nk} of hcloud ${what} create takes one value.")
          continue ;;
        *)
          _HETZNER_REFUSED+=("${what} '${pname}': ${key}: ${raw} is a ${type}. Write a value (true and false only for a switch of hcloud ${what} create).")
          continue ;;
      esac
    fi
    for k in "${!vals[@]}"; do
      # hcloud receives values verbatim: expand a leading ~ (for example
      # public-key-from-file: ~/.ssh/...).
      # shellcheck disable=SC2088 # literal "~/" prefix match, expanded via $HOME on the same line
      [[ "${vals[k]}" != "~/"* ]] || vals[k]="${HOME}/${vals[k]:2}"
      _HETZNER_ARGS+=("--${nk}=${vals[k]}")
    done
    val="$(IFS=,; printf '%s' "${vals[*]}")"
    _HETZNER_ARGVAL["${key}"]="${val}"
  done < <(hetzner::json -j --arg w "${what}" --argjson i "${i}" '
    .[$w][$i] | keys_unsorted[] | select(startswith("#") | not) | select(. != "id") | . + "\u0000"')
  if [[ "${what}" == "server" ]] && (( ! has_key )); then
    _HETZNER_ARGS+=("--ssh-key=")
  fi
  (( ${#_HETZNER_REFUSED[@]} == n0 ))
}

# hetzner::_print_refused: one error per entry of _HETZNER_REFUSED. A
# refusal shows descriptor values (as JSON, with escapes such as \t): error
# prints each backslash as it is, not as a control character.
hetzner::_print_refused() {
  local r
  for r in "${_HETZNER_REFUSED[@]}"; do
    error "hetzner: refused ${r}"
  done
}

# hetzner::refs
# The step before the first create: checks every key and value of every part
# that this run creates (a part with no id that is not #cloud.root), with
# hetzner::_part_args: the key form and the flag list of the kind, the
# values, the references, the images, the switches. It checks the notes that
# the hooks turn into flags (#subnets, #targets, #services) of every part for
# control characters. It reports all refusals and stops (rc 1) before
# anything is created. The create loop checks each part again right before its create,
# and gives hcloud only the flags that the check made.
hetzner::refs() {
  local mark what i kinds row
  mark="$(hetzner::owner_mark)" || return 1
  _HETZNER_REFUSED=()
  _HETZNER_LISTS=()
  kinds="$(printf '"%s",' "${_HETZNER_CREATE_KINDS[@]}")"
  while IFS=$'\x1f' read -r what i; do
    [[ -n "${what}" ]] || continue
    hetzner::_part_args "${what}" "${i}" "${mark}" later || true
  done < <(hetzner::json -r --argjson ws "[${kinds%,}]" '
    . as $root | $ws[] as $w
    | ($root[$w] // []) | to_entries[] | .key as $i | .value
    | select(type == "object" and .id == null and ((.["#cloud.root"] // "") | tostring) != "true")
    | [$w, ($i | tostring)] | join("\u001f")')
  # The notes that the hooks turn into hcloud flags (#subnets, #targets,
  # #services), of every part (also one that exists): no control character.
  while IFS= read -r -d '' row; do
    _HETZNER_REFUSED+=("${row}")
  done < <(hetzner::json -j --argjson ws "[${kinds%,}]" '
    . as $root | $ws[] as $w
    | ($root[$w] // [])[] | . as $p
    | select(type == "object")
    | ("#subnets", "#targets", "#services") as $note
    | select($p[$note] != null)
    | select($p[$note] | '"${_HETZNER_JQ_CTRL}"')
    | "\($w) \u0027\($p.name // "")\u0027: \($note): \($p[$note] | tojson) has a control character (U+0000 to U+001F or U+007F). Write the values without it.\u0000"')
  (( ${#_HETZNER_REFUSED[@]} > 0 )) || return 0
  hetzner::_print_refused
  error "hetzner: the descriptor has ${#_HETZNER_REFUSED[@]} refused key(s) or reference(s). $(hetzner::_changes_note) Correct the descriptor, or mark your part as the message says, then run the provision again."
  return 1
}

# hetzner::_still_ours <kind> <id> <mark> [<name>]
# Right before a write: reads the part BY ID and checks that it still has
# this cluster's owner mark (and the name, when given). Prints the part as
# JSON. In a dry run, a part that this run did not create yet has no id (the
# plan shows its name): nothing to read, prints {}. rc 1 with an error.
# The check and the write after it are two API calls: another actor can
# change the part between them (the API has no precondition on labels).
hetzner::_still_ours() {
  local kind="${1}" id="${2}" mark="${3}" want="${4-}" json state
  if [[ ! "${id}" =~ ^[0-9]+$ ]]; then
    if [[ -n "${CLOUD_DRY_RUN:-}" ]]; then
      printf '{}\n'
      return 0
    fi
    error "hetzner: did not change ${kind} '${id}': it is not a verified id. lok8s changes a part only by the id of a part with the owner mark of this cluster."
    return 1
  fi
  if ! json="$(hcloud "${kind}" describe "${id}" -o json)" ||
     ! state="$(jq -er --arg id "${id}" --arg key "${_HETZNER_OWNER_KEY}" --arg mark "${mark}" --arg n "${want}" '
         if type != "object" then empty
         elif (.id | tostring) != $id then "the answer is for the id \(.id)"
         elif ((.labels // {})[$key] // "") != $mark then "it does not have the owner mark of this cluster now (\($key)=\((.labels // {})[$key] // "none"); this cluster: \($mark))"
         elif $n != "" and .name != $n then "its name is \(.name) now, not \($n)"
         else "ok" end' <<<"${json}" 2>/dev/null)"; then
    error "hetzner: did not change ${kind} ${id}: could not read it right before the change ('hcloud ${kind} describe ${id}')."
    return 1
  fi
  if [[ "${state}" != "ok" ]]; then
    error "hetzner: did not change ${kind} ${id}: ${state}. It changed after lok8s checked it (another run or person). Check it with 'hcloud ${kind} describe ${id}'."
    return 1
  fi
  printf '%s\n' "${json}"
}

# ── Steps and outcome ────────────────────────────────────

# hetzner::_run_steps
# The steps of provider::provision, in order: the lookup, the check of every
# reference, then one step per kind. Each hcloud write of the run goes to the
# file _HETZNER_WRITES (dry-run::cmd). A failed step prints the changes of
# this run (hetzner::_outcome) and stops (rc 1).
hetzner::_run_steps() {
  local _HETZNER_WRITES="${CLOUD_LOG_FILE%.log}.writes" step
  : > "${_HETZNER_WRITES}" || return 1
  for step in lookup refs "${_HETZNER_CREATE_KINDS[@]}"; do
    if ! "hetzner::${step}"; then
      error "hetzner ${step} failed"
      hetzner::_outcome
      return 1
    fi
  done
}

# hetzner::_outcome
# After a failed step: lists each write of this run (_HETZNER_WRITES) with
# its result. A create shows the id of the new part when the lookup found it.
# A write that failed can still be done at Hetzner (an answer can be lost).
# Prints nothing when the run made no write.
hetzner::_outcome() {
  [[ -n "${_HETZNER_WRITES:-}" && -s "${_HETZNER_WRITES}" ]] || return 0
  local rc line kind name id re='^hcloud ([a-z-]+) create .*--name=([^ ]+)'
  error "hetzner: the provision stopped after it made changes, so the result is partial. Each part that lok8s created has the owner mark: the next provision adopts it and does the rest. The changes of this run:"
  while IFS=$'\t' read -r rc line; do
    [[ -n "${line}" ]] || continue
    if [[ "${line}" =~ ${re} ]]; then
      kind="${BASH_REMATCH[1]}" name="${BASH_REMATCH[2]}"
      id="$(hetzner::json -r --arg k "${kind}" --arg n "${name}" \
        '[.[$k][]? | select(.name == $n) | .id // empty][0] // "" | tostring' 2>/dev/null)" || id=""
      if [[ "${rc}" == "0" ]]; then
        printf '  created %s %s (id %s)\n' "${kind}" "${name}" "${id:-not known yet}" >&2
      else
        printf '  not confirmed: created %s %s? hcloud exit %s. The %s can exist now; the next provision finds it by its name and its owner mark.\n' \
          "${kind}" "${name}" "${rc}" "${kind}" >&2
      fi
    elif [[ "${rc}" == "0" ]]; then
      printf '  done: %s\n' "${line}" >&2
    else
      printf '  not confirmed (hcloud exit %s; it can be done or not): %s\n' "${rc}" "${line}" >&2
    fi
  done < "${_HETZNER_WRITES}"
}

# ── Generic create loop ─────────────────────────────────

hetzner::create() {
  local -r what="${1}"; shift

  # Skip if this resource type doesn't exist in the descriptor
  local has
  has=$(hetzner::json -r --arg w "${what}" '.[$w] // empty | length')
  if [[ -z "${has}" ]] || [[ "${has}" == "0" ]]; then
    debug "skip ${what} (not in descriptor)"
    return 0
  fi

  local -r create_after="$(type -t hetzner::create::after)"
  local -r create_hook="$(type -t hetzner::create::hook)"

  # Every create gets the owner mark of this cluster. The hooks read `mark`.
  local mark
  mark="$(hetzner::owner_mark)" || return 1

  # `id` is the verified id the part had before this run (empty: created
  # now). `rid` is the verified id of the part, for every hcloud call of the
  # hooks: they never act by name (a name can be another cluster's part).
  local i id name rid out
  # Read the server stream on a DEDICATED fd (9), not stdin: the bare-metal
  # create::hook runs `ssh` (installimage), and ssh reads stdin — on stdin that
  # would SWALLOW the remaining loop rows, so only the FIRST bare-metal server
  # got processed and the rest stranded in rescue (KubeOne then looped on
  # `sudo: command not found`). Feeding the loop via fd 9 makes it immune to any
  # stdin-reading body command.
  while IFS=',' read -r i id name <&9; do
    rid="${id}"
    # Check for #cloud.root (bare metal / pre-existing server)
    local is_root
    is_root=$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i]["#cloud.root"] // ""')

    # Bare metal (#cloud.root) MUST precede the id-skip below: robot hardware
    # ALWAYS "exists" (has an id), so an id-based skip would strand every bare-
    # metal worker whose id resolved non-empty in the rescue system — only the
    # one that happened to resolve an empty id got installimaged, the rest were
    # skipped and KubeOne then looped forever on `sudo: command not found`
    # (rescue = no sudo). The hook itself self-gates on rescue mode (installimages
    # a rescue-booted node, no-ops an already-installed one), so running it
    # unconditionally per bare-metal server is correct + idempotent.
    if [[ "${is_root}" == "true" ]]; then
      _hetzner_print "🔧 bare metal \033[3m${what}\033[0m \033[1m${name}\033[0m (pre-existing)"

      # Extract #-prefixed metadata for the hooks. A conversion that fails
      # stops here: a cut field list would shift every key and value after it.
      # shellcheck disable=2016
      if ! out="$(hetzner::json -rc \
        --arg what "${what}" \
        --argjson i "${i}" \
        --argjson refs "${_HETZNER_REF_KEYS}" "${_HETZNER_JQ_VALUE}"' . as $root |
          .[$what][$i]
            | to_entries
            | .[]
            | "--" + .key, hcloud_value($root; $refs)
        ')"; then
        error "hetzner: could not read the fields of ${what} '${name}' from the descriptor."
        return 1
      fi
      mapfile -t fields <<<"${out}"

      # Run the create hook (for cloud-init) if defined, but without hcloud create
      if [[ -n "${create_hook-}" ]]; then
        # For bare metal, the hook gets called with a no-op command
        # The hook reads fields (including #cloud.d, #external-ip, etc.)
        hetzner::create::hook true
      fi

      # A hook that fails (a part changed after the check) stops the step.
      if [[ -n "${create_after-}" ]]; then
        hetzner::create::after || return 1
      fi

      continue
    fi

    # Cloud servers: skip if already created (has an id). (Bare metal handled
    # above — it must NOT be id-skipped; see the #cloud.root branch.)
    [[ -z "${id}" ]] || {
      _hetzner_print "🦗 skip \033[3m${what}\033[0m \033[1m${name}\033[0m (${id})"

      if [[ -n "${create_after-}" ]]; then
        hetzner::create::after || return 1
      fi

      continue
    }

    # Cloud VMs: standard hcloud create

    # fetch all fields
    # A boolean value is a bare hcloud switch (`without-ipv6: true` →
    # `--without-ipv6=true`, the form cobra takes with no value token). It
    # travels as one "--key=value" token plus an empty placeholder that the
    # consumer below drops. Before this, `true` became `--without-ipv6 true`
    # and hcloud read the value as a stray argument. The switch matters for
    # an IPv4-only server, e.g. a KubeOne worker that pulls images over IPv4.
    # A conversion that fails stops here: a cut field list would shift every
    # key and value after it, and hcloud would get the wrong flags.
    # shellcheck disable=2016
    if ! out="$(hetzner::json -rc \
      --arg what "${what}" \
      --argjson i "${i}" \
      --argjson refs "${_HETZNER_REF_KEYS}" "${_HETZNER_JQ_VALUE}"' . as $root |
        .[$what][$i]
          | to_entries
          | .[]
          | "--" + .key + (
              if (.value | type) == "boolean" then "=" + (.value | tostring) else "" end
            ), (
            if (.value | type) == "boolean" then "" else hcloud_value($root; $refs) end
        )
      ')"; then
      error "hetzner: could not read the fields of ${what} '${name}' from the descriptor. Nothing was created for it."
      return 1
    fi
    mapfile -t fields <<<"${out}"

    # The flags of the create: every key and value of the part is checked
    # again here, right before the create, and the flags are made from
    # exactly what the check passed (hetzner::_part_args): a key of another
    # form is refused, a reference is resolved from fresh lists to the ids of
    # parts with this cluster's owner mark (never a name), an image id must
    # be plain digits. The hooks get the checked values (the server hook
    # exports the fields to cloud-init).
    local x okey
    local -a cmd=() args=()
    _HETZNER_REFUSED=()
    _HETZNER_LISTS=()
    hetzner::_part_args "${what}" "${i}" "${mark}" || true
    if (( ${#_HETZNER_REFUSED[@]} > 0 )); then
      hetzner::_print_refused
      error "hetzner: did not create ${what} '${name}': ${#_HETZNER_REFUSED[@]} of its keys or references are refused (above). $(hetzner::_changes_note)"
      return 1
    fi
    args=("${_HETZNER_ARGS[@]}")
    for (( x=0; x < ${#fields[@]}; x=x+2 )); do
      okey="${fields[x]#--}"
      okey="${okey%%=*}"
      [[ -z "${_HETZNER_ARGVAL[${okey}]+set}" ]] || fields[x + 1]="${_HETZNER_ARGVAL[${okey}]}"
    done

    # check for create hook. The owner mark comes after the descriptor's own
    # --label: hcloud merges repeated --label flags and the last value of a
    # key wins, so a descriptor cannot set another owner value.
    cmd=( 'dry-run::cmd' 'hcloud' "${what}" 'create' "${args[@]}"
          "--label=${_HETZNER_OWNER_KEY}=${mark}" "${@}" )
    [[ -z "${create_hook-}" ]] ||
      cmd=( "hetzner::create::hook" "${cmd[@]}" )

    _hetzner_print "📦 create \033[3m${what}\033[0m \033[1m${name}\033[0m"
    _hetzner_log "   $ ${cmd[*]}"
    # A create that fails stops the provision HERE, with Hetzner's own reason
    # on the line above. The exit status used to be dropped: a capacity
    # refusal ("error during placement (resource_unavailable)") printed one
    # line and the run walked on into the KubeOne inventory, which reported
    # ZERO control-plane hosts and hinted at name drift. Nothing is rolled
    # back: a re-run keeps what exists (the id check above) and creates the
    # rest. A failed create can be done all the same (only the answer was
    # lost): the part then has the owner mark, and a re-run adopts it.
    if ! "${cmd[@]}"; then
      error "hetzner: could not create ${what} '${name}', or Hetzner did not confirm it. Hetzner refused or failed the request; its message is above. The result is not known: when only the answer was lost, the ${what} exists now with the owner mark of this cluster, and a re-run adopts it. For a placement refusal (resource_unavailable), choose another server type or location in the descriptor, or retry later. A re-run keeps what exists and creates the rest."
      return 1
    fi

    if [[ -n "${create_after-}" ]]; then
      # The hook changes the new part by its id. The id comes from the
      # lookup, which accepts only a part with this cluster's mark. A dry
      # run created nothing: the printed plan shows the name.
      if [[ -n "${CLOUD_DRY_RUN:-}" ]]; then
        rid="${name}"
      else
        hetzner::lookup "${what}" >/dev/null || return 1
        rid="$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i].id // "" | tostring')"
        if [[ -z "${rid}" ]]; then
          error "hetzner: created ${what} '${name}', but found no ${what} with this name and the owner mark ${_HETZNER_OWNER_KEY}=${mark}. Nothing else was changed on it. Check it with 'hcloud ${what} describe ${name}', then run the provision again."
          return 1
        fi
      fi
      hetzner::create::after || return 1
    fi
  done 9< <(hetzner::json::loop "${what}")
  unset IFS

  unset -f \
    hetzner::create::hook \
    hetzner::create::after

  hetzner::lookup "${what}"
}
