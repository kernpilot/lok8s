#!/usr/bin/env bats
# lo_heal_node_ips_test.bats — a kind node must never stay registered on the
# WRONG docker network's address.
#
# Why this exists
# ---------------
# Observed live on a dev cluster (2026-08-17). Shared-registry clusters are
# dual-homed: kind creates the node on the cluster network and
# lo::connect_nodes_to_registry_network attaches a second NIC on the registry
# network. kind's entrypoint derives the node address from docker DNS, and for
# a dual-homed container that lookup can flip to the registry network after an
# endpoint re-attach — the entrypoint then rewrites --node-ip to the registry
# address on the next container restart. That address is dynamically allocated,
# so it drifts too, and the node ends up registered on an IP that answers
# nowhere.
#
# What makes it vicious is that it does not look like a failure. kubelet's node
# LEASE is a separate path from its node STATUS, so the lease keeps renewing and
# the node keeps reporting Ready — while every status update is rejected with
#
#   "Failed to set some node status fields" err="failed to validate nodeIP:
#    node IP: \"10.125.200.2\" not found in the host's network interfaces"
#
# leaving the addresses frozen forever. Nothing can reach INTO the node:
# apiserver→kubelet and every cross-node pod route break. On that cluster the
# cert-manager webhook lived on that node, so every webhook-validated apply
# failed and `lo up` died on `networking` + `cnpg-plugin` — after burning all six
# of bootstrap's CRD/webhook retries, which can NEVER win here because this is
# not the transient race those retries exist for.
#
# So the detection must key on the kubelet FLAG vs the node's address on the
# cluster network — never on the Node object alone, whose status is exactly what
# freezes. (The Node object IS consulted for one thing: catching a prior heal
# that died between the sed and the kubelet restart — see the latch-up test.)

setup() {
  load "../test_helper"
  setup_tmpdir
  export KIND_EXPERIMENTAL_DOCKER_NETWORK="lok8s-test"
  export CALLS="${BATS_TEST_TMPDIR}/calls"
  : > "${CALLS}"
}

teardown() { teardown_tmpdir; }

# Two nodes: `good` already agrees with the cluster network, `bad` is pinned to
# the registry network's (stale) address.
#
# `docker exec` does not fake the repair — it REDIRECTS the node paths onto a
# per-node fake rootfs and then eval's the heal's own scripts (the repair and
# the certificate probe). So the sed loop, the certificate moves and the
# /kind/old-ipv4 write under test are the real ones; systemctl, kubeadm,
# crictl, openssl and the CNI nudge are stubs, logged to $CALLS. A mock that
# reimplemented the repair would pass no matter what the heal did.
#
# A fake certificate is the line "cert <ip>" and its key the line "key <ip>":
# the openssl stub prints <ip> among the SANs, and the kubeadm stub writes the
# pair for the address in the kubeadm config, as kubeadm does.
node_flag()  { echo "${BATS_TEST_TMPDIR}/root.${1}/var/lib/kubelet/kubeadm-flags.env"; }
node_oldip() { echo "${BATS_TEST_TMPDIR}/root.${1}/kind/old-ipv4"; }
node_conf()  { echo "${BATS_TEST_TMPDIR}/root.${1}/etc/kubernetes/kubelet.conf"; }
node_pki()   { echo "${BATS_TEST_TMPDIR}/root.${1}/etc/kubernetes/pki"; }
node_heal()  { echo "${BATS_TEST_TMPDIR}/root.${1}/kind/heal-pki"; }
node_kconf() { echo "${BATS_TEST_TMPDIR}/root.${1}/kind/kubeadm.conf"; }

# write_pair <dir> <ip> — a matching fake pair for ip.
write_pair() {
  mkdir -p "${1}"
  echo "cert ${2}" > "${1}/apiserver.crt"
  echo "key ${2}" > "${1}/apiserver.key"
}

# pair_at <dir> — "<certificate address>/<key address>" of the pair at dir
# (an empty side for a missing file).
pair_at() {
  local c="" k=""
  if [[ -f "${1}/apiserver.crt" ]]; then c=$(sed -n 's/^cert //p' "${1}/apiserver.crt"); fi
  if [[ -f "${1}/apiserver.key" ]]; then k=$(sed -n 's/^key //p' "${1}/apiserver.key"); fi
  echo "${c}/${k}"
}

# make_control_plane <node> — the files that only a kind control-plane node
# has: the cluster CA and the apiserver serving pair for the stale address.
# Also a kubeadm config that still names the stale address, like every other
# file the entrypoint rewrote.
make_control_plane() {
  mkdir -p "$(node_pki "${1}")"
  echo ca-cert > "$(node_pki "${1}")/ca.crt"
  echo ca-key  > "$(node_pki "${1}")/ca.key"
  write_pair "$(node_pki "${1}")" 172.31.0.2
  echo 'advertiseAddress: 172.31.0.2' > "$(node_kconf "${1}")"
}

# mark_repaired <node> — the state the sed leaves: the flag and the kubeadm
# config on the cluster-network address. /kind/old-ipv4 stays as it is.
mark_repaired() {
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.3 --node-labels="' > "$(node_flag "${1}")"
  echo 'advertiseAddress: 10.9.0.3' > "$(node_kconf "${1}")"
}

