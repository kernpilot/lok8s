#!/usr/bin/env bats
# lo_heal_node_ips_test.bats — a kind node must never stay registered on the
# WRONG docker network's address.
#
# Why this exists
# ---------------
# Observed live on kubehz-dev (2026-08-17). Shared-registry clusters are
# dual-homed: kind creates the node on the cluster network and
# lo::connect_nodes_to_registry_network attaches a second NIC on the registry
# network. Docker names endpoints by attach order, so a RECREATED registry
# endpoint can come back as eth0 — and kind's entrypoint, which derives
# --node-ip from the first interface it finds, then pins kubelet to the registry
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
# apiserver→kubelet and every cross-node pod route break. On kubehz-dev the
# cert-manager webhook lived on that node, so every webhook-validated apply
# failed and `lo up` died on `networking` + `cnpg-plugin` — after burning all six
# of bootstrap's CRD/webhook retries, which can NEVER win here because this is
# not the transient race those retries exist for.
#
# So the detection must key on the kubelet FLAG vs the node's address on the
# cluster network — never on the Node object, whose status is exactly what
# freezes.

setup() {
  load "../test_helper"
  setup_tmpdir
  export KIND_EXPERIMENTAL_DOCKER_NETWORK="lok8s-test"
  export _CALLS="${BATS_TEST_TMPDIR}/calls"
  : > "${_CALLS}"
}

teardown() { teardown_tmpdir; }

# Two nodes: `good` already agrees with the cluster network, `bad` is pinned to
# the registry network's (stale) address.
#
# `docker exec` does not fake the repair — it REDIRECTS the node paths onto a
# per-node fake rootfs and then eval's the heal's own script. So the sed and the
# /kind/old-ipv4 write under test are the real ones; only systemctl is a no-op.
# A mock that reimplemented the repair would pass no matter what the heal did.
_flag()  { echo "${BATS_TEST_TMPDIR}/root.${1}/var/lib/kubelet/kubeadm-flags.env"; }
_oldip() { echo "${BATS_TEST_TMPDIR}/root.${1}/kind/old-ipv4"; }

_load() {
  warn() { echo "warn: ${*}" >&2; }
  source "${_PROJECT_ROOT}/.lok8s/drivers/lo/utils/network.sh"

  local node
  for node in good bad; do
    mkdir -p "$(dirname "$(_flag "${node}")")" "$(dirname "$(_oldip "${node}")")"
  done
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.2 --node-labels="'  > "$(_flag good)"
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2 --node-labels="' > "$(_flag bad)"

  kind() { [[ "${1:-}" == "get" ]] && printf '%s\n' good bad; return 0; }

  docker() {
    case "${1:-}" in
      inspect)
        # $2 = node name. Its address on the CLUSTER network.
        case "${2}" in
          good) echo "10.9.0.2" ;;
          bad)  echo "10.9.0.3" ;;
        esac
        ;;
      exec)
        local node="${2}"
        # `docker exec <node> cat …` — the read the heal greps the flag out of.
        if [[ "${3:-}" == "cat" ]]; then cat "$(_flag "${node}")"; return 0; fi
        # `docker exec <node> bash -c '<repair script>'` — run the REAL script.
        echo "repair:${node}" >> "${_CALLS}"
        local script="${*: -1}"
        script="${script//\/var\/lib\/kubelet\/kubeadm-flags.env/$(_flag "${node}")}"
        script="${script//\/kind\/old-ipv4/$(_oldip "${node}")}"
        systemctl() { echo "kubelet-restart:${node}" >> "${_CALLS}"; }
        eval "${script}"
        ;;
    esac
    return 0
  }

  kubectl() { echo "cni-restart" >> "${_CALLS}"; return 0; }
}

@test "heal_node_ips: repairs ONLY the node pinned to the wrong network" {
  _load
  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  grep -q '^repair:bad$' "${_CALLS}" || {
    echo "The node pinned to the registry address (172.31.0.2) was NOT repaired." >&2
    echo "It stays Ready via its lease while every route INTO it is black-holed." >&2
    return 1
  }
  ! grep -q '^repair:good$' "${_CALLS}" || {
    echo "A node whose --node-ip already matched the cluster network was" >&2
    echo "needlessly restarted — the heal must be a no-op when nothing drifted." >&2
    return 1
  }
  grep -q -- '--node-ip=10.9.0.3' "$(_flag bad)"
}

@test "heal_node_ips: a healthy cluster is a silent no-op" {
  _load
  # Repair the drifted node up front, so nothing needs healing.
  sed -i 's|--node-ip=172.31.0.2|--node-ip=10.9.0.3|' "$(_flag bad)"

  lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  [ ! -s "${_CALLS}" ] || {
    echo "The heal acted on a cluster where every --node-ip already matched:" >&2
    cat "${_CALLS}" >&2
    echo "Restarting kubelet + the CNI agent on every provision is not free." >&2
    return 1
  }
}

@test "heal_node_ips: a dual-stack --node-ip is warned about, never rewritten" {
  # --node-ip=v4,v6 is a deliberate config, not the drift this heals. A naive
  # rewrite would silently drop the v6 half and break dual-stack clusters.
  _load
  echo 'KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2,fd00::2 --node-labels="' > "$(_flag bad)"

  run lo::heal_node_ips lotest "${BATS_TEST_TMPDIR}/kubeconfig"

  ! grep -q '^repair:bad$' "${_CALLS}" || {
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

  grep -q '^repair:bad$' "${_CALLS}"
  ! grep -q '^cni-restart$' "${_CALLS}" || {
    echo "kubectl was invoked without a kubeconfig — it would talk to whatever" >&2
    echo "\$KUBECONFIG/current-context happens to point at: the WRONG cluster." >&2
    return 1
  }
}
