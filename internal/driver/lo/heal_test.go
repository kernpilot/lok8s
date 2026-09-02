package lo

// heal_test.go — port of tests/unit/lo_heal_node_ips_test.bats: a kind node
// must never stay registered on the WRONG docker network's address (the
// 2026-08-17 kubehz-dev incident — see heal.go for the full story).
//
// Like the bats suite, `docker exec` does not fake the repair — it
// REDIRECTS the node paths onto a per-node fake rootfs and then runs the
// heal's own generated script with a REAL local bash (no docker, no
// daemon). So the sed loop, the \b anchors and the /kind/old-ipv4 write
// under test are the real ones; systemctl and the CNI nudge are logged. A
// mock that reimplemented the repair would pass no matter what the heal
// generated.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

type healFixture struct {
	t        *testing.T
	d        *Driver
	runner   *fakeRunner
	errBuf   *bytes.Buffer
	rootDir  string
	calls    []string
	observed map[string]string // kubectl InternalIP override per node
}

func (h *healFixture) root(node string) string { return filepath.Join(h.rootDir, "root."+node) }
func (h *healFixture) flagPath(node string) string {
	return filepath.Join(h.root(node), "var/lib/kubelet/kubeadm-flags.env")
}
func (h *healFixture) oldIPPath(node string) string {
	return filepath.Join(h.root(node), "kind/old-ipv4")
}
func (h *healFixture) confPath(node string) string {
	return filepath.Join(h.root(node), "etc/kubernetes/kubelet.conf")
}