_load() {
  warn() { echo "warn: ${*}" >&2; }
  # network.sh declares `import ^utils/ip`. Outside `lo` there is no argsh
  # runtime to provide `import`, and PATH then resolves it to ImageMagick's
  # `import(1)` — which fails on "unable to open X server", from a line that
  # has nothing to do with anything under test. Every other suite that sources
  # a lib directly stubs it for the same reason. ip.sh is sourced by hand for
  # the same reason (ip::validate_format).
  import() { :; }
  source "${_PROJECT_ROOT}/.lok8s/utils/ip.sh"
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/network.sh"

  local node
  for node in good bad; do
    mkdir -p "${BATS_TEST_TMPDIR}/root.${node}/var/lib/kubelet" \
             "${BATS_TEST_TMPDIR}/root.${node}/kind" \
             "${BATS_TEST_TMPDIR}/root.${node}/etc/kubernetes"
  done
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.2 --node-labels="'  > "$(node_flag good)"
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2 --node-labels="' > "$(node_flag bad)"
  # What a drifted node really looks like: the entrypoint has rewritten the
  # stale address into old-ipv4 AND the kubeadm files.
  printf '%s' "172.31.0.2" > "$(node_oldip bad)"
  printf '%s' "10.9.0.2"   > "$(node_oldip good)"
  echo 'server: https://172.31.0.2:6443' > "$(node_conf bad)"
  # A superstring of the stale address — the \b-anchored sed must NOT touch it.
  echo 'peer: 172.31.0.20' >> "$(node_conf bad)"

  kind() { [[ "${1:-}" == "get" ]] && printf '%s\n' good bad; return 0; }
  # The waits of the API server check: logged, never slept.
  sleep() { echo "sleep ${*}" >> "${BATS_TEST_TMPDIR}/sleeps"; }

  docker() {
    case "${1:-}" in
      inspect)
        # $2 = node name, then --format <template>. A broken template in prod
        # yields an EMPTY address (silent skip), so the mock only answers when
        # the template actually selects the cluster network — mutating the
        # format string in network.sh makes every heal test fail.
        local fmt="${*}"
        [[ "${fmt}" == *".NetworkSettings.Networks"* ]] || return 0
        [[ "${fmt}" == *"${KIND_EXPERIMENTAL_DOCKER_NETWORK}"* ]] || return 0
        case "${2}" in
          good) echo "10.9.0.2" ;;
          bad)  echo "${INSPECT_BAD:-10.9.0.3}" ;;
        esac
        ;;
      exec)
        local node="${2}"
        # `docker exec <node> cat …` — the read the heal greps the flag out of.
        if [[ "${3:-}" == "cat" ]]; then cat "$(node_flag "${node}")"; return 0; fi
        # `docker exec <node> systemctl restart kubelet` — the latch-up path.
        if [[ "${3:-}" == "systemctl" ]]; then
          echo "kubelet-restart:${node}" >> "${CALLS}"; return 0
        fi
        local script="${*: -1}"
        script="${script//\/var\/lib\/kubelet\/kubeadm-flags.env/$(node_flag "${node}")}"
        script="${script//\/etc\/kubernetes/${BATS_TEST_TMPDIR}/root.${node}/etc/kubernetes}"
        script="${script//\/kind\//${BATS_TEST_TMPDIR}/root.${node}/kind/}"
        # `docker exec <node> sh -c '<certificate probe>'` — run the REAL probe
        # against the fake rootfs. The openssl stub answers only the call of
        # the probe, in the format of openssl 3, and fails like openssl on a
        # file that is not a certificate.
        if [[ "${3:-}" == "sh" ]]; then
          openssl() {
            local ip
            [[ "${*}" == "x509 -in $(node_pki "${node}")/apiserver.crt -noout -ext subjectAltName" ]] || {
              echo "openssl stub: unexpected call: ${*}" >&2; return 1
            }
            ip=$(sed -n 's/^cert //p' "${3}" 2>/dev/null)
            [[ -n "${ip}" ]] || { echo "Could not find certificate from ${3}" >&2; return 1; }
            printf 'X509v3 Subject Alternative Name: \n    DNS:lotest-control-plane, DNS:kubernetes, DNS:localhost, IP Address:10.96.0.1, IP Address:%s, IP Address:127.0.0.1\n' "${ip}"
          }
          ( eval "${script}" ) || return "$?"
          return 0
        fi
        # `docker exec <node> bash -c '<repair script>'` — run the REAL script
        # against the fake rootfs.
        echo "repair:${node}" >> "${CALLS}"
        systemctl() { echo "kubelet-restart:${node}" >> "${CALLS}"; }
        # crictl answers the list of the controller-manager and scheduler
        # containers, logs each stop and fails on any other call.
        crictl() {
          case "${*}" in
            'ps -q --name kube-controller-manager|kube-scheduler') printf 'cm1\nsched1\n' ;;
            'stop cm1'|'stop sched1') echo "crictl-stop:${node} ${2}" >> "${CALLS}"; echo "${2}" ;;
            *) echo "crictl stub: unexpected call: ${*}" >&2; return 1 ;;
          esac
        }
        # kubeadm records what it would see: its arguments, whether the old
        # pair is still in pki/, the config file it reads and /kind/old-ipv4.
        # It fails on any other call than kind's fix_certificate one. Then it
        # does what kubeadm's certs phase does: it keeps an apiserver.crt that
        # is in pki/ and does not read its SANs (exit 0, nothing written),
        # fails on a key without a certificate, and fails when the cluster CA
        # is not in pki/. KUBEADM_FAIL makes it fail: six noise lines, a klog
        # error, info and warning line, the reason and kubeadm's --v=5 hint on
        # stderr. It writes "[certs] …" on stdout (after the stderr lines on a
        # failure), which must reach no output of the heal.
        kubeadm() {
          local pki pair=absent addr i
          pki="$(node_pki "${node}")"
          if [[ -e "${pki}/apiserver.crt" || -e "${pki}/apiserver.key" ]]; then pair=present; fi
          echo "kubeadm:${node} ${*} | old-pair=${pair} | conf=$(cat "${@: -1}") | old-ipv4=$(cat "$(node_oldip "${node}")")" >> "${CALLS}"
          [[ "${*}" == "init phase certs apiserver --config $(node_kconf "${node}")" ]] || {
            echo "kubeadm stub: not the call of kind's fix_certificate: ${*}" >&2; return 1
          }
          if [[ -f "${pki}/apiserver.crt" && -f "${pki}/ca.crt" ]]; then
            echo '[certs] Using existing apiserver certificate and key on disk'
            return 0
          fi
          [[ ! -f "${pki}/apiserver.key" ]] || {
            echo 'error execution phase certs/apiserver: failure loading apiserver certificate' >&2; return 1
          }
          [[ -f "${pki}/ca.crt" && -f "${pki}/ca.key" ]] || {
            echo "kubeadm stub: the cluster CA is not in ${pki}" >&2; return 1
          }
          if [[ -n "${KUBEADM_FAIL:-}" ]]; then
            for i in 1 2 3 4 5 6; do echo "kubeadm stub: noise line ${i}" >&2; done
            echo 'E1110 16:20:03.100000    3243 certs.go:1] kubeadm stub: a klog error' >&2
            echo 'I1110 16:20:03.110000    3243 version.go:1] kubeadm stub: a klog note' >&2
            echo 'W1110 16:20:03.113013    3243 initconfiguration.go:363] [config] WARNING: Ignored configuration document with GroupVersionKind kubeadm.k8s.io/v1beta4, Kind=JoinConfiguration' >&2
            echo 'error execution phase certs/apiserver: test failure' >&2
            echo 'To see the stack trace of this error execute with --v=5 or higher' >&2
            echo '[certs] Generating "apiserver" certificate and key'
            return 1
          fi
          echo '[certs] Generating "apiserver" certificate and key'
          addr=$(sed -n 's/^advertiseAddress: //p' "${@: -1}")
          write_pair "${pki}" "${addr}"
        }
        # A subshell, like the real `bash -c`: an `exit` in the script ends
        # only the script, and its status is the status of `docker exec`.
        ( eval "${script}" ) || return "$?"
        ;;
    esac
    return 0
  }

  # `kubectl get --raw /readyz` = a check of the wait for a steady API server
  # (log; READYZ is a space-separated list of answers in order, 0 = ready,
  # and every check after the list is ready); `kubectl get node` = the
  # latch-up InternalIP read (answer, don't log); `kubectl delete pod` = the
  # CNI nudge (log). OBSERVED_BAD overrides what node `bad` reports, so tests
  # can simulate a frozen status.
  kubectl() {
    if [[ "${*}" == *" get --raw /readyz" ]]; then
      echo "readyz" >> "${CALLS}"
      local n answer
      n=$(grep -c '^readyz$' "${CALLS}")
      answer=$(echo "${READYZ:-}" | awk -v n="${n}" '{ print (n <= NF ? $n : 0) }')
      return "${answer:-0}"
    fi
    if [[ "${*}" == *"get"*"node"* ]]; then
      case "${*}" in
        *good*) echo "10.9.0.2" ;;
        *bad*)  echo "${OBSERVED_BAD:-10.9.0.3}" ;;
      esac
      return 0
    fi
    echo "cni-restart" >> "${CALLS}"
    return 0
  }
}

