# shellcheck shell=bash disable=SC2016,SC2034,SC2154
# SC2016: single-quoted $ here is jq/yq/awk program text or a remote-script literal
# resources.sh — Hetzner resource-type dispatchers
# Each function sets up create hooks and delegates to hetzner::create.

hetzner::ssh-key() {
  hetzner::create 'ssh-key'
}

hetzner::floating-ip() {
  hetzner::create 'floating-ip'
}

hetzner::network() {
  # The hook reads the network by its verified id (rid), never by its name,
  # and checks its owner mark right before each add-subnet (a failed check
  # stops the step). A failed add-subnet stops the step too; an answer that
  # the subnet exists is accepted only when a new read shows it.
  hetzner::create::after() {
    local x network_zone type ip_range vswitch_id live err
    local -a subargs=()

    while IFS=',' read -r x network_zone type ip_range vswitch_id; do
      _hetzner_print "   ⭐ add \033[3msubnet\033[0m ${ip_range}"

      live="$(hetzner::_still_ours network "${rid}" "${mark}" "${name}")" || return 1
      if jq -e --arg r "${ip_range}" 'any(.subnets[]?; .ip_range == $r)' >/dev/null 2>&1 <<<"${live}"; then
        _hetzner_print "   🦗 skip \033[3msubnet\033[0m \033[1m${ip_range}\033[0m"
        continue
      fi

      subargs=()
      [[ -z "${vswitch_id}" ]] ||
        subargs+=("--vswitch-id" "${vswitch_id}")

      if ! err="$(dry-run::cmd hcloud network add-subnet "${rid}" \
          --network-zone "${network_zone}" \
          --type "${type}" \
          --ip-range "${ip_range}" "${subargs[@]}" 2>&1)"; then
        if [[ "${err}" == *"already"* ]] &&
           live="$(hetzner::_still_ours network "${rid}" "${mark}" "${name}")" &&
           jq -e --arg r "${ip_range}" 'any(.subnets[]?; .ip_range == $r)' >/dev/null 2>&1 <<<"${live}"; then
          _hetzner_print "   🦗 keep \033[3msubnet\033[0m \033[1m${ip_range}\033[0m (it exists)"
          continue
        fi
        error "hetzner: network ${name}: could not add the subnet ${ip_range}: ${err}"
        return 1
      fi
      [[ -z "${CLOUD_DRY_RUN:-}" ]] || printf '%s\n' "${err}"
    done < <(
      hetzner::json::loop \
        ".network[${i}][\"#subnets\"]" '.["network-zone"], .type, .["ip-range"], .["vswitch-id"]' \
        2>/dev/null
    )
    unset IFS
    return 0
  }

  hetzner::create 'network'
}

# ── #wipe-devices: guarded device wipe on FRESH (rescue-mode) install ──
#
# Declared per-server in the descriptor as `#wipe-devices`. Two forms:
#   true                          → wipe ALL physical disks (lsblk TYPE==disk)
#   [ {device?, model?, id?}, … ] → wipe each matched device, each behind a
#                                   udev model/id sanity guard (assert-or-abort)
#
# Honored TRANSPARENTLY by the bare-metal install path (not a separate command)
# and doubly gated: (1) rescue mode (RAM-booted, pre-installimage) AND (2) an
# installimage config actually present so the OS disk WILL be reinstalled right
# after. A live/already-installed node, or a rescue node with no installimage,
# is never wiped. Absent ⇒ no wipe (safe default).

# _safe — allow only shell-inert characters in a descriptor value that gets
# templated into the generated remote script. The descriptor is
# operator-authored, but a destructive op never templates arbitrary-shaped
# data into a shell (defense-in-depth against injection).
hetzner::wipe-devices::_safe() {
  local v="${1:-}" re='^[A-Za-z0-9_./:@=+ -]+$'
  [[ -n "${v}" ]] || return 0
  [[ "${v}" =~ ${re} ]] || return 1
}

# hetzner::wipe-devices::script <wipe-json>
# PURE: emit a bash remote-script string for the given #wipe-devices value.
# Empty / null / false ⇒ no-op (emits nothing). Anything other than a boolean
# or an array ⇒ rejected (return 1, emits nothing). The value is NEVER eval'd.
# shellcheck disable=SC2016  # the single-quoted printf formats below are LITERAL remote-script code — the $-refs MUST survive un-expanded to run on the server
hetzner::wipe-devices::script() {
  local wipe_json="${1:-}"
  [[ -n "${wipe_json}" && "${wipe_json}" != "null" ]] || return 0

  local json_type
  json_type="$(jq -r 'type' <<<"${wipe_json}" 2>/dev/null)" || {
    error "wipe-devices: value is not valid JSON"
    return 1
  }
  case "${json_type}" in
    boolean)
      # only `true` is a wipe request; `false` is an explicit no-op
      [[ "$(jq -r '.' <<<"${wipe_json}")" == "true" ]] || return 0
      ;;
    array) ;;
    *)
      error "wipe-devices: expected 'true' or an array of devices, got ${json_type}"
      return 1
      ;;
  esac

  # Header + rationale. FULL-disk discard (NOT head-only): Ceph bluestore
  # writes redundant bdev-label copies at size-scaled offsets ACROSS the whole
  # disk; a head-only wipe (sgdisk/wipefs) leaves the far copies, so a
  # freshly-prepared OSD then asserts _check_main_bdev_label N!=M (akin to
  # rook/rook#17716). We blkdiscard the ENTIRE device; if a device rejects
  # discard we ABORT — there is NO partial fallback (a head-only dd-zero would
  # leave those far copies behind and still emit the success sentinel: a silent
  # false-green). A device is either fully discarded or the install aborts loud.
  cat <<'WIPE_HEADER'
set -euo pipefail
# lok8s #wipe-devices — full-device blkdiscard on a fresh (rescue-mode) install.
# Full discard, not head-only: Ceph bluestore writes redundant bdev-label copies
# across the whole disk; a head wipe leaves the far copies and a fresh OSD then
# asserts _check_main_bdev_label N!=M (akin to rook/rook#17716). If a device does
# NOT support blkdiscard the install ABORTS — never a partial wipe.
WIPE_HEADER

  if [[ "${json_type}" == "boolean" ]]; then
    cat <<'WIPE_ALL'
