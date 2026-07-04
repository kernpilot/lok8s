# shellcheck shell=bash disable=SC2034,SC2154
# resources.sh — Hetzner resource-type dispatchers
# Each function sets up create hooks and delegates to hetzner::create.

hetzner::ssh-key() {
  hetzner::create 'ssh-key'
}

hetzner::floating-ip() {
  hetzner::create 'floating-ip'
}

hetzner::network() {
  hetzner::create::after() {
    local x network_zone type ip_range
    local -a subargs=()

    while IFS=',' read -r x network_zone type ip_range vswitch_id; do
      _hetzner_print "   ⭐ add \033[3msubnet\033[0m ${ip_range}"

      if hcloud network describe "${name}" | grep -q "${ip_range}"; then
        _hetzner_print "   🦗 skip \033[3msubnet\033[0m \033[1m${ip_range}\033[0m"
        continue
      fi

      [[ -z "${vswitch_id}" ]] ||
        subargs+=("--vswitch-id" "${vswitch_id}")

      dry-run::cmd hcloud network add-subnet "${name}" \
        --network-zone "${network_zone}" \
        --type "${type}" \
        --ip-range "${ip_range}" "${subargs[@]}"
    done < <(
      hetzner::json::loop \
        ".network[${i}][\"#subnets\"]" '.["network-zone"], .type, .["ip-range"], .["vswitch-id"]' \
        2>/dev/null
    )
    unset IFS
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
  local v="${1:-}" _re='^[A-Za-z0-9_./:@=+ -]+$'
  [[ -n "${v}" ]] || return 0
  [[ "${v}" =~ $_re ]] || return 1
}

# hetzner::wipe-devices::script <wipe-json>
# PURE: emit a bash remote-script string for the given #wipe-devices value.
# Empty / null / false ⇒ no-op (emits nothing). Anything other than a boolean
# or an array ⇒ rejected (return 1, emits nothing). The value is NEVER eval'd.
# shellcheck disable=SC2016  # the single-quoted printf formats below are LITERAL remote-script code — the $-refs MUST survive un-expanded to run on the server
hetzner::wipe-devices::script() {
  local wipe_json="${1:-}"
  [[ -n "${wipe_json}" && "${wipe_json}" != "null" ]] || return 0

  local _type
  _type="$(jq -r 'type' <<<"${wipe_json}" 2>/dev/null)" || {
    error "wipe-devices: value is not valid JSON"
    return 1
  }
  case "${_type}" in
    boolean)
      # only `true` is a wipe request; `false` is an explicit no-op
      [[ "$(jq -r '.' <<<"${wipe_json}")" == "true" ]] || return 0
      ;;
    array) ;;
    *)
      error "wipe-devices: expected 'true' or an array of devices, got ${_type}"
      return 1
      ;;
  esac

  # Header + rationale. FULL-disk discard (NOT head-only): Ceph bluestore
  # writes redundant bdev-label copies at size-scaled offsets ACROSS the whole
  # disk; a head-only wipe (sgdisk/wipefs) leaves the far copies, so a
  # freshly-prepared OSD then asserts _check_main_bdev_label N!=M (kin to
  # rook/rook#17716). We blkdiscard the entire device; dd-zero is the fallback
  # when the device rejects discard.
  cat <<'WIPE_HEADER'
set -euo pipefail
# lok8s #wipe-devices — FULL-disk wipe on a fresh (rescue-mode) install.
# Full discard, not head-only: Ceph bluestore writes redundant bdev-label
# copies across the whole disk; a head wipe leaves the far copies and a fresh
# OSD then asserts _check_main_bdev_label N!=M (kin to rook/rook#17716).
WIPE_HEADER

  if [[ "${_type}" == "boolean" ]]; then
    cat <<'WIPE_ALL'
echo "lok8s: #wipe-devices=true — wiping ALL physical disks"
lsblk -dn -o NAME,TYPE | while read -r _name _dtype; do
  [ "${_dtype}" = "disk" ] || continue
  _dev="/dev/${_name}"
  [ -b "${_dev}" ] || { echo "ABORT: ${_dev} not a block device"; exit 1; }
  echo "lok8s: wipe ${_dev}"
  blkdiscard -f "${_dev}" || dd if=/dev/zero of="${_dev}" bs=1M count=8192
  wipefs -a "${_dev}" || true
