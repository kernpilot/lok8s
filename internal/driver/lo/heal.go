package lo

// heal.go — lo::heal_node_ips: repair kind nodes that registered their
// InternalIP on the WRONG docker network.
//
// Shared-registry clusters are dual-homed: kind creates the node on the
// cluster network ($KIND_EXPERIMENTAL_DOCKER_NETWORK) and
// connectNodesToRegistryNetwork attaches a second NIC on the registry
// network. kind's entrypoint derives the node address from docker DNS
// (getent ahostsv4 on the container hostname), and for a dual-homed
// container that lookup can flip to the REGISTRY network's address after an
// endpoint re-attach — the entrypoint then rewrites --node-ip and every
// other address reference to it on the next container restart. That address
// is dynamically allocated, so it also drifts, leaving the node registered
// on an IP that answers nowhere.
//
// The failure is silent and brutal: the node stays Ready (the lease
// heartbeat is a separate path from node status), but kubelet rejects every
// node-status update ("failed to validate nodeIP") and nothing can reach
// INTO the node — apiserver→kubelet and every cross-node pod route break.
// Observed 2026-08-17 on kubehz-dev: cert-manager-webhook lived there, so
// every webhook-validated apply failed and `lo up` died on `networking` +
// `cnpg-plugin` after burning all 6 of bootstrap's CRD/webhook retries —
// retries that can never win, because this is not the transient race they
// exist for.
//
// The repair replicates the entrypoint's OWN update mechanism, but keyed on
// the cluster-network address instead of DNS luck: sed the stale address
// across the same files_to_update set the entrypoint maintains, write
// /kind/old-ipv4 so the next boot's diff is quiet, restart kubelet.
// Idempotent and silent when everything already agrees. The remediation
// runs INSIDE the node container (docker exec bash -c <script>) — the
// generated script is byte-identical to the bash heredoc, \b-anchored sed
// included.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/ui"
)

var nodeIPFlagRe = regexp.MustCompile(`--node-ip=([^ "]*)`)

// healNodeIPs repairs drifted nodes (bash: lo::heal_node_ips). Advisory by
// contract: a node we cannot reach into is not a reason to fail the
// provision — per-node failures warn and continue, and the function only
// errors on nothing (always nil).
func (d *Driver) healNodeIPs(ctx context.Context, clusterName, kubeconfig string, errOut io.Writer) error {
	network := getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK")
	if network == "" {
		return nil
	}

	var healed []string
	nodes, _ := d.output(ctx, "kind", "get", "nodes", "--name", clusterName)
	for _, node := range strings.Fields(nodes) {
		// The node's address on the CLUSTER network — the only correct
		// --node-ip.
		want, _ := d.output(ctx, "docker", "inspect", node,
			"--format", fmt.Sprintf(`{{with index .NetworkSettings.Networks "%s"}}{{.IPAddress}}{{end}}`, network))
		if want == "" {
			continue
		}

		flags, _ := d.output(ctx, "docker", "exec", node, "cat", "/var/lib/kubelet/kubeadm-flags.env")
		have := ""
		if m := nodeIPFlagRe.FindStringSubmatch(flags); m != nil {
			have = m[1]
		}
		if have == "" {
			continue
		}

		if have == want {
			// The file agrees — but a prior heal may have died between the
			// sed and the kubelet restart, leaving kubelet running on the
			// dead address while the file (the only thing the drift check
			// reads) looks repaired. The Node object's InternalIP is the
			// running truth: node status is frozen at the last ACCEPTED
			// address, so a mismatch means kubelet never restarted onto the
			// repaired flag. Restart it now.
			if kubeconfig == "" {
				continue
			}
			observed, _ := d.output(ctx, "kubectl", "--kubeconfig", kubeconfig, "get", "node", node,
				"-o", `jsonpath={.status.addresses[?(@.type=="InternalIP")].address}`)
			if observed != "" && observed != want {
				ui.Warnf(errOut, "lo: %s flag already repaired but the node still reports %s — restarting kubelet", node, observed)
				if err := d.runQuiet(ctx, "docker", "exec", node, "systemctl", "restart", "kubelet"); err != nil {
					ui.Warnf(errOut, "lo: could not restart kubelet on %s", node)
				}
				healed = append(healed, node)
			}
			continue
		}

		// Dual-stack (--node-ip=v4,v6) is a deliberate config, not the drift
		// this heals — a naive rewrite would silently drop the v6 half.
		// Warn, don't touch.
		if strings.Contains(have, ",") {
			ui.Warnf(errOut, "lo: %s has a dual-stack --node-ip=%s — not healing (expected %s on %s)", node, have, want, network)
			continue
		}

		// Same file set kind's entrypoint updates on an address change
		// (manifests only exist on control-plane nodes —
		// existence-guarded). \b-anchored like the entrypoint's own sed, so
		// 10.125.200.2 can't match inside .200.20.
		ui.Warnf(errOut, "lo: %s registered --node-ip=%s (wrong network) — repointing to %s on %s", node, have, want, network)
		script := healScript(have, want)
		if err := d.deps.Runner.Run(ctx, execx.Cmd{
			Name: "docker", Args: []string{"exec", node, "bash", "-c", script},
			Stdout: io.Discard, Stderr: io.Discard,
		}); err != nil {
			ui.Warnf(errOut, "lo: could not repair %s — see 'docker exec %s systemctl status kubelet'", node, node)
			continue
		}
		healed = append(healed, node)
	}

	if len(healed) == 0 {
		return nil
	}

	// The CNI caches the node address too (Cilium mirrors it into CiliumNode
	// and every peer's tunnel map), so a repaired node keeps routing to the
	// dead IP until its agent re-registers. Best-effort: on a first
	// provision there is no CNI yet, and bootstrap installs a fresh one
	// moments later either way.
	if kubeconfig == "" {
		return nil
	}
	for _, node := range healed {
		d.runQuiet(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "kube-system", "delete", "pod",
			"-l", "k8s-app=cilium", "--field-selector", "spec.nodeName="+node)
	}
	return nil
}

// healScript builds the in-node repair script — byte-identical to the bash
// double-quoted heredoc, including the escaped-dots sed
// (`${have//./\\.}` → `10\.125\.200\.2`) and the literal `"${f}"` quoting
// the bash \-escapes preserved.
func healScript(have, want string) string {
	haveEsc := strings.ReplaceAll(have, ".", `\.`)
	return `
      for f in /etc/kubernetes/manifests/etcd.yaml \
               /etc/kubernetes/manifests/kube-apiserver.yaml \
               /etc/kubernetes/manifests/kube-controller-manager.yaml \
               /etc/kubernetes/manifests/kube-scheduler.yaml \
               /etc/kubernetes/controller-manager.conf \
               /etc/kubernetes/scheduler.conf \
               /etc/kubernetes/kubelet.conf \
               /kind/kubeadm.conf \
               /var/lib/kubelet/kubeadm-flags.env; do
        [ -f "${f}" ] && sed -i 's|\b` + haveEsc + `\b|` + want + `|g' "${f}"
      done
      printf '%s' '` + want + `' > /kind/old-ipv4
      systemctl restart kubelet
    `
}
