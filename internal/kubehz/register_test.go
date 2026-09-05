package kubehz

// register_test.go ports tests/unit/kubehz_register_test.bats and
// tests/unit/kubehz_claim_test.bats (claim + re-enroll), plus the assess
// printer cases of kubehz_handover_test.bats. The api is an httptest TLS
// server; the fingerprint tools and kubectl are the fake Runner.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

const registryListFixture = `{"ok":true,"data":[
  {"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-03-01T00:00:00Z"},
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
],"meta":{"page":1,"perPage":500,"total":3}}`

func loSpec(h *harness) string {
	return h.writeSpec("test.kubehz.dev", "kind: Lo\nspec:\n  cluster:\n    domain: test.kubehz.dev\n")
}

// ── get_ssh_fingerprint ──────────────────────────────────

func TestFingerprintLoKind(t *testing.T) {
	h := newHarness(t)
	fp, err := h.ctx.SSHFingerprint(context.Background(), loSpec(h))
	mustOK(t, err, h.output())
	if fp != "lo:test.kubehz.dev" {
		t.Fatalf("fp = %q", fp)
	}
}

func TestFingerprintKubeOneReadsKeyFile(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("k.dev", "kind: KubeOne\nspec:\n  hcloud:\n    sshPublicKeyFile: "+filepath.Join(h.base, "test_key.pub")+"\n")
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if c.Name != "ssh-keygen" || !strings.Contains(argvLine(c), " -E md5") {
			t.Fatalf("ssh-keygen called without -E md5: %s", argvLine(c))
		}
		io.WriteString(c.Stdout, "256 MD5:ec:ea:8f:11:f3:c6:e8:10:c1:58:40:be:24:87:a8:04 test@host (ED25519)\n")
		return nil
	}
	fp, err := h.ctx.SSHFingerprint(context.Background(), spec)
	mustOK(t, err, h.output())
	if fp != "MD5:ec:ea:8f:11:f3:c6:e8:10:c1:58:40:be:24:87:a8:04" {
		t.Fatalf("fp = %q", fp)
	}
	if !strings.Contains(argvLine(h.runner.calls[0]), "-lf "+filepath.Join(h.base, "test_key.pub")) {
		t.Fatalf("key file not passed: %s", argvLine(h.runner.calls[0]))
	}
}

func TestFingerprintCapiQueriesHcloud(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("c.dev", "kind: Capi\nspec:\n  hcloud:\n    sshKeyName: my-key\n")
	h.runner.handler = func(c execx.Cmd, stdin string) error {
		switch c.Name {
		case "hcloud":
			io.WriteString(c.Stdout, `{"public_key": "ssh-ed25519 AAAA mock-capi-key"}`)
		case "ssh-keygen":
			if stdin != "ssh-ed25519 AAAA mock-capi-key\n" || !strings.Contains(argvLine(c), "-E md5") {
				t.Fatalf("ssh-keygen stdin=%q argv=%s", stdin, argvLine(c))
			}
			io.WriteString(c.Stdout, "256 MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99 test@host (ED25519)\n")
		}
		return nil
	}
	fp, err := h.ctx.SSHFingerprint(context.Background(), spec)
	mustOK(t, err, h.output())
	if fp != "MD5:aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99" {
		t.Fatalf("fp = %q", fp)
	}
}

func TestFingerprintUnknownKindFails(t *testing.T) {
	h := newHarness(t)
	spec := h.writeSpec("u.dev", "kind: UnknownKind\n")
	_, err := h.ctx.SSHFingerprint(context.Background(), spec)
	mustErr(t, err)
	mustContain(t, h.output(), "Cannot extract SSH fingerprint for kind=unknownkind")
}

// ── register_cluster ─────────────────────────────────────