echo "lok8s: #wipe-devices=true — wiping ALL physical disks"
lsblk -dn -o NAME,TYPE | while read -r _name _dtype; do
  [ "${_dtype}" = "disk" ] || continue
  _dev="/dev/${_name}"
  [ -b "${_dev}" ] || { echo "ABORT: ${_dev} not a block device"; exit 1; }
  echo "lok8s: wipe ${_dev}"
  blkdiscard -f "${_dev}" || { echo "ABORT: ${_dev} does not support blkdiscard — refusing a partial wipe that could leave stale on-disk state (e.g. Ceph BlueStore labels). Wipe it manually in rescue, or remove it from #wipe-devices."; exit 1; }
  wipefs -a "${_dev}" || true
done
WIPE_ALL
  else
    local entry device model id target v
    while IFS= read -r entry; do
      [[ -n "${entry}" ]] || continue
      # each array entry must be an object {device?,model?,id?} — reject a bare
      # string/number/null cleanly rather than emitting noisy jq type errors.
      [[ "$(jq -r 'type' <<<"${entry}")" == "object" ]] || {
        error "wipe-devices: array entries must be objects with device/model/id, got: ${entry}"
        return 1
      }
      device="$(jq -r '.device // ""' <<<"${entry}")"
      model="$(jq -r '.model // ""' <<<"${entry}")"
      id="$(jq -r '.id // ""' <<<"${entry}")"

      # boundary check: nothing shell-active reaches the templated script
      for v in "${device}" "${model}" "${id}"; do
        hetzner::wipe-devices::_safe "${v}" || {
          error "wipe-devices: refusing unsafe descriptor value: ${v}"
          return 1
        }
      done

      # path-safety (defense against traversal): a 'device' must be an absolute
      # /dev/… path, an 'id' a bare /dev/disk/by-id name. Reject '..' anywhere
      # and any '/' inside an id — the dd/blkdiscard target must never be a
      # relative or attacker-shaped path pointing outside /dev.
      if [[ -n "${device}" ]]; then
        [[ "${device}" == /dev/* && "${device}" != *..* ]] || {
          error "wipe-devices: 'device' must be an absolute /dev path without '..': ${device}"
          return 1
        }
      fi
      if [[ -n "${id}" ]]; then
        [[ "${id}" != */* && "${id}" != *..* ]] || {
          error "wipe-devices: 'id' must be a bare by-id name (no '/' or '..'): ${id}"
          return 1
        }
      fi

      if [[ -n "${device}" ]]; then
        target="${device}"
      elif [[ -n "${id}" ]]; then
        target="/dev/disk/by-id/${id}"
      else
        error "wipe-devices: entry needs at least 'device' or 'id'"
        return 1
      fi

      printf '\n# --- wipe %s ---\n' "${target}"
      printf '_props="$(udevadm info --query=property --name="%s" 2>/dev/null || true)"\n' "${target}"
      # model sanity: assert-or-abort BEFORE any destructive call. Whole-line
      # exact match (-qxF, like the id asserts below): a declared model that is
      # only a PREFIX of the real ID_MODEL must NOT match the wrong disk.
      if [[ -n "${model}" ]]; then
        printf 'printf "%%s\\n" "$_props" | grep -qxF "ID_MODEL=%s" || { echo "ABORT: %s identity mismatch (model)"; exit 1; }\n' \
          "${model}" "${target}"
      fi
      # id sanity: a stable id — udev ID_SERIAL or ID_WWN, or a /dev/disk/by-id match
      if [[ -n "${id}" ]]; then
        printf '_id_ok=0\n'
        printf 'printf "%%s\\n" "$_props" | grep -qxF "ID_SERIAL=%s" && _id_ok=1 || true\n' "${id}"
        printf 'printf "%%s\\n" "$_props" | grep -qxF "ID_WWN=%s" && _id_ok=1 || true\n' "${id}"
        printf 'if [ -e "/dev/disk/by-id/%s" ] && [ "$(readlink -f "/dev/disk/by-id/%s")" = "$(readlink -f "%s")" ]; then _id_ok=1; fi\n' \
          "${id}" "${id}" "${target}"
        printf '[ "$_id_ok" = 1 ] || { echo "ABORT: %s identity mismatch (id)"; exit 1; }\n' "${target}"
      fi
      printf 'echo "lok8s: wipe %s"\n' "${target}"
      # block-device assert: never blkdiscard a target that is not a real
      # block special file (a mistyped/renamed by-id path must fail closed).
      printf '[ -b "%s" ] || { echo "ABORT: %s not a block device"; exit 1; }\n' "${target}" "${target}"
      printf 'blkdiscard -f "%s" || { echo "ABORT: %s does not support blkdiscard — refusing a partial wipe that could leave stale on-disk state (e.g. Ceph BlueStore labels). Wipe it manually in rescue, or remove it from #wipe-devices."; exit 1; }\n' "${target}" "${target}"
      printf 'wipefs -a "%s" || true\n' "${target}"
    done < <(jq -c '.[]' <<<"${wipe_json}")
  fi

  printf '\necho "__LOK8S_WIPE_DONE__"\n'
}

