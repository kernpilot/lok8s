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

    # The parts with a descriptor name and without this cluster's mark.
    # shellcheck disable=2016
    rows="$(hetzner::json -r --argjson remote "${remote}" --arg field "${resource}" \
      --arg key "${_HETZNER_OWNER_KEY}" --arg mark "${mark}" '
      [ .[$field][]? | .name ] as $names
      | ($remote // [])[]
      | select(.name as $n | any($names[]; . == $n))
      | select((.labels // {})[$key] != $mark)
      | [ (.id | tostring), .name, ((.labels // {})[$key] // "") ] | @tsv')" || return 1
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

# hetzner::_ref_kinds_json: _HETZNER_REF_KIND as a JSON object, for jq.
hetzner::_ref_kinds_json() {
  local k out=""
  for k in "${!_HETZNER_REF_KIND[@]}"; do
    out+="${out:+,}\"${k}\":\"${_HETZNER_REF_KIND[${k}]}\""
  done
  printf '{%s}\n' "${out}"
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
  local kind="${_HETZNER_REF_KIND[${key}]}" pname where el type value idx tid tname row
  local one=1 bad=0 n_what n_kind
  local -a ids=() els=()
  REPLY=""
  pname="$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i].name // ""')"
  where="${what} '${pname}': ${key}"
  # A server create takes lists of ssh keys, networks, volumes and firewalls;
  # every other reference flag takes one part.
  if [[ "${what}" == "server" ]]; then
    case "${key}" in ssh-key|network|volume|firewall) one=0 ;; esac
  fi
  mapfile -t els < <(jq -c 'if type == "array" then .[] else . end' <<<"${raw}" 2>/dev/null)
  if (( ${#els[@]} == 0 )); then
    _HETZNER_REFUSED+=("${where} is empty. Write an index, an id or a name, or remove the key.")
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
        | if . == null then "missing" else [ (.id // "" | tostring), (.name // "") ] | join("\u001f") end')"
      if [[ "${row}" == "missing" ]]; then
        _HETZNER_REFUSED+=("${where}: the descriptor has no ${kind} with the index ${idx}.")
        bad=1; continue
      fi
      IFS=$'\x1f' read -r tid tname <<<"${row}"
      if [[ -z "${tid}" ]]; then
        n_what="$(hetzner::_create_order "${what}")" || n_what=""
        if n_kind="$(hetzner::_create_order "${kind}")"; then
          if [[ -n "${later}" && -n "${n_what}" ]] && (( n_kind < n_what )); then
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

# hetzner::_image_ok <what> <name> <image> <mark>
# An image that is not all digits is a name: hcloud resolves a name only
# among the public system and app images (a snapshot has no name), so it is
# not checked. An all-digit image is an id: a system or app image passes; a
# snapshot or a backup is a part of the project and needs this cluster's
# owner mark. A refusal is added to _HETZNER_REFUSED; rc 1.
hetzner::_image_ok() {
  local what="${1}" pname="${2}" image="${3}" mark="${4}" row type m msg
  [[ "${image}" =~ ^[0-9]+$ ]] || return 0
  if ! hetzner::_list image; then
    _HETZNER_REFUSED+=("${what} '${pname}': image ${image}: could not list the images ('hcloud image list'). Check the hcloud token and the API.")
    return 1
  fi
  row="$(jq -r --arg v "${image}" --arg key "${_HETZNER_OWNER_KEY}" '
    [ (. // [])[] | select((.id | tostring) == $v) ] | first
    | if . == null then "none" else [ (.type // ""), ((.labels // {})[$key] // "") ] | join("\u001f") end' <<<"${REPLY}" 2>/dev/null)" || row=""
  IFS=$'\x1f' read -r type m <<<"${row}"
  case "${type}" in
    system|app) return 0 ;;
    none) msg="no image has the id ${image}" ;;
    "") msg="could not read the image list" ;;
    *)
      [[ "${m}" != "${mark}" ]] || return 0
      if [[ -z "${m}" ]]; then
        msg="image ${image} (a ${type}) does not have the owner mark of this cluster (${_HETZNER_OWNER_KEY}=${mark}). If it is yours, first make sure with 'hcloud image describe ${image}', then mark it one time: hcloud image add-label ${image} ${_HETZNER_OWNER_KEY}=${mark}"
      else
        msg="image ${image} (a ${type}) has the owner mark of another cluster (${_HETZNER_OWNER_KEY}=${m}; this cluster: ${mark})"
      fi ;;
  esac
  _HETZNER_REFUSED+=("${what} '${pname}': image: ${msg}.")
  return 1
}

# hetzner::_print_refused: one error per entry of _HETZNER_REFUSED.
hetzner::_print_refused() {
  local r
  for r in "${_HETZNER_REFUSED[@]}"; do
    error "hetzner: refused ${r}"
  done
}

# hetzner::refs
# The step before the first create: checks every reference and every image
# id of every part that this run creates (a part with no id that is not
# #cloud.root). It reports all refusals and stops (rc 1) before anything is
# created. The create loop resolves each reference again, right before its
# create, and gives hcloud only the verified ids.
hetzner::refs() {
  local mark what i okey nk raw pname
  mark="$(hetzner::owner_mark)" || return 1
  _HETZNER_REFUSED=()
  _HETZNER_LISTS=()
  local kinds
  kinds="$(printf '"%s",' "${_HETZNER_CREATE_KINDS[@]}")"
  while IFS=$'\x1f' read -r what i okey; do
    [[ -n "${what}" ]] || continue
    nk="${okey%%#*}"
    raw="$(hetzner::json -c --arg w "${what}" --argjson i "${i}" --arg k "${okey}" '.[$w][$i][$k]')"
    if [[ "${nk}" == "image" ]]; then
      pname="$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i].name // ""')"
      hetzner::_image_ok "${what}" "${pname}" "$(jq -r 'tostring' <<<"${raw}")" "${mark}" || true
    else
      hetzner::_ref_ids "${what}" "${i}" "${nk}" "${raw}" "${mark}" later || true
    fi
  done < <(hetzner::json -r --argjson kinds "$(hetzner::_ref_kinds_json)" --argjson ws "[${kinds%,}]" '
    . as $root | $ws[] as $w
    | ($root[$w] // []) | to_entries[] | .key as $i | .value
    | select(type == "object" and .id == null and ((.["#cloud.root"] // "") | tostring) != "true")
    | keys_unsorted[]
    | select((sub("#.*$"; "")) as $nk | $kinds[$nk] != null or $nk == "image")
    | [$w, ($i | tostring), .] | join("\u001f")')
  (( ${#_HETZNER_REFUSED[@]} > 0 )) || return 0
  hetzner::_print_refused
  error "hetzner: ${#_HETZNER_REFUSED[@]} reference(s) of the descriptor are refused. $(hetzner::_changes_note) Correct the descriptor, or mark your part as the message says, then run the provision again."
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
  local rc line kind name id re='^hcloud ([a-z-]+) create .*--name ([^ ]+)'
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
    # and hcloud read the value as a stray argument, so a server could not be
    # created without IPv6.
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

    # filter args from fields (skip #-prefixed). Each reference (a key of
    # _HETZNER_REF_KIND, read without its "#..." suffix) is resolved again
    # here, right before the create, from fresh lists: hcloud gets only the
    # ids of parts with this cluster's owner mark, never a name. The fields
    # get the ids too (the server hook exports them to cloud-init).
    local field val okey nk raw
    local -a cmd=() args=()
    _HETZNER_REFUSED=()
    _HETZNER_LISTS=()
    for (( x=0; x < ${#fields[@]}; x=x+2 )); do
      [[ "${fields[x]:2:1}" != "#" ]] ||
        continue

      okey="${fields[x]#--}"
      okey="${okey%%=*}"
      nk="${okey%%#*}"
      if [[ -n "${_HETZNER_REF_KIND[${nk}]:-}" ]]; then
        raw="$(hetzner::json -c --arg w "${what}" --argjson i "${i}" --arg k "${okey}" '.[$w][$i][$k]')"
        hetzner::_ref_ids "${what}" "${i}" "${nk}" "${raw}" "${mark}" || continue
        fields[x + 1]="${REPLY}"
      elif [[ "${nk}" == "image" ]]; then
        hetzner::_image_ok "${what}" "${name}" "${fields[x + 1]}" "${mark}" || continue
      fi

      # A boolean switch is one "--key=value" token; its placeholder is dropped
      # (the hook-suffix strip below applies to it like to any other key).
      if [[ "${fields[x]}" == --*=* ]]; then
        args+=("${fields[x]/%#*}")
        continue
      fi

      val="${fields[x + 1]}"
      # hcloud receives values verbatim — expand a leading ~ ourselves
      # (e.g. ssh-key public-key-from-file: ~/.ssh/...)
      # shellcheck disable=SC2088 # literal "~/" prefix match — expanded via $HOME on the same line, not meant to tilde-expand
      [[ "${val}" != "~/"* ]] || val="${HOME}/${val:2}"
      args+=("${fields[x]/%#*}" "${val}")
    done

    if (( ${#_HETZNER_REFUSED[@]} > 0 )); then
      hetzner::_print_refused
      error "hetzner: did not create ${what} '${name}': ${#_HETZNER_REFUSED[@]} of its references are refused (above). $(hetzner::_changes_note)"
      return 1
    fi

    # check for create hook. The owner mark comes after the descriptor's own
    # --label: hcloud merges repeated --label flags and the last value of a
    # key wins, so a descriptor cannot set another owner value.
    cmd=( 'dry-run::cmd' 'hcloud' "${what}" 'create' "${args[@]}"
          '--label' "${_HETZNER_OWNER_KEY}=${mark}" "${@}" )
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