@test "heal_node_ips: repairs ONLY the drifted node — flag, old-ipv4, kubeadm files, kubelet, CNI" {
  _load
  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  grep -q '^repair:bad$' "${CALLS}" || {
    echo "The node pinned to the registry address (172.31.0.2) was NOT repaired." >&2
    echo "It stays Ready via its lease while every route INTO it is black-holed." >&2
    return 1
  }
  ! grep -q '^repair:good$' "${CALLS}" || {
    echo "A node whose --node-ip already matched the cluster network was" >&2
    echo "needlessly restarted — the heal must be a no-op when nothing drifted." >&2
    return 1
  }
  grep -q -- '--node-ip=10.9.0.3' "$(node_flag bad)" || {
    echo "kubeadm-flags.env still carries the dead address." >&2; return 1
  }
  grep -q '10.9.0.3' "$(node_conf bad)" || {
    echo "kubelet.conf still references the dead address — kind's entrypoint" >&2
    echo "updates its whole files_to_update set on an address change, and so" >&2
    echo "must the heal." >&2
    return 1
  }
  grep -q 'peer: 172.31.0.20' "$(node_conf bad)" || {
    echo "The sed rewrote 172.31.0.20 — a SUPERSTRING of the stale address." >&2
    echo "The \b anchors are load-bearing: without them a repair corrupts any" >&2
    echo "address that merely starts with the stale one." >&2
    return 1
  }
  [ "$(cat "$(node_oldip bad)")" = "10.9.0.3" ] || {
    echo "/kind/old-ipv4 was not updated ('$(cat "$(node_oldip bad)")') — the" >&2
    echo "entrypoint diffs it against docker DNS on the next container restart" >&2
    echo "and would rewrite the address files all over again." >&2
    return 1
  }
  grep -q '^kubelet-restart:bad$' "${CALLS}" || {
    echo "kubelet was never restarted — the repaired flag is not read until" >&2
    echo "restart, so the node keeps running on the dead address." >&2
    return 1
  }
  grep -q '^cni-restart$' "${CALLS}" || {
    echo "The CNI agent was not restarted — peers keep routing to the dead" >&2
    echo "address they cached from CiliumNode." >&2
    return 1
  }
}

