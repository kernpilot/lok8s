package lo

// heal_test.go — port of tests/unit/lo_heal_node_ips_test.bats: a kind node
// must never stay registered on the WRONG docker network's address (the
// 2026-08-17 incident on a dev cluster — see heal.go for the full story).
//
// Like the bats suite, `docker exec` does not fake the repair — it
// REDIRECTS the node paths onto a per-node fake rootfs and then runs the
// heal's own generated scripts (the repair and the certificate probe) with
// a REAL local shell (no docker, no daemon). So the sed loop, the \b
// anchors, the certificate moves and the /kind/old-ipv4 write under test
// are the real ones; systemctl, kubeadm, crictl and the CNI nudge are
// logged, and mv can be made to fail. A mock that reimplemented the repair
// would pass no matter what the heal generated.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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
	// kubeadmStale makes the in-node kubeadm stub write the pair for the old
	// address 172.31.0.2 and exit 0: a renewal that does not change the
	// certificate.
	kubeadmStale bool
	// mvFails makes the in-node mv fail on the move of the old pair into
	// /kind/heal-pki: "all" moves nothing, "key" moves the certificate only.
	mvFails string
	// readyz holds the answers of the /readyz checks in order (true = ready);
	// every check after the last answer is ready.
	readyz []bool
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
func (h *healFixture) healPKIPath(node string) string {
	return filepath.Join(h.root(node), "kind/heal-pki")
}
func (h *healFixture) kubeadmConfPath(node string) string {
	return filepath.Join(h.root(node), "kind/kubeadm.conf")
}
func (h *healFixture) certsDir() string { return filepath.Join(h.rootDir, "certs") }

// certPEM is a real serving certificate for the apiserver of node `bad`,
// with ip among the IP SANs (beside the service and loopback addresses, so
// a check that reads only one SAN fails). The kubeadm stub copies it from
// the fixture's certs/ directory.
func (h *healFixture) certPEM(ip string) string {
	h.t.Helper()
	path := filepath.Join(h.certsDir(), ip+".crt")
	if raw, err := os.ReadFile(path); err == nil {
		return string(raw)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		h.t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "kube-apiserver"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"lotest-control-plane", "kubernetes", "localhost"},
		IPAddresses:  []net.IP{net.ParseIP("10.96.0.1"), net.ParseIP(ip), net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		h.t.Fatal(err)
	}
	out := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	testutil.WriteFile(h.t, path, out)
	return out
}

// writePair puts a matching pair for ip at dir: the certificate names ip,
// the key file says "key <ip>".
func (h *healFixture) writePair(dir, ip string) {
	h.t.Helper()
	testutil.WriteFile(h.t, filepath.Join(dir, "apiserver.crt"), h.certPEM(ip))
	testutil.WriteFile(h.t, filepath.Join(dir, "apiserver.key"), "key "+ip+"\n")
}

// makeControlPlane gives node the files that only a kind control-plane node
// has: the cluster CA and the apiserver serving pair for the stale address.
// Also a kubeadm config that still names the stale address, like every other
// file the entrypoint rewrote.
func (h *healFixture) makeControlPlane(node string) {
	h.t.Helper()
	testutil.WriteFile(h.t, filepath.Join(h.pkiPath(node), "ca.crt"), "ca-cert\n")
	testutil.WriteFile(h.t, filepath.Join(h.pkiPath(node), "ca.key"), "ca-key\n")
	h.writePair(h.pkiPath(node), "172.31.0.2")
	testutil.WriteFile(h.t, h.kubeadmConfPath(node), "advertiseAddress: 172.31.0.2\n")
}

// markRepaired gives node the state the sed leaves: the flag and the kubeadm
// config on the cluster-network address. /kind/old-ipv4 stays as it is.
func (h *healFixture) markRepaired(node string) {
	h.t.Helper()
	testutil.WriteFile(h.t, h.flagPath(node), `KUBELET_KUBEADM_ARGS="--node-ip=10.9.0.3 --node-labels="`+"\n")
	testutil.WriteFile(h.t, h.kubeadmConfPath(node), "advertiseAddress: 10.9.0.3\n")
}