done
WIPE_ALL
  else
    local _entry _device _model _id _target _v
    while IFS= read -r _entry; do
      [[ -n "${_entry}" ]] || continue
      _device="$(jq -r '.device // ""' <<<"${_entry}")"
      _model="$(jq -r '.model // ""' <<<"${_entry}")"
      _id="$(jq -r '.id // ""' <<<"${_entry}")"

      # boundary check: nothing shell-active reaches the templated script
      for _v in "${_device}" "${_model}" "${_id}"; do
        hetzner::wipe-devices::_safe "${_v}" || {
          error "wipe-devices: refusing unsafe descriptor value: ${_v}"
          return 1
        }
      done

      # path-safety (defense against traversal): a 'device' must be an absolute
      # /dev/… path, an 'id' a bare /dev/disk/by-id name. Reject '..' anywhere
      # and any '/' inside an id — the dd/blkdiscard target must never be a
      # relative or attacker-shaped path pointing outside /dev.
      if [[ -n "${_device}" ]]; then
        [[ "${_device}" == /dev/* && "${_device}" != *..* ]] || {
          error "wipe-devices: 'device' must be an absolute /dev path without '..': ${_device}"
          return 1
        }
      fi
      if [[ -n "${_id}" ]]; then
        [[ "${_id}" != */* && "${_id}" != *..* ]] || {
          error "wipe-devices: 'id' must be a bare by-id name (no '/' or '..'): ${_id}"
          return 1
        }
      fi

      if [[ -n "${_device}" ]]; then
        _target="${_device}"
      elif [[ -n "${_id}" ]]; then
        _target="/dev/disk/by-id/${_id}"
      else
        error "wipe-devices: entry needs at least 'device' or 'id'"
        return 1
      fi

      printf '\n# --- wipe %s ---\n' "${_target}"
      printf '_props="$(udevadm info --query=property --name="%s" 2>/dev/null || true)"\n' "${_target}"
      # model sanity: assert-or-abort BEFORE any destructive call. Whole-line
      # exact match (-qxF, like the id asserts below): a declared model that is
      # only a PREFIX of the real ID_MODEL must NOT match the wrong disk.
      if [[ -n "${_model}" ]]; then
        printf 'printf "%%s\\n" "$_props" | grep -qxF "ID_MODEL=%s" || { echo "ABORT: %s identity mismatch (model)"; exit 1; }\n' \
          "${_model}" "${_target}"
      fi
      # id sanity: a stable id — udev ID_SERIAL or ID_WWN, or a /dev/disk/by-id match
      if [[ -n "${_id}" ]]; then
        printf '_id_ok=0\n'
        printf 'printf "%%s\\n" "$_props" | grep -qxF "ID_SERIAL=%s" && _id_ok=1 || true\n' "${_id}"
        printf 'printf "%%s\\n" "$_props" | grep -qxF "ID_WWN=%s" && _id_ok=1 || true\n' "${_id}"
        printf 'if [ -e "/dev/disk/by-id/%s" ] && [ "$(readlink -f "/dev/disk/by-id/%s")" = "$(readlink -f "%s")" ]; then _id_ok=1; fi\n' \
          "${_id}" "${_id}" "${_target}"
        printf '[ "$_id_ok" = 1 ] || { echo "ABORT: %s identity mismatch (id)"; exit 1; }\n' "${_target}"
      fi
      printf 'echo "lok8s: wipe %s"\n' "${_target}"
      # block-device assert: never dd/blkdiscard a target that is not a real
      # block special file (a mistyped/renamed by-id path must fail closed).
      printf '[ -b "%s" ] || { echo "ABORT: %s not a block device"; exit 1; }\n' "${_target}" "${_target}"
      printf 'blkdiscard -f "%s" || dd if=/dev/zero of="%s" bs=1M count=8192\n' "${_target}" "${_target}"
      printf 'wipefs -a "%s" || true\n' "${_target}"
    done < <(jq -c '.[]' <<<"${wipe_json}")
  fi

  printf '\necho "__LOK8S_WIPE_DONE__"\n'
}