# hetzner::wipe-devices <server_index> <ssh_user@ip> <ssh_opts...>
# Read .server[<server_index>]["#wipe-devices"], validate at the boundary (type-check
# ONLY — never eval), generate the wipe script and pipe it over ssh. Requires
# the success sentinel AND no ABORT in the output. Returns non-zero on any
# abort/failure so the install path fails LOUDLY rather than installing over
# un-wiped disks.
hetzner::wipe-devices() {
  local server_index="${1}" dest="${2}"
  shift 2
  local -a ssh_opts=("${@}")

  local wipe_json
  wipe_json="$(hetzner::json -c ".server[${server_index}][\"#wipe-devices\"] // null")"
  [[ -n "${wipe_json}" && "${wipe_json}" != "null" ]] || return 0

  # Boundary validation: only `true` or an array are legal; reject (never eval)
  # anything else.
  local json_type
  json_type="$(jq -r 'type' <<<"${wipe_json}" 2>/dev/null)" || {
    error "wipe-devices: #wipe-devices on server[${server_index}] is not valid JSON"
    return 1
  }
  case "${json_type}" in
    array) ;;
    boolean)
      [[ "$(jq -r '.' <<<"${wipe_json}")" == "true" ]] || return 0
      ;;
    *)
      error "wipe-devices: #wipe-devices must be 'true' or an array of devices (got ${json_type})"
      return 1
      ;;
  esac

  local script
  script="$(hetzner::wipe-devices::script "${wipe_json}")" || {
    error "wipe-devices: could not build wipe script for server[${server_index}]"
    return 1
  }
  [[ -n "${script}" ]] || return 0

  _hetzner_print "   🧨 \033[1m#wipe-devices\033[0m: wiping declared devices on \033[1m${dest}\033[0m (fresh install)"

  local out ok=1
  out="$(printf '%s\n' "${script}" | ssh "${ssh_opts[@]}" "${dest}" 'bash -s' 2>&1)" || ok=0
  grep -q '__LOK8S_WIPE_DONE__' <<<"${out}" || ok=0
  # anchored to the explicit 'ABORT:' prefix our guards emit — a stray 'ABORT'
  # substring in unrelated output must NOT be read as a wipe failure.
  ! grep -q '^ABORT:' <<<"${out}" || ok=0
  if (( ! ok )); then
    error "wipe-devices: device wipe FAILED on ${dest} — aborting install (nothing else touched)"
    grep -E 'ABORT|rror|annot' <<<"${out}" | sed 's/^/      /' >&2 || true
    return 1
  fi
  _hetzner_print "   ✅ device wipe complete on \033[1m${dest}\033[0m"
}

