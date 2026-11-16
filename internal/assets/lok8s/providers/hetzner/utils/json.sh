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

dry-run::cmd() {
  _hetzner_log "cmd: ${*}"
  if [[ -n "${CLOUD_DRY_RUN}" ]]; then
    mkdir -p "${CLOUD_DRY_RUN_PATH}"
    echo "${@}" | tee -a "${CLOUD_DRY_RUN_PATH}/dry-run-cmd.log"
  else
    "${@}"
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

# The descriptor keys that refer to another resource. On these keys a number
# is an index into the top-level list of that resource type, and the create
# loop replaces it with that resource's hcloud id (`"ssh-key": [0]`,
# `"network": 0`, `"server": 1` on a volume). A number on any other key is a
# literal and passes verbatim (`"size": 100` on a volume, `"image": 12345` for
# a snapshot). Before, every number was dereferenced, and `"size": 100`
# reached hcloud as `--size null` (2026-10-23).
_HETZNER_REF_KEYS='["ssh-key","network","server","volume","placement-group","firewall"]'

# jq definition: the hcloud value of one descriptor entry (.key, .value).
# $root is the whole descriptor, $refs is _HETZNER_REF_KEYS. A list on a
# reference key becomes the comma-separated ids; a list on any other key
# becomes its comma-separated values.
_HETZNER_JQ_VALUE='
  def hcloud_value($root; $refs):
    .key as $key
    | if any($refs[]; . == $key) | not then
        (if (.value | type) == "array" then .value | map(tostring) | join(",") else .value end)
      elif (.value | type) == "number" then
        $root[$key][.value].id
      elif (.value | type) == "array" then
        [ .value[] | $root[$key][.].id ] | @csv
      else
        .value
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
  if [[ -z "${domain}" || "${domain}" == "." || "${domain}" == "/" ]]; then
    error "hetzner: the domain of the cluster is not known (PROVIDER_DOMAIN is empty). lok8s needs it for the owner mark ${_HETZNER_OWNER_KEY}. Run the provider through lo (lo provision, lo recover): lo sets it."
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
      error "hetzner: could not list the ${resource} parts ('hcloud ${resource} list'). Nothing was created or changed. Check the hcloud token and the API, then run the provision again."
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
      error "hetzner: refused ${kind} '${rname}' (id ${rid}): it exists, but it does not have the owner mark of this cluster (${_HETZNER_OWNER_KEY}=${mark}). lok8s does not use or change a part that can belong to another person or cluster. If this ${kind} is a part of this cluster (an older lok8s made it), mark it one time: hcloud ${kind} add-label ${rid} ${_HETZNER_OWNER_KEY}=${mark}. If it is not, give the ${kind} another name in the descriptor."
    else
      error "hetzner: refused ${kind} '${rname}' (id ${rid}): it has the owner mark of another cluster (${_HETZNER_OWNER_KEY}=${rmark}; this cluster: ${mark}). Give the ${kind} another name in the descriptor. Only if it is a part of this cluster (for example, after you renamed the domain), mark it again: hcloud ${kind} add-label --overwrite ${rid} ${_HETZNER_OWNER_KEY}=${mark}."
    fi
  done
  error "hetzner: ${#refused[@]} existing part(s) do not have the owner mark of this cluster. Nothing was created or changed. Do the step above for each part, then run the provision again."
  return 1
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
  local i id name rid
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

      # Extract #-prefixed metadata for the hooks
      # shellcheck disable=2016
      mapfile -t fields < <(hetzner::json -rc \
        --arg what "${what}" \
        --argjson i "${i}" \
        --argjson refs "${_HETZNER_REF_KEYS}" "${_HETZNER_JQ_VALUE}"' . as $root |
          .[$what][$i]
            | to_entries
            | .[]
            | "--" + .key, hcloud_value($root; $refs)
        '
      )

      # Run the create hook (for cloud-init) if defined, but without hcloud create
      if [[ -n "${create_hook-}" ]]; then
        # For bare metal, the hook gets called with a no-op command
        # The hook reads fields (including #cloud.d, #external-ip, etc.)
        hetzner::create::hook true
      fi

      [[ -z "${create_after-}" ]] ||
        hetzner::create::after

      continue
    fi

    # Cloud servers: skip if already created (has an id). (Bare metal handled
    # above — it must NOT be id-skipped; see the #cloud.root branch.)
    [[ -z "${id}" ]] || {
      _hetzner_print "🦗 skip \033[3m${what}\033[0m \033[1m${name}\033[0m (${id})"

      [[ -z "${create_after-}" ]] ||
        hetzner::create::after

      continue
    }

    # Cloud VMs: standard hcloud create

    # fetch all fields
    # A boolean value is a bare hcloud switch (`without-ipv6: true` →
    # `--without-ipv6=true`, the form cobra takes with no value token). It
    # travels as one "--key=value" token plus an empty placeholder that the
    # consumer below drops. Before this, `true` became `--without-ipv6 true`
    # and hcloud read the value as a stray argument (2026-09-30, B263: the
    # crowd's KubeOne workers needed IPv4-only image pulls).
    # shellcheck disable=2016
    mapfile -t fields < <(hetzner::json -rc \
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
      '
    )

    # filter args from fields (skip #-prefixed)
    local field val
    local -a cmd=() args=()
    for (( x=0; x < ${#fields[@]}; x=x+2 )); do
      [[ "${fields[x]:2:1}" != "#" ]] ||
        continue

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
    # ZERO control-plane hosts and hinted at name drift (B248, 2026-09-24,
    # crowd hour72). Nothing is rolled back: a re-run keeps what exists (the
    # id check above) and creates the rest.
    if ! "${cmd[@]}"; then
      error "hetzner: could not create ${what} '${name}'. Hetzner refused or failed the request; its message is above. For a placement refusal (resource_unavailable), choose another server type or location in the descriptor, or retry later. A re-run keeps what exists and creates the rest."
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
      hetzner::create::after
    fi
  done 9< <(hetzner::json::loop "${what}")
  unset IFS

  unset -f \
    hetzner::create::hook \
    hetzner::create::after

  hetzner::lookup "${what}"
}