func TestRegisterClusterPostsAndPrintsFingerprint(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	h.handle("POST /api/clusters/register", 200, `{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}`)
	cfg := &Config{APIURL: h.apiURL(), Access: "registered"}
	mustOK(t, h.ctx.RegisterCluster(context.Background(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.output(), "Claim it in the dashboard")
	mustContain(t, h.output(), "fingerprint: lo:test.kubehz.dev")
	r := h.lastReq("POST", "/api/clusters/register")
	if r == nil || r.Auth != "" || r.Body != `{"domain":"test.kubehz.dev","fingerprint":"lo:test.kubehz.dev"}` {
		t.Fatalf("register request: %+v", r)
	}
}

func TestRegisterClusterManagedNotesGate(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	h.handle("POST /api/clusters/register", 200, `{"id": "cl-001"}`)
	cfg := &Config{APIURL: h.apiURL(), Access: "managed"}
	mustOK(t, h.ctx.RegisterCluster(context.Background(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.output(), "Supporter+")
	mustContain(t, h.output(), "Claim it in the dashboard")
}

func TestRegisterClusterRefusesPlainHTTP(t *testing.T) {
	h := newHarness(t)
	cfg := &Config{APIURL: "http://api.kubehz.dev"}
	mustOK(t, h.ctx.RegisterCluster(context.Background(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.output(), "must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request may run over plain HTTP")
	}
}

func TestRegisterClusterSoftFailures(t *testing.T) {
	t.Run("empty id", func(t *testing.T) {
		h := newHarness(t)
		delete(h.env, "KUBEHZ_TOKEN")
		h.handle("POST /api/clusters/register", 200, `{"message": "something went wrong"}`)
		mustOK(t, h.ctx.RegisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
		mustContain(t, h.output(), "returned no cluster id")
	})
	t.Run("api unreachable", func(t *testing.T) {
		h := newHarness(t)
		delete(h.env, "KUBEHZ_TOKEN")
		h.handle("POST /api/clusters/register", 502, `bad gateway`)
		mustOK(t, h.ctx.RegisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
		mustContain(t, h.output(), "kubehz API request failed")
	})
	t.Run("fingerprint failure", func(t *testing.T) {
		h := newHarness(t)
		delete(h.env, "KUBEHZ_TOKEN")
		spec := h.writeSpec("test.kubehz.dev", "kind: UnknownKind\n")
		mustOK(t, h.ctx.RegisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
		mustContain(t, h.output(), "Could not extract SSH fingerprint")
		if len(h.reqs()) != 0 {
			t.Fatal("no request without a fingerprint")
		}
	})
}

// ── direct_claim ─────────────────────────────────────────

func TestDirectClaimRegistersWithBearer(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_TOKEN"] = "khzt_test"
	h.handleFunc("POST /api/clusters/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer khzt_test" {
			t.Fatalf("missing bearer")
		}
		io.WriteString(w, `{"id":"cl-777","claimed":true}`)
	})
	mustOK(t, h.ctx.directClaim(context.Background(), &Config{}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
	mustContain(t, h.output(), "registered and claimed to your account")
	// TOKEN CONTAINMENT: the bearer never lands on a stream.
	mustNotContain(t, h.output(), "khzt_test")
}

func TestDirectClaimNonClaimedFails(t *testing.T) {
	h := newHarness(t)
	h.handle("POST /api/clusters/register", 200, `{"id":"cl-1","claimed":false}`)
	mustErr(t, h.ctx.directClaim(context.Background(), &Config{}, "test.kubehz.dev", loSpec(h), h.apiURL()))
}

func TestDirectClaimConnectsHcloudToken(t *testing.T) {
	t.Run("writable", func(t *testing.T) {
		h := newHarness(t)
		h.env["KUBEHZ_TOKEN"], h.env["HCLOUD_TOKEN"] = "khzt_test", "hc_test"
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
		h.handleFunc("POST /api/credentials", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer khzt_test" {
				t.Fatal("cred missing bearer")
			}
			io.WriteString(w, `{"data":{"stored":true,"validation":{"checked":true,"authenticated":true,"writable":true}}}`)
		})
		mustOK(t, h.ctx.directClaim(context.Background(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "provisioning is enabled")
		r := h.lastReq("POST", "/api/credentials")
		if r.Body != `{"type":"hcloud_token","value":"hc_test","validate":true,"clusterId":"cl-9"}` {
			t.Fatalf("credential body: %s", r.Body)
		}
		mustNotContain(t, h.output(), "hc_test")
	})
	t.Run("read-only (string false — the bats jq stub shape)", func(t *testing.T) {
		h := newHarness(t)
		h.env["KUBEHZ_TOKEN"], h.env["HCLOUD_TOKEN"] = "khzt_test", "hc_test"
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
		h.handle("POST /api/credentials", 200, `{"data":{"stored":true,"validation":{"writable":"false"}}}`)
		mustOK(t, h.ctx.directClaim(context.Background(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "READ-ONLY")
	})
	t.Run("read-only JSON false is masked by jq's // (bash quirk, preserved)", func(t *testing.T) {
		h := newHarness(t)
		h.env["KUBEHZ_TOKEN"], h.env["HCLOUD_TOKEN"] = "khzt_test", "hc_test"
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
		h.handle("POST /api/credentials", 200, `{"data":{"stored":true,"validation":{"writable":false}}}`)
		mustOK(t, h.ctx.directClaim(context.Background(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "provisioning is enabled")
	})
	t.Run("HCLOUD_TOKEN unset skips", func(t *testing.T) {
		h := newHarness(t)
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
		mustOK(t, h.ctx.directClaim(context.Background(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "HCLOUD_TOKEN is unset — skipping token connect")
		if h.anyReq("POST", "/api/credentials") {
			t.Fatal("no credential POST without a token")
		}
	})
}

// ── ensure_claim_key ─────────────────────────────────────

func TestEnsureClaimKeyUploadsAndReplaces(t *testing.T) {
	h := newHarness(t)
	h.env["HCLOUD_TOKEN"] = "hc_test"
	h.env["HCLOUD_API_BASE"] = h.apiURL()
	h.handle("POST /api/clusters/register", 200, `{"id":"cl-5","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-test.kubehz.dev"}}`)
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[{"id":42}]}`)
	h.handle("DELETE /v1/ssh_keys/42", 204, ``)
	h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
	mustOK(t, h.ctx.ensureClaimKey(context.Background(), "test.kubehz.dev", h.apiURL()), h.output())
	mustContain(t, h.output(), "Claim key 'kubehz-claim-test.kubehz.dev' uploaded")
	mustContain(t, h.output(), "fingerprint: aa:bb")
	if !h.anyReq("DELETE", "/v1/ssh_keys/42") || !h.anyReq("POST", "/v1/ssh_keys") {
		t.Fatalf("replace-by-name sequence missing: %+v", h.reqs())
	}
	if r := h.lastReq("POST", "/v1/ssh_keys"); r.Auth != "Bearer hc_test" || r.Body != `{"name":"kubehz-claim-test.kubehz.dev","public_key":"ssh-ed25519 AAAA k"}` {
		t.Fatalf("upload: %+v", r)
	}
	if r := h.lastReq("POST", "/api/clusters/register"); r.Auth != "" || r.Body != `{"domain":"test.kubehz.dev","claimKey":true}` {
		t.Fatalf("mint: %+v", r)
	}
	mustNotContain(t, h.output(), "hc_test")
}

func TestEnsureClaimKeyRefusesPlainHTTPHcloudBase(t *testing.T) {
	h := newHarness(t)
	h.env["HCLOUD_API_BASE"] = "http://api.hetzner.cloud"
	mustErr(t, h.ctx.ensureClaimKey(context.Background(), "test.kubehz.dev", h.apiURL()))
	mustContain(t, h.output(), "HCLOUD_API_BASE must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request over plain HTTP")
	}
}

// ── deregister_cluster ───────────────────────────────────

func TestDeregisterResolvesOldestAndDeletesByID(t *testing.T) {
	h := newHarness(t)
	h.env["KUBEHZ_TOKEN"] = "test-token"
	h.handle("GET /api/clusters", 200, registryListFixture)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true,"data":{"deleted":true,"id":"cl-old"}}`)
	mustOK(t, h.ctx.DeregisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
	mustContain(t, h.output(), "removed from the platform")
	mustContain(t, h.output(), "cl-old")
	for _, r := range h.reqs() {
		if r.Method == "DELETE" && strings.Contains(r.Query, "domain=") {
			t.Fatal("query-string DELETE (route does not exist)")
		}
	}
	if r := h.lastReq("GET", "/api/clusters"); r.Query != "perPage=500" || r.Auth != "Bearer test-token" {
		t.Fatalf("list: %+v", r)
	}
}

func TestDeregisterNoRowIsIdempotent(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","createdAt":"2026-03-01T00:00:00Z"}]}`)
	mustOK(t, h.ctx.DeregisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
	mustContain(t, h.output(), "no cluster is registered for test.kubehz.dev")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run without a resolved id")
	}
}

func TestDeregisterLookupFailureNeverDeletes(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /api/clusters", 500, `{}`)
	mustErr(t, h.ctx.DeregisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "was not removed")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run when the lookup failed")
	}
}

func TestDeregisterRefusedDeleteReportsStatus(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /api/clusters", 200, registryListFixture)
	h.handle("DELETE /api/clusters/cl-old", 404, `{"ok":false,"data":{"message":"cluster not found"}}`)
	mustErr(t, h.ctx.DeregisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "HTTP 404: cluster not found")
	mustContain(t, h.output(), "was not removed")
}

func TestDeregisterRefusesPlainHTTP(t *testing.T) {
	h := newHarness(t)
	mustErr(t, h.ctx.DeregisterCluster(context.Background(), &Config{APIURL: "http://api.kubehz.dev"}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request over plain HTTP")
	}
}

func TestDeregisterRetiresHcloudClaimKey(t *testing.T) {
	h := newHarness(t)
	h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
	h.handle("GET /api/clusters", 200, registryListFixture)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[{"id":7}]}`)
	h.handle("DELETE /v1/ssh_keys/7", 204, ``)
	mustOK(t, h.ctx.DeregisterCluster(context.Background(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
	if r := h.lastReq("GET", "/v1/ssh_keys"); r == nil || r.Query != "name=kubehz-claim-test.kubehz.dev" {
		t.Fatalf("key lookup: %+v", r)
	}
	if !h.anyReq("DELETE", "/v1/ssh_keys/7") {
		t.Fatal("claim key not retired")
	}
}

// ── resolve_cluster_id ───────────────────────────────────

func TestResolveClusterIDPageCapWarning(t *testing.T) {
	h := newHarness(t)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[],"meta":{"pagination":{"total":600}}}`)
	_, err := h.ctx.ResolveClusterID(context.Background(), "test.kubehz.dev", h.apiURL())
	if err != errNotRegistered {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, h.output(), "tenant has 600 clusters (first 500 checked)")
}

// ── status subcommand ────────────────────────────────────

func TestStatusAccessNone(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("test.kubehz.dev", specYAML("KubeOne", "    access:\n"))
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "not registered (access: none)")
	mustContain(t, h.output(), "API URL: <not set>")
}

func registeredSpec(h *harness) {
	h.writeSpec("test.kubehz.dev", specYAML("KubeOne", "    access: registered\n    apiUrl: "+h.apiURL()+"\n"))
	h.env["KUBEHZ_TOKEN"] = "test-token"
}

func TestStatusReadsRowNotEnvelope(t *testing.T) {
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-08-19T10:00:00Z","connected":true,"createdAt":"2026-01-01T00:00:00Z"},
  {"id":"cl-other","domain":"other.example.com","status":"Error","createdAt":"2026-03-01T00:00:00Z"}
],"meta":{"page":1,"perPage":500,"total":3}}`)
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "Agent:   cronjob (cronjob/kubehz-heartbeat — every 5 minutes)")
	mustContain(t, h.output(), "Status:  Running (id: cl-old)")
	mustContain(t, h.output(), "Beat:    2026-08-19T10:00:00Z (connected)")
	mustNotContain(t, h.output(), "Status:  unknown")
}

func TestStatusNoRow(t *testing.T) {
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-03-01T00:00:00Z"}]}`)
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "not registered (no cluster for test.kubehz.dev")
}

func TestStatusNon2xxIsUnknown(t *testing.T) {
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 401, `{"ok":false}`)
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "unknown (HTTP 401")
}

func TestStatusStaleBeatAndNoBeat(t *testing.T) {
	h := newHarness(t)
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-01-01T00:00:00Z","connected":false}]}`)
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	// jq's `.connected // empty` erases a JSON false, so the bash prints the
	// bare beat line for a stale row (quirk preserved; see the report).
	mustContain(t, h.output(), "Beat:    2026-01-01T00:00:00Z\n")
	mustNotContain(t, h.output(), "stale")
	h.reset()
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev","status":"Running","lastHeartbeat":"2026-01-01T00:00:00Z","connected":"false"}]}`)
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "Beat:    2026-01-01T00:00:00Z (stale — outside the reporting window)")
	h.reset()
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev","status":"Pending"}]}`)
	mustOK(t, h.ctx.Status(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "Beat:    none yet — deploy the heartbeat agent")
}

func TestRegisterAndDeregisterSubcommandsRejectAccessNone(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("test.kubehz.dev", "kind: Lo\nspec:\n  kubehz:\n    access:\n")
	mustErr(t, h.ctx.Register(context.Background(), "test.kubehz.dev"))
	mustContain(t, h.output(), "access is 'none'")
	h.reset()
	mustErr(t, h.ctx.Deregister(context.Background(), "test.kubehz.dev"))
	mustContain(t, h.output(), "access is 'none'")
}

// ── claim (mode-3 nonce) ─────────────────────────────────

func kubectlStub(h *harness, cmMissing, secretMissing bool) {
	h.runner.handler = func(c execx.Cmd, _ string) error {
		line := argvLine(c)
		switch {
		case strings.Contains(line, "get configmap kubehz-agent-config"):
			if cmMissing {
				return exitErr(1)
			}
		case strings.Contains(line, "get secret kubehz-agent"):
			if secretMissing {
				return exitErr(1)
			}
			if strings.Contains(line, "jsonpath") {
				io.WriteString(c.Stdout, base64.StdEncoding.EncodeToString([]byte("khz_agt_bats")))
			}
		}
		return nil
	}
}

func TestClaimRejectsMalformedNonce(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, false, false)
	mustErr(t, h.ctx.Claim(context.Background(), "khzt_wrong_prefix_value_000000"))
	mustContain(t, h.output(), "invalid claim nonce")
	mustNotContain(t, h.output(), "khzt_wrong_prefix_value_000000")
	if len(h.runner.calls) != 0 {
		t.Fatal("no kubectl call may run")
	}
	h.reset()
	mustErr(t, h.ctx.Claim(context.Background(), "khzn_short"))
	mustContain(t, h.output(), "invalid claim nonce")
}

func TestClaimPlacesNonceInOneAnnotateCall(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, false, false)
	nonce := "khzn_batsPlacedNonce_43charsBase64urlValue000"
	mustOK(t, h.ctx.Claim(context.Background(), nonce), h.output())
	mustContain(t, h.output(), "claim nonce placed")
	mustContain(t, h.output(), "15 minutes")
	mustNotContain(t, h.output(), nonce)
	if h.runner.countCalls("annotate") != 1 {
		t.Fatalf("annotate calls: %v", h.runner.lines())
	}
	var ann string
	for _, l := range h.runner.lines() {
		if strings.Contains(l, "annotate") {
			ann = l
		}
	}
	for _, want := range []string{"kubehz.cloud/claim-nonce=" + nonce, "kubehz.cloud/claim-nonce-placed=1700000000", "--overwrite"} {
		mustContain(t, ann, want)
	}
}

// ClaimNonce keeps the ticket off argv: `-` reads one line from stdin,
// an empty flag falls back to KUBEHZ_CLAIM_NONCE, and nothing supplied is
// reported as "" (the caller prints the argsh missing-flag refusal).
func TestClaimNonceFlagStdinAndEnv(t *testing.T) {
	h := newHarness(t)
	got, err := h.ctx.ClaimNonce("khzn_fromflag_000000000000000000", strings.NewReader("khzn_ignored_stdin_0000000000000\n"))
	if err != nil || got != "khzn_fromflag_000000000000000000" {
		t.Fatalf("flag: %q %v", got, err)
	}
	got, err = h.ctx.ClaimNonce("-", strings.NewReader("  khzn_fromstdin_00000000000000000  \nsecond line\n"))
	if err != nil || got != "khzn_fromstdin_00000000000000000" {
		t.Fatalf("stdin: %q %v", got, err)
	}
	got, err = h.ctx.ClaimNonce("-", strings.NewReader("khzn_noNewline_000000000000000000"))
	if err != nil || got != "khzn_noNewline_000000000000000000" {
		t.Fatalf("stdin without newline: %q %v", got, err)
	}
	h.env[ClaimNonceEnv] = "khzn_fromenv_0000000000000000000"
	got, err = h.ctx.ClaimNonce("", strings.NewReader("khzn_ignored_stdin_0000000000000\n"))
	if err != nil || got != "khzn_fromenv_0000000000000000000" {
		t.Fatalf("env: %q %v", got, err)
	}
	delete(h.env, ClaimNonceEnv)
	got, err = h.ctx.ClaimNonce("", strings.NewReader(""))
	if err != nil || got != "" {
		t.Fatalf("nothing supplied: %q %v", got, err)
	}
	// `-` with nothing on stdin is an error, and it names the cause.
	h.reset()
	_, err = h.ctx.ClaimNonce("-", strings.NewReader("\n"))
	mustErr(t, err)
	mustContain(t, h.output(), "no claim nonce on stdin")
}

func TestClaimMissingConfigMap(t *testing.T) {
	h := newHarness(t)
	kubectlStub(h, true, false)
	mustErr(t, h.ctx.Claim(context.Background(), "khzn_batsPlacedNonce_43charsBase64urlValue000"))
	mustContain(t, h.output(), "not found")
	mustContain(t, h.output(), "Deploy the heartbeat agent")
}

// ── claim-code ───────────────────────────────────────────

func TestClaimCodePrintsCNeverA(t *testing.T) {
	h := newHarness(t)
	h.runner.handler = func(c execx.Cmd, _ string) error {
		if strings.Contains(argvLine(c), "jsonpath={.data.claim-code}") {
			io.WriteString(c.Stdout, base64.StdEncoding.EncodeToString([]byte("khzc_test_code")))
		}
		return nil
	}
	mustOK(t, h.ctx.ClaimCode(context.Background()), h.output())
	if h.out.String() != "khzc_test_code\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	mustContain(t, h.errOut.String(), "Paste this one-time claim code")
	mustNotContain(t, h.output(), "khz_agt_")
}

func TestClaimCodeSecretAbsent(t *testing.T) {
	h := newHarness(t)
	h.runner.handler = func(c execx.Cmd, _ string) error { return exitErr(1) }
	mustErr(t, h.ctx.ClaimCode(context.Background()))
	mustContain(t, h.output(), "secret/kubehz-agent not found in kubehz-system")
}

// ── re-enroll ────────────────────────────────────────────

func reEnrollSpec(h *harness, apiURL string) {
	h.writeSpec("test.kubehz.dev", specYAML("KubeOne", "    access: registered\n    apiUrl: "+apiURL+"\n"))
	h.env["KUBEHZ_TOKEN"] = "khzt_bats"
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-999","domain":"other.kubehz.dev"},{"id":"cl-123","domain":"test.kubehz.dev"}]}`)
}

func TestReEnrollHashesAndReportsRotated(t *testing.T) {
	h := newHarness(t)
	reEnrollSpec(h, h.apiURL())
	kubectlStub(h, false, false)
	h.handleFunc("POST /api/clusters/cl-123/agent-token", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer khzt_bats" {
			t.Fatal("agent-token POST missing the user bearer")
		}
		io.WriteString(w, `{"rotated":true,"clusterId":"cl-123"}`)
	})
	mustOK(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "agent token re-enrolled for test.kubehz.dev (cl-123)")
	mustContain(t, h.output(), "heartbeats resume")
	mustNotContain(t, h.output(), "khz_agt_bats")
	sum := sha256.Sum256([]byte("khz_agt_bats"))
	var body map[string]string
	_ = json.Unmarshal([]byte(h.lastReq("POST", "/agent-token").Body), &body)
	if body["agentTokenHash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash = %q", body["agentTokenHash"])
	}
}