@test "heal_node_ips: a healthy cluster is a silent no-op" {
  _load
  # Repair the drifted node up front, so nothing needs healing.
  sed -i 's|--node-ip=172.31.0.2|--node-ip=10.9.0.3|' "$(node_flag bad)"

  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  [ ! -s "${CALLS}" ] || {
    echo "The heal acted on a cluster where every --node-ip already matched:" >&2
    cat "${CALLS}" >&2
    echo "Restarting kubelet + the CNI agent on every provision is not free." >&2
    return 1
  }
}

@test "heal_node_ips: latch-up — flag repaired but node status frozen restarts kubelet" {
  # A prior heal died between the sed and the kubelet restart: the flag file
  # says the right address (so the drift check alone would skip forever), but
  # kubelet still runs on the dead one — visible as a frozen Node InternalIP.
  _load
  sed -i 's|--node-ip=172.31.0.2|--node-ip=10.9.0.3|' "$(node_flag bad)"
  export OBSERVED_BAD="172.31.0.2"

  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  grep -q '^kubelet-restart:bad$' "${CALLS}" || {
    echo "The half-repaired node was skipped: its flag file matches, so without" >&2
    echo "the Node-InternalIP cross-check the drift is permanently undetectable." >&2
    return 1
  }
  ! grep -q '^repair:bad$' "${CALLS}" || {
    echo "The full repair script re-ran on a node whose files are already" >&2
    echo "correct — only the missed kubelet restart was needed." >&2
    return 1
  }
  ! grep -q '^kubelet-restart:good$' "${CALLS}" || {
    echo "A fully healthy node's kubelet was restarted." >&2; return 1
  }
}

@test "heal_node_ips: a docker inspect answer that is not an IP address never reaches the node" {
  # The docker inspect answer is spliced into the repair script. A value that
  # is not an IP address must not be written anywhere, or run.
  _load
  export INSPECT_BAD="10.9.0.3'; touch ${BATS_TEST_TMPDIR}/pwned; echo '"

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  ! grep -q '^repair:bad$' "${CALLS}" && [ ! -e "${BATS_TEST_TMPDIR}/pwned" ] || {
    echo "A non-IP docker answer was spliced into the repair script." >&2; return 1
  }
  grep -q -- '--node-ip=172.31.0.2' "$(node_flag bad)" || {
    echo "kubeadm-flags.env was rewritten with a non-IP value." >&2; return 1
  }
  grep -q 'not an IP address' <<< "${output}" || {
    echo "The skip must be reported: ${output}" >&2; return 1
  }
}

@test "heal_node_ips: a dual-stack --node-ip is warned about, never rewritten" {
  # --node-ip=v4,v6 is a deliberate config, not the drift this heals. A naive
  # rewrite would silently drop the v6 half and break dual-stack clusters.
  _load
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2,fd00::2 --node-labels="' > "$(node_flag bad)"

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  ! grep -q '^repair:bad$' "${CALLS}" || {
    echo "A dual-stack --node-ip was rewritten — the v6 address is now lost." >&2
    return 1
  }
  grep -q 'dual-stack' <<< "${output}" || {
    echo "A dual-stack mismatch was skipped SILENTLY; it must still be reported," >&2
    echo "or a genuinely misconfigured dual-stack node looks healthy forever." >&2
    return 1
  }
}

@test "heal_node_ips: skips the CNI nudge when no kubeconfig is available" {
  # On a FIRST provision there is no CNI yet (bootstrap installs it moments
  # later), and driver::provision may call the heal before a kubeconfig exists.
  _load
  lo::heal_node_ips lotest

  grep -q '^repair:bad$' "${CALLS}"
  ! grep -qE '^(cni-restart|readyz)$' "${CALLS}" || {
    echo "kubectl was invoked without a kubeconfig — it would talk to whatever" >&2
    echo "\$KUBECONFIG/current-context happens to point at: the WRONG cluster." >&2
    return 1
  }
}

