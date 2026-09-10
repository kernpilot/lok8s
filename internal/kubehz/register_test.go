package kubehz

// register_test.go ports tests/unit/kubehz_register_test.bats: register,
// the direct claim, the claim key, deregister and the cluster-id lookup.
// The api is an httptest TLS server; hcloud calls go to the same server.

import (
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

const registryListFixture = `{"ok":true,"data":[
  {"id":"cl-other","domain":"other.example.com","status":"Running","createdAt":"2026-03-01T00:00:00Z"},
  {"id":"cl-new","domain":"test.kubehz.dev","status":"Creating","createdAt":"2026-02-01T00:00:00Z"},
  {"id":"cl-old","domain":"test.kubehz.dev","status":"Running","createdAt":"2026-01-01T00:00:00Z"}
],"meta":{"page":1,"perPage":500,"total":3}}`

// ── register_cluster ─────────────────────────────────────

func TestRegisterClusterPostsAndPrintsFingerprint(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	h.handle("POST /api/clusters/register", 200, `{"id": "cl-001", "domain": "test.kubehz.dev", "registered": true}`)
	cfg := &Config{APIURL: h.apiURL(), Access: "registered"}
	mustOK(t, h.ctx.RegisterCluster(t.Context(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
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
	mustOK(t, h.ctx.RegisterCluster(t.Context(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
	mustContain(t, h.output(), "Supporter+")
	mustContain(t, h.output(), "Claim it in the dashboard")
}

func TestRegisterClusterRefusesPlainHTTP(t *testing.T) {
	h := newHarness(t)
	cfg := &Config{APIURL: "http://api.kubehz.dev"}
	mustOK(t, h.ctx.RegisterCluster(t.Context(), cfg, "test.kubehz.dev", loSpec(h)), h.output())
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
		mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
		mustContain(t, h.output(), "returned no cluster id")
	})
	t.Run("api unreachable", func(t *testing.T) {
		h := newHarness(t)
		delete(h.env, "KUBEHZ_TOKEN")
		h.handle("POST /api/clusters/register", 502, `bad gateway`)
		mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", loSpec(h)), h.output())
		mustContain(t, h.output(), "kubehz API request failed")
	})
	t.Run("fingerprint failure", func(t *testing.T) {
		h := newHarness(t)
		delete(h.env, "KUBEHZ_TOKEN")
		spec := h.writeSpec("test.kubehz.dev", "kind: UnknownKind\n")
		mustOK(t, h.ctx.RegisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec), h.output())
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
	mustOK(t, h.ctx.directClaim(t.Context(), &Config{}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
	mustContain(t, h.output(), "registered and claimed to your account")
	// TOKEN CONTAINMENT: the bearer never lands on a stream.
	mustNotContain(t, h.output(), "khzt_test")
}

func TestDirectClaimNonClaimedFails(t *testing.T) {
	h := newHarness(t)
	h.handle("POST /api/clusters/register", 200, `{"id":"cl-1","claimed":false}`)
	mustErr(t, h.ctx.directClaim(t.Context(), &Config{}, "test.kubehz.dev", loSpec(h), h.apiURL()))
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
		mustOK(t, h.ctx.directClaim(t.Context(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
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
		mustOK(t, h.ctx.directClaim(t.Context(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "READ-ONLY")
	})
	t.Run("read-only JSON false is masked by jq's // (bash quirk, preserved)", func(t *testing.T) {
		h := newHarness(t)
		h.env["KUBEHZ_TOKEN"], h.env["HCLOUD_TOKEN"] = "khzt_test", "hc_test"
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
		h.handle("POST /api/credentials", 200, `{"data":{"stored":true,"validation":{"writable":false}}}`)
		mustOK(t, h.ctx.directClaim(t.Context(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "provisioning is enabled")
	})
	t.Run("HCLOUD_TOKEN unset skips", func(t *testing.T) {
		h := newHarness(t)
		h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
		mustOK(t, h.ctx.directClaim(t.Context(), &Config{ConnectToken: "true"}, "test.kubehz.dev", loSpec(h), h.apiURL()), h.output())
		mustContain(t, h.output(), "HCLOUD_TOKEN is unset — skipping token connect")
		if h.anyReq("POST", "/api/credentials") {
			t.Fatal("no credential POST without a token")
		}
	})
}

// ── ensure_claim_key ─────────────────────────────────────

func TestEnsureClaimKeyUploadsAndReplaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["HCLOUD_TOKEN"] = "hc_test"
	h.env["HCLOUD_API_BASE"] = h.apiURL()
	h.handle("POST /api/clusters/register", 200, `{"id":"cl-5","claimKey":{"publicKey":"ssh-ed25519 AAAA k","fingerprint":"aa:bb","name":"kubehz-claim-test.kubehz.dev"}}`)
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[{"id":42}]}`)
	h.handle("DELETE /v1/ssh_keys/42", 204, ``)
	h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
	mustOK(t, h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL()), h.output())
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
	t.Parallel()
	h := newHarness(t)
	h.env["HCLOUD_API_BASE"] = "http://api.hetzner.cloud"
	mustErr(t, h.ctx.ensureClaimKey(t.Context(), "test.kubehz.dev", h.apiURL()))
	mustContain(t, h.output(), "HCLOUD_API_BASE must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request over plain HTTP")
	}
}

// ── deregister_cluster ───────────────────────────────────

func TestDeregisterResolvesOldestAndDeletesByID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["KUBEHZ_TOKEN"] = "test-token"
	h.handle("GET /api/clusters", 200, registryListFixture)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true,"data":{"deleted":true,"id":"cl-old"}}`)
	mustOK(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
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
	t.Parallel()
	h := newHarness(t)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[{"id":"cl-other","domain":"other.example.com","createdAt":"2026-03-01T00:00:00Z"}]}`)
	mustOK(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
	mustContain(t, h.output(), "no cluster is registered for test.kubehz.dev")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run without a resolved id")
	}
}

func TestDeregisterLookupFailureNeverDeletes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.handle("GET /api/clusters", 500, `{}`)
	mustErr(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "was not removed")
	if h.anyReq("DELETE", "/api/clusters") {
		t.Fatal("DELETE must not run when the lookup failed")
	}
}

func TestDeregisterRefusedDeleteReportsStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.handle("GET /api/clusters", 200, registryListFixture)
	h.handle("DELETE /api/clusters/cl-old", 404, `{"ok":false,"data":{"message":"cluster not found"}}`)
	mustErr(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "HTTP 404: cluster not found")
	mustContain(t, h.output(), "was not removed")
}

func TestDeregisterRefusesPlainHTTP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustErr(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: "http://api.kubehz.dev"}, "test.kubehz.dev", ""))
	mustContain(t, h.output(), "must use HTTPS")
	if len(h.reqs()) != 0 {
		t.Fatal("no request over plain HTTP")
	}
}

func TestDeregisterRetiresHcloudClaimKey(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = "hc_test", h.apiURL()
	h.handle("GET /api/clusters", 200, registryListFixture)
	h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
	h.handle("GET /v1/ssh_keys", 200, `{"ssh_keys":[{"id":7}]}`)
	h.handle("DELETE /v1/ssh_keys/7", 204, ``)
	mustOK(t, h.ctx.DeregisterCluster(t.Context(), &Config{APIURL: h.apiURL()}, "test.kubehz.dev", ""), h.output())
	if r := h.lastReq("GET", "/v1/ssh_keys"); r == nil || r.Query != "name=kubehz-claim-test.kubehz.dev" {
		t.Fatalf("key lookup: %+v", r)
	}
	if !h.anyReq("DELETE", "/v1/ssh_keys/7") {
		t.Fatal("claim key not retired")
	}
}

// ── resolve_cluster_id ───────────────────────────────────

func TestResolveClusterIDPageCapWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[],"meta":{"pagination":{"total":600}}}`)
	_, err := h.ctx.ResolveClusterID(t.Context(), "test.kubehz.dev", h.apiURL())
	if !errors.Is(err, errNotRegistered) {
		t.Fatalf("err = %v", err)
	}
	mustContain(t, h.output(), "tenant has 600 clusters (first 500 checked)")
}

// keep os imported for the base64 helpers' file-free tests
var _ = os.Getenv
