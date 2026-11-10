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
// Observed 2026-08-17 on a dev cluster: cert-manager-webhook lived there, so
// every webhook-validated apply failed and `lo up` died on `networking` +
// `cnpg-plugin` after burning all 6 of bootstrap's CRD/webhook retries —
// retries that can never win, because this is not the transient race they
// exist for.
//
// The repair replicates the entrypoint's OWN update mechanism, but keyed on
// the cluster-network address instead of DNS luck: sed the stale address
// across the same files_to_update set the entrypoint maintains, renew the
// apiserver serving certificate on a control-plane node (the entrypoint's
// fix_certificate) and restart the controller manager and the scheduler
// there, write /kind/old-ipv4 so the next boot's diff is quiet, restart
// kubelet. Idempotent and silent when everything already agrees.
// The remediation runs INSIDE the node container (docker exec bash -c
// <script>). The generated script is the bash heredoc byte for byte, \b-anchored
// sed included, except for the `\`-newline continuations, which bash removes
// inside double quotes (TestHealScriptMatchesBash).

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strings"

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
	// tried: a repair script ran on a node, also one that failed.
	tried := false
	nodes, _ := d.output(ctx, "kind", "get", "nodes", "--name", clusterName)
	for node := range strings.FieldsSeq(nodes) {
		// The node's address on the CLUSTER network — the only correct
		// --node-ip.
		want, _ := d.output(ctx, "docker", "inspect", node,
			"--format", fmt.Sprintf(`{{with index .NetworkSettings.Networks "%s"}}{{.IPAddress}}{{end}}`, network))
		if want == "" {
			continue
		}
		// The address is spliced into the repair script (sed, printf), so
		// only a real IP may reach the node.
		if net.ParseIP(want) == nil {
			ui.WarnTo(errOut, "lo: %s: docker reports %q on %s, not an IP address — not healing", node, want, network)
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
			// The flag agrees, but on a control-plane node the apiserver
			// certificate can still name the old address: a heal whose
			// renewal failed (the script puts the old pair back and stops),
			// a heal by an older lo that did not renew, or a heal that
			// stopped with the pair in /kind/heal-pki. Run the repair again
			// on such a node. With have == want the sed changes nothing,
			// and the script restores and renews the pair.
			if reason := d.certRenewalReason(ctx, node, want); reason != "" {
				ui.WarnTo(errOut, "lo: %s %s — renewing the certificate", node, reason)
				tried = true
				if d.repairNode(ctx, node, want, want, errOut) {
					healed = append(healed, node)
				}
				continue
			}
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
				ui.WarnTo(errOut, "lo: %s flag already repaired but the node still reports %s — restarting kubelet", node, observed)
				if err := d.runQuiet(ctx, "docker", "exec", node, "systemctl", "restart", "kubelet"); err != nil {
					ui.WarnTo(errOut, "lo: could not restart kubelet on %s", node)
				}
				healed = append(healed, node)
			}
			continue
		}

		// Dual-stack (--node-ip=v4,v6) is a deliberate config, not the drift
		// this heals — a naive rewrite would silently drop the v6 half.
		// Warn, don't touch.
		if strings.Contains(have, ",") {
			ui.WarnTo(errOut, "lo: %s has a dual-stack --node-ip=%s — not healing (expected %s on %s)", node, have, want, network)
			continue
		}

		// Same file set kind's entrypoint updates on an address change
		// (manifests only exist on control-plane nodes —
		// existence-guarded). \b-anchored like the entrypoint's own sed, so
		// 10.125.200.2 can't match inside .200.20. On a control-plane node
		// the script also renews the apiserver certificate (see healScript).
		ui.WarnTo(errOut, "lo: %s registered --node-ip=%s (wrong network) — repointing to %s on %s", node, have, want, network)
		tried = true
		if d.repairNode(ctx, node, have, want, errOut) {
			healed = append(healed, node)
		}
	}

	if len(healed) == 0 && !tried {
		return nil
	}
	if kubeconfig == "" {
		return nil
	}
	// Wait also after a repair that failed: on the full path the sed runs
	// before kubeadm, so a failed renewal has already changed the static pod
	// manifests, and kubelet starts those pods again.
	d.waitAPIServer(ctx, kubeconfig, errOut)

	// The CNI caches the node address too (Cilium mirrors it into CiliumNode
	// and every peer's tunnel map), so a repaired node keeps routing to the
	// dead IP until its agent re-registers. Best-effort: on a first
	// provision there is no CNI yet, and bootstrap installs a fresh one
	// moments later either way.
	for _, node := range healed {
		_ = d.runQuiet(ctx, "kubectl", "--kubeconfig", kubeconfig, "-n", "kube-system", "delete", "pod",
			"-l", "k8s-app=cilium", "--field-selector", "spec.nodeName="+node)
	}
	return nil
}

// The API server must pass healReadyInRow /readyz checks in a row,
// healReadyInterval seconds apart, within healReadyMaxChecks checks. Each
// check stops after healReadyTimeout: an API server that accepts the
// connection and does not answer fails the check and does not stop the wait.
const (
	healReadyInRow     = 5
	healReadyInterval  = 2
	healReadyMaxChecks = 60
	healReadyTimeout   = "5s"
)