// pairAt reads the pair at dir as the addresses it names: the IP SAN that is
// not the service or loopback address, and the address in the key file.
func (h *healFixture) pairAt(dir string) (certIP, keyIP string) {
	h.t.Helper()
	if raw, err := os.ReadFile(filepath.Join(dir, "apiserver.crt")); err == nil {
		block, _ := pem.Decode(raw)
		if block == nil {
			return "not-a-pem", ""
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			h.t.Fatal(err)
		}
		for _, ip := range cert.IPAddresses {
			if s := ip.String(); s != "10.96.0.1" && s != "127.0.0.1" {
				certIP = s
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "apiserver.key")); err == nil {
		keyIP = strings.TrimSpace(strings.TrimPrefix(string(raw), "key "))
	}
	return certIP, keyIP
}

// assertPair fails unless pki/ of node holds a matching pair for ip.
func (h *healFixture) assertPair(node, ip, why string) {
	h.t.Helper()
	if c, k := h.pairAt(h.pkiPath(node)); c != ip || k != ip {
		h.t.Fatalf("%s: pki/ holds a certificate for %q and a key for %q, want both for %q\nsteps %q\n%s", why, c, k, ip, h.steps(), h.errBuf.String())
	}
}

func (h *healFixture) assertNoHealPKI(node, why string) {
	h.t.Helper()
	if _, err := os.Stat(h.healPKIPath(node)); !os.IsNotExist(err) {
		h.t.Fatalf("%s: /kind/heal-pki still exists (stat: %v)", why, err)
	}
}

// reset forgets what the previous heal run recorded.
func (h *healFixture) reset() {
	h.t.Helper()
	h.calls = nil
	h.errBuf.Reset()
	if err := os.Remove(h.stepsPath()); err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
}

func (h *healFixture) heal() {
	h.t.Helper()
	if err := h.d.healNodeIPs(h.t.Context(), "lotest", "/fake/kubeconfig", h.errBuf); err != nil {
		h.t.Fatal(err)
	}
}

// inNode redirects the node paths of an in-node script onto the fake rootfs
// of node.
func (h *healFixture) inNode(node, script string) string {
	script = strings.ReplaceAll(script, "/var/lib/kubelet/kubeadm-flags.env", h.flagPath(node))
	script = strings.ReplaceAll(script, "/etc/kubernetes", filepath.Join(h.root(node), "etc/kubernetes"))
	return strings.ReplaceAll(script, "/kind/", filepath.Join(h.root(node), "kind")+"/")
}

// runInNode runs an in-node script with a real local shell, with the output
// streams of `docker exec` (c.Stdout, c.Stderr).
func runInNode(c execx.Cmd, shell, script string) error {
	cmd := exec.Command(shell, "-c", script)
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	if cmd.Stdout == nil {
		cmd.Stdout = io.Discard
	}
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}
	return cmd.Run()
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
				// `docker exec <node> sh -c '<certificate probe>'` — the REAL
				// probe against the fake rootfs.
				if len(c.Args) > 3 && c.Args[2] == "sh" && c.Args[3] == "-c" {
					return runInNode(c, "sh", h.inNode(node, c.Args[len(c.Args)-1]))
				}
				// `docker exec <node> bash -c '<repair script>'` — run the
				// REAL script against the fake rootfs with stubbed systemctl,
				// kubeadm and crictl. The stubs append to one ordered steps
				// log. The crictl stub answers the list of the
				// controller-manager and scheduler containers, logs each stop
				// and fails on any other call.
				// The kubeadm line records what kubeadm would see: its
				// arguments, whether the old pair is still in pki/, the
				// config file it reads and /kind/old-ipv4. The stub fails on
				// any other call than kind's fix_certificate one. Then it
				// does what kubeadm's certs phase does: it keeps an
				// apiserver.crt that is in pki/ and does not read its SANs
				// (exit 0, nothing written), fails on a key without a
				// certificate, and fails when the cluster CA is not in pki/.
				// Otherwise it writes the pair for the address in the
				// config. Its stdout line ("[certs] …", after the stderr
				// lines on a failure) must reach no warning. A failure writes
				// six noise lines, a klog error, info and warning line, the
				// error and kubeadm's --v=5 hint to stderr. kubeadmStale
				// makes it write the pair for the old address and exit 0.
				// The mv stub fails like mv on the move of the old pair into
				// /kind/heal-pki when mvFails is set, and runs the real mv
				// otherwise.
				h.calls = append(h.calls, "repair:"+node)
				fail, stale := "false", "false"
				if h.kubeadmFails {
					fail = "true"
				}
				if h.kubeadmStale {
					stale = "true"
				}
				stubs := fmt.Sprintf(`systemctl() { echo kubelet-restart:%[1]s >> '%[2]s'; }
mv() {
  if [ -n '%[9]s' ] && [ "${*: -1}" = '%[10]s/' ]; then
    if [ '%[9]s' = key ]; then
      command mv -f "$2" "${*: -1}"
      echo "mv: cannot move '$3' to '${*: -1}apiserver.key': Permission denied" >&2
    else
      echo "mv: cannot move '$2' to '${*: -1}apiserver.crt': Permission denied" >&2
    fi
    return 1
  fi
  command mv "$@"
}
crictl() {
  case "$*" in
    'ps -q --name kube-controller-manager|kube-scheduler') printf 'cm1\nsched1\n' ;;
    'stop cm1'|'stop sched1') echo "crictl-stop:%[1]s $2" >> '%[2]s'; echo "$2" ;;
    *) echo "crictl stub: unexpected call: $*" >&2; return 1 ;;
  esac
}
kubeadm() {
  local pki='%[3]s' pair=absent addr
  if [ -e "${pki}/apiserver.crt" ] || [ -e "${pki}/apiserver.key" ]; then pair=present; fi
  echo "kubeadm:%[1]s $* | old-pair=${pair} | conf=$(cat "${@: -1}") | old-ipv4=$(cat '%[4]s')" >> '%[2]s'
  if [ "$*" != 'init phase certs apiserver --config %[6]s' ]; then
    echo "kubeadm stub: not the call of kind's fix_certificate: $*" >&2; return 1
  fi
  if [ -f "${pki}/apiserver.crt" ] && [ -f "${pki}/ca.crt" ]; then
    echo '[certs] Using existing apiserver certificate and key on disk'
    return 0
  fi
  if [ -f "${pki}/apiserver.key" ]; then
    echo 'error execution phase certs/apiserver: failure loading apiserver certificate' >&2; return 1
  fi
  if [ ! -f "${pki}/ca.crt" ] || [ ! -f "${pki}/ca.key" ]; then
    echo "kubeadm stub: the cluster CA is not in ${pki}" >&2; return 1
  fi
  if %[5]s; then
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
  if %[8]s; then addr=172.31.0.2; fi
  cp '%[7]s'/"${addr}.crt" "${pki}/apiserver.crt" && echo "key ${addr}" > "${pki}/apiserver.key"
}
`, node, h.stepsPath(), h.pkiPath(node), h.oldIPPath(node), fail, h.kubeadmConfPath(node), h.certsDir(),
					stale, h.mvFails, h.healPKIPath(node))
				return runInNode(c, "bash", stubs+h.inNode(node, c.Args[len(c.Args)-1]))
			}
			return nil
		case "kubectl":
			if strings.Contains(joined, "/readyz") {
				// Only the exact call counts: the kubeconfig of the cluster
				// (without it kubectl asks the cluster that $KUBECONFIG
				// names) and a request timeout.
				if joined != "--kubeconfig /fake/kubeconfig --request-timeout=5s get --raw /readyz" {
					h.t.Errorf("unexpected /readyz call: kubectl %s", joined)
					h.calls = append(h.calls, "readyz-unexpected")
					return errors.New("exit status 1")
				}
				h.calls = append(h.calls, "readyz")
				if len(h.readyz) > 0 {
					ready := h.readyz[0]
					h.readyz = h.readyz[1:]
					if !ready {
						return errors.New("exit status 1")
					}
				}
				return nil
			}
			if strings.Contains(joined, "get node") {
				for node, ip := range h.observed {
					if strings.Contains(joined, " "+node+" ") || strings.HasSuffix(joined, " "+node) ||
						strings.Contains(joined, "node "+node) {
						writeOut(c, ip)
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
	// The kubeadm stub copies the certificates for both addresses.
	h.certPEM("172.31.0.2")
	h.certPEM("10.9.0.3")
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

// kubeadmSteps returns the kubeadm lines of the steps log.
func (h *healFixture) kubeadmSteps() []string {
	var out []string
	for _, s := range h.steps() {
		if strings.HasPrefix(s, "kubeadm:") {
			out = append(out, s)
		}
	}
	return out
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

// stops reads the container stops recorded by the in-script crictl stub.
func (h *healFixture) stops() []string {
	var out []string
	for _, s := range h.steps() {
		if strings.HasPrefix(s, "crictl-stop:") {
			out = append(out, s)
		}
	}
	return out
}

func (h *healFixture) called(marker string) bool {
	return slices.Contains(h.calls, marker)
}

func TestHealRepairsOnlyTheDriftedNode(t *testing.T) {
	h := newHealFixture(t)
	h.heal()

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
	if !slices.Contains(h.restarts(), "kubelet-restart:bad") {
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

	h.heal()
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

	h.heal()
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
	h.heal()
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

	h.heal()
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
	if h.called("cni-restart") || h.called("readyz") {
		t.Fatalf("kubectl was invoked without a kubeconfig: %q", h.calls)
	}
}

// readyzRun returns the /readyz checks and the CNI nudges of h.calls in
// order.
func (h *healFixture) readyzRun() (seq []string) {
	for _, c := range h.calls {
		if c == "readyz" || c == "cni-restart" {
			seq = append(seq, c)
		}
	}
	return seq
}

// recordSleeps makes the driver record each wait instead of sleeping.
func (h *healFixture) recordSleeps() *[]time.Duration {
	var sleeps []time.Duration
	h.d.sleep = func(ctx context.Context, d time.Duration) error {
		sleeps = append(sleeps, d)
		return ctx.Err()
	}
	return &sleeps
}

func TestHealWaitsForASteadyAPIServer(t *testing.T) {
	// After a repair, kubelet can start etcd again a few seconds after its
	// own restart, and the API server then refuses the next step of lo up.
	// The heal waits until /readyz passes five checks in a row, 2 s apart,
	// after the repair and before the CNI nudge. A good check between bad
	// ones does not count as steady.
	h := newHealFixture(t)
	sleeps := h.recordSleeps()
	h.readyz = []bool{false, false, true, false, true, true, true, true, true, false}
	h.heal()

	want := append(slices.Repeat([]string{"readyz"}, 9), "cni-restart")
	if got := h.readyzRun(); !slices.Equal(got, want) {
		t.Fatalf("the heal did not wait for five good /readyz checks in a row before the CNI nudge:\n got  %q\n want %q", got, want)
	}
	if repair, first := slices.Index(h.calls, "repair:bad"), slices.Index(h.calls, "readyz"); repair < 0 || first < repair {
		t.Fatalf("the /readyz checks did not come after the repair: %q", h.calls)
	}
	if want := slices.Repeat([]time.Duration{2 * time.Second}, 8); !slices.Equal(*sleeps, want) {
		t.Fatalf("the checks are not 2 s apart (8 waits between 9 checks): %v", *sleeps)
	}
	if strings.Contains(h.errBuf.String(), "/readyz") {
		t.Fatalf("a warning about /readyz, but the API server became steady:\n%s", h.errBuf.String())
	}
}

func TestHealWarnsWhenTheAPIServerIsNotSteady(t *testing.T) {
	// An API server that never passes five checks in a row stops the wait
	// after 60 checks with a warning. The heal stays advisory: the CNI
	// nudge still runs, and lo up goes on.
	h := newHealFixture(t)
	sleeps := h.recordSleeps()
	h.readyz = slices.Repeat([]bool{true, true, true, true, false}, 12)
	h.heal()

	want := append(slices.Repeat([]string{"readyz"}, 60), "cni-restart")
	if got := h.readyzRun(); !slices.Equal(got, want) {
		t.Fatalf("the wait did not stop after 60 checks: got %d entries %q", len(got), got)
	}
	if len(*sleeps) != 60 {
		t.Fatalf("%d waits, want 60 (one after each check)", len(*sleeps))
	}
	const msg = "[warn] lo: the API server did not pass 5 /readyz checks in a row after the node repair (60 checks, 2 s apart). If a later step fails, run lo up again.\n"
	if !strings.Contains(h.errBuf.String(), msg) {
		t.Fatalf("no warning about the API server that is not steady:\n%s", h.errBuf.String())
	}
}

func TestHealWaitsAfterAFailedRepair(t *testing.T) {
	// A failed renewal stops the script after the sed, which has already
	// changed the static pod manifests, so kubelet starts the API server
	// again. The heal waits for it, but sends no CNI nudge: no node was
	// repaired.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.kubeadmFails = true
	h.heal()

	if !strings.Contains(h.errBuf.String(), "could not repair bad: ") {
		t.Fatalf("the repair did not fail:\n%s", h.errBuf.String())
	}
	want := slices.Repeat([]string{"readyz"}, 5)
	if got := h.readyzRun(); !slices.Equal(got, want) {
		t.Fatalf("after a failed repair the heal must wait for the API server and send no CNI nudge:\n got  %q\n want %q", got, want)
	}

	// The next heal takes the renewal path (the flag agrees, the certificate
	// does not), and kubeadm fails again: the same wait.
	h.reset()
	h.heal()
	if w := h.errBuf.String(); !strings.Contains(w, "renewing the certificate") || !strings.Contains(w, "could not repair bad: ") {
		t.Fatalf("the second heal did not fail on the renewal path:\n%s", w)
	}
	if got := h.readyzRun(); !slices.Equal(got, want) {
		t.Fatalf("after a failed renewal the heal must wait for the API server and send no CNI nudge:\n got  %q\n want %q", got, want)
	}
}

func TestHealRenewsAPIServerCertOnControlPlane(t *testing.T) {
	// On a control-plane node the apiserver serving certificate names the node
	// address. After the heal, controller-manager and scheduler connect to
	// the new address, so the certificate must name it too. kind
	// renews it at boot only when /kind/old-ipv4 differs, and the heal writes
	// the new address there, so the heal must renew it itself.
	h := newHealFixture(t)
	h.makeControlPlane("bad")

	h.heal()

	steps := h.steps()
	renew := slices.IndexFunc(steps, func(s string) bool { return strings.HasPrefix(s, "kubeadm:bad ") })
	if renew < 0 {
		t.Fatalf("the apiserver certificate of the control-plane node was not renewed: it still names 172.31.0.2, so every client of https://10.9.0.3:6443 fails TLS. Steps: %q\n%s", steps, h.errBuf.String())
	}
	got := steps[renew]
	if want := "kubeadm:bad init phase certs apiserver --config " + h.kubeadmConfPath("bad") + " | "; !strings.HasPrefix(got, want) {
		t.Fatalf("the kubeadm call is not the one of kind's fix_certificate (kubeadm init phase certs apiserver --config /kind/kubeadm.conf):\n%s", got)
	}
	if !strings.Contains(got, " | old-pair=absent | ") {
		t.Fatalf("the old pair was still in pki/ when kubeadm ran: kubeadm keeps an apiserver.crt that is in pki/ and does not read its SANs, so the stale certificate stays:\n%s", got)
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
	// controller-manager and scheduler keep the server address of their
	// kubeconfig from their start. A stop before the renewal starts them
	// against the old certificate.
	if want := []string{"crictl-stop:bad cm1", "crictl-stop:bad sched1", "kubelet-restart:bad"}; !slices.Equal(steps[renew+1:], want) {
		t.Fatalf("after the renewal the script must stop the controller-manager and scheduler containers, then restart kubelet:\n got  %q\n want %q", steps[renew+1:], want)
	}
	h.assertPair("bad", "10.9.0.3", "after a good renewal")
	h.assertNoHealPKI("bad", "after a good renewal")
	if strings.Contains(h.errBuf.String(), "could not repair") || strings.Contains(h.errBuf.String(), "[certs]") {
		t.Fatalf("a good renewal wrote a failure or kubeadm's stdout:\n%s", h.errBuf.String())
	}
}

func TestHealWorkerSkipsCertRenewal(t *testing.T) {
	// A worker runs no apiserver and has no apiserver.crt. The guard keeps
	// kubeadm off it, and the rest of the repair runs as before. A node with
	// only one file of the pair gets no renewal either (kind's
	// fix_certificate needs both), and its next heal does not try again.
	for _, tc := range []struct{ name, crt, key string }{
		{name: "no pki files"},
		{name: "only apiserver.crt", crt: "172.31.0.2"},
		{name: "only apiserver.key", key: "172.31.0.2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHealFixture(t)
			if tc.crt != "" {
				testutil.WriteFile(t, filepath.Join(h.pkiPath("bad"), "apiserver.crt"), h.certPEM(tc.crt))
			}
			if tc.key != "" {
				testutil.WriteFile(t, filepath.Join(h.pkiPath("bad"), "apiserver.key"), "key "+tc.key+"\n")
			}

			h.heal()
			if s := h.kubeadmSteps(); len(s) != 0 {
				t.Fatalf("kubeadm ran on a node without the whole apiserver pair: %q", s)
			}
			if s := h.stops(); len(s) != 0 {
				t.Fatalf("containers were stopped on a node without the whole apiserver pair: %q", s)
			}
			if !slices.Contains(h.restarts(), "kubelet-restart:bad") {
				t.Fatalf("the worker repair did not finish (no kubelet restart): %q\n%s", h.steps(), h.errBuf.String())
			}
			h.assertNoHealPKI("bad", "a node without the whole pair")
			if c, k := h.pairAt(h.pkiPath("bad")); c != tc.crt || k != tc.key {
				t.Fatalf("the heal changed the pki/ files of a node without the whole pair: crt %q key %q", c, k)
			}

			h.reset()
			h.heal()
			if h.called("repair:bad") {
				t.Fatalf("the next heal ran the repair again on a node without the whole pair:\n%s", h.errBuf.String())
			}
		})
	}
}

func TestHealCertRenewalFailureKeepsTheOldPair(t *testing.T) {
	// kind's fix_certificate skips a node without apiserver.crt. So a failed
	// renewal must put the old pair back, and must leave /kind/old-ipv4 at the
	// old address, so that a node boot retries the renewal. It must also stop
	// before the kubelet restart and report the failure with kubeadm's
	// reason.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.kubeadmFails = true

	h.heal()
	h.assertPair("bad", "172.31.0.2", "a failed renewal must keep the old pair: without it the apiserver has no serving certificate, and kind's fix_certificate skips the node")
	h.assertNoHealPKI("bad", "after the old pair came back")
	if got := readFileT(t, h.oldIPPath("bad")); got != "172.31.0.2" {
		t.Fatalf("/kind/old-ipv4 = %q after a failed renewal: a node boot sees no address change and does not retry the renewal", got)
	}
	if slices.Contains(h.restarts(), "kubelet-restart:bad") {
		t.Fatal("kubelet restarted after a failed renewal: the script must stop and report")
	}
	if s := h.stops(); len(s) != 0 {
		t.Fatalf("containers were stopped after a failed renewal: they still connect with the old certificate in place: %q", s)
	}
	warning := h.errBuf.String()
	if !strings.Contains(warning, "could not repair bad: ") {
		t.Fatalf("a failed renewal was not reported:\n%s", warning)
	}
	if !strings.Contains(warning, "could not repair bad: kubeadm stub: noise line 6; E1110 16:20:03.100000    3243 certs.go:1] kubeadm stub: a klog error; error execution phase certs/apiserver: test failure. The next") {
		t.Fatalf("the warning must carry the last three lines of kubeadm's stderr without klog info and warning lines and without the --v=5 hint:\n%s", warning)
	}
	if strings.Contains(warning, "noise line 5") || strings.Contains(warning, "[certs]") {
		t.Fatalf("the warning is not bounded to the end of kubeadm's stderr, or carries its stdout:\n%s", warning)
	}
	if strings.Contains(warning, "systemctl status") || !strings.Contains(warning, "The next lo up tries again.") {
		t.Fatalf("the warning names the wrong next step:\n%s", warning)
	}
}

func TestHealCertRenewalFailThenRerun(t *testing.T) {
	// The first heal repairs the files, but kubeadm fails. The flag then
	// already names the cluster-network address, so the next heal must read
	// the certificate, find the old address and run the renewal again. A
	// third heal finds a good certificate and does nothing.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.kubeadmFails = true
	h.heal()
	h.assertPair("bad", "172.31.0.2", "after the failed renewal")

	h.kubeadmFails = false
	h.reset()
	h.heal()
	if !strings.Contains(h.errBuf.String(), "lo: bad has --node-ip=10.9.0.3, but its apiserver certificate does not name that address — renewing the certificate") {
		t.Fatalf("the second heal did not report the old certificate:\n%s", h.errBuf.String())
	}
	s := h.kubeadmSteps()
	if len(s) != 1 || !strings.Contains(s[0], " | old-pair=absent | conf=advertiseAddress: 10.9.0.3 | ") {
		t.Fatalf("the second heal did not renew the certificate the way of fix_certificate: %q\n%s", s, h.errBuf.String())
	}
	h.assertPair("bad", "10.9.0.3", "after the second heal")
	h.assertNoHealPKI("bad", "after the second heal")
	if got := readFileT(t, h.oldIPPath("bad")); got != "10.9.0.3" {
		t.Fatalf("/kind/old-ipv4 = %q after the renewal, want the cluster-network address", got)
	}
	if !slices.Contains(h.restarts(), "kubelet-restart:bad") || !h.called("cni-restart") || len(h.stops()) != 2 {
		t.Fatalf("the second heal did not finish the repair (container stops, kubelet and CNI restart): steps %q calls %q", h.steps(), h.calls)
	}

	h.reset()
	h.heal()
	if len(h.calls) != 0 || len(h.steps()) != 0 {
		t.Fatalf("the third heal acted on a node with a good certificate: calls %q steps %q", h.calls, h.steps())
	}
}

func TestHealRenewsCertLeftStaleByAnOlderHeal(t *testing.T) {
	// An older lo repaired the files, wrote old-ipv4 and restarted kubelet,
	// but did not renew the certificate. Nothing else will ever renew it.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.markRepaired("bad")
	testutil.WriteFile(t, h.oldIPPath("bad"), "10.9.0.3")

	h.heal()
	if s := h.kubeadmSteps(); len(s) != 1 {
		t.Fatalf("the heal did not renew a certificate that names the old address: %q\n%s", s, h.errBuf.String())
	}
	h.assertPair("bad", "10.9.0.3", "after the renewal")
}

func TestHealRestoresAPairStuckInHealPKI(t *testing.T) {
	// A heal stopped between the mv of the pair and the end of kubeadm (lo
	// killed, the node stopped). The files are repaired and the pair, or part
	// of it, is in /kind/heal-pki. The next heal must put the pair back and
	// renew it. When kubeadm fails, pki/ must hold a pair whose key matches
	// its certificate.
	type state struct {
		name       string
		healPKI    []string // files of the old pair in /kind/heal-pki
		pkiOld     []string // files of the old pair left in pki/
		pkiNew     []string // files of a new pair kubeadm wrote to pki/
		keptOnFail string   // the address of the pair in pki/ after a failed renewal
	}
	states := []state{
		{name: "after the mv of the certificate", healPKI: []string{"apiserver.crt"}, pkiOld: []string{"apiserver.key"}, keptOnFail: "172.31.0.2"},
		{name: "after the mv of both files", healPKI: []string{"apiserver.crt", "apiserver.key"}, keptOnFail: "172.31.0.2"},
		{name: "after kubeadm wrote the new key", healPKI: []string{"apiserver.crt", "apiserver.key"}, pkiNew: []string{"apiserver.key"}, keptOnFail: "172.31.0.2"},
		{name: "after kubeadm wrote the new pair", healPKI: []string{"apiserver.crt", "apiserver.key"}, pkiNew: []string{"apiserver.crt", "apiserver.key"}, keptOnFail: "10.9.0.3"},
	}
	for _, st := range states {
		for _, fails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, kubeadm fails=%v", st.name, fails), func(t *testing.T) {
				h := newHealFixture(t)
				h.makeControlPlane("bad")
				h.markRepaired("bad")
				// Build the pairs in scratch dirs, then place the files.
				oldDir, newDir := filepath.Join(h.rootDir, "old"), filepath.Join(h.rootDir, "new")
				h.writePair(oldDir, "172.31.0.2")
				h.writePair(newDir, "10.9.0.3")
				for _, f := range []string{"apiserver.crt", "apiserver.key"} {
					os.Remove(filepath.Join(h.pkiPath("bad"), f))
				}
				place := func(files []string, from, to string) {
					for _, f := range files {
						testutil.WriteFile(t, filepath.Join(to, f), readFileT(t, filepath.Join(from, f)))
					}
				}
				place(st.healPKI, oldDir, h.healPKIPath("bad"))
				place(st.pkiOld, oldDir, h.pkiPath("bad"))
				place(st.pkiNew, newDir, h.pkiPath("bad"))
				h.kubeadmFails = fails

				h.heal()
				if !strings.Contains(h.errBuf.String(), "lo: bad has its apiserver key pair in /kind/heal-pki from an interrupted heal — renewing the certificate") {
					t.Fatalf("the heal did not report the stuck pair:\n%s", h.errBuf.String())
				}
				if s := h.kubeadmSteps(); len(s) != 1 || !strings.Contains(s[0], " | old-pair=absent | ") {
					t.Fatalf("the heal did not renew the restored pair: %q\n%s", s, h.errBuf.String())
				}
				h.assertNoHealPKI("bad", "after the heal")
				if fails {
					h.assertPair("bad", st.keptOnFail, "a failed renewal after an interrupted heal")
					if !strings.Contains(h.errBuf.String(), "could not repair bad: ") {
						t.Fatalf("the failure was not reported:\n%s", h.errBuf.String())
					}
					return
				}
				h.assertPair("bad", "10.9.0.3", "after the renewal")
				if got := readFileT(t, h.oldIPPath("bad")); got != "10.9.0.3" {
					t.Fatalf("/kind/old-ipv4 = %q after the renewal", got)
				}
			})
		}
	}
}

func TestHealStopsWhenTheOldPairCannotMoveAside(t *testing.T) {
	// kubeadm keeps an apiserver.crt that is still in pki/ and exits 0. So a
	// mkdir or a mv of the old pair that fails must stop the script before
	// kubeadm, with the old pair in pki/, old-ipv4 at the old address and no
	// kubelet restart. Else the heal passes, the certificate stays on the old
	// address, and every later lo up renews it again.
	for _, tc := range []struct{ name, mvFails, reason string }{
		{name: "mkdir fails", reason: "could not repair bad: mkdir: "},
		{name: "mv moves nothing", mvFails: "all", reason: "could not repair bad: mv: cannot move "},
		{name: "mv moves the certificate only", mvFails: "key", reason: "could not repair bad: mv: cannot move "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHealFixture(t)
			h.makeControlPlane("bad")
			h.mvFails = tc.mvFails
			if tc.mvFails == "" {
				// A file where the directory must go: mkdir -p fails.
				testutil.WriteFile(t, h.healPKIPath("bad"), "not a directory\n")
			}

			h.heal()
			warning := h.errBuf.String()
			if !strings.Contains(warning, tc.reason) || !strings.Contains(warning, ". The next lo up tries again.") {
				t.Fatalf("the failure was not reported with its reason (want %q):\n%s", tc.reason, warning)
			}
			if s := h.kubeadmSteps(); len(s) != 0 {
				t.Fatalf("kubeadm ran although the old pair did not move aside: it keeps the old pair and exits 0: %q", s)
			}
			h.assertPair("bad", "172.31.0.2", "after a failed move of the old pair")
			if got := readFileT(t, h.oldIPPath("bad")); got != "172.31.0.2" {
				t.Fatalf("/kind/old-ipv4 = %q: the script did not stop", got)
			}
			if r, s := h.restarts(), h.stops(); len(r) != 0 || len(s) != 0 {
				t.Fatalf("the script did not stop: kubelet restarts %q, container stops %q", r, s)
			}
			if tc.mvFails == "" {
				// The script stopped at the mkdir: it did not go on to the
				// mv and its clean-up.
				if strings.Contains(warning, "mv: ") {
					t.Fatalf("the script went on after the failed mkdir:\n%s", warning)
				}
				if got := readFileT(t, h.healPKIPath("bad")); got != "not a directory\n" {
					t.Fatalf("the script went on after the failed mkdir: /kind/heal-pki = %q", got)
				}
				return
			}
			h.assertNoHealPKI("bad", "after the old pair came back")
		})
	}
}

func TestHealWarnsWhenTheRenewalDoesNotTakeEffect(t *testing.T) {
	// The script exits 0, but the certificate still does not name the node
	// address (here kubeadm writes the pair for the old address). The heal
	// reads the certificate again, so a renewal that changed nothing does not
	// pass as a repair. The address part is done (kubelet runs on the new
	// address), so the CNI nudge still runs. The next heal renews again.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.kubeadmStale = true
	h.heal()

	const msg = "[warn] lo: the certificate renewal on bad did not complete: the node has --node-ip=10.9.0.3, but its apiserver certificate does not name that address. The next lo up tries again.\n"
	if !strings.Contains(h.errBuf.String(), msg) {
		t.Fatalf("a renewal that left the old certificate was not reported:\n%s", h.errBuf.String())
	}
	if s := h.kubeadmSteps(); len(s) != 1 {
		t.Fatalf("the heal did not run the renewal: %q", s)
	}
	if !slices.Contains(h.restarts(), "kubelet-restart:bad") || !h.called("cni-restart") {
		t.Fatalf("the address repair did not finish (kubelet restart, CNI nudge): steps %q calls %q", h.steps(), h.calls)
	}

	h.kubeadmStale = false
	h.reset()
	h.heal()
	if w := h.errBuf.String(); !strings.Contains(w, "renewing the certificate") || strings.Contains(w, "did not complete") {
		t.Fatalf("the next heal did not renew the certificate:\n%s", w)
	}
	h.assertPair("bad", "10.9.0.3", "after the next heal")
}

func TestHealRenewsACertThatNamesALongerAddress(t *testing.T) {
	// The certificate names 10.9.0.30, and the node address is 10.9.0.3, the
	// start of 10.9.0.30. A check that compares the text without a bound
	// finds the node address in that SAN and keeps the wrong certificate.
	h := newHealFixture(t)
	h.makeControlPlane("bad")
	h.markRepaired("bad")
	h.writePair(h.pkiPath("bad"), "10.9.0.30")
	testutil.WriteFile(t, h.oldIPPath("bad"), "10.9.0.3")

	h.heal()
	if !strings.Contains(h.errBuf.String(), "lo: bad has --node-ip=10.9.0.3, but its apiserver certificate does not name that address — renewing the certificate") {
		t.Fatalf("a certificate for 10.9.0.30 passed as one for 10.9.0.3:\n%s", h.errBuf.String())
	}
	if s := h.kubeadmSteps(); len(s) != 1 {
		t.Fatalf("the heal did not renew the certificate: %q", s)
	}
	h.assertPair("bad", "10.9.0.3", "after the renewal")
}

// logLines splits a call log of the bash side into its lines (none for an
// empty or missing file).
func logLines(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

// TestHealScriptMatchesBash holds the two copies of the heal in lockstep. It
// runs lo::heal_node_ips of the frozen tree with kind, docker and kubectl
// stubbed, and healNodeIPs with a fake runner that answers the same. Then it
// compares the repair script that each hands to `docker exec … bash -c`, the
// certificate probes (before and after the repair), the warnings, the
// kubectl calls and the waits. Two known differences are normalised: bash
// removes the `\`-newline continuations of the double-quoted script, and the
// bash probe runs openssl where Go runs cat (deviation D43).
func TestHealScriptMatchesBash(t *testing.T) {
	root := testutil.RepoRoot(t)
	locale := bashLocale(t)
	pemOld := (&healFixture{t: t, rootDir: t.TempDir()}).certPEM("172.31.0.2")
	sansOld := "X509v3 Subject Alternative Name: \n    DNS:lotest-control-plane, DNS:kubernetes, DNS:localhost, IP Address:10.96.0.1, IP Address:172.31.0.2, IP Address:127.0.0.1"
	sansNew := strings.ReplaceAll(sansOld, "172.31.0.2", "10.9.0.3")
	pemNew := (&healFixture{t: t, rootDir: t.TempDir()}).certPEM("10.9.0.3")
	sansLonger := strings.ReplaceAll(sansOld, "172.31.0.2", "10.9.0.30")
	pemLonger := (&healFixture{t: t, rootDir: t.TempDir()}).certPEM("10.9.0.30")
	longLine := strings.Repeat("x", 450)
	for _, tc := range []struct {
		name               string
		flag, want         string
		goProbe, bashProbe string // the probe answers before the repair script
		goAfter, bashAfter string // the probe answers after it
		repairErr          string // stderr of a failing repair
		notReady           bool   // /readyz never passes
	}{
		{name: "drifted node", flag: "172.31.0.2", want: "10.9.0.3"},
		{name: "drifted node, repair fails", flag: "172.31.0.2", want: "10.9.0.3",
			repairErr: "line one\n\n   kubeadm stub: noise\nW1110 16:20:03.113013    3243 a.go:1] note\nerror execution phase certs/apiserver: x\r\n  To see the stack trace  \nI1110 16:20:03.2 1 b.go:2] info\n"},
		{name: "drifted node, klog error line", flag: "172.31.0.2", want: "10.9.0.3",
			repairErr: "a\nb\nE1110 16:20:03.1 1 c.go:3] an error\nIW1110 not klog\nW110 not klog\n"},
		{name: "drifted node, only klog notes", flag: "172.31.0.2", want: "10.9.0.3", repairErr: "W1110 1 a.go:1] x\nTo see the stack trace of this error\n"},
		{name: "drifted node, long stderr", flag: "172.31.0.2", want: "10.9.0.3", repairErr: "a\n" + longLine + "\n"},
		{name: "drifted node, no stderr", flag: "172.31.0.2", want: "10.9.0.3", repairErr: "\n"},
		{name: "drifted node, renewed", flag: "172.31.0.2", want: "10.9.0.3", goAfter: pemNew, bashAfter: sansNew},
		{name: "drifted node, renewal without effect", flag: "172.31.0.2", want: "10.9.0.3", goAfter: pemOld, bashAfter: sansOld},
		{name: "pair in heal-pki", flag: "10.9.0.3", want: "10.9.0.3", goProbe: "heal-pki", bashProbe: "heal-pki",
			goAfter: pemNew, bashAfter: sansNew},
		{name: "pair stays in heal-pki", flag: "10.9.0.3", want: "10.9.0.3", goProbe: "heal-pki", bashProbe: "heal-pki",
			goAfter: "heal-pki", bashAfter: "heal-pki"},
		{name: "old certificate", flag: "10.9.0.3", want: "10.9.0.3", goProbe: pemOld, bashProbe: sansOld,
			goAfter: pemNew, bashAfter: sansNew},
		{name: "old certificate, renewal without effect", flag: "10.9.0.3", want: "10.9.0.3", goProbe: pemOld, bashProbe: sansOld,
			goAfter: pemOld, bashAfter: sansOld},
		{name: "old certificate, repair fails", flag: "10.9.0.3", want: "10.9.0.3", goProbe: pemOld, bashProbe: sansOld,
			repairErr: "error execution phase certs/apiserver: x\n"},
		{name: "certificate for a longer address", flag: "10.9.0.3", want: "10.9.0.3", goProbe: pemLonger, bashProbe: sansLonger,
			goAfter: pemNew, bashAfter: sansNew},
		{name: "good certificate", flag: "10.9.0.3", want: "10.9.0.3", goProbe: pemNew, bashProbe: sansNew},
		{name: "worker", flag: "10.9.0.3", want: "10.9.0.3"},
		{name: "not an IP", flag: "172.31.0.2", want: "10.9.0.3'; touch /tmp/x; echo '"},
		{name: "API server not steady", flag: "172.31.0.2", want: "10.9.0.3", notReady: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The bash side.
			out := t.TempDir()
			script := `import() { :; }; warn() { printf '[warn] %s\n' "$*"; }
source "$1/.lok8s/utils/ip.sh"; source "$1/.lok8s/drivers/lo/utils/network.sh"
kind() { echo n1; }
docker() {
  case "$1 $3" in
    "inspect --format") printf '%s\n' "${WANT}" ;;
    "exec cat") printf 'KUBELET_KUBEADM_ARGS="--node-ip=%s --node-labels="\n' "${FLAG}" ;;
    "exec sh") printf '%s\n' "${@: -1}" >> "${OUT}/probes"
      if [ -e "${OUT}/script" ]; then printf '%s\n' "${PROBE_AFTER}"; else printf '%s\n' "${PROBE}"; fi ;;
    "exec bash") printf '%s' "${@: -1}" > "${OUT}/script"
      if [ -n "${REPAIR_ERR}" ]; then printf '%s' "${REPAIR_ERR}" >&2; return 1; fi ;;
  esac
}
kubectl() {
  printf '%s\n' "$*" >> "${OUT}/kubectl"
  if [[ "$*" == *" get --raw /readyz" ]]; then [ -z "${NOT_READY}" ]; return; fi
  if [[ "$*" == *" get node "* ]]; then printf '%s\n' "${WANT}"; fi
}
sleep() { printf '%s\n' "$*" >> "${OUT}/sleep"; }
lo::heal_node_ips lotest /fake/kubeconfig`
			cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", root)
			cmd.Env = append(os.Environ(), "WANT="+tc.want, "FLAG="+tc.flag, "PROBE="+tc.bashProbe,
				"PROBE_AFTER="+tc.bashAfter, "REPAIR_ERR="+tc.repairErr, "OUT="+out,
				"KIND_EXPERIMENTAL_DOCKER_NETWORK=lok8s-test")
			if tc.notReady {
				cmd.Env = append(cmd.Env, "NOT_READY=1")
			} else {
				cmd.Env = append(cmd.Env, "NOT_READY=")
			}
			cmd.Env = append(cmd.Env, locale...)
			var bashErr strings.Builder
			cmd.Stderr = &bashErr
			bashOut, err := cmd.Output()
			if err != nil || bashErr.Len() != 0 {
				t.Fatalf("the bash heal failed: %v\n%s", err, bashErr.String())
			}

			// The Go side.
			d, runner, errBuf, _ := testDriver(t)
			t.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "lok8s-test")
			var goScript string
			var goProbes, goKubectl, goSleeps []string
			d.sleep = func(ctx context.Context, dur time.Duration) error {
				goSleeps = append(goSleeps, fmt.Sprint(dur.Seconds()))
				return ctx.Err()
			}
			runner.handler = func(c execx.Cmd) error {
				a := append(slices.Clone(c.Args), "", "", "")
				switch c.Name + " " + a[0] + " " + a[2] {
				case "kind get --name":
					writeOut(c, "n1\n")
				case "docker inspect --format":
					writeOut(c, tc.want+"\n")
				case "docker exec cat":
					writeOut(c, `KUBELET_KUBEADM_ARGS="--node-ip=`+tc.flag+` --node-labels="`+"\n")
				case "docker exec sh":
					goProbes = append(goProbes, c.Args[len(c.Args)-1])
					if goScript != "" {
						writeOut(c, tc.goAfter+"\n")
					} else {
						writeOut(c, tc.goProbe+"\n")
					}
				case "docker exec bash":
					goScript = c.Args[len(c.Args)-1]
					if tc.repairErr != "" {
						writeErr(c, tc.repairErr)
						return errors.New("exit status 1")
					}
				}
				if c.Name == "kubectl" {
					goKubectl = append(goKubectl, strings.Join(c.Args, " "))
					if c.Args[len(c.Args)-1] == "/readyz" {
						if tc.notReady {
							return errors.New("exit status 1")
						}
						return nil
					}
					if slices.Contains(c.Args, "get") {
						writeOut(c, tc.want+"\n")
					}
				}
				return nil
			}
			if err := d.healNodeIPs(t.Context(), "lotest", "/fake/kubeconfig", errBuf); err != nil {
				t.Fatal(err)
			}

			if got, want := string(bashOut), errBuf.String(); got != want {
				t.Errorf("the warnings differ:\n bash %q\n Go   %q", got, want)
			}
			bashScript, _ := os.ReadFile(filepath.Join(out, "script"))
			if got, want := string(bashScript), strings.ReplaceAll(goScript, "\\\n", ""); got != want {
				t.Errorf("the repair scripts differ:\n--- bash\n%s\n--- Go (continuations removed)\n%s", got, want)
			}
			bashKubectl, _ := os.ReadFile(filepath.Join(out, "kubectl"))
			if got, want := logLines(bashKubectl), goKubectl; !slices.Equal(got, want) {
				t.Errorf("the kubectl calls differ:\n bash %q\n Go   %q", got, want)
			}
			bashSleeps, _ := os.ReadFile(filepath.Join(out, "sleep"))
			if got, want := logLines(bashSleeps), goSleeps; !slices.Equal(got, want) {
				t.Errorf("the waits differ:\n bash %q\n Go   %q", got, want)
			}
			bashProbes, _ := os.ReadFile(filepath.Join(out, "probes"))
			openssl := "openssl x509 -in /etc/kubernetes/pki/apiserver.crt -noout -ext subjectAltName"
			got := logLines([]byte(strings.ReplaceAll(string(bashProbes), openssl, "cat /etc/kubernetes/pki/apiserver.crt")))
			if !slices.Equal(got, goProbes) {
				t.Errorf("the certificate probes differ:\n bash %q\n Go   %q", got, goProbes)
			}
		})
	}
}
