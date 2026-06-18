# shellcheck shell=bash disable=SC2034
# json.sh — Hetzner JSON descriptor engine
# JSON helpers, lookup, and generic resource create loop.
# Operates on the global CLOUD_HETZNER_JSON state.

# Log file for all provider operations (set by provider::provision)
: "${CLOUD_LOG_FILE:="/tmp/lok8s-hetzner.log"}"

# ── Output helpers ─────────────────────────────────────

_hetzner_log() {
  echo "[$(date -Iseconds)] $*" >> "${CLOUD_LOG_FILE}"
}

_hetzner_print() {
  _hetzner_log "$*"
  if [[ -z "${CLOUD_QUIET:-}" ]]; then
    echo -e "$*"
  fi
}

# ── Dry-run helpers ──────────────────────────────────────

dry-run::cmd() {
  _hetzner_log "cmd: $*"
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

# ── Lookup ───────────────────────────────────────────────

hetzner::lookup() {
  local resources="${1-}"
  [[ -n "${resources}" ]] ||
    mapfile -t resources < <(hetzner::json -r '[to_entries[] | select(.value | type == "array") | .key] | .[]')

  for resource in "${resources[@]}"; do
    _hetzner_print "☁️  fetching \033[3m${resource}\033[0m..."

    # shellcheck disable=2016
    hetzner::json::edit -r \
      --argjson remote "$(hcloud "${resource}" list -o json)" \
      --arg field "${resource}" 'if $remote == null then . else
        .[$field] = [ .[$field][] | .name as $name |
          . + reduce $remote[] as $item ({};
            if $item.name == $name then $item | {id} else . end
          )
        ]
      end'
  done
}

# ── Generic create loop ─────────────────────────────────

hetzner::create() {
  local -r what="${1}"; shift

  # Skip if this resource type doesn't exist in the descriptor
  local _has
  _has=$(hetzner::json -r --arg w "${what}" '.[$w] // empty | length')
  if [[ -z "${_has}" ]] || [[ "${_has}" == "0" ]]; then
    debug "skip ${what} (not in descriptor)"
    return 0
  fi

  local -r create_after="$(type -t hetzner::create::after)"
  local -r create_hook="$(type -t hetzner::create::hook)"

  local i id name
  while IFS=',' read -r i id name; do
    # Check for #cloud.root (bare metal / pre-existing server)
    local is_root
    is_root=$(hetzner::json -r --arg w "${what}" --argjson i "${i}" '.[$w][$i]["#cloud.root"] // ""')

    [[ -z "${id}" ]] || {
      _hetzner_print "🦗 skip \033[3m${what}\033[0m \033[1m${name}\033[0m (${id})"

      [[ -z "${create_after-}" ]] ||
        hetzner::create::after

      continue
    }

    # Bare metal servers: don't create via hcloud, just run hooks
    if [[ "${is_root}" == "true" ]]; then
      _hetzner_print "🔧 bare metal \033[3m${what}\033[0m \033[1m${name}\033[0m (pre-existing)"

      # Extract #-prefixed metadata for the hooks
      # shellcheck disable=2016
      mapfile -t fields < <(hetzner::json -rc \
        --arg what "${what}" \
        --argjson i "${i}" '. as $root |
          .[$what][$i]
            | to_entries
            | .[]
            | "--" + .key, (
              if .value
                | type == "number"
              then
                $root[.key][.value].id
              elif .value
                | type == "array"
              then
                [ .key as $key
                  | .value[]
                  | $root[$key][.].id
                ] | @csv
              else
                .value
              end
          )
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

    # Cloud VMs: standard hcloud create

    # fetch all fields
    # shellcheck disable=2016
    mapfile -t fields < <(hetzner::json -rc \
      --arg what "${what}" \
      --argjson i "${i}" '. as $root |
        .[$what][$i]
          | to_entries
          | .[]
          | "--" + .key, (
            if .value
              | type == "number"
            then
              $root[.key][.value].id
            elif .value
              | type == "array"
            then
              [ .key as $key
                | .value[]
                | $root[$key][.].id
              ] | @csv
            else
              .value
            end
        )
      '
    )

    # filter args from fields (skip #-prefixed)
    local field _val
    local -a cmd=() args=()
    for (( x=0; x < ${#fields[@]}; x=x+2 )); do
      [[ "${fields[x]:2:1}" != "#" ]] ||
        continue

      _val="${fields[x + 1]}"
      # hcloud receives values verbatim — expand a leading ~ ourselves
      # (e.g. ssh-key public-key-from-file: ~/.ssh/...)
      # shellcheck disable=SC2088 # literal "~/" prefix match — expanded via $HOME on the same line, not meant to tilde-expand
      [[ "${_val}" != "~/"* ]] || _val="${HOME}/${_val:2}"
      args+=("${fields[x]/%#*}" "${_val}")
    done

    # check for create hook
    cmd=( 'dry-run::cmd' 'hcloud' "${what}" 'create' "${args[@]}" "${@}" )
    [[ -z "${create_hook-}" ]] ||
      cmd=( "hetzner::create::hook" "${cmd[@]}" )

    _hetzner_print "📦 create \033[3m${what}\033[0m \033[1m${name}\033[0m"
    _hetzner_log "   $ ${cmd[*]}"
    "${cmd[@]}"

    [[ -z "${create_after-}" ]] ||
      hetzner::create::after
  done < <(hetzner::json::loop "${what}")
  unset IFS

  unset -f \
    hetzner::create::hook \
    hetzner::create::after

  hetzner::lookup "${what}"
}