# hetzner::wipe-devices <i> <ssh_user@ip> <ssh_opts...>
# Read .server[<i>]["#wipe-devices"], validate at the boundary (type-check
# ONLY — never eval), generate the wipe script and pipe it over ssh. Requires
# the success sentinel AND no ABORT in the output. Returns non-zero on any
# abort/failure so the install path fails LOUDLY rather than installing over
# un-wiped disks.
hetzner::wipe-devices() {
  local _i="${1}" _dest="${2}"
  shift 2
  local -a _ssh_opts=("${@}")

  local _wipe_json
  _wipe_json="$(hetzner::json -c ".server[${_i}][\"#wipe-devices\"] // null")"
  [[ -n "${_wipe_json}" && "${_wipe_json}" != "null" ]] || return 0

  # Boundary validation: only `true` or an array are legal; reject (never eval)
  # anything else.
  local _type
  _type="$(jq -r 'type' <<<"${_wipe_json}" 2>/dev/null)" || {
    error "wipe-devices: #wipe-devices on server[${_i}] is not valid JSON"
    return 1
  }
  case "${_type}" in
    array) ;;
    boolean)
      [[ "$(jq -r '.' <<<"${_wipe_json}")" == "true" ]] || return 0
      ;;
    *)
      error "wipe-devices: #wipe-devices must be 'true' or an array of devices (got ${_type})"
      return 1
      ;;
  esac

  local _script
  _script="$(hetzner::wipe-devices::script "${_wipe_json}")" || {
    error "wipe-devices: could not build wipe script for server[${_i}]"
    return 1
  }
  [[ -n "${_script}" ]] || return 0

  _hetzner_print "   🧨 \033[1m#wipe-devices\033[0m: wiping declared devices on \033[1m${_dest}\033[0m (fresh install)"

  local _out _ok=1
  _out="$(printf '%s\n' "${_script}" | ssh "${_ssh_opts[@]}" "${_dest}" 'bash -s' 2>&1)" || _ok=0
  grep -q '__LOK8S_WIPE_DONE__' <<<"${_out}" || _ok=0
  # anchored to the explicit 'ABORT:' prefix our guards emit — a stray 'ABORT'
  # substring in unrelated output must NOT be read as a wipe failure.
  ! grep -q '^ABORT:' <<<"${_out}" || _ok=0
  if (( ! _ok )); then
    error "wipe-devices: device wipe FAILED on ${_dest} — aborting install (nothing else touched)"
    grep -E 'ABORT|rror|annot' <<<"${_out}" | sed 's/^/      /' >&2 || true
    return 1
  fi
  _hetzner_print "   ✅ device wipe complete on \033[1m${_dest}\033[0m"
}