// waitAPIServer waits after a repair until the API server is steady (bash:
// lo::heal_wait_apiserver). A kubelet restart on a control-plane node can
// make kubelet start etcd and the controller manager again a few seconds
// later (seen on kindest/node v1.31.12). The API server then refuses
// requests, and the next step of lo up (the first kubectl apply) fails. One
// good check is not enough, because the restart of etcd can come after it.
// A server that is not steady after healReadyMaxChecks checks gets a
// warning; the heal stays advisory.
func (d *Driver) waitAPIServer(ctx context.Context, kubeconfig string, errOut io.Writer) {
	inRow := 0
	for range healReadyMaxChecks {
		if d.runQuiet(ctx, "kubectl", "--kubeconfig", kubeconfig, "--request-timeout="+healReadyTimeout, "get", "--raw", "/readyz") == nil {
			inRow++
			if inRow == healReadyInRow {
				return
			}
		} else {
			inRow = 0
		}
		if d.sleepSeconds(ctx, healReadyInterval) != nil {
			return
		}
	}
	ui.WarnTo(errOut, "lo: the API server did not pass %d /readyz checks in a row after the node repair (%d checks, %d s apart). If a later step fails, run lo up again.",
		healReadyInRow, healReadyMaxChecks, healReadyInterval)
}

// repairNode runs the repair script in the node (bash:
// lo::heal_node_repair). The script's stdout is discarded. On a failure the
// warning carries the end of its stderr (kubeadm writes its reason there),
// and the function returns false.
//
// After a script that exited 0, the function reads the certificate again
// (certRenewalReason). A renewal that did not change the certificate (for
// example, a kubeadm that exits 0 and writes a certificate without the node
// address) gets a warning, and the next lo up tries again. The function
// still returns true: the script ran to its end, so kubelet runs on the new
// address, and the CNI agent must register it again. A worker has no
// certificate and passes the check.
func (d *Driver) repairNode(ctx context.Context, node, have, want string, errOut io.Writer) bool {
	stderr, err := d.errOutput(ctx, "docker", "exec", node, "bash", "-c", healScript(have, want))
	if err != nil {
		ui.WarnTo(errOut, "lo: could not repair %s: %s. The next lo up tries again.", node, healErrTail(stderr))
		return false
	}
	if reason := d.certRenewalReason(ctx, node, want); reason != "" {
		ui.WarnTo(errOut, "lo: the certificate renewal on %s did not complete: the node %s. The next lo up tries again.", node, reason)
	}
	return true
}

// klogNoteRe matches a klog line of level info or warning ("W1110
// 16:20:03.113013 …"). kubeadm writes one for each document of kind's
// /kind/kubeadm.conf that the init phase does not read.
var klogNoteRe = regexp.MustCompile(`^[IW][0-9]{4} `)