# readyz_run — the /readyz checks and the CNI nudges of $CALLS, in order, on
# one line.
readyz_run() { grep -E '^(readyz|cni-restart)$' "${CALLS}" | tr '\n' ' '; }

@test "heal_node_ips: after a repair, waits for five good /readyz checks in a row, 2 s apart, before the CNI nudge" {
  # After a repair, kubelet can start etcd again a few seconds after its own
  # restart, and the API server then refuses the next step of lo up. A good
  # check between bad ones does not count as steady.
  _load
  READYZ="1 1 0 1 0 0 0 0 0 1" run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "${status}" -eq 0 ]

  local want
  want="$(printf 'readyz %.0s' 1 2 3 4 5 6 7 8 9)cni-restart "
  [ "$(readyz_run)" = "${want}" ] || {
    echo "The heal did not wait for five good /readyz checks in a row before the" >&2
    echo "CNI nudge:" >&2
    echo " got  '$(readyz_run)'" >&2
    echo " want '${want}'" >&2
    return 1
  }
  [ "$(grep -n '^repair:bad$' "${CALLS}" | cut -d: -f1)" -lt "$(grep -n '^readyz$' "${CALLS}" | head -1 | cut -d: -f1)" ] || {
    echo "The /readyz checks did not come after the repair:" >&2
    cat "${CALLS}" >&2
    return 1
  }
  [ "$(grep -c '^sleep 2$' "${BATS_TEST_TMPDIR}/sleeps")" -eq 8 ] && [ "$(wc -l < "${BATS_TEST_TMPDIR}/sleeps")" -eq 8 ] || {
    echo "The checks are not 2 s apart (8 waits between 9 checks):" >&2
    cat "${BATS_TEST_TMPDIR}/sleeps" >&2
    return 1
  }
  [[ "${output}" != *"/readyz"* ]] || {
    echo "A warning about /readyz, but the API server became steady: ${output}" >&2
    return 1
  }
}

@test "heal_node_ips: an API server that is not steady after 60 checks gets a warning, and the heal goes on" {
  _load
  local i answers=""
  for (( i = 0; i < 12; i++ )); do answers+="0 0 0 0 1 "; done
  READYZ="${answers}" run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "${status}" -eq 0 ]

  [ "$(grep -c '^readyz$' "${CALLS}")" -eq 60 ] && [[ "$(readyz_run)" == *"readyz cni-restart " ]] || {
    echo "The wait did not stop after 60 checks, or the CNI nudge did not follow:" >&2
    echo "$(readyz_run)" >&2
    return 1
  }
  [ "$(wc -l < "${BATS_TEST_TMPDIR}/sleeps")" -eq 60 ] || {
    echo "$(wc -l < "${BATS_TEST_TMPDIR}/sleeps") waits, want 60 (one after each check)" >&2
    return 1
  }
  [[ "${output}" == *"warn: lo: the API server did not pass 5 /readyz checks in a row after the node repair (60 checks, 2 s apart). If a later step fails, run lo up again."* ]] || {
    echo "No warning about the API server that is not steady: ${output}" >&2
    return 1
  }
}

@test "heal_node_ips: after a failed repair, waits for the API server but sends no CNI nudge" {
  # A failed renewal stops the script after the sed, which has already changed
  # the static pod manifests, so kubelet starts the API server again. No node
  # was repaired, so no CNI nudge.
  _load
  make_control_plane bad
  export KUBEADM_FAIL=1

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "${status}" -eq 0 ]

  grep -qF 'could not repair bad: ' <<< "${output}" || {
    echo "The repair did not fail: ${output}" >&2; return 1
  }
  [ "$(readyz_run)" = "readyz readyz readyz readyz readyz " ] || {
    echo "After a failed repair the heal must wait for the API server and send no" >&2
    echo "CNI nudge: '$(readyz_run)'" >&2
    return 1
  }

  # The next heal takes the renewal path (the flag agrees, the certificate does
  # not), and kubeadm fails again: the same wait.
  : > "${CALLS}"
  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  grep -qF 'renewing the certificate' <<< "${output}" && grep -qF 'could not repair bad: ' <<< "${output}" || {
    echo "The second heal did not fail on the renewal path: ${output}" >&2; return 1
  }
  [ "$(readyz_run)" = "readyz readyz readyz readyz readyz " ] || {
    echo "After a failed renewal the heal must wait for the API server and send no" >&2
    echo "CNI nudge: '$(readyz_run)'" >&2
    return 1
  }
}