hetzner::server() {
  hetzner::create::after() {
    local findex fid
    findex="$(hetzner::json -r ".server[${i}][\"#floating-ip\"]")"
    [[ "${findex}" != "null" ]] || return 0

    fid="$(hetzner::json -r ".[\"floating-ip\"][${findex}].id")"
    dry-run::cmd hcloud floating-ip assign "${fid}" "${name}"
  }

  # Per-server cloud-init: reads #cloud.d from each server entry,
  # exports all #-prefixed fields as CLOUD_ENV_* variables, and
  # generates a per-server cloud-config user-data file.
  hetzner::create::hook() { (
    # Capture the server index BEFORE the field-export loop below reuses `i`
    # as its counter (needed to read this server's #wipe-devices fresh).
    local _server_index="${i}"

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
      local _ssh_id
      _ssh_id="$(hetzner::json -r '.sshPrivateKey // ""')"
      # shellcheck disable=SC2088 # literal "~/" prefix match — expanded via $HOME on the next clause, not meant to tilde-expand
      [[ "${_ssh_id}" != "~/"* ]] || _ssh_id="${HOME}/${_ssh_id:2}"
      local -a _ssh_opts=(
        -o BatchMode=yes
        -o StrictHostKeyChecking=no
        -o UserKnownHostsFile=/dev/null
        -o LogLevel=ERROR
      )
      [[ -z "${_ssh_id}" ]] || _ssh_opts+=(-i "${_ssh_id}")

      if [[ -n "${external_ip}" ]]; then
        _hetzner_log "bare metal: checking ${external_ip} for rescue mode"

        # Check if server is in rescue mode (installimage binary exists)
        if ssh -o ConnectTimeout=10 "${_ssh_opts[@]}" \
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
            hetzner::wipe-devices "${_server_index}" "${ssh_user}@${external_ip}" "${_ssh_opts[@]}" || {
              error "device wipe failed/aborted on ${external_ip}"
              return 1
            }

            _hetzner_print "   📋 running installimage with ${installimage_conf}"
            scp "${_ssh_opts[@]}" "${installimage_conf}" "${ssh_user}@${external_ip}:/tmp/installimage.conf"

            # installimage `-x` post-install runs in the installed chroot:
            # install cloud-init + seed our user-data so the node self-boots
            # via cloud-init on first boot, exactly like a cloud VM. Without
            # it the Hetzner base image (no cloud-init) boots unconfigured —
            # no vswitch, no private network.
            local _ix=""
            if declare -F cloud-config::installimage-post-install &>/dev/null; then
              if cloud-config::installimage-post-install \
                   | ssh "${_ssh_opts[@]}" "${ssh_user}@${external_ip}" \
                       'cat > /tmp/lok8s-post-install && chmod +x /tmp/lok8s-post-install'; then
                _ix=" -x /tmp/lok8s-post-install"
              else
                warn "could not stage installimage post-install on ${external_ip}"
              fi
            fi

            ssh "${_ssh_opts[@]}" "${ssh_user}@${external_ip}" \
              "echo 'debconf debconf/frontend select Noninteractive' | debconf-set-selections 2>/dev/null; \
               /root/.oldroot/nfs/install/installimage -a -c /tmp/installimage.conf${_ix} && reboot" || {
              error "installimage failed on ${external_ip}"
              return 1
            }

            _hetzner_print "   ⏳ waiting for reboot..."
            sleep 30
            local _attempts=0
            while (( _attempts < 60 )); do
              if ssh -o ConnectTimeout=5 "${_ssh_opts[@]}" \
                   "${ssh_user}@${external_ip}" true 2>/dev/null; then
                _hetzner_print "   ✅ server rebooted and reachable"
                break
              fi
              _attempts=$(( _attempts + 1 ))
              sleep 5
            done
            if (( _attempts >= 60 )); then
              error "server ${external_ip} not reachable after installimage reboot"
              return 1
            fi
            # Bare-metal bootstrap runs SOLELY here — after a fresh rescue-mode
            # installimage. cloud-init self-configures on first boot from the
            # user-data the post-install seeded; wait for it before the cluster
            # driver touches the node. An already-installed node (not in rescue)
            # self-bootstrapped on its own first boot, so it is left untouched.
            local _ci_done=0
            if ssh "${_ssh_opts[@]}" "${ssh_user}@${external_ip}" \
                 'command -v cloud-init >/dev/null 2>&1'; then
              _hetzner_print "   ⏳ waiting for cloud-init self-bootstrap on \033[1m${external_ip}\033[0m"
              if timeout 600 ssh "${_ssh_opts[@]}" "${ssh_user}@${external_ip}" \
                   'cloud-init status --wait >/dev/null 2>&1'; then
                _hetzner_print "   ✅ cloud-init self-bootstrap complete"
                _ci_done=1
              else
                warn "cloud-init did not finish cleanly on ${external_ip} (cloud-init status --long)"
              fi
            fi

            # Fallback: if cloud-init is absent or errored, apply the node
            # config directly over SSH (write_files + runcmd:true scripts).
            if (( ! _ci_done )) && declare -F cloud-config::remote-script &>/dev/null; then
              local _rs
              _rs="$(cloud-config::remote-script)"
              if [[ -n "${_rs}" ]]; then
                _hetzner_print "   🔧 fallback: applying node config directly to \033[1m${external_ip}\033[0m"
                local _out _ok=1
                _out="$(printf '%s\n' "${_rs}" | ssh "${_ssh_opts[@]}" \
                          "${ssh_user}@${external_ip}" 'bash -s' 2>&1)" || _ok=0
                grep -q '__LOK8S_BOOTINIT_DONE__' <<<"${_out}" || _ok=0
                ! grep -q 'lok8s: boot-init FAILED' <<<"${_out}" || _ok=0
                if (( ! _ok )); then
                  warn "node config apply incomplete on ${external_ip}:"
                  grep -E 'FAILED|rror|annot' <<<"${_out}" | sed 's/^/      /' >&2 || true
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

hetzner::load-balancer() {
  hetzner::create::after() {
    # Attach the LB to the first descriptor network — required before
    # use-private-ip targets can be added.
    local _net_name _err
    _net_name="$(hetzner::json -r '.network[0].name // ""')"
    if [[ -n "${_net_name}" ]]; then
      if ! _err=$(hcloud load-balancer attach-to-network "${name}" \
          --network "${_net_name}" 2>&1); then
        # idempotent re-runs: already attached is fine
        [[ "${_err}" == *"already attached"* ]] ||
          warn "lb ${name}: attach-to-network failed: ${_err}"
      fi
    fi

    # NOTE: rows are read as JSON objects (jq -c), NOT csv — label
    # selectors legitimately contain commas and csv-splitting mangled
    # them (and the resulting hcloud errors were silently swallowed).
    local _row target_type target_value use_private_ip
    while IFS= read -r _row; do
      [[ -n "${_row}" ]] || continue
      target_type=$(jq -r '.type // empty' <<<"${_row}")
      target_value=$(jq -r '.value // empty' <<<"${_row}")
      use_private_ip=$(jq -r '.["use-private-ip"] // "false"' <<<"${_row}")
      [[ -n "${target_type}" ]] || continue

      _hetzner_print "   ⭐ add \033[3mtarget\033[0m ${target_type}: ${target_value}"

      local -a target_args=()
      case "${target_type}" in
        server)
          if [[ "${target_value}" =~ ^[0-9]+$ ]]; then
            target_value="$(hetzner::json -r --arg idx "${target_value}" '.server[($idx | tonumber)].id // .server[($idx | tonumber)].name')"
          fi
          target_args+=("--server" "${target_value}")
          ;;
        label-selector)
          target_args+=("--label-selector" "${target_value}")
          ;;
        ip)
          target_args+=("--ip" "${target_value}")
          ;;
      esac

      [[ "${use_private_ip}" == "false" ]] ||
        target_args+=("--use-private-ip")

      if ! _err=$(hcloud load-balancer add-target "${name}" \
          "${target_args[@]}" 2>&1); then
        [[ "${_err}" == *"already"* ]] ||
          warn "lb ${name}: add-target failed: ${_err}"
      fi
    done < <(hetzner::json -c ".[\"load-balancer\"][${i}][\"#targets\"][]?" 2>/dev/null)

    local protocol listen_port dest_port
    while IFS= read -r _row; do
      [[ -n "${_row}" ]] || continue
      protocol=$(jq -r '.protocol // empty' <<<"${_row}")
      listen_port=$(jq -r '.["listen-port"] // empty' <<<"${_row}")
      dest_port=$(jq -r '.["destination-port"] // empty' <<<"${_row}")
      [[ -n "${protocol}" ]] || continue

      _hetzner_print "   ⭐ add \033[3mservice\033[0m ${protocol}:${listen_port} -> ${dest_port}"

      if ! _err=$(hcloud load-balancer add-service "${name}" \
          --protocol "${protocol}" \
          --listen-port "${listen_port}" \
          --destination-port "${dest_port}" 2>&1); then
        [[ "${_err}" == *"already"* ]] ||
          warn "lb ${name}: add-service failed: ${_err}"
      fi
    done < <(hetzner::json -c ".[\"load-balancer\"][${i}][\"#services\"][]?" 2>/dev/null)
  }

  hetzner::create 'load-balancer'
}
