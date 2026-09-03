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

// The api ships the join recipe with the ticket: it carries the ticket, so
// it lands in a private file under TMPDIR, never in the project tree, and
// nothing is executed. Without a script (an older api, or a plane without
// an endpoint yet) the old guide pointer stays.
const joinTicketJSON = `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"worker-1","expiresAt":"2026-08-07T20:00:00Z","endpoint":"https://kkp1.kubermatic.kkp.example:6443","script":"#!/bin/bash\nset -euo pipefail\nTICKET='a1b2c3.d4e5f6g7h8i9j0k1'\n"}}`

func TestSpaceMintJoinWritesTheScriptPrivately(t *testing.T) {
	h := newHarness(t)
	tmp := filepath.Join(h.base, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	h.env["TMPDIR"] = tmp
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustOK(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1"), h.output())
	path := filepath.Join(tmp, "kubehz-join-worker-1.sh")
	mustContain(t, h.output(), path)
	mustContain(t, h.output(), "https://kkp1.kubermatic.kkp.example:6443")
	mustContain(t, h.output(), "Read it, then copy it")
	mustContain(t, h.output(), "scp "+path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("script not written: %v", err)
	}
	if !strings.Contains(string(raw), "TICKET='a1b2c3.d4e5f6g7h8i9j0k1'") {
		t.Fatalf("script content: %q", raw)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("script mode %v, want 0600 — it carries the ticket", st.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(h.base, "kubehz-join-worker-1.sh")); err == nil {
		t.Fatal("script written into the project tree")
	}
	if len(h.runner.calls) != 0 {
		t.Fatalf("something was executed: %+v", h.runner.calls)
	}

	// A re-mint supersedes the file: whatever sat under the name (here a
	// world-readable leftover) is replaced by a fresh owner-only file.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	h.handle("POST /api/spaces/sp-123/join-token", 201, strings.Replace(joinTicketJSON, "TICKET='a1b2c3.d4e5f6g7h8i9j0k1'", "TICKET='f6f6f6.remintedremintd'", 1))
	mustOK(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1"), h.output())
	raw, _ = os.ReadFile(path)
	if !strings.Contains(string(raw), "TICKET='f6f6f6.remintedremintd'") {
		t.Fatalf("re-mint did not replace the script: %q", raw)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("re-minted script mode want 0600: %v %v", st, err)
	}
}

// Without TMPDIR the OS default is used — pinned through the real
// environment, which os.TempDir reads, pointed at the harness tree.
func TestSpaceMintJoinFallsBackToTheOSTempDir(t *testing.T) {
	h := newHarness(t)
	tmp := filepath.Join(h.base, "os-tmp")
	_ = os.MkdirAll(tmp, 0o755)
	delete(h.env, "TMPDIR")
	t.Setenv("TMPDIR", tmp)
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustOK(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1"), h.output())
	if _, err := os.Stat(filepath.Join(tmp, "kubehz-join-worker-1.sh")); err != nil {
		t.Fatalf("script not under os.TempDir(): %v", err)
	}
}

// The file is created exclusively after the stale name is removed: a
// symlink planted under the predictable name must never carry the ticket
// to its target.
func TestSpaceMintJoinNeverWritesThroughAPlantedSymlink(t *testing.T) {
	h := newHarness(t)
	tmp := filepath.Join(h.base, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	h.env["TMPDIR"] = tmp
	target := filepath.Join(h.base, "victim")
	if err := os.WriteFile(target, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(tmp, "kubehz-join-worker-1.sh")); err != nil {
		t.Fatal(err)
	}
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustOK(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1"), h.output())
	if raw, _ := os.ReadFile(target); string(raw) != "untouched\n" {
		t.Fatalf("ticket written through the planted symlink: %q", raw)
	}
	if st, err := os.Lstat(filepath.Join(tmp, "kubehz-join-worker-1.sh")); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("expected a fresh regular file, got %v %v", st, err)
	}
}

func TestSpaceMintJoinReportsAnUnwritableTempDir(t *testing.T) {
	h := newHarness(t)
	h.env["TMPDIR"] = filepath.Join(h.base, "does-not-exist")
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustErr(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1"))
	mustContain(t, h.output(), "could not write the join script for 'worker-1'")
}

// The node name becomes part of a filesystem path; the CLI validates it at
// the boundary (the same DNS-label rule the api enforces) instead of
// trusting the caller.
func TestSpaceMintJoinRejectsAPathShapedNodeName(t *testing.T) {
	h := newHarness(t)
	h.env["TMPDIR"] = h.base
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustErr(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "../escaped"))
	mustContain(t, h.output(), "is not a node name the platform accepts")
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.base), "kubehz-join-escaped.sh")); err == nil {
		t.Fatal("script escaped the temp dir")
	}
}

func TestSpaceMintJoinWithoutScriptKeepsTheGuidePointer(t *testing.T) {
	h := newHarness(t)
	h.handle("POST /api/spaces/sp-123/join-token", 201, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"worker-1","expiresAt":"2026-08-07T20:00:00Z"}}`)
	mustOK(t, h.ctx.spaceMintJoin(context.Background(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1"), h.output())
	mustContain(t, h.output(), "Spaces → Joining nodes")
	if strings.Contains(h.output(), "Join script") {
		t.Fatalf("script block without a script:\n%s", h.output())
	}
}