@test "heal_node_ips: renews the apiserver certificate on a control-plane node — after the sed, before old-ipv4 and the kubelet restart" {
  # On a control-plane node the apiserver serving certificate names the node
  # address. After the heal, controller-manager and scheduler connect to the
  # new address, so the certificate must name it too. kind renews it at
  # boot only when /kind/old-ipv4 differs, and the heal writes the new address
  # there, so the heal must renew it itself.
  _load
  make_control_plane bad

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  local renew restart
  renew=$(grep -n '^kubeadm:bad ' "${CALLS}") || {
    echo "The apiserver certificate of the control-plane node was not renewed: it" >&2
    echo "still names 172.31.0.2, so every client of https://10.9.0.3:6443 fails TLS." >&2
    cat "${CALLS}" >&2
    echo "${output}" >&2
    return 1
  }
  [[ "${renew}" == *":kubeadm:bad init phase certs apiserver --config $(node_kconf bad) | "* ]] || {
    echo "The kubeadm call is not the one of kind's fix_certificate" >&2
    echo "(kubeadm init phase certs apiserver --config /kind/kubeadm.conf):" >&2
    echo "${renew}" >&2
    return 1
  }
  [[ "${renew}" == *" | old-pair=absent | "* ]] || {
    echo "The old pair was still in pki/ when kubeadm ran: kubeadm keeps an" >&2
    echo "apiserver.crt that is in pki/ and does not read its SANs, so the stale" >&2
    echo "certificate stays: ${renew}" >&2
    return 1
  }
  [[ "${renew}" == *" | conf=advertiseAddress: 10.9.0.3 | "* ]] || {
    echo "kubeadm read a config with the old address: the renewal must come AFTER" >&2
    echo "the sed, or the new certificate names the old address again: ${renew}" >&2
    return 1
  }
  [[ "${renew}" == *" | old-ipv4=172.31.0.2" ]] || {
    echo "/kind/old-ipv4 already held the new address when kubeadm ran: write it" >&2
    echo "AFTER the renewal, so that a heal that stops before the renewal leaves" >&2
    echo "kind's boot-time fix_certificate in place: ${renew}" >&2
    return 1
  }
  restart=$(grep -n '^kubelet-restart:bad$' "${CALLS}" | cut -d: -f1)
  [[ -n "${restart}" ]] && (( restart > ${renew%%:*} )) || {
    echo "kubelet did not restart after the renewal:" >&2
    cat "${CALLS}" >&2
    return 1
  }
  # controller-manager and scheduler keep the server address of their
  # kubeconfig from their start. A stop before the renewal starts them against
  # the old certificate.
  [ "$(sed -n "$(( ${renew%%:*} + 1 )),$(( ${renew%%:*} + 3 ))p" "${CALLS}" | tr '\n' ' ')" = "crictl-stop:bad cm1 crictl-stop:bad sched1 kubelet-restart:bad " ] || {
    echo "After the renewal the script must stop the controller-manager and scheduler" >&2
    echo "containers, then restart kubelet:" >&2
    cat "${CALLS}" >&2
    return 1
  }
  [ "$(pair_at "$(node_pki bad)")" = "10.9.0.3/10.9.0.3" ] || {
    echo "pki/ after the heal holds $(pair_at "$(node_pki bad)"), not the renewed pair." >&2; return 1
  }
  [ ! -e "$(node_heal bad)" ] || {
    echo "The moved-aside pair stayed in /kind/heal-pki after a good renewal." >&2
    return 1
  }
  ! grep -qE 'could not repair|\[certs\]' <<< "${output}" || {
    echo "A good renewal wrote a failure, or kubeadm's stdout reached the output:" >&2
    echo "${output}" >&2
    return 1
  }
}

# worker_case <crt address or -> <key address or -> — a node without the whole
# apiserver pair gets the repair but no renewal, and its next heal does not
# try again.
worker_case() {
  rm -rf "${BATS_TEST_TMPDIR}/root.good" "${BATS_TEST_TMPDIR}/root.bad"
  : > "${CALLS}"
  _load
  local pki before label="crt=${1} key=${2}"
  pki="$(node_pki bad)"
  mkdir -p "${pki}"
  if [[ "${1}" != - ]]; then echo "cert ${1}" > "${pki}/apiserver.crt"; fi
  if [[ "${2}" != - ]]; then echo "key ${2}" > "${pki}/apiserver.key"; fi
  before=$(pair_at "${pki}")

  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  ! grep -qE '^(kubeadm|crictl-stop):' "${CALLS}" || {
    echo "${label}: kubeadm ran, or containers were stopped, on a node without the" >&2
    echo "whole apiserver pair:" >&2
    grep -E '^(kubeadm|crictl-stop):' "${CALLS}" >&2
    return 1
  }
  grep -q '^kubelet-restart:bad$' "${CALLS}" || {
    echo "${label}: the worker repair did not finish (no kubelet restart)." >&2
    cat "${CALLS}" >&2
    return 1
  }
  [ ! -e "$(node_heal bad)" ] || {
    echo "${label}: the heal made /kind/heal-pki on a node without the whole pair." >&2; return 1
  }
  [ "$(pair_at "${pki}")" = "${before}" ] || {
    echo "${label}: the heal changed pki/ (${before} -> $(pair_at "${pki}"))." >&2; return 1
  }

  : > "${CALLS}"
  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  ! grep -q '^repair:bad$' "${CALLS}" || {
    echo "${label}: the next heal ran the repair again on a node without the whole pair." >&2
    return 1
  }
}