hetzner::server() {
  # Assign the floating IP by the verified ids of the IP and the server
  # (rid), never by a name. Both are read by id and checked right before the
  # assign (a failed check stops the step). A floating IP on this server
  # already stays; a failed assign stops the step.
  hetzner::create::after() {
    local findex fid fip err
    findex="$(hetzner::json -r ".server[${i}][\"#floating-ip\"]")"
    [[ "${findex}" != "null" ]] || return 0

    fid="$(hetzner::json -r ".[\"floating-ip\"][${findex}].id // \"\" | tostring")"
    [[ -n "${fid}" || -z "${CLOUD_DRY_RUN:-}" ]] ||
      fid="$(hetzner::json -r ".[\"floating-ip\"][${findex}].name // \"\"")"
    if [[ -z "${fid}" || -z "${rid}" ]]; then
      warn "hetzner: server ${name}: did not assign floating-ip ${findex}. The floating IP or the server has no id of this cluster (a bare-metal server cannot get a floating IP)."
      return 0
    fi
    fip="$(hetzner::_still_ours floating-ip "${fid}" "${mark}")" || return 1
    hetzner::_still_ours server "${rid}" "${mark}" "${name}" >/dev/null || return 1
    if jq -e --arg s "${rid}" '((.server // "") | tostring) == $s' >/dev/null 2>&1 <<<"${fip}"; then
      _hetzner_print "   🦗 keep \033[3mfloating-ip\033[0m ${fid} on server ${rid}"
      return 0
    fi
    if ! err="$(dry-run::cmd hcloud floating-ip assign "${fid}" "${rid}" 2>&1)"; then
      error "hetzner: server ${name}: could not assign floating-ip ${fid}: ${err}"
      return 1
    fi
    [[ -z "${CLOUD_DRY_RUN:-}" ]] || printf '%s\n' "${err}"
    return 0
  }

  # Per-server cloud-init: reads #cloud.d from each server entry,
  # exports all #-prefixed fields as CLOUD_ENV_* variables, and
  # generates a per-server cloud-config user-data file.
  hetzner::create::hook() { (
    # Capture the server index BEFORE the field-export loop below reuses `i`
    # as its counter (needed to read this server's #wipe-devices fresh).
    local server_index="${i}"

    # shellcheck disable=SC2030  # the hook body is a per-server subshell; reusing i cannot leak out
    for (( i=0; i < ${#fields[@]}; i=i+2 )); do
      field="${fields[i]#--}"

      # set CLOUD_PATHD for per-server cloud.d modules
      [[ "${field}" != "#cloud.d" ]] ||
        export CLOUD_PATHD="${fields[i+1]}"

      # export all #-prefixed fields as CLOUD_ENV_* for cloud-config templates
      # shellcheck disable=SC2001
      field="$(echo "${field^^}" | sed "s/[^A-Z]/_/g")"
      export "CLOUD_ENV_${field}=${fields[i+1]}"
    done

    # For bare metal servers (#cloud.root), handle installimage + cloud-init
    if [[ "${CLOUD_ENV__CLOUD_ROOT:-}" == "true" ]]; then
      local external_ip="${CLOUD_ENV__EXTERNAL_IP:-}"
      local ssh_user="${CLOUD_USER:-root}"
      local installimage_conf="${CLOUD_ENV__INSTALLIMAGE:-}"

      # Non-interactive SSH for the install path. Host keys are NOT
      # pinned here on purpose: they change twice during provisioning
      # (rescue system -> installed OS), so pinning would always fail.
      # Identity comes from the descriptor's sshPrivateKey.
      local ssh_id
      ssh_id="$(hetzner::json -r '.sshPrivateKey // ""')"
      # shellcheck disable=SC2088 # literal "~/" prefix match — expanded via $HOME on the next clause, not meant to tilde-expand
      [[ "${ssh_id}" != "~/"* ]] || ssh_id="${HOME}/${ssh_id:2}"
      local -a ssh_opts=(
        -o BatchMode=yes
        -o StrictHostKeyChecking=no
        -o UserKnownHostsFile=/dev/null
        -o LogLevel=ERROR
      )
      [[ -z "${ssh_id}" ]] || ssh_opts+=(-i "${ssh_id}")

      if [[ -n "${external_ip}" ]]; then
        _hetzner_log "bare metal: checking ${external_ip} for rescue mode"

        # Check if server is in rescue mode (installimage binary exists)
        if ssh -o ConnectTimeout=10 "${ssh_opts[@]}" \
             "${ssh_user}@${external_ip}" \
             'test -x /root/.oldroot/nfs/install/installimage' 2>/dev/null; then

          _hetzner_print "🔧 rescue mode detected on \033[1m${external_ip}\033[0m"

          if [[ -n "${installimage_conf}" ]] && [[ -f "${installimage_conf}" ]]; then
            # Honor #wipe-devices ONLY when installimage WILL run — co-gated with
            # the installimage-present check so a wipe can NEVER happen unless the
            # OS disk is about to be reinstalled. We are in the rescue branch
            # (RAM-booted, disks not yet reinstalled) AND have an installimage
            # config, so wiping here is safe. Doing this BEFORE the config check
            # would brick a node declared with #wipe-devices but no/invalid
            # #installimage (wiped OS disk, then "skipping install"). A declared
            # model/id mismatch, an unverified block device, or a traversal-shaped
            # target aborts (non-zero) and touches nothing.
            hetzner::wipe-devices "${server_index}" "${ssh_user}@${external_ip}" "${ssh_opts[@]}" || {
              error "device wipe failed/aborted on ${external_ip}"
              return 1
            }

            _hetzner_print "   📋 running installimage with ${installimage_conf}"
            scp "${ssh_opts[@]}" "${installimage_conf}" "${ssh_user}@${external_ip}:/tmp/installimage.conf"

            # installimage `-x` post-install runs in the installed chroot:
            # install cloud-init + seed our user-data so the node self-boots
            # via cloud-init on first boot, exactly like a cloud VM. Without
            # it the Hetzner base image (no cloud-init) boots unconfigured —
            # no vswitch, no private network.
            local post_install_flag=""
            if declare -F cloud-config::installimage-post-install &>/dev/null; then
              if cloud-config::installimage-post-install \
                   | ssh "${ssh_opts[@]}" "${ssh_user}@${external_ip}" \
                       'cat > /tmp/lok8s-post-install && chmod +x /tmp/lok8s-post-install'; then
                post_install_flag=" -x /tmp/lok8s-post-install"
              else
                warn "could not stage installimage post-install on ${external_ip}"
              fi
            fi

            # shellcheck disable=SC2029  # post_install_flag expands CLIENT-side by design
            ssh "${ssh_opts[@]}" "${ssh_user}@${external_ip}" \
              "echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections 2>/dev/null; \
               /root/.oldroot/nfs/install/installimage -a -c /tmp/installimage.conf${post_install_flag} && reboot" || {
              error "installimage failed on ${external_ip}"
              return 1
            }

            _hetzner_print "   ⏳ waiting for reboot..."
            sleep 30
            local attempts=0
            while (( attempts < 60 )); do
              if ssh -o ConnectTimeout=5 "${ssh_opts[@]}" \
                   "${ssh_user}@${external_ip}" true 2>/dev/null; then
                _hetzner_print "   ✅ server rebooted and reachable"
                break
              fi
              attempts=$(( attempts + 1 ))
              sleep 5
            done
            if (( attempts >= 60 )); then
              error "server ${external_ip} not reachable after installimage reboot"
              return 1
            fi
            # Bare-metal bootstrap runs SOLELY here — after a fresh rescue-mode
            # installimage. cloud-init self-configures on first boot from the
            # user-data the post-install seeded; wait for it before the cluster
            # driver touches the node. An already-installed node (not in rescue)
            # self-bootstrapped on its own first boot, so it is left untouched.
            local ci_done=0
            if ssh "${ssh_opts[@]}" "${ssh_user}@${external_ip}" \
                 'command -v cloud-init >/dev/null 2>&1'; then
              _hetzner_print "   ⏳ waiting for cloud-init self-bootstrap on \033[1m${external_ip}\033[0m"
              if timeout 600 ssh "${ssh_opts[@]}" "${ssh_user}@${external_ip}" \
                   'cloud-init status --wait >/dev/null 2>&1'; then
                _hetzner_print "   ✅ cloud-init self-bootstrap complete"
                ci_done=1
              else
                warn "cloud-init did not finish cleanly on ${external_ip} (cloud-init status --long)"
              fi
            fi

            # Fallback: if cloud-init is absent or errored, apply the node
            # config directly over SSH (write_files + runcmd:true scripts).
            if (( ! ci_done )) && declare -F cloud-config::remote-script &>/dev/null; then
              local remote_script
              remote_script="$(cloud-config::remote-script)"
              if [[ -n "${remote_script}" ]]; then
                _hetzner_print "   🔧 fallback: applying node config directly to \033[1m${external_ip}\033[0m"
                local out ok=1
                out="$(printf '%s\n' "${remote_script}" | ssh "${ssh_opts[@]}" \
                          "${ssh_user}@${external_ip}" 'bash -s' 2>&1)" || ok=0
                grep -q '__LOK8S_BOOTINIT_DONE__' <<<"${out}" || ok=0
                ! grep -q 'lok8s: boot-init FAILED' <<<"${out}" || ok=0
                if (( ! ok )); then
                  warn "node config apply incomplete on ${external_ip}:"
                  grep -E 'FAILED|rror|annot' <<<"${out}" | sed 's/^/      /' >&2 || true
                fi
              fi
            fi
          else
            warn "rescue mode but no installimage config specified — skipping install"
          fi
        else
          _hetzner_log "bare metal: ${external_ip} not in rescue mode (already installed)"
        fi
      fi
      return 0
    fi

    # Cloud VMs: generate cloud-config and pass to hcloud
    if declare -F cloud-config::generate &>/dev/null; then
      dry-run::cmd "${@}" \
        --user-data-from-file <(dry-run::cloud-config "${name}" "$(cloud-config::generate)")
    else
      dry-run::cmd "${@}"
    fi
  ) }

  hetzner::create 'server'
}

hetzner::volume() {
  hetzner::create 'volume'
}

# hetzner::_lb_targets <live-json>
# One line per top-level target of a described LB:
# "<type> <selector|ip|server id> use-private-ip=<bool>"; "(no target)" when
# it has none.
hetzner::_lb_targets() {
  local out
  out="$(jq -r '(.targets // [])[]
    | "\(.type) \(.label_selector.selector // .ip.ip // (.server.id | tostring)) use-private-ip=\(.use_private_ip // false)"' \
    <<<"${1}" 2>/dev/null)" || out=""
  printf '%s\n' "${out:-(no target)}"
}

# hetzner::_shell_word <value>
# <value> as one shell word, for a command that a message tells the user to
# run. A value of only the characters A-Z a-z 0-9 . _ / = , : @ % + - stays
# as it is; any other value is quoted with printf %q. A label selector can
# have a space, a "!" or parentheses ("env in (a, b)", "!key").
hetzner::_shell_word() {
  local plain='^[A-Za-z0-9._/=,:@%+-]+$'
  if [[ "${1}" =~ ${plain} ]]; then
    printf '%s' "${1}"
  else
    printf '%q' "${1}"
  fi
}

# hetzner::_lb_add_target <lb> <lb-name> <mark> <server-id> <server-name>
#                         <api-type> <value> <want-private> <add-target args...>
# add-target with retries: HETZNER_LB_ADD_TRIES attempts (default 4); the
# wait starts at HETZNER_LB_ADD_DELAY seconds (default 2) and doubles. Right
# before each attempt it reads both ends by id and checks their owner mark:
# the load balancer, and the server of a server target (<server-id>; empty
# for a selector or an ip target). An add can be done while its answer is
# lost: then the next attempt gets the API error target_already_defined.
# That answer is a success only when a new read of the load balancer by id
# shows the target (<api-type> <value>, as hetzner::_lb_target_private
# reads it) with use_private_ip <want-private>. rc 1 with the last hcloud
# error on stdout; rc 2 when a read or a check before an attempt failed (the
# message is on stderr): then the caller stops.
hetzner::_lb_add_target() {
  local lb="${1}" lbname="${2}" mark="${3}" sid="${4}" sname="${5}"
  local api_type="${6}" value="${7}" want="${8}"; shift 8
  local tries="${HETZNER_LB_ADD_TRIES:-4}" delay="${HETZNER_LB_ADD_DELAY:-2}" n err="" live
  # hcloud prints an API error as "<message> (<code>)" or "<message> (<code>,
  # <correlation id>)". Only this code: another "already" is not this target.
  local defined='\(target_already_defined[,)]'
  for (( n = 1; n <= tries; n++ )); do
    hetzner::_still_ours load-balancer "${lb}" "${mark}" "${lbname}" >/dev/null || return 2
    if [[ -n "${sid}" ]]; then
      hetzner::_still_ours server "${sid}" "${mark}" "${sname}" >/dev/null || return 2
    fi
    err="$(dry-run::cmd hcloud load-balancer add-target "${lb}" "${@}" 2>&1)" && return 0
    if [[ "${err}" =~ ${defined} ]] &&
       live="$(hetzner::_still_ours load-balancer "${lb}" "${mark}" "${lbname}")" &&
       [[ "$(hetzner::_lb_target_private "${live}" "${api_type}" "${value}")" == "${want}" ]]; then
      _hetzner_log "lb ${lb}: add-target attempt ${n}/${tries}: the target is defined already, and a new read shows it with use-private-ip ${want}"
      return 0
    fi
    _hetzner_log "lb ${lb}: add-target attempt ${n}/${tries} failed: ${err}"
    (( n == tries )) || sleep "${delay}"
    delay=$(( delay * 2 ))
  done
  printf '%s' "${err}"
  return 1
}

# hetzner::_lb_target_private <live-json> <api-type> <value>
# The use_private_ip of the live target with this type and value ("true" or
# "false"); empty when the load balancer has no such target.
hetzner::_lb_target_private() {
  jq -r --arg type "${2}" --arg value "${3}" '
    [ (.targets // [])[]
      | select(.type == $type)
      | select(if $type == "server" then ((.server.id // "") | tostring) == $value
               elif $type == "label_selector" then (.label_selector.selector // "") == $value
               else (.ip.ip // "") == $value end)
      | (.use_private_ip // false) | tostring
    ] | first // ""' <<<"${1}" 2>/dev/null || true
}

# hetzner::_owned_selector <selector> <mark>
# The label selector of a load-balancer target, limited to the servers with
# the owner mark of this cluster: a selector of the cluster and role labels
# alone also selects a server of another cluster with the same labels. Adds
# ",lok8s.dev/owner=<mark>". A selector that names lok8s.dev/owner already
# must name it as exactly that term, one time; any other form fails (rc 1).
hetzner::_owned_selector() {
  local selector="${1}" term="${_HETZNER_OWNER_KEY}=${2}" rest
  if [[ "${selector}" != *"${_HETZNER_OWNER_KEY}"* ]]; then
    printf '%s\n' "${selector:+${selector},}${term}"
    return 0
  fi
  rest=",${selector},"
  [[ "${rest}" == *",${term},"* ]] || return 1
  rest="${rest/",${term},"/,}"
  [[ "${rest}" != *"${_HETZNER_OWNER_KEY}"* ]] || return 1
  printf '%s\n' "${selector}"
}

# hetzner::_lb_audit <lb-id> <lb-name> <mark>
# After the target reconcile: reads the load balancer by its id again and
# removes EVERY target that can route to a server of another owner: a label
# selector that does not hold exactly lok8s.dev/owner=<mark> (also one that
# the descriptor no longer has), and a server target whose server does not
# have the mark. An ip target stays: lok8s cannot know whose address it is.
# This fails closed: when the add of the selector with the mark failed, the
# old selector is removed all the same, and the load balancer routes to
# fewer servers until the next run. Right before each removal it reads the
# load balancer by id again and checks its owner mark: a failed read or
# check stops the audit at once. rc 1 when a read, a check or a removal
# fails (the message names the target); the caller then stops the hook of
# this load balancer, so no service changes after it.
hetzner::_lb_audit() {
  local lb="${1}" lbname="${2}" mark="${3}" live servers type value owned err hint word rc=0
  local -a rm_flags=()
  live="$(hetzner::_still_ours load-balancer "${lb}" "${mark}" "${lbname}")" || return 1
  servers='[]'
  if jq -e 'any((.targets // [])[]; .type == "server")' >/dev/null 2>&1 <<<"${live}"; then
    if ! servers="$(hcloud server list -o json)" ||
       ! jq -e 'type == "array" or type == "null"' >/dev/null 2>&1 <<<"${servers}"; then
      error "lb ${lbname}: could not list the servers ('hcloud server list'), so the server targets of the load balancer are not checked."
      return 1
    fi
  fi
  while IFS=$'\x1f' read -r type value; do
    [[ -n "${type}" ]] || continue
    case "${type}" in
      label_selector)
        if owned="$(hetzner::_owned_selector "${value}" "${mark}")" && [[ "${owned}" == "${value}" ]]; then
          continue
        fi
        _hetzner_print "   ♻️  remove \033[3mtarget\033[0m label-selector: ${value} (it does not hold ${_HETZNER_OWNER_KEY}=${mark}, so it can select a server of another owner)"
        rm_flags=(--label-selector "${value}") ;;
      server)
        if jq -e --arg id "${value}" --arg key "${_HETZNER_OWNER_KEY}" --arg mark "${mark}" \
             'any((. // [])[]; (.id | tostring) == $id and (.labels // {})[$key] == $mark)' >/dev/null 2>&1 <<<"${servers}"; then
          continue
        fi
        _hetzner_print "   ♻️  remove \033[3mtarget\033[0m server: ${value} (the server does not have the owner mark of this cluster)"
        rm_flags=(--server "${value}") ;;
      *) continue ;;
    esac
    hetzner::_still_ours load-balancer "${lb}" "${mark}" "${lbname}" >/dev/null || return 1
    if ! err=$(dry-run::cmd hcloud load-balancer remove-target "${lb}" "${rm_flags[@]}" 2>&1); then
      # One shell word per argument: a selector can have a space.
      hint="hcloud load-balancer remove-target"
      for word in "${lbname}" "${rm_flags[@]}"; do hint+=" $(hetzner::_shell_word "${word}")"; done
      error "lb ${lbname}: could not remove the target ${type} '${value}': ${err}. It can route to a server of another owner. Remove it with '${hint}', then run the provision again."
      rc=1
    fi
  done < <(jq -r '(.targets // [])[]
    | [ (.type // ""), (.label_selector.selector // (.server.id // "" | tostring)) ] | join("\u001f")' <<<"${live}" 2>/dev/null)
  return "${rc}"
}

hetzner::load-balancer() {
  # Set by the create::after hook below when a check or a write fails. The
  # hook then returns 0, so the create loop goes on with the next load
  # balancer; this step fails at the end.
  _HETZNER_LB_FAILED=0

  # Every hcloud call of the hook goes to the load balancer by its verified
  # id (rid) and to the network and servers by their verified ids, never by
  # a name: a name can be another cluster's part. Right before EACH write
  # (attach, target add or remove, each retry, the audit removals, service
  # add or update), the load balancer is read by id and its owner mark is
  # checked again (and the network or server of the write). A failed read or
  # check stops the hook of this load balancer at once: no write after it.
  # A failed write fails the step; an answer that the thing exists already is
  # accepted only when a new read shows it.
  hetzner::create::after() {
    # Attach the LB to the first descriptor network — required before
    # use-private-ip targets can be added. Both ends are checked first.
    local net_name net_ref err arc live
    net_name="$(hetzner::json -r '.network[0].name // ""')"
    net_ref="$(hetzner::json -r '.network[0].id // "" | tostring')"
    [[ -n "${net_ref}" || -z "${CLOUD_DRY_RUN:-}" ]] || net_ref="${net_name}"
    if [[ -n "${net_name}" && -z "${net_ref}" ]]; then
      warn "lb ${name}: did not attach the load balancer to network '${net_name}': the network has no id of this cluster."
    elif [[ -n "${net_name}" ]]; then
      if ! hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}" >/dev/null ||
         ! hetzner::_still_ours network "${net_ref}" "${mark}" "${net_name}" >/dev/null; then
        _HETZNER_LB_FAILED=1
        return 0
      fi
      if ! err=$(dry-run::cmd hcloud load-balancer attach-to-network "${rid}" \
          --network "${net_ref}" 2>&1); then
        # A re-run: the load balancer is on the network already.
        if [[ "${err}" != *"already"* ]]; then
          error "lb ${name}: could not attach the load balancer to network '${net_name}' (id ${net_ref}): ${err}"
          _HETZNER_LB_FAILED=1
          return 0
        fi
        if ! live="$(hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}")"; then
          _HETZNER_LB_FAILED=1
          return 0
        fi
        if ! jq -e --arg n "${net_ref}" 'any((.private_net // [])[]; ((.network // "") | tostring) == $n)' \
             >/dev/null 2>&1 <<<"${live}"; then
          error "lb ${name}: could not attach the load balancer to network '${net_name}' (id ${net_ref}): ${err}. A new read does not show the network on the load balancer."
          _HETZNER_LB_FAILED=1
          return 0
        fi
      fi
    fi

    # Reconcile the targets and services of this LB to the descriptor
    # (issue #296). An adopted LB keeps what it had: an add-target that
    # answered "already" left a target with use-private-ip=true private after
    # the descriptor dropped use-private-ip. Read the live LB once and compare.
    # `hcloud ... -o json` prints the API schema: targets[].type is server,
    # label_selector or ip, with use_private_ip; services[] carry protocol,
    # listen_port and destination_port. The read checks the owner mark too:
    # an LB that cannot be read and checked gets no change. A dry run of a
    # new LB reads as empty: every target and service is added.
    if ! live="$(hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}")"; then
      error "lb ${name}: did not change the targets and services of the load balancer: lok8s could not read it and check its owner mark (the message above)."
      _HETZNER_LB_FAILED=1
      return 0
    fi

    # NOTE: rows are read as JSON objects (jq -c), NOT csv — label
    # selectors legitimately contain commas and csv-splitting mangled
    # them (and the resulting hcloud errors were silently swallowed).
    local row target_type target_value use_private_ip want_private live_private
    local api_type match_value line server_name owned
    # shellcheck disable=SC2031  # i is the create-loop index. Only the server hook's subshell reuses i (SC2030); this hook runs in the current shell
    while IFS= read -r row; do
      [[ -n "${row}" ]] || continue
      target_type=$(jq -r '.type // empty' <<<"${row}")
      target_value=$(jq -r '.value // empty' <<<"${row}")
      use_private_ip=$(jq -r '.["use-private-ip"] // "false"' <<<"${row}")
      [[ -n "${target_type}" ]] || continue
      want_private="true"
      [[ "${use_private_ip}" != "false" ]] || want_private="false"

      local -a target_args=()
      match_value="${target_value}"
      server_name=""
      case "${target_type}" in
        server)
          # A server of the descriptor only (an index or its name), by its
          # verified id. Any other name can be another cluster's server.
          server_name="${target_value}"
          if [[ "${target_value}" =~ ^[0-9]+$ ]]; then
            server_name="$(hetzner::json -r --arg idx "${target_value}" '.server[($idx | tonumber)].name // ""')"
          fi
          if [[ -z "${server_name}" ]] ||
             ! hetzner::json -e --arg n "${server_name}" 'any(.server[]?; .name == $n)' >/dev/null 2>&1; then
            error "lb ${name}: refused target server '${target_value}': it is not a server of the descriptor. lok8s adds only the servers of this cluster as targets."
            _HETZNER_LB_FAILED=1
            continue
          fi
          target_value="$(hetzner::json -r --arg n "${server_name}" '[.server[]? | select(.name == $n) | .id // empty][0] // "" | tostring')"
          if [[ -z "${target_value}" ]]; then
            # The server step runs after this one: a server made in this run
            # has no id yet. The next provision adds the target.
            _hetzner_print "   🦗 later \033[3mtarget\033[0m server: ${server_name} (not made yet; the next provision adds it)"
            continue
          fi
          target_args+=("--server" "${target_value}")
          # The live target holds the server id only.
          match_value="${target_value}"
          api_type="server"
          ;;
        label-selector)
          if ! owned="$(hetzner::_owned_selector "${target_value}" "${mark}")"; then
            error "lb ${name}: refused target label-selector '${target_value}': it names ${_HETZNER_OWNER_KEY} in another form or with another value. Remove that term from the descriptor: lok8s adds ${_HETZNER_OWNER_KEY}=${mark} itself."
            _HETZNER_LB_FAILED=1
            continue
          fi
          target_value="${owned}"
          match_value="${owned}"
          target_args+=("--label-selector" "${owned}")
          api_type="label_selector"
          ;;
        ip)
          target_args+=("--ip" "${target_value}")
          api_type="ip"
          ;;
        *)
          warn "lb ${name}: skipped target type '${target_type}'. Use server, label-selector or ip."
          continue
          ;;
      esac

      # use_private_ip of the live target with this type and value; empty when
      # the LB has no such target.
      live_private="$(hetzner::_lb_target_private "${live}" "${api_type}" "${match_value}")"

      local -a private_arg=()
      [[ "${want_private}" == "false" ]] || private_arg=("--use-private-ip")

      if [[ "${live_private}" == "${want_private}" ]]; then
        _hetzner_print "   🦗 keep \033[3mtarget\033[0m ${target_type}: ${target_value}"
        continue
      fi

      # A write follows: read the load balancer (and the server of a server
      # target) by id and check the owner mark right before it. A changed
      # part stops the hook of this load balancer.
      if ! hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}" >/dev/null ||
         { [[ "${api_type}" == "server" ]] &&
           ! hetzner::_still_ours server "${target_value}" "${mark}" "${server_name}" >/dev/null; }; then
        _HETZNER_LB_FAILED=1
        return 0
      fi

      if [[ -n "${live_private}" ]]; then
        # The API cannot change use_private_ip on a target, and it refuses a
        # second target of the same type and value: remove, then add. Between
        # the two the LB does not route to this target. With one target (the
        # API LB of a cluster) the port is down until the add is done.
        # A private target needs the LB on a network. A descriptor without a
        # network gives no attach above, so check the live LB before the
        # remove.
        if [[ "${want_private}" == "true" ]] &&
           ! jq -e '(.private_net // []) | length > 0' >/dev/null 2>&1 <<<"${live}"; then
          error "lb ${name}: kept target ${target_type} '${target_value}' with use-private-ip ${live_private}. The load balancer is on no private network, so the target cannot get use-private-ip true. Attach the load balancer to the network ('hcloud load-balancer attach-to-network'), then run the provision again."
          _HETZNER_LB_FAILED=1
          continue
        fi
        _hetzner_print "   ♻️  replace \033[3mtarget\033[0m ${target_type}: ${target_value} (use-private-ip ${live_private} -> ${want_private})"
        while IFS= read -r line; do _hetzner_print "      before: ${line}"; done < <(hetzner::_lb_targets "${live}")
        if ! err=$(dry-run::cmd hcloud load-balancer remove-target "${rid}" \
            "${target_args[@]}" 2>&1); then
          error "lb ${name}: could not remove target ${target_type} '${target_value}' to set use-private-ip to ${want_private}: ${err}. Remove it with 'hcloud load-balancer remove-target', then run the provision again."
          _HETZNER_LB_FAILED=1
          continue
        fi
        arc=0
        local add_sid=""
        [[ "${api_type}" != "server" ]] || add_sid="${target_value}"
        err=$(hetzner::_lb_add_target "${rid}" "${name}" "${mark}" "${add_sid}" "${server_name}" \
          "${api_type}" "${match_value}" "${want_private}" \
          "${target_args[@]}" "${private_arg[@]}") || arc=$?
        if (( arc == 2 )); then
          error "lb ${name}: removed target ${target_type} '${target_value}', and did not add it again: the load balancer or the server changed (the message above). The load balancer does not route to this target now."
          _HETZNER_LB_FAILED=1
          return 0
        fi
        if (( arc != 0 )); then
          # One shell word per argument: a selector can have a space.
          local hint="hcloud load-balancer add-target" word
          for word in "${name}" "${target_args[@]}" "${private_arg[@]}"; do
            hint+=" $(hetzner::_shell_word "${word}")"
          done
          error "lb ${name}: removed target ${target_type} '${target_value}', but could not add it again with use-private-ip ${want_private} in ${HETZNER_LB_ADD_TRIES:-4} attempts: ${err}. The load balancer does not route to this target now. Add it with '${hint}', or run the provision again."
          _HETZNER_LB_FAILED=1
        fi
        if [[ -z "${CLOUD_DRY_RUN:-}" ]]; then
          local after
          after="$(hcloud load-balancer describe "${rid}" -o json 2>/dev/null)" || after=""
          while IFS= read -r line; do _hetzner_print "      after:  ${line}"; done < <(hetzner::_lb_targets "${after}")
        fi
        continue
      fi

      _hetzner_print "   ⭐ add \033[3mtarget\033[0m ${target_type}: ${target_value}"
      if ! err=$(dry-run::cmd hcloud load-balancer add-target "${rid}" \
          "${target_args[@]}" "${private_arg[@]}" 2>&1); then
        # "already": accepted only when a new read shows the target.
        if [[ "${err}" == *"already"* ]]; then
          if ! live="$(hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}")"; then
            _HETZNER_LB_FAILED=1
            return 0
          fi
          [[ "$(hetzner::_lb_target_private "${live}" "${api_type}" "${match_value}")" != "${want_private}" ]] ||
            continue
        fi
        error "lb ${name}: could not add target ${target_type} '${target_value}': ${err}. Run the provision again."
        _HETZNER_LB_FAILED=1
        continue
      fi
    done < <(hetzner::json -c ".[\"load-balancer\"][${i}][\"#targets\"][]?" 2>/dev/null)

    # Every target that can route to a server of another owner goes: an old
    # selector without the owner mark (an older lok8s made it), a selector
    # that the descriptor dropped, a server target without the mark.
    # A failed audit stops the hook: no service write after it.
    if ! hetzner::_lb_audit "${rid}" "${name}" "${mark}"; then
      _HETZNER_LB_FAILED=1
      return 0
    fi

    local protocol listen_port dest_port live_service live_protocol live_dest
    # shellcheck disable=SC2031  # i is the create-loop index. Only the server hook's subshell reuses i (SC2030); this hook runs in the current shell
    while IFS= read -r row; do
      [[ -n "${row}" ]] || continue
      protocol=$(jq -r '.protocol // empty' <<<"${row}")
      listen_port=$(jq -r '.["listen-port"] // empty' <<<"${row}")
      dest_port=$(jq -r '.["destination-port"] // empty' <<<"${row}")
      [[ -n "${protocol}" ]] || continue

      # Read the load balancer right before each service: a changed owner or
      # a failed read stops the hook.
      if ! live="$(hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}")"; then
        _HETZNER_LB_FAILED=1
        return 0
      fi
      # The live service on this listen port (the API allows one per port).
      live_service="$(jq -c --arg port "${listen_port}" '
        [ (.services // [])[] | select((.listen_port | tostring) == $port) ] | first // empty' <<<"${live}" 2>/dev/null)" || live_service=""

      if [[ -n "${live_service}" ]]; then
        live_protocol=$(jq -r '.protocol // ""' <<<"${live_service}")
        live_dest=$(jq -r '.destination_port // "" | tostring' <<<"${live_service}")
        if [[ "${live_protocol}" == "${protocol}" && "${live_dest}" == "${dest_port}" ]]; then
          _hetzner_print "   🦗 keep \033[3mservice\033[0m ${protocol}:${listen_port} -> ${dest_port}"
          continue
        fi
        # update-service changes the service in place: the port keeps serving.
        _hetzner_print "   ♻️  update \033[3mservice\033[0m ${listen_port}: ${live_protocol} -> ${live_dest} to ${protocol} -> ${dest_port}"
        if ! err=$(dry-run::cmd hcloud load-balancer update-service "${rid}" \
            --listen-port "${listen_port}" \
            --protocol "${protocol}" \
            --destination-port "${dest_port}" 2>&1); then
          error "lb ${name}: could not update service ${listen_port} from ${live_protocol} -> ${live_dest} to ${protocol} -> ${dest_port}: ${err}. Update it with 'hcloud load-balancer update-service', then run the provision again."
          _HETZNER_LB_FAILED=1
        fi
        continue
      fi

      _hetzner_print "   ⭐ add \033[3mservice\033[0m ${protocol}:${listen_port} -> ${dest_port}"

      if ! err=$(dry-run::cmd hcloud load-balancer add-service "${rid}" \
          --protocol "${protocol}" \
          --listen-port "${listen_port}" \
          --destination-port "${dest_port}" 2>&1); then
        # "already": accepted only when a new read shows this service.
        if [[ "${err}" == *"already"* ]]; then
          if ! live="$(hetzner::_still_ours load-balancer "${rid}" "${mark}" "${name}")"; then
            _HETZNER_LB_FAILED=1
            return 0
          fi
          if jq -e --arg port "${listen_port}" --arg p "${protocol}" --arg d "${dest_port}" '
               any((.services // [])[]; (.listen_port | tostring) == $port and .protocol == $p
                 and ((.destination_port // "") | tostring) == $d)' >/dev/null 2>&1 <<<"${live}"; then
            continue
          fi
        fi
        error "lb ${name}: could not add service ${protocol}:${listen_port} -> ${dest_port}: ${err}. Run the provision again."
        _HETZNER_LB_FAILED=1
      fi
    done < <(hetzner::json -c ".[\"load-balancer\"][${i}][\"#services\"][]?" 2>/dev/null)
    return 0
  }

  hetzner::create 'load-balancer' || return 1
  (( _HETZNER_LB_FAILED == 0 )) || {
    error "hetzner: the load balancer does not match the descriptor; the messages above name each target or service. Correct it, then run the provision again."
    return 1
  }
}