// healErrTail bounds the stderr of the repair script for a warning (bash:
// lo::heal_err_tail): the last three lines, joined with "; ", at most 400
// characters. It skips blank lines, klog info and warning lines, and
// kubeadm's hint "To see the stack trace of this error execute with --v=5
// or higher", so the error line of kubeadm stays in the warning.
func healErrTail(stderr string) string {
	var lines []string
	for l := range strings.Lines(stderr) {
		l = strings.TrimSpace(l)
		if l == "" || klogNoteRe.MatchString(l) || strings.HasPrefix(l, "To see the stack trace") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "no error output"
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	tail := []rune(strings.Join(lines, "; "))
	if len(tail) > 400 {
		tail = tail[:400]
	}
	return string(tail)
}

// certProbe is the in-node read of the renewal check. It prints heal-pki
// when an interrupted heal left the pair in /kind/heal-pki. On a node with
// the whole serving pair (a control-plane node) it prints the apiserver
// certificate. On a worker it prints nothing.
//
// Deviation D43: the bash twin runs the same test with `openssl x509 -in
// <crt> -noout -ext subjectAltName` in place of `cat <crt>` and reads the
// SAN text (kind's node images ship openssl). Go parses the PEM with
// crypto/x509, so it needs no openssl in the node.
const certProbe = `if [ -d /kind/heal-pki ]; then echo heal-pki; elif [ -f /etc/kubernetes/pki/apiserver.key ]; then cat /etc/kubernetes/pki/apiserver.crt; fi`

// certRenewalReason tells why a node whose --node-ip already agrees needs
// the repair script again, or returns "" when it does not. repairNode also
// calls it after a script that exited 0. A node that cannot be read, a
// worker, and a certificate that does not parse return "" (the bash twin
// skips them the same way).
func (d *Driver) certRenewalReason(ctx context.Context, node, want string) string {
	out, _ := d.output(ctx, "docker", "exec", node, "sh", "-c", certProbe)
	if out == "heal-pki" {
		return "has its apiserver key pair in /kind/heal-pki from an interrupted heal"
	}
	block, _ := pem.Decode([]byte(out))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	if slices.ContainsFunc(cert.IPAddresses, net.ParseIP(want).Equal) {
		return ""
	}
	return fmt.Sprintf("has --node-ip=%s, but its apiserver certificate does not name that address", want)
}

// healScript builds the in-node repair script — the bash double-quoted
// heredoc byte for byte, including the escaped-dots sed
// (`${have//./\\.}` → `10\.125\.200\.2`) and the literal `"${f}"` quoting
// the bash \-escapes preserved. TestHealScriptMatchesBash compares the two.
//
// The first block puts back a pair that an interrupted heal left in
// /kind/heal-pki (between the mv and the end of kubeadm). kind's
// fix_certificate skips a node without apiserver.crt, so without this step
// the pair would never come back. A complete pair in pki/ wins: kubeadm
// wrote a new pair, or the mv did not start. Otherwise every file in
// /kind/heal-pki comes back, so the key and the certificate always match.
//
// The certificate step is the entrypoint's fix_certificate. kubeadm puts the
// node address into the apiserver serving certificate as an IP SAN. After
// the sed, controller-manager and scheduler connect to https://<want>:6443
// (their kubeconfigs name the node address), but the certificate still names
// <have>, so they fail TLS. kubelet.conf names the node address too on newer
// kind node images (v1.37.0) and <cluster>-control-plane on older ones
// (v1.31.12), a name that the certificate also has. kind renews the
// certificate at boot only when the address in /kind/old-ipv4 differs, and
// the heal writes <want> there, so without this step the certificate stays
// wrong after every later boot too.
//
//   - Same guard and same kubeadm call as fix_certificate: only a node with
//     apiserver.crt AND apiserver.key runs kube-apiserver (a worker skips
//     the step), and the config is /kind/kubeadm.conf, which the sed has
//     just moved to <want>.
//   - The old pair must leave pki/ first: `kubeadm init phase certs
//     apiserver` keeps an apiserver.crt that the cluster CA signed and does
//     not read its SANs ("Using existing apiserver certificate and key on
//     disk", exit 0). With the old pair in pki/, the call changes nothing.
//   - Moved aside, not removed, for the same reason as the first block. A
//     failed mkdir stops the script before anything moves. On a failure of
//     the mv or of kubeadm, the script puts back every file of the old pair
//     that is in /kind/heal-pki and exits 1 before old-ipv4 and the kubelet
//     restart. Without this check, a mv that moves nothing lets kubeadm keep
//     the old pair (exit 0), and the heal passes. The caller warns with the
//     reason from stderr. The next lo up finds the flag already at <want>
//     and a certificate without <want> (certRenewalReason), and runs the
//     script again. old-ipv4 still holds <have>, so a node boot that finds
//     a new address also runs kind's fix_certificate.
//   - Before old-ipv4 for the same reason: a heal that stops before the
//     renewal leaves that boot-time renewal in place.
//   - No extra apiserver restart: kube-apiserver watches its certificate
//     files and loads the new pair. On the have == want path this is the
//     only reload, because the sed changes no manifest. On the full path
//     the manifest sed also makes kubelet create the apiserver static pod
//     again.
//   - After the renewal the script stops the kube-controller-manager and
//     kube-scheduler containers, and kubelet starts them again. Their
//     manifests do not name the node address, so the sed does not restart
//     them, and they read the server address of their kubeconfig only at
//     start. A process that still connects to <have> fails TLS on the new
//     certificate until it starts again (seen with kube-scheduler on
//     kindest/node v1.31.12 after a node restart: TLS errors for the whole
//     minute after the heal). A failed stop does not stop the script: the
//     renewal is done.
func healScript(have, want string) string {
	haveEsc := strings.ReplaceAll(have, ".", `\.`)
	return `
      if [ -d /kind/heal-pki ]; then
        if ! { [ -f /etc/kubernetes/pki/apiserver.crt ] && [ -f /etc/kubernetes/pki/apiserver.key ]; }; then
          for k in apiserver.crt apiserver.key; do
            if [ -f "/kind/heal-pki/${k}" ]; then mv -f "/kind/heal-pki/${k}" /etc/kubernetes/pki/ || exit 1; fi
          done
        fi
        rm -rf /kind/heal-pki
      fi
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
      if [ -f /etc/kubernetes/pki/apiserver.crt ] && [ -f /etc/kubernetes/pki/apiserver.key ]; then
        mkdir -p /kind/heal-pki || exit 1
        mv -f /etc/kubernetes/pki/apiserver.crt /etc/kubernetes/pki/apiserver.key /kind/heal-pki/ &&
          kubeadm init phase certs apiserver --config /kind/kubeadm.conf >/dev/null || {
          for k in apiserver.crt apiserver.key; do
            if [ -f "/kind/heal-pki/${k}" ]; then mv -f "/kind/heal-pki/${k}" /etc/kubernetes/pki/ || exit 1; fi
          done
          rm -rf /kind/heal-pki
          exit 1
        }
        rm -rf /kind/heal-pki
        for c in $(crictl ps -q --name 'kube-controller-manager|kube-scheduler'); do crictl stop "${c}" >/dev/null; done
      fi
      printf '%s' '` + want + `' > /kind/old-ipv4
      systemctl restart kubelet
    `
}