func newHealFixture(t *testing.T) *healFixture {
	t.Helper()
	d, runner, errBuf, _ := testDriver(t)
	t.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s-test")

	h := &healFixture{
		t: t, d: d, runner: runner, errBuf: errBuf,
		rootDir:  t.TempDir(),
		observed: map[string]string{"good": "10.9.0.2", "bad": "10.9.0.3"},
	}

	// Two nodes: `good` already agrees with the cluster network, `bad` is
	// pinned to the registry network's (stale) address.
	for _, node := range []string{"good", "bad"} {
		for _, sub := range []string{"var/lib/kubelet", "kind", "etc/kubernetes"} {
			os.MkdirAll(filepath.Join(h.root(node), sub), 0o755)
		}
	}
	writeFile(t, h.flagPath("good"), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.2 --node-labels="`+"\n")
	writeFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2 --node-labels="`+"\n")
	// What a drifted node really looks like: the entrypoint has rewritten
	// the stale address into old-ipv4 AND the kubeadm files.
	os.WriteFile(h.oldIPPath("bad"), []byte("172.31.0.2"), 0o644)
	os.WriteFile(h.oldIPPath("good"), []byte("10.9.0.2"), 0o644)
	// A SUPERSTRING of the stale address — the \b-anchored sed must NOT
	// touch it.
	writeFile(t, h.confPath("bad"), "server: https://172.31.0.2:6443\npeer: 172.31.0.20\n")

	runner.handler = func(c execx.Cmd) error {
		joined := strings.Join(c.Args, " ")
		switch c.Name {
		case "kind":
			if len(c.Args) > 0 && c.Args[0] == "get" {
				writeOut(c, "good\nbad\n")
			}
			return nil
		case "docker":
			if len(c.Args) == 0 {
				return nil
			}
			switch c.Args[0] {
			case "inspect":
				// A broken template in prod yields an EMPTY address (silent
				// skip), so the fake only answers when the template actually
				// selects the cluster network — mutating the format string
				// makes every heal test fail.
				if !strings.Contains(joined, ".NetworkSettings.Networks") ||
					!strings.Contains(joined, "lok8s-test") {
					return nil
				}
				switch c.Args[1] {
				case "good":
					writeOut(c, "10.9.0.2\n")
				case "bad":
					writeOut(c, "10.9.0.3\n")
				}
				return nil
			case "exec":
				node := c.Args[1]
				if len(c.Args) > 2 && c.Args[2] == "cat" {
					writeOut(c, readFileT(t, h.flagPath(node)))
					return nil
				}
				if len(c.Args) > 2 && c.Args[2] == "systemctl" {
					h.calls = append(h.calls, "kubelet-restart:"+node)
					return nil
				}
				// `docker exec <node> bash -c '<repair script>'` — run the
				// REAL script against the fake rootfs with a stubbed
				// systemctl.
				h.calls = append(h.calls, "repair:"+node)
				script := c.Args[len(c.Args)-1]
				script = strings.ReplaceAll(script, "/var/lib/kubelet/kubeadm-flags.env", h.flagPath(node))
				script = strings.ReplaceAll(script, "/etc/kubernetes", filepath.Join(h.root(node), "etc/kubernetes"))
				script = strings.ReplaceAll(script, "/kind/", filepath.Join(h.root(node), "kind")+"/")
				wrapped := fmt.Sprintf("systemctl() { echo kubelet-restart:%s >> %s; }\n%s",
					node, filepath.Join(h.rootDir, "restarts"), script)
				out, err := exec.Command("bash", "-c", wrapped).CombinedOutput()
				if err != nil {
					return fmt.Errorf("repair script failed: %v\n%s", err, out)
				}
				return nil
			}
			return nil
		case "kubectl":
			if strings.Contains(joined, "get node") {
				for node, ip := range h.observed {
					if strings.Contains(joined, " "+node+" ") || strings.HasSuffix(joined, " "+node) ||
						strings.Contains(joined, "node "+node) {
						writeOut(c, ip)
						_ = node
						break
					}
				}
				return nil
			}
			h.calls = append(h.calls, "cni-restart")
			return nil
		}
		return nil
	}
	return h
}

// restarts reads the kubelet restarts recorded by the in-script systemctl
// stub PLUS the direct `docker exec … systemctl` calls.
func (h *healFixture) restarts() []string {
	var out []string
	if raw, err := os.ReadFile(filepath.Join(h.rootDir, "restarts")); err == nil {
		for _, l := range strings.Fields(string(raw)) {
			out = append(out, l)
		}
	}
	for _, c := range h.calls {
		if strings.HasPrefix(c, "kubelet-restart:") {
			out = append(out, c)
		}
	}
	return out
}

func (h *healFixture) called(marker string) bool {
	for _, c := range h.calls {
		if c == marker {
			return true
		}
	}
	return false
}

func TestHealRepairsOnlyTheDriftedNode(t *testing.T) {
	h := newHealFixture(t)
	if err := h.d.healNodeIPs(context.Background(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}

	if !h.called("repair:bad") {
		t.Fatal("the node pinned to the registry address (172.31.0.2) was NOT repaired — it stays Ready via its lease while every route INTO it is black-holed")
	}
	if h.called("repair:good") {
		t.Fatal("a node whose --node-ip already matched the cluster network was needlessly restarted — the heal must be a no-op when nothing drifted")
	}
	if !strings.Contains(readFileT(t, h.flagPath("bad")), "--node-ip=10.9.0.3") {
		t.Fatal("kubeadm-flags.env still carries the dead address")
	}
	conf := readFileT(t, h.confPath("bad"))
	if !strings.Contains(conf, "10.9.0.3") {
		t.Fatal("kubelet.conf still references the dead address — kind's entrypoint updates its whole files_to_update set on an address change, and so must the heal")
	}
	if !strings.Contains(conf, "peer: 172.31.0.20") {
		t.Fatal("the sed rewrote 172.31.0.20 — a SUPERSTRING of the stale address. The \\b anchors are load-bearing: without them a repair corrupts any address that merely starts with the stale one")
	}
	if got := readFileT(t, h.oldIPPath("bad")); got != "10.9.0.3" {
		t.Fatalf("/kind/old-ipv4 was not updated (%q) — the entrypoint diffs it against docker DNS on the next container restart and would rewrite the address files all over again", got)
	}
	restarted := false
	for _, r := range h.restarts() {
		if r == "kubelet-restart:bad" {
			restarted = true
		}
	}
	if !restarted {
		t.Fatal("kubelet was never restarted — the repaired flag is not read until restart, so the node keeps running on the dead address")
	}
	if !h.called("cni-restart") {
		t.Fatal("the CNI agent was not restarted — peers keep routing to the dead address they cached from CiliumNode")
	}
}

func TestHealHealthyClusterIsSilentNoop(t *testing.T) {
	h := newHealFixture(t)
	// Repair the drifted node up front, so nothing needs healing.
	writeFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.3 --node-labels="`+"\n")

	if err := h.d.healNodeIPs(context.Background(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if len(h.calls) != 0 || len(h.restarts()) != 0 {
		t.Fatalf("the heal acted on a cluster where every --node-ip already matched: %v — restarting kubelet + the CNI agent on every provision is not free", h.calls)
	}
}

func TestHealLatchUpRestartsKubeletOnly(t *testing.T) {
	// A prior heal died between the sed and the kubelet restart: the flag
	// file says the right address (so the drift check alone would skip
	// forever), but kubelet still runs on the dead one — visible as a
	// frozen Node InternalIP.
	h := newHealFixture(t)
	writeFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.3 --node-labels="`+"\n")
	h.observed["bad"] = "172.31.0.2"

	if err := h.d.healNodeIPs(context.Background(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if !h.called("kubelet-restart:bad") {
		t.Fatal("the half-repaired node was skipped: its flag file matches, so without the Node-InternalIP cross-check the drift is permanently undetectable")
	}
	if h.called("repair:bad") {
		t.Fatal("the full repair script re-ran on a node whose files are already correct — only the missed kubelet restart was needed")
	}
	if h.called("kubelet-restart:good") {
		t.Fatal("a fully healthy node's kubelet was restarted")
	}
}

func TestHealDualStackWarnedNeverRewritten(t *testing.T) {
	// --node-ip=v4,v6 is a deliberate config, not the drift this heals. A
	// naive rewrite would silently drop the v6 half and break dual-stack
	// clusters.
	h := newHealFixture(t)
	writeFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2,fd00::2 --node-labels="`+"\n")

	if err := h.d.healNodeIPs(context.Background(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if h.called("repair:bad") {
		t.Fatal("a dual-stack --node-ip was rewritten — the v6 address is now lost")
	}
	if !strings.Contains(h.errBuf.String(), "dual-stack") {
		t.Fatalf("a dual-stack mismatch was skipped SILENTLY; it must still be reported, or a genuinely misconfigured dual-stack node looks healthy forever:\n%s", h.errBuf.String())
	}
}

func TestHealSkipsCNINudgeWithoutKubeconfig(t *testing.T) {
	// On a FIRST provision there is no CNI yet (bootstrap installs it
	// moments later), and Provision may call the heal before a kubeconfig
	// exists — kubectl must NOT be invoked, or it would talk to whatever
	// $KUBECONFIG/current-context happens to point at: the WRONG cluster.
	h := newHealFixture(t)
	if err := h.d.healNodeIPs(context.Background(), "lotest", "", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if !h.called("repair:bad") {
		t.Fatal("drifted node not repaired")
	}
	if h.called("cni-restart") {
		t.Fatal("kubectl was invoked without a kubeconfig")
	}
}

func TestNodesOnRegistryNetworkGate(t *testing.T) {
	// The gate for the heal after the shared→per-project default flip: a
	// cluster provisioned under the OLD default still carries registry NICs
	// while its spec now reads "not shared" — membership, not the spec,
	// must decide.
	d, runner, _, _ := testDriver(t)
	jsonPath := filepath.Join(t.TempDir(), ".registries.json")
	writeFile(t, jsonPath, `{"shared":false,"tls":false,"port":80,"network":{"name":"lok8s-registries","cidr":"10.125.200.0/24"},"project_network":"lok8s","registries":[]}`)
	t.Setenv("LOK8S_REGISTRY_JSON", jsonPath)

	members := "lok8s-registry-io-docker bad "
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "docker" {
			writeOut(c, members+"\n")
			return nil
		}
		if c.Name == "kind" {
			writeOut(c, "good\nbad\n")
		}
		return nil
	}

	if !d.nodesOnRegistryNetwork(context.Background(), "lotest") {
		t.Fatal("node 'bad' is attached to the registry network but the gate said no — an already-drifted pre-flip cluster would never be healed")
	}

	members = "lok8s-registry-io-docker "
	if d.nodesOnRegistryNetwork(context.Background(), "lotest") {
		t.Fatal("no cluster node is on the registry network, yet the gate fired")
	}
}