func TestReEnrollAlreadyLive(t *testing.T) {
	h := newHarness(t)
	reEnrollSpec(h, h.apiURL())
	kubectlStub(h, false, false)
	h.handle("POST /api/clusters/cl-123/agent-token", 200, `{"rotated":false,"clusterId":"cl-123"}`)
	mustOK(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "already the live one")
	mustNotContain(t, h.output(), "re-enrolled for")
}

func TestReEnrollConflict(t *testing.T) {
	h := newHarness(t)
	reEnrollSpec(h, h.apiURL())
	kubectlStub(h, false, false)
	h.handle("POST /api/clusters/cl-123/agent-token", 409, `{"statusCode":409,"data":{"code":"AGENT_TOKEN_CONFLICT","message":"That agent token is already registered"}}`)
	mustErr(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"))
	mustContain(t, h.output(), "re-enroll refused (HTTP 409 AGENT_TOKEN_CONFLICT): That agent token is already registered")
}

func TestReEnrollPreconditions(t *testing.T) {
	t.Run("requires token", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, h.apiURL())
		delete(h.env, "KUBEHZ_TOKEN")
		mustErr(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"))
		mustContain(t, h.output(), "KUBEHZ_TOKEN is required")
	})
	t.Run("missing secret", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, h.apiURL())
		kubectlStub(h, false, true)
		mustErr(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"))
		mustContain(t, h.output(), "no agent-token")
	})
	t.Run("plain http", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, "http://api.kubehz.dev")
		mustErr(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"))
		mustContain(t, h.output(), "must use HTTPS")
		if len(h.reqs()) != 0 {
			t.Fatal("no request over plain HTTP")
		}
	})
	t.Run("unresolvable id", func(t *testing.T) {
		h := newHarness(t)
		reEnrollSpec(h, h.apiURL())
		kubectlStub(h, false, false)
		h.handle("GET /api/clusters", 200, `{"data":[]}`)
		mustErr(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"))
		mustContain(t, h.output(), "no cluster found for test.kubehz.dev")
	})
	t.Run("shared and none", func(t *testing.T) {
		h := newHarness(t)
		h.writeSpec("test.kubehz.dev", specYAML("Kubehz", "    hosting: shared\n    apiUrl: https://x\n"))
		mustErr(t, h.ctx.ReEnroll(context.Background(), "test.kubehz.dev"))
		mustContain(t, h.output(), "no heartbeat agent to re-enroll")
	})
}