@test "heal_node_ips: a node without the whole apiserver pair gets no certificate renewal" {
  # A worker runs no apiserver and has no apiserver.crt. The guard keeps
  # kubeadm off it, and the rest of the repair runs as before. A node with only
  # one file of the pair gets no renewal either (kind's fix_certificate needs
  # both), so a guard that tests one file only fails here.
  worker_case - -
  worker_case 172.31.0.2 -
  worker_case - 172.31.0.2
}

@test "heal_node_ips: a failed renewal keeps the old pair and old-ipv4, skips the kubelet restart and warns with kubeadm's reason" {
  # kind's fix_certificate skips a node without apiserver.crt. So a failed
  # renewal must put the old pair back, and must leave /kind/old-ipv4 at the
  # old address, so that a node boot retries the renewal. It must also stop
  # before the kubelet restart and report the failure with kubeadm's reason.
  _load
  make_control_plane bad
  export KUBEADM_FAIL=1

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  [ "$(pair_at "$(node_pki bad)")" = "172.31.0.2/172.31.0.2" ] || {
    echo "A failed renewal lost the old pair ($(pair_at "$(node_pki bad)")): the" >&2
    echo "apiserver has no serving certificate, and kind's fix_certificate skips a" >&2
    echo "node without one." >&2
    return 1
  }
  [ ! -e "$(node_heal bad)" ] || {
    echo "/kind/heal-pki stayed after the old pair came back." >&2; return 1
  }
  [ "$(cat "$(node_oldip bad)")" = "172.31.0.2" ] || {
    echo "/kind/old-ipv4 was written after a failed renewal: a node boot sees" >&2
    echo "no address change and does not retry the renewal." >&2
    return 1
  }
  ! grep -q '^kubelet-restart:bad$' "${CALLS}" || {
    echo "kubelet restarted after a failed renewal: the script must stop and report." >&2
    return 1
  }
  ! grep -q '^crictl-stop:' "${CALLS}" || {
    echo "Containers were stopped after a failed renewal: they still connect with" >&2
    echo "the old certificate in place." >&2
    return 1
  }
  grep -qF 'could not repair bad: ' <<< "${output}" || {
    echo "A failed renewal was not reported:" >&2
    echo "${output}" >&2
    return 1
  }
  grep -qF 'could not repair bad: kubeadm stub: noise line 6; E1110 16:20:03.100000    3243 certs.go:1] kubeadm stub: a klog error; error execution phase certs/apiserver: test failure. The next' <<< "${output}" || {
    echo "The warning must carry the last three lines of kubeadm's stderr without" >&2
    echo "klog info and warning lines and without the --v=5 hint: ${output}" >&2
    return 1
  }
  ! grep -qE 'noise line 5|\[certs\]' <<< "${output}" || {
    echo "The warning is not bounded to the end of kubeadm's stderr, or carries" >&2
    echo "its stdout: ${output}" >&2
    return 1
  }
  ! grep -qF 'systemctl status' <<< "${output}" && grep -qF 'The next lo up tries again.' <<< "${output}" || {
    echo "The warning names the wrong next step: ${output}" >&2; return 1
  }
}

@test "heal_node_ips: the next heal renews a certificate that a failed renewal left, and leaves a good one alone" {
  # The first heal repairs the files, but kubeadm fails. The flag then already
  # names the cluster-network address, so the next heal must read the
  # certificate, find the old address and run the renewal again. A third heal
  # finds a good certificate and does nothing.
  _load
  make_control_plane bad
  export KUBEADM_FAIL=1
  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  [ "$(pair_at "$(node_pki bad)")" = "172.31.0.2/172.31.0.2" ]

  unset KUBEADM_FAIL
  : > "${CALLS}"
  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  grep -qF 'lo: bad has --node-ip=10.9.0.3, but its apiserver certificate does not name that address — renewing the certificate' <<< "${output}" || {
    echo "The second heal did not report the old certificate: ${output}" >&2; return 1
  }
  [ "$(grep -c '^kubeadm:bad ' "${CALLS}")" = 1 ] \
    && grep -q ' | old-pair=absent | conf=advertiseAddress: 10.9.0.3 | ' "${CALLS}" || {
    echo "The second heal did not renew the certificate the way of fix_certificate:" >&2
    cat "${CALLS}" >&2
    echo "${output}" >&2
    return 1
  }
  [ "$(pair_at "$(node_pki bad)")" = "10.9.0.3/10.9.0.3" ] && [ ! -e "$(node_heal bad)" ] || {
    echo "After the second heal pki/ holds $(pair_at "$(node_pki bad)")." >&2; return 1
  }
  [ "$(cat "$(node_oldip bad)")" = "10.9.0.3" ] || {
    echo "/kind/old-ipv4 is not the cluster-network address after the renewal." >&2; return 1
  }
  grep -q '^kubelet-restart:bad$' "${CALLS}" && grep -q '^cni-restart$' "${CALLS}" \
    && [ "$(grep -c '^crictl-stop:bad ' "${CALLS}")" = 2 ] || {
    echo "The second heal did not finish the repair (container stops, kubelet and" >&2
    echo "CNI restart):" >&2
    cat "${CALLS}" >&2
    return 1
  }

  : > "${CALLS}"
  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  [ ! -s "${CALLS}" ] || {
    echo "The third heal acted on a node with a good certificate:" >&2
    cat "${CALLS}" >&2
    return 1
  }
}

