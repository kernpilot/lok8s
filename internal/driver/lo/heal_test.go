package lo

// heal_test.go — port of tests/unit/lo_heal_node_ips_test.bats: a kind node
// must never stay registered on the WRONG docker network's address (the
// 2026-08-17 kubehz-dev incident — see heal.go for the full story).
//
// Like the bats suite, `docker exec` does not fake the repair — it
// REDIRECTS the node paths onto a per-node fake rootfs and then runs the
// heal's own generated script with a REAL local bash (no docker, no
// daemon). So the sed loop, the \b anchors, the certificate move and the
// /kind/old-ipv4 write under test are the real ones; systemctl, kubeadm and
// the CNI nudge are logged. A mock that reimplemented the repair would pass
// no matter what the heal generated.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

type healFixture struct {
	t        *testing.T
	d        *Driver
	runner   *fakeRunner
	errBuf   *bytes.Buffer
	rootDir  string
	calls    []string
	observed map[string]string // kubectl InternalIP override per node
	// kubeadmFails makes the in-node kubeadm stub exit 1.
	kubeadmFails bool
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
func (h *healFixture) pkiPath(node string) string {
	return filepath.Join(h.root(node), "etc/kubernetes/pki")
}
func (h *healFixture) kubeadmConfPath(node string) string {
	return filepath.Join(h.root(node), "kind/kubeadm.conf")
}

// makeControlPlane gives node the files that only a kind control-plane node
// has: the apiserver serving pair. Also a kubeadm config that still names the
// stale address, like every other file the entrypoint rewrote.
func (h *healFixture) makeControlPlane(node string) {
	h.t.Helper()
	testutil.WriteFile(h.t, filepath.Join(h.pkiPath(node), "apiserver.crt"), "old-cert\n")
	testutil.WriteFile(h.t, filepath.Join(h.pkiPath(node), "apiserver.key"), "old-key\n")
	testutil.WriteFile(h.t, h.kubeadmConfPath(node), "advertiseAddress: 172.31.0.2\n")
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
	testutil.WriteFile(t, h.flagPath("good"), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.2 --node-labels="`+"\n")
	testutil.WriteFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2 --node-labels="`+"\n")
	// What a drifted node really looks like: the entrypoint has rewritten
	// the stale address into old-ipv4 AND the kubeadm files.
	os.WriteFile(h.oldIPPath("bad"), []byte("172.31.0.2"), 0o644)
	os.WriteFile(h.oldIPPath("good"), []byte("10.9.0.2"), 0o644)
	// A SUPERSTRING of the stale address — the \b-anchored sed must NOT
	// touch it.
	testutil.WriteFile(t, h.confPath("bad"), "server: https://172.31.0.2:6443\npeer: 172.31.0.20\n")

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
				// REAL script against the fake rootfs with stubbed systemctl
				// and kubeadm. Both stubs append to one ordered steps log.
				// The kubeadm line records what kubeadm would see: its
				// arguments, whether the old pair is still in pki/, the
				// config file it reads and /kind/old-ipv4.
				h.calls = append(h.calls, "repair:"+node)
				script := c.Args[len(c.Args)-1]
				script = strings.ReplaceAll(script, "/var/lib/kubelet/kubeadm-flags.env", h.flagPath(node))
				script = strings.ReplaceAll(script, "/etc/kubernetes", filepath.Join(h.root(node), "etc/kubernetes"))
				script = strings.ReplaceAll(script, "/kind/", filepath.Join(h.root(node), "kind")+"/")
				fail := "false"
				if h.kubeadmFails {
					fail = "true"
				}
				stubs := fmt.Sprintf(`systemctl() { echo kubelet-restart:%[1]s >> '%[2]s'; }
kubeadm() {
  local pki='%[3]s' pair=absent
  if [ -e "${pki}/apiserver.crt" ] || [ -e "${pki}/apiserver.key" ]; then pair=present; fi
  echo "kubeadm:%[1]s $* | old-pair=${pair} | conf=$(cat "${@: -1}") | old-ipv4=$(cat '%[4]s')" >> '%[2]s'
  if %[5]s; then return 1; fi
  echo renewed > "${pki}/apiserver.crt" && echo renewed > "${pki}/apiserver.key"
}
`, node, h.stepsPath(), h.pkiPath(node), h.oldIPPath(node), fail)
				out, err := exec.Command("bash", "-c", stubs+script).CombinedOutput()
				if err != nil {
					return fmt.Errorf("repair script failed: %w\n%s", err, out)
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

func (h *healFixture) stepsPath() string { return filepath.Join(h.rootDir, "steps") }

// steps reads the ordered log of the in-script systemctl and kubeadm stubs.
func (h *healFixture) steps() []string {
	raw, err := os.ReadFile(h.stepsPath())
	if err != nil || len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// restarts reads the kubelet restarts recorded by the in-script systemctl
// stub PLUS the direct `docker exec … systemctl` calls.
func (h *healFixture) restarts() []string {
	var out []string
	for _, s := range h.steps() {
		if strings.HasPrefix(s, "kubelet-restart:") {
			out = append(out, s)
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
	return slices.Contains(h.calls, marker)
}

func TestHealRepairsOnlyTheDriftedNode(t *testing.T) {
	h := newHealFixture(t)
	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
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
	testutil.WriteFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.3 --node-labels="`+"\n")

	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
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
	testutil.WriteFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.3 --node-labels="`+"\n")
	h.observed["bad"] = "172.31.0.2"

	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
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

func TestHealNonIPAddressNeverReachesTheNode(t *testing.T) {
	// The docker inspect answer is spliced into the repair script. A value
	// that is not an IP address must not be written anywhere.
	h := newHealFixture(t)
	orig := h.runner.handler
	h.runner.handler = func(c execx.Cmd) error {
		if c.Name == "docker" && len(c.Args) > 1 && c.Args[0] == "inspect" && c.Args[1] == "bad" {
			writeOut(c, "10.9.0.3'; touch /tmp/pwned; echo '\n")
			return nil
		}
		return orig(c)
	}
	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if h.called("repair:bad") {
		t.Fatal("a non-IP docker answer was spliced into the repair script")
	}
	if !strings.Contains(readFileT(t, h.flagPath("bad")), "--node-ip=172.31.0.2") {
		t.Fatal("kubeadm-flags.env was rewritten with a non-IP value")
	}
	if !strings.Contains(h.errBuf.String(), "not an IP address") {
		t.Fatalf("the skip must be reported:\n%s", h.errBuf.String())
	}
}

func TestHealDualStackWarnedNeverRewritten(t *testing.T) {
	// --node-ip=v4,v6 is a deliberate config, not the drift this heals. A
	// naive rewrite would silently drop the v6 half and break dual-stack
	// clusters.
	h := newHealFixture(t)
	testutil.WriteFile(t, h.flagPath("bad"), `KUBELET_KUBEADM_ARGS="--node-ip=172.31.0.2,fd00::2 --node-labels="`+"\n")

	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
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
	if err := h.d.healNodeIPs(t.Context(), "lotest", "", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if !h.called("repair:bad") {
		t.Fatal("drifted node not repaired")
	}
	if h.called("cni-restart") {
		t.Fatal("kubectl was invoked without a kubeconfig")
	}
}

func TestHealRenewsAPIServerCertOnControlPlane(t *testing.T) {
	// On a control-plane node the apiserver serving certificate names the node
	// address. After the heal, kubelet, controller-manager and scheduler
	// connect to the new address, so the certificate must name it too. kind
	// renews it at boot only when /kind/old-ipv4 differs, and the heal writes
	// the new address there, so the heal must renew it itself.
	h := newHealFixture(t)
	h.makeControlPlane("bad")

	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}

	steps := h.steps()
	renew := slices.IndexFunc(steps, func(s string) bool { return strings.HasPrefix(s, "kubeadm:bad ") })
	if renew < 0 {
		t.Fatalf("the apiserver certificate of the control-plane node was not renewed: it still names 172.31.0.2, so every client of https://10.9.0.3:6443 fails TLS. Steps: %q", steps)
	}
	got := steps[renew]
	if want := "kubeadm:bad init phase certs apiserver --config " + h.kubeadmConfPath("bad") + " | "; !strings.HasPrefix(got, want) {
		t.Fatalf("the kubeadm call is not the one of kind's fix_certificate (kubeadm init phase certs apiserver --config /kind/kubeadm.conf):\n%s", got)
	}
	if !strings.Contains(got, " | old-pair=absent | ") {
		t.Fatalf("the old pair was still in pki/ when kubeadm ran: kubeadm keeps a certificate that has every SAN and fails on one that does not, it never replaces it:\n%s", got)
	}
	if !strings.Contains(got, " | conf=advertiseAddress: 10.9.0.3 | ") {
		t.Fatalf("kubeadm read a config with the old address: the renewal must come AFTER the sed, or the new certificate names the old address again:\n%s", got)
	}
	if !strings.HasSuffix(got, " | old-ipv4=172.31.0.2") {
		t.Fatalf("/kind/old-ipv4 already held the new address when kubeadm ran: write it AFTER the renewal, so that a heal that stops before the renewal leaves kind's boot-time fix_certificate in place:\n%s", got)
	}
	if restart := slices.Index(steps, "kubelet-restart:bad"); restart < renew {
		t.Fatalf("kubelet did not restart after the renewal: steps %q", steps)
	}
	if crt := readFileT(t, filepath.Join(h.pkiPath("bad"), "apiserver.crt")); crt != "renewed\n" {
		t.Fatalf("apiserver.crt after the heal = %q, want the renewed certificate", crt)
	}
	if _, err := os.Stat(filepath.Join(h.root("bad"), "kind/heal-pki")); !os.IsNotExist(err) {
		t.Fatalf("the moved-aside pair stayed in /kind/heal-pki after a good renewal (stat: %v)", err)
	}
}

func TestHealWorkerSkipsCertRenewal(t *testing.T) {
	// A worker runs no apiserver and has no apiserver.crt. The guard keeps
	// kubeadm off it, and the rest of the repair runs as before.
	h := newHealFixture(t)

	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}
	for _, s := range h.steps() {
		if strings.HasPrefix(s, "kubeadm:") {
			t.Fatalf("kubeadm ran on a worker, which has no apiserver certificate: %s", s)
		}
	}
	if !slices.Contains(h.restarts(), "kubelet-restart:bad") {
		t.Fatalf("the worker repair did not finish (no kubelet restart): %q\n%s", h.steps(), h.errBuf.String())
	}
	if _, err := os.Stat(filepath.Join(h.root("bad"), "kind/heal-pki")); !os.IsNotExist(err) {
		t.Fatalf("the heal made /kind/heal-pki on a worker (stat: %v)", err)
	}
}

func TestHealCertRenewalFailureKeepsTheOldPair(t *testing.T) {
	// kind's fix_certificate skips a node without apiserver.crt. So a failed
	// renewal must put the old pair back, and must leave /kind/old-ipv4 at the
	// old address, so that the next node boot retries the renewal. It must
	// also stop before the kubelet restart and report the failure.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.kubeadmFails = true

	if err := h.d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		t.Fatal(err)
	}
	if crt := readFileT(t, filepath.Join(h.pkiPath("bad"), "apiserver.crt")); crt != "old-cert\n" {
		t.Fatalf("a failed renewal lost apiserver.crt (%q): the apiserver has no serving certificate, and kind's fix_certificate skips a node without one", crt)
	}
	if key := readFileT(t, filepath.Join(h.pkiPath("bad"), "apiserver.key")); key != "old-key\n" {
		t.Fatalf("a failed renewal lost apiserver.key (%q)", key)
	}
	if got := readFileT(t, h.oldIPPath("bad")); got != "172.31.0.2" {
		t.Fatalf("/kind/old-ipv4 = %q after a failed renewal: the next boot sees no address change and does not retry the renewal", got)
	}
	if slices.Contains(h.restarts(), "kubelet-restart:bad") {
		t.Fatal("kubelet restarted after a failed renewal: the script must stop and report")
	}
	if !strings.Contains(h.errBuf.String(), "could not repair bad") {
		t.Fatalf("a failed renewal was not reported:\n%s", h.errBuf.String())
	}
}

func TestNodesOnRegistryNetworkGate(t *testing.T) {
	// The gate for the heal after the shared→per-project default flip: a
	// cluster provisioned under the OLD default still carries registry NICs
	// while its spec now reads "not shared" — membership, not the spec,
	// must decide.
	d, runner, _, _ := testDriver(t)
	jsonPath := filepath.Join(t.TempDir(), ".registries.json")
	testutil.WriteFile(t, jsonPath, `{"shared":false,"tls":false,"port":80,"network":{"name":"lok8s-registries","cidr":"10.125.200.0/24"},"project_network":"lok8s","registries":[]}`)
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

	if !d.nodesOnRegistryNetwork(t.Context(), "lotest") {
		t.Fatal("node 'bad' is attached to the registry network but the gate said no — an already-drifted pre-flip cluster would never be healed")
	}

	members = "lok8s-registry-io-docker "
	if d.nodesOnRegistryNetwork(t.Context(), "lotest") {
		t.Fatal("no cluster node is on the registry network, yet the gate fired")
	}
}