// ── assess ───────────────────────────────────────────────

func TestRenderAssessmentAlignedProbes(t *testing.T) {
	h := newHarness(t)
	response := `{"ok":true,"data":{"assessedAt":"2026-07-29T09:00:00Z","assessment":{"collectedAt":"2026-07-29T08:58:00Z","k8sVersion":"v1.33.2","datastore":"etcd","etcdReachable":true,"capiManaged":true,"cni":"cilium","podCidr":"10.244.0.0/16","serviceCidr":"10.96.0.0/12","storageClasses":[{"name":"hcloud-volumes","provisioner":"csi.hetzner.cloud","isDefault":true}],"pvSummary":{"count":2,"totalGi":40,"byProvisioner":{"csi.hetzner.cloud":{"count":2,"totalGi":40}}},"loadBalancers":1,"webhooks":{"validating":3,"mutating":1},"cpUsage":{"nodes":5,"cpNodes":3,"etcdDbBytes":null}},"feasibility":{"path":"restore","reasons":["datastore=etcd and a snapshot is obtainable"],"warnings":["1 provider-coupled StorageClass (csi.hetzner.cloud)"]}}}`
	mustOK(t, h.ctx.RenderAssessment("test.kubehz.dev", []byte(response)), h.output())
	for _, want := range []string{
		"kubehz assessment — test.kubehz.dev", "collected 2026-07-29T08:58:00Z", "v1.33.2", "etcd · reachable",
		"pause the CAPI controllers", "cilium", "pods 10.244.0.0/16 · services 10.96.0.0/12",
		"1 classes · 2 PVs · 40Gi", "csi.hetzner.cloud — 2 PVs · 40Gi", "3 validating · 1 mutating",
		"5 total · 3 control-plane", "Feasibility: restore", "datastore=etcd and a snapshot is obtainable",
		"provider-coupled StorageClass",
	} {
		mustContain(t, h.output(), want)
	}
	mustContain(t, h.output(), "✓ kubernetes     v1.33.2")
}

