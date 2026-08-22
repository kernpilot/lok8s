#!/usr/bin/env bats
# lo_heal_node_ips_test.bats — a kind node must never stay registered on the
# WRONG docker network's address.
#
# Why this exists
# ---------------
# Observed live on kubehz-dev (2026-08-17). Shared-registry clusters are
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
# apiserver→kubelet and every cross-node pod route break. On kubehz-dev the
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
# per-node fake rootfs and then eval's the heal's own script. So the sed loop
# and the /kind/old-ipv4 write under test are the real ones; systemctl and the
# CNI nudge are logged to $CALLS. A mock that reimplemented the repair would
# pass no matter what the heal did.
node_flag()  { echo "${BATS_TEST_TMPDIR}/root.${1}/var/lib/kubelet/kubeadm-flags.env"; }
node_oldip() { echo "${BATS_TEST_TMPDIR}/root.${1}/kind/old-ipv4"; }
node_conf()  { echo "${BATS_TEST_TMPDIR}/root.${1}/etc/kubernetes/kubelet.conf"; }

_load() {
  warn() { echo "warn: ${*}" >&2; }
  # network.sh declares `import ^utils/ip`. Outside `lo` there is no argsh
  # runtime to provide `import`, and PATH then resolves it to ImageMagick's
  # `import(1)` — which fails on "unable to open X server", from a line that
  # has nothing to do with anything under test. Every other suite that sources
  # a lib directly stubs it for the same reason.
  import() { :; }
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
          bad)  echo "10.9.0.3" ;;
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
        # `docker exec <node> bash -c '<repair script>'` — run the REAL script
        # against the fake rootfs.
        echo "repair:${node}" >> "${CALLS}"
        local script="${*: -1}"
        script="${script//\/var\/lib\/kubelet\/kubeadm-flags.env/$(node_flag "${node}")}"
        script="${script//\/etc\/kubernetes/${BATS_TEST_TMPDIR}/root.${node}/etc/kubernetes}"
        script="${script//\/kind\//${BATS_TEST_TMPDIR}/root.${node}/kind/}"
        systemctl() { echo "kubelet-restart:${node}" >> "${CALLS}"; }
        eval "${script}"
        ;;
    esac
    return 0
  }

  # `kubectl get node` = the latch-up InternalIP read (answer, don't log);
  # `kubectl delete pod` = the CNI nudge (log). OBSERVED_BAD overrides what
  # node `bad` reports, so tests can simulate a frozen status.
  kubectl() {
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
  ! grep -q '^cni-restart$' "${CALLS}" || {
    echo "kubectl was invoked without a kubeconfig — it would talk to whatever" >&2
    echo "\$KUBECONFIG/current-context happens to point at: the WRONG cluster." >&2
    return 1
  }
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
