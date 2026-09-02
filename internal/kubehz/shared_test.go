package kubehz

// shared_test.go ports tests/unit/kubehz_shared_test.bats.

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spaceSpec(h *harness, block string) string {
	return h.writeSpec("acme.example.org", "kind: Kubehz\nspec:\n  kubehz:\n    hosting: shared\n    apiUrl: "+h.apiURL()+"\n"+block)
}

func TestSpaceConfigDefaults(t *testing.T) {
	h := newHarness(t)
	sp, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, ""))
	mustOK(t, err, h.output())
	if sp.Slug != "acme" || sp.Name != "acme" || len(sp.Nodes) != 0 {
		t.Fatalf("%+v", sp)
	}
}

func TestSpaceConfigParseFailureNeverDefaultsSlug(t *testing.T) {
	h := newHarness(t)
	broken := filepath.Join(h.base, "broken.yaml")
	_ = os.WriteFile(broken, []byte("{{ not yaml"), 0o644)
	_, err := h.ctx.SpaceConfig("acme.example", broken)
	mustErr(t, err)
}

const spaceBlock = "    space:\n      slug: acme\n      name: Acme Prod\n      plan: shared-s\n      nodes: [worker-1, worker-2]\n"

func TestProvisionSharedCreatesWaitsMints(t *testing.T) {
	h := newHarness(t)
	spec := spaceSpec(h, spaceBlock)
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	h.handle("POST /api/spaces", 201, `{"ok":true,"data":{"id":"sp-123","slug":"acme","status":"Pending"}}`)
	h.handle("GET /api/spaces/sp-123", 200, `{"ok":true,"data":{"id":"sp-123","status":"Active"}}`)
	h.handle("POST /api/spaces/sp-123/join-token", 201, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"w","expiresAt":"2026-08-07T20:00:00Z"}}`)
	cfg := &Config{APIURL: h.apiURL(), Hosting: "shared"}
	mustOK(t, h.ctx.ProvisionShared(context.Background(), cfg, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' is Active (id: sp-123)")
	mustContain(t, h.output(), "worker-1")
	mustContain(t, h.output(), "worker-2")
	if strings.Count(h.output(), "a1b2c3.d4e5f6g7h8i9j0k1") != 2 {
		t.Fatalf("ticket count:\n%s", h.output())
	}
	if r := h.lastReq("POST", "/api/spaces"); r.Body != `{"name":"Acme Prod","slug":"acme","planId":"shared-s"}` {
		t.Fatalf("create body: %s", r.Body)
	}
}

func TestProvisionSharedAdoptsExisting(t *testing.T) {
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-777","slug":"acme","status":"Active"}]}`)
	h.handle("GET /api/spaces/sp-777", 200, `{"ok":true,"data":{"id":"sp-777","status":"Active"}}`)
	mustOK(t, h.ctx.ProvisionShared(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' is Active (id: sp-777)")
	mustContain(t, h.output(), "No nodes declared under spec.kubehz.space.nodes")
	for _, r := range h.reqs() {
		if r.Method == "POST" {
			t.Fatal("adoption must be read-only")
		}
	}
}

func TestProvisionSharedNoShardAvailable(t *testing.T) {
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	h.handle("POST /api/spaces", 409, `{"ok":false,"data":{"code":"NO_SHARD_AVAILABLE","message":"no capacity"}}`)
	mustErr(t, h.ctx.ProvisionShared(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec))
	mustContain(t, h.output(), "no shared control plane has room")
	mustContain(t, h.output(), "hosting: self")
}

func TestProvisionSharedLostRaceAdopts(t *testing.T) {
	h := newHarness(t)
	spec := spaceSpec(h, "")
	posted := false
	h.handleFunc("GET /api/spaces", func(w http.ResponseWriter, r *http.Request) {
		if posted {
			io.WriteString(w, `{"ok":true,"data":[{"id":"sp-race","slug":"acme","status":"Active"}]}`)
		} else {
			io.WriteString(w, `{"ok":true,"data":[]}`)
		}
	})
	h.handleFunc("POST /api/spaces", func(w http.ResponseWriter, r *http.Request) {
		posted = true
		w.WriteHeader(409)
		io.WriteString(w, `{"ok":false,"data":{"code":"CONFLICT","message":"slug exists"}}`)
	})
	h.handle("GET /api/spaces/sp-race", 200, `{"ok":true,"data":{"id":"sp-race","status":"Active"}}`)
	mustOK(t, h.ctx.ProvisionShared(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' is Active (id: sp-race)")
}

func TestProvisionSharedRequiresToken(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	mustErr(t, h.ctx.ProvisionShared(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spaceSpec(h, "")))
	mustContain(t, h.output(), "KUBEHZ_TOKEN is required")
}

func TestDestroyShared(t *testing.T) {
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-9","slug":"acme","status":"Active"}]}`)
	h.handle("DELETE /api/spaces/sp-9", 200, `{"ok":true}`)
	mustOK(t, h.ctx.DestroyShared(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' removed (id: sp-9)")

	h.reset()
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	mustOK(t, h.ctx.DestroyShared(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "nothing to destroy")
}

func TestSpaceStatusRendersTable(t *testing.T) {
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-5","slug":"acme","status":"Active","planId":"shared-free"}]}`)
	h.handle("GET /api/spaces/sp-5/nodes", 200, `{"ok":true,"data":{"nodes":[{"name":"worker-1","status":"Ready","lane":"hcloud"}],"usage":{"nodes":1,"maxNodes":2}}}`)
	mustOK(t, h.ctx.SpaceStatus(context.Background(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Phase:   Active")
	mustContain(t, h.output(), "Plan:    shared-free")
	mustContain(t, h.output(), "Nodes:   1/2")
	mustContain(t, h.output(), "  worker-1  Ready  hcloud")
}

func TestSpaceAPICarriesTheBearer(t *testing.T) {
	h := newHarness(t)
	h.handleFunc("GET /api/spaces/sp-auth", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer khz_test_token" {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"no bearer"}`)
			return
		}
		io.WriteString(w, `{"ok":true,"data":{"id":"sp-auth"}}`)
	})
	cfg := &Config{APIURL: h.apiURL()}
	res, err := h.ctx.spaceAPI(context.Background(), cfg, "GET", "/api/spaces/sp-auth", nil)
	mustOK(t, err, h.output())
	if !is2xx(res.Status) {
		t.Fatalf("status %d", res.Status)
	}
	// An empty token env sends "Bearer " and the api answers 401 → non-2xx.
	h.env["KUBEHZ_TOKEN"] = ""
	res, err = h.ctx.spaceAPI(context.Background(), cfg, "GET", "/api/spaces/sp-auth", nil)
	mustOK(t, err, h.output())
	if is2xx(res.Status) {
		t.Fatal("an empty bearer must not pass")
	}
	if r := h.lastReq("GET", "/api/spaces/sp-auth"); strings.TrimSpace(r.Auth) != "Bearer" {
		t.Fatalf("empty bearer header = %q", r.Auth)
	}
}

func TestSpaceWaitActiveFailsFast(t *testing.T) {
	h := newHarness(t)
	polls := 0
	h.handleFunc("GET /api/spaces/sp-x", func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"unauthorized"}`)
	})
	mustErr(t, h.ctx.spaceWaitActive(context.Background(), &Config{APIURL: h.apiURL()}, "sp-x", 60))
	mustContain(t, h.output(), "refused the token")
	if polls != 1 {
		t.Fatalf("polled %d times (must fail fast)", polls)
	}
	h.reset()
	h.handle("GET /api/spaces/sp-y", 404, `{"error":"gone"}`)
	mustErr(t, h.ctx.spaceWaitActive(context.Background(), &Config{APIURL: h.apiURL()}, "sp-y", 60))
	mustContain(t, h.output(), "vanished")
}

func TestJoinSubcommandMintsTicket(t *testing.T) {
	h := newHarness(t)
	spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-1","slug":"acme"}]}`)
	h.handle("POST /api/spaces/sp-1/join-token", 201, `{"data":{"token":"t.t","expiresAt":"soon"}}`)
	mustOK(t, h.ctx.Join(context.Background(), "acme.example.org", "worker-9"), h.output())
	mustContain(t, h.output(), "Node 'worker-9' — join ticket (valid until soon, single use):")
	mustContain(t, h.output(), "    t.t")
	if r := h.lastReq("POST", "/join-token"); r.Body != `{"nodeName":"worker-9"}` {
		t.Fatalf("mint body: %s", r.Body)
	}
	h.reset()
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	mustErr(t, h.ctx.Join(context.Background(), "acme.example.org", "worker-9"))
	mustContain(t, h.output(), "No space 'acme' found — run 'lo provision' first")
}