func TestRenderAssessmentNoneYet(t *testing.T) {
	h := newHarness(t)
	mustOK(t, h.ctx.RenderAssessment("test.kubehz.dev", []byte(`{"data":{"assessment":null,"assessedAt":null,"feasibility":null}}`)), h.output())
	mustContain(t, h.output(), "No assessment recorded for test.kubehz.dev yet")
	mustContain(t, h.output(), "24h")
}

func TestAssessSharedAndFetch(t *testing.T) {
	h := newHarness(t)
	h.writeSpec("sp.dev", specYAML("Kubehz", "    hosting: shared\n    apiUrl: https://x\n"))
	mustOK(t, h.ctx.Assess(context.Background(), "sp.dev"), h.output())
	mustContain(t, h.output(), "Assessment does not apply to hosting: shared")

	h.reset()
	registeredSpec(h)
	h.handle("GET /api/clusters", 200, `{"data":[{"id":"cl-1","domain":"test.kubehz.dev"}]}`)
	h.handle("GET /api/clusters/cl-1/assessment", 200, `{"data":{"assessment":null}}`)
	mustOK(t, h.ctx.Assess(context.Background(), "test.kubehz.dev"), h.output())
	mustContain(t, h.output(), "No assessment recorded")
	if r := h.lastReq("GET", "/assessment"); r.Auth != "Bearer test-token" {
		t.Fatalf("assessment auth: %+v", r)
	}
}

// keep os imported for the base64 helpers' file-free tests
var _ = os.Getenv