@test "heal_node_ips: renews a certificate that a heal by an older lo left on the old address" {
  # An older lo repaired the files, wrote old-ipv4 and restarted kubelet, but
  # did not renew the certificate. Nothing else will ever renew it.
  _load
  make_control_plane bad
  mark_repaired bad
  printf '%s' "10.9.0.3" > "$(node_oldip bad)"

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  [ "$(grep -c '^kubeadm:bad ' "${CALLS}")" = 1 ] || {
    echo "The heal did not renew a certificate that names the old address:" >&2
    cat "${CALLS}" >&2
    echo "${output}" >&2
    return 1
  }
  [ "$(pair_at "$(node_pki bad)")" = "10.9.0.3/10.9.0.3" ] || {
    echo "pki/ holds $(pair_at "$(node_pki bad)") after the renewal." >&2; return 1
  }
}

# stuck_pair_case <state> <kubeadm fails: 0|1> <address of the pair in pki/
# after a failed renewal> — a heal stopped between the mv of the pair and the
# end of kubeadm. The files are repaired, and the pair, or part of it, is in
# /kind/heal-pki.
stuck_pair_case() {
  local state="${1}" fail="${2}" kept="${3}" pki heal want=10.9.0.3
  local label="${state}, kubeadm fails=${fail}"
  rm -rf "${BATS_TEST_TMPDIR}/root.good" "${BATS_TEST_TMPDIR}/root.bad"
  : > "${CALLS}"
  _load
  make_control_plane bad
  mark_repaired bad
  pki="$(node_pki bad)" heal="$(node_heal bad)"
  rm -f "${pki}/apiserver.crt" "${pki}/apiserver.key"
  mkdir -p "${heal}"
  case "${state}" in
    crt-moved)  echo "cert 172.31.0.2" > "${heal}/apiserver.crt"; echo "key 172.31.0.2" > "${pki}/apiserver.key" ;;
    both-moved) write_pair "${heal}" 172.31.0.2 ;;
    new-key)    write_pair "${heal}" 172.31.0.2; echo "key 10.9.0.3" > "${pki}/apiserver.key" ;;
    new-pair)   write_pair "${heal}" 172.31.0.2; write_pair "${pki}" 10.9.0.3 ;;
  esac
  if (( fail )); then export KUBEADM_FAIL=1; want="${kept}"; else unset KUBEADM_FAIL; fi

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"
  unset KUBEADM_FAIL

  grep -qF 'lo: bad has its apiserver key pair in /kind/heal-pki from an interrupted heal — renewing the certificate' <<< "${output}" || {
    echo "${label}: the stuck pair was not reported: ${output}" >&2; return 1
  }
  [ "$(grep -c '^kubeadm:bad .* | old-pair=absent | ' "${CALLS}")" = 1 ] || {
    echo "${label}: the restored pair was not renewed:" >&2
    cat "${CALLS}" >&2
    echo "${output}" >&2
    return 1
  }
  [ ! -e "${heal}" ] || { echo "${label}: /kind/heal-pki still exists." >&2; return 1; }
  [ "$(pair_at "${pki}")" = "${want}/${want}" ] || {
    echo "${label}: pki/ holds $(pair_at "${pki}"), want ${want}/${want}: the key and" >&2
    echo "the certificate must match." >&2
    return 1
  }
}

@test "heal_node_ips: a pair stuck in /kind/heal-pki comes back and is renewed; a failed renewal leaves a matching pair" {
  # kind's fix_certificate skips a node without apiserver.crt, so a pair left
  # in /kind/heal-pki would never come back by itself.
  stuck_pair_case crt-moved  0 172.31.0.2
  stuck_pair_case crt-moved  1 172.31.0.2
  stuck_pair_case both-moved 0 172.31.0.2
  stuck_pair_case both-moved 1 172.31.0.2
  stuck_pair_case new-key    0 172.31.0.2
  stuck_pair_case new-key    1 172.31.0.2
  stuck_pair_case new-pair   0 10.9.0.3
  stuck_pair_case new-pair   1 10.9.0.3
}

@test "nodes_on_registry_network: true only when a cluster node holds a registry NIC" {
  # The gate for the heal after the shared→per-project default flip: a cluster
  # provisioned under the OLD default still carries registry NICs while its
  # spec now reads "not shared" — membership, not the spec, must decide.
  _load
  registry::network_name() { echo "lok8s-registries"; }

  docker() {
    [[ "${1:-}" == "network" && "${2:-}" == "inspect" ]] || return 1
    echo "${REGISTRY_MEMBERS:-}"
  }

  REGISTRY_MEMBERS="lok8s-registry-io-docker bad "
  lo::nodes_on_registry_network lotest || {
    echo "Node 'bad' is attached to the registry network but the gate said no —" >&2
    echo "an already-drifted pre-flip cluster would never be healed." >&2
    return 1
  }

  REGISTRY_MEMBERS="lok8s-registry-io-docker "
  ! lo::nodes_on_registry_network lotest || {
    echo "No cluster node is on the registry network, yet the gate fired." >&2
    return 1
  }
}
