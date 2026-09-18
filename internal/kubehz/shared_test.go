package kubehz

// shared_test.go ports tests/unit/kubehz_shared_test.bats.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func spaceSpec(h *harness, block string) string {
	return h.writeSpec("acme.example.org", "kind: Kubehz\nspec:\n  kubehz:\n    hosting: shared\n    apiUrl: "+h.apiURL()+"\n"+block)
}

func TestSpaceConfigDefaults(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sp, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, ""))
	mustOK(t, err, h.output())
	if sp.Slug != "acme" || sp.Name != "acme" || len(sp.Nodes) != 0 {
		t.Fatalf("%+v", sp)
	}
	// A spec with no limits block names no number. lo holds no default of
	// its own: zero means "not set", and the field is left out of the
	// request so the api applies the platform's default.
	if sp.MaxNodes != 0 || sp.MaxNamespaces != 0 || sp.MaxObjectKiB != 0 {
		t.Fatalf("%+v", sp)
	}
}

func TestSpaceConfigReadsTheThreeNumbers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sp, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, spaceBlock))
	mustOK(t, err, h.output())
	if sp.MaxNodes != 3 || sp.MaxNamespaces != 2 || sp.MaxObjectKiB != 128 {
		t.Fatalf("%+v", sp)
	}
	if len(sp.Nodes) != 2 {
		t.Fatalf("%+v", sp.Nodes)
	}
}

// Only the SHAPE of each number is local: a whole number, 1 or more. There is
// no upper bound here, because the ceiling belongs to the account and lives on
// the platform. A value that fails the shape check is refused with the field,
// the value and the rule named.
func TestSpaceLimitsBounds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		block string
		want  string
	}{
		// Only the SHAPE is local: a whole number, 1 or more. Zero and a
		// negative number can never be a ceiling, so they die here.
		{"    space:\n      limits:\n        nodes: 0\n", "invalid spec.kubehz.space.limits.nodes: 0 (expected a whole number of 1 or more)"},
		{"    space:\n      limits:\n        nodes: -1\n", "invalid spec.kubehz.space.limits.nodes: -1 (expected a whole number of 1 or more)"},
		{"    space:\n      limits:\n        namespaces: 0\n", "invalid spec.kubehz.space.limits.namespaces: 0 (expected a whole number of 1 or more)"},
		{"    space:\n      limits:\n        objectCapKiB: 0\n", "invalid spec.kubehz.space.limits.objectCapKiB: 0 (expected a whole number of 1 or more)"},
		{"    space:\n      limits:\n        nodes: two\n", "invalid spec.kubehz.space.limits.nodes: two (expected a whole number of 1 or more)"},
		// A scalar that is not a whole number is quoted back the same way a
		// word is. A fraction and a boolean are both shapes a spec really
		// carries, and "expected a whole number" alone never says which value
		// was read. The bash twin pins the same two.
		{"    space:\n      limits:\n        nodes: 2.5\n", "invalid spec.kubehz.space.limits.nodes: 2.5 (expected a whole number of 1 or more)"},
		{"    space:\n      limits:\n        nodes: true\n", "invalid spec.kubehz.space.limits.nodes: true (expected a whole number of 1 or more)"},
		// A map has no single value to quote back.
		{"    space:\n      limits:\n        namespaces: {a: 1}\n", "invalid spec.kubehz.space.limits.namespaces: expected a whole number of 1 or more"},
	} {
		h := newHarness(t)
		_, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, tc.block))
		mustErr(t, err)
		mustContain(t, h.output(), tc.want)
	}
	// There is NO local upper bound any more. What an account may ask for
	// differs per account and extra resources are bought, so a number the
	// platform will refuse still leaves this CLI: the api answers with the
	// account's real ceiling. These three were refused locally before.
	h := newHarness(t)
	sp, err := h.ctx.SpaceConfig("acme.example.org",
		spaceSpec(h, "    space:\n      limits:\n        nodes: 6\n        namespaces: 4\n        objectCapKiB: 513\n"))
	mustOK(t, err, h.output())
	if sp.MaxNodes != 6 || sp.MaxNamespaces != 4 || sp.MaxObjectKiB != 513 {
		t.Fatalf("%+v", sp)
	}
	// The smallest shape each number may take is still accepted.
	h2 := newHarness(t)
	sp2, err := h2.ctx.SpaceConfig("acme.example.org",
		spaceSpec(h2, "    space:\n      limits:\n        nodes: 1\n        namespaces: 1\n        objectCapKiB: 1\n"))
	mustOK(t, err, h2.output())
	if sp2.MaxNodes != 1 || sp2.MaxNamespaces != 1 || sp2.MaxObjectKiB != 1 {
		t.Fatalf("%+v", sp2)
	}
}

func TestSpaceConfigRefusesARetiredPlan(t *testing.T) {
	t.Parallel()
	// ANY plan node is a plan. yq's `//` reads `false` and `""` as absent, so
	// a check built on it would let those two specs through unrefused.
	for _, block := range []string{
		"    space:\n      plan: shared-s\n",
		"    space:\n      plan: false\n",
		"    space:\n      plan: \"\"\n",
		"    space:\n      plan: 0\n",
		"    space:\n      plan:\n        id: shared-s\n",
	} {
		h := newHarness(t)
		_, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, block))
		mustErr(t, err)
		mustContain(t, h.output(), "spec.kubehz.space.plan is not valid: space plans are retired")
		mustContain(t, h.output(), "spec.kubehz.space.limits.objectCapKiB instead.")
	}
	// An explicit null is not a plan.
	h := newHarness(t)
	_, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, "    space:\n      plan: null\n"))
	mustOK(t, err, h.output())
}

// spec.kubehz.space.nodes holds the machine names and keeps that meaning. A
// list under limits.nodes is the two fields confused: the message names both.
func TestSpaceConfigRefusesANodeListUnderLimits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, err := h.ctx.SpaceConfig("acme.example.org", spaceSpec(h, "    space:\n      limits:\n        nodes: [worker-1]\n"))
	mustErr(t, err)
	mustContain(t, h.output(), "spec.kubehz.space.limits.nodes is a list: it is the node ceiling")
	mustContain(t, h.output(), "Set spec.kubehz.space.limits.nodes to a whole number of 1 or more.")
	mustContain(t, h.output(), "The machine names stay under spec.kubehz.space.nodes.")
}

// The machine-name list under spec.kubehz.space.nodes is untouched by the
// limits block: it still mints one join ticket per name.
func TestSpaceConfigKeepsTheMachineNameList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sp, err := h.ctx.SpaceConfig("acme.example.org",
		spaceSpec(h, "    space:\n      nodes: [worker-1, worker-2, worker-3]\n"))
	mustOK(t, err, h.output())
	if len(sp.Nodes) != 3 || sp.Nodes[0] != "worker-1" {
		t.Fatalf("%+v", sp.Nodes)
	}
	// No limits block: no number is set, so none is sent.
	if sp.MaxNodes != 0 || sp.MaxNamespaces != 0 || sp.MaxObjectKiB != 0 {
		t.Fatalf("%+v", sp)
	}
}

func TestSpaceConfigParseFailureNeverDefaultsSlug(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	broken := filepath.Join(h.base, "broken.yaml")
	_ = os.WriteFile(broken, []byte("{{ not yaml"), 0o644)
	_, err := h.ctx.SpaceConfig("acme.example", broken)
	mustErr(t, err)
}

const spaceBlock = "    space:\n      slug: acme\n      name: Acme Prod\n      nodes: [worker-1, worker-2]\n      limits:\n        nodes: 3\n        namespaces: 2\n        objectCapKiB: 128\n"

func TestProvisionSharedCreatesWaitsMints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, spaceBlock)
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	h.handle("POST /api/spaces", 201, `{"ok":true,"data":{"id":"sp-123","slug":"acme","status":"Pending"}}`)
	h.handle("GET /api/spaces/sp-123", 200, `{"ok":true,"data":{"id":"sp-123","status":"Active"}}`)
	h.handle("POST /api/spaces/sp-123/join-token", 201, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"w","expiresAt":"2026-08-07T20:00:00Z"}}`)
	cfg := &Config{APIURL: h.apiURL(), Hosting: "shared"}
	mustOK(t, h.ctx.ProvisionShared(t.Context(), cfg, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' is Active (id: sp-123)")
	mustContain(t, h.output(), "worker-1")
	mustContain(t, h.output(), "worker-2")
	if strings.Count(h.output(), "a1b2c3.d4e5f6g7h8i9j0k1") != 2 {
		t.Fatalf("ticket count:\n%s", h.output())
	}
	if r := h.lastReq("POST", "/api/spaces"); r.Body != `{"name":"Acme Prod","slug":"acme","maxNodes":3,"maxNamespaces":2,"maxObjectKiB":128}` {
		t.Fatalf("create body: %s", r.Body)
	}
}

// A limit the spec does not name is LEFT OUT of the create body. lo holds no
// default of its own, so the api applies the platform's — and a value the
// account may later have more of is never pinned by this CLI. The bash twin
// sends the same bytes (tests/unit/kubehz_shared_test.bats).
func TestProvisionSharedOmitsTheLimitsTheSpecLeavesOut(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		block string
		want  string
	}{
		{
			"no limits block at all",
			"    space:\n      slug: acme\n      name: Acme Prod\n",
			`{"name":"Acme Prod","slug":"acme"}`,
		},
		{
			"one number only",
			"    space:\n      slug: acme\n      name: Acme Prod\n      limits:\n        namespaces: 2\n",
			`{"name":"Acme Prod","slug":"acme","maxNamespaces":2}`,
		},
		{
			"an explicit null is not a value",
			"    space:\n      slug: acme\n      name: Acme Prod\n      limits:\n        nodes: null\n        objectCapKiB: 128\n",
			`{"name":"Acme Prod","slug":"acme","maxObjectKiB":128}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			spec := spaceSpec(h, tc.block)
			h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
			h.handle("POST /api/spaces", 201, `{"ok":true,"data":{"id":"sp-1","slug":"acme","status":"Pending"}}`)
			h.handle("GET /api/spaces/sp-1", 200, `{"ok":true,"data":{"id":"sp-1","status":"Active"}}`)
			cfg := &Config{APIURL: h.apiURL(), Hosting: "shared"}
			mustOK(t, h.ctx.ProvisionShared(t.Context(), cfg, "acme.example.org", spec), h.output())
			if r := h.lastReq("POST", "/api/spaces"); r.Body != tc.want {
				t.Fatalf("create body:\n got: %s\nwant: %s", r.Body, tc.want)
			}
		})
	}
}

func TestProvisionSharedAdoptsExisting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-777","slug":"acme","status":"Active"}]}`)
	h.handle("GET /api/spaces/sp-777", 200, `{"ok":true,"data":{"id":"sp-777","status":"Active"}}`)
	mustOK(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' is Active (id: sp-777)")
	mustContain(t, h.output(), "No nodes declared under spec.kubehz.space.nodes")
	for _, r := range h.reqs() {
		if r.Method == "POST" {
			t.Fatal("adoption must be read-only")
		}
	}
}

func TestProvisionSharedNoShardAvailable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	h.handle("POST /api/spaces", 409, `{"ok":false,"data":{"code":"NO_SHARD_AVAILABLE","message":"no capacity"}}`)
	mustErr(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec))
	mustContain(t, h.output(), "no shared control plane has room")
	mustContain(t, h.output(), "hosting: self")
}

func TestProvisionSharedLostRaceAdopts(t *testing.T) {
	t.Parallel()
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
	mustOK(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' is Active (id: sp-race)")
}

func TestProvisionSharedRequiresToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	mustErr(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spaceSpec(h, "")))
	mustContain(t, h.output(), "KUBEHZ_TOKEN is required")
}

func TestDestroyShared(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-9","slug":"acme","status":"Active"}]}`)
	h.handle("DELETE /api/spaces/sp-9", 200, `{"ok":true}`)
	mustOK(t, h.ctx.DestroyShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Space 'acme' removed (id: sp-9)")

	h.reset()
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	mustOK(t, h.ctx.DestroyShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "nothing to destroy")
}

func TestSpaceStatusRendersTable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-5","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256}]}`)
	h.handle("GET /api/spaces/sp-5/nodes", 200, `{"ok":true,"data":{"nodes":[{"name":"worker-1","status":"Ready","lane":"hcloud"}],"usage":{"nodes":1,"maxNodes":2}}}`)
	mustOK(t, h.ctx.SpaceStatus(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Phase:   Active")
	mustContain(t, h.output(), "Limits:  nodes 2, namespaces 1, object cap 256 KiB")
	mustContain(t, h.output(), "Nodes:   1/2")
	mustContain(t, h.output(), "  worker-1  Ready  hcloud")
}

func TestSpaceAPICarriesTheBearer(t *testing.T) {
	t.Parallel()
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
	res, err := h.ctx.spaceAPI(t.Context(), cfg, "GET", "/api/spaces/sp-auth", nil)
	mustOK(t, err, h.output())
	if !is2xx(res.Status) {
		t.Fatalf("status %d", res.Status)
	}
	// An empty token env sends "Bearer " and the api answers 401 → non-2xx.
	h.env["KUBEHZ_TOKEN"] = ""
	res, err = h.ctx.spaceAPI(t.Context(), cfg, "GET", "/api/spaces/sp-auth", nil)
	mustOK(t, err, h.output())
	if is2xx(res.Status) {
		t.Fatal("an empty bearer must not pass")
	}
	if r := h.lastReq("GET", "/api/spaces/sp-auth"); strings.TrimSpace(r.Auth) != "Bearer" {
		t.Fatalf("empty bearer header = %q", r.Auth)
	}
}

func TestSpaceWaitActiveFailsFast(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	polls := 0
	h.handleFunc("GET /api/spaces/sp-x", func(w http.ResponseWriter, r *http.Request) {
		polls++
		w.WriteHeader(401)
		io.WriteString(w, `{"error":"unauthorized"}`)
	})
	mustErr(t, h.ctx.spaceWaitActive(t.Context(), &Config{APIURL: h.apiURL()}, "sp-x", 60))
	mustContain(t, h.output(), "refused the token")
	if polls != 1 {
		t.Fatalf("polled %d times (must fail fast)", polls)
	}
	h.reset()
	h.handle("GET /api/spaces/sp-y", 404, `{"error":"gone"}`)
	mustErr(t, h.ctx.spaceWaitActive(t.Context(), &Config{APIURL: h.apiURL()}, "sp-y", 60))
	mustContain(t, h.output(), "vanished")
}

func TestJoinSubcommandMintsTicket(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-1","slug":"acme"}]}`)
	h.handle("POST /api/spaces/sp-1/join-token", 201, `{"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","expiresAt":"soon"}}`)
	mustOK(t, h.ctx.Join(t.Context(), "acme.example.org", "worker-9", false), h.output())
	mustContain(t, h.output(), "Node 'worker-9' — join ticket (valid until soon, single use):")
	mustContain(t, h.output(), "    a1b2c3.d4e5f6g7h8i9j0k1")
	if r := h.lastReq("POST", "/join-token"); r.Body != `{"nodeName":"worker-9"}` {
		t.Fatalf("mint body: %s", r.Body)
	}
	h.reset()
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	mustErr(t, h.ctx.Join(t.Context(), "acme.example.org", "worker-9", false))
	mustContain(t, h.output(), "No space 'acme' found — run 'lo provision' first")
}

// The api ships the join recipe with the ticket: it carries the ticket, so
// it lands in a private file under a fresh 0700 directory below TMPDIR,
// never in the project tree, nothing is executed, and the terminal does
// not repeat the ticket unless asked (--print-token). Without a script (an
// older api, or a plane without an endpoint yet) the old guide pointer
// stays and the ticket is printed — the terminal is the only channel then.
const joinTicketJSON = `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"worker-1","expiresAt":"2026-08-07T20:00:00Z","endpoint":"https://kkp1.kubermatic.kkp.example:6443","script":"#!/bin/bash\nset -euo pipefail\nTICKET='a1b2c3.d4e5f6g7h8i9j0k1'\n"}}`

const joinTicket = "a1b2c3.d4e5f6g7h8i9j0k1"

// joinScripts lists every join script written below tmp for node, oldest
// first (the mint never overwrites: one fresh directory per ticket).
func joinScripts(t *testing.T, tmp, node string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(tmp, "kubehz-join-*", "kubehz-join-"+node+".sh"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

func TestSpaceMintJoinWritesTheScriptPrivately(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tmp := filepath.Join(h.base, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	h.env["TMPDIR"] = tmp
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	paths := joinScripts(t, tmp, "worker-1")
	if len(paths) != 1 {
		t.Fatalf("scripts written: %v", paths)
	}
	path := paths[0]
	mustContain(t, h.output(), path)
	mustContain(t, h.output(), "https://kkp1.kubermatic.kkp.example:6443")
	mustContain(t, h.output(), "Read it, then copy it")
	mustContain(t, h.output(), "scp "+path)
	// The script carries the ticket; the terminal does not repeat it.
	mustNotContain(t, h.output(), joinTicket)
	mustContain(t, h.output(), "--print-token")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("script not written: %v", err)
	}
	if !strings.Contains(string(raw), "TICKET='"+joinTicket+"'") {
		t.Fatalf("script content: %q", raw)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("script mode %v, want 0600 — it carries the ticket", st.Mode().Perm())
	}
	if dst, err := os.Stat(filepath.Dir(path)); err != nil || dst.Mode().Perm() != 0o700 {
		t.Fatalf("script directory mode want 0700: %v %v", dst, err)
	}
	// The script sits in its own directory below tmp, never directly in it
	// (the old predictable <tmp>/kubehz-join-<node>.sh location).
	if filepath.Dir(path) == tmp {
		t.Fatalf("script written at the predictable location: %s", path)
	}
	if _, err := os.Stat(filepath.Join(h.base, "kubehz-join-worker-1.sh")); err == nil {
		t.Fatal("script written into the project tree")
	}
	if len(h.runner.calls) != 0 {
		t.Fatalf("something was executed: %+v", h.runner.calls)
	}

	// A re-mint gets its own directory: the first script is left as it
	// was (it expires with its ticket), the new one is owner-only again.
	h.handle("POST /api/spaces/sp-123/join-token", 201, strings.Replace(joinTicketJSON, "TICKET='"+joinTicket+"'", "TICKET='f6f6f6.remintedremintd'", 1))
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	paths = joinScripts(t, tmp, "worker-1")
	if len(paths) != 2 {
		t.Fatalf("re-mint did not write a second script: %v", paths)
	}
	first, _ := os.ReadFile(path)
	if !strings.Contains(string(first), "TICKET='"+joinTicket+"'") {
		t.Fatalf("re-mint touched the first script: %q", first)
	}
	var fresh string
	for _, p := range paths {
		if p != path {
			fresh = p
		}
	}
	raw, _ = os.ReadFile(fresh)
	if !strings.Contains(string(raw), "TICKET='f6f6f6.remintedremintd'") {
		t.Fatalf("re-minted script content: %q", raw)
	}
	if st, err := os.Stat(fresh); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("re-minted script mode want 0600: %v %v", st, err)
	}
}

// --print-token: the ticket is shown on the terminal as well.
func TestSpaceMintJoinPrintTokenEchoesTheTicket(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tmp := filepath.Join(h.base, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	h.env["TMPDIR"] = tmp
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", true), h.output())
	mustContain(t, h.output(), "    "+joinTicket)
	mustNotContain(t, h.output(), "--print-token")
	if len(joinScripts(t, tmp, "worker-1")) != 1 {
		t.Fatal("the script is written with --print-token too")
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
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	if len(joinScripts(t, tmp, "worker-1")) != 1 {
		t.Fatal("script not under os.TempDir()")
	}
}

// A shared TMPDIR can carry pre-planted names — the old predictable
// <tmp>/kubehz-join-<node>.sh as a symlink to a victim file, or as a
// foreign file. Neither is touched and neither receives the ticket: the
// script lands in a directory nobody could name in advance.
func TestSpaceMintJoinIgnoresPlantedNames(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tmp := filepath.Join(h.base, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	h.env["TMPDIR"] = tmp
	victim := filepath.Join(h.base, "victim")
	if err := os.WriteFile(victim, []byte("untouched\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(tmp, "kubehz-join-worker-1.sh")
	if err := os.Symlink(victim, planted); err != nil {
		t.Fatal(err)
	}
	// A foreign directory under the prefix too — the mint must not adopt it.
	foreign := filepath.Join(tmp, "kubehz-join-foreign")
	if err := os.MkdirAll(foreign, 0o777); err != nil {
		t.Fatal(err)
	}
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	if raw, _ := os.ReadFile(victim); string(raw) != "untouched\n" {
		t.Fatalf("ticket written through the planted symlink: %q", raw)
	}
	if st, err := os.Lstat(planted); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the planted symlink was replaced or removed: %v %v", st, err)
	}
	paths := joinScripts(t, tmp, "worker-1")
	if len(paths) != 1 || strings.HasPrefix(paths[0], foreign+string(filepath.Separator)) {
		t.Fatalf("script placement: %v", paths)
	}
}

// The mint has already happened when the write fails: the user is told the
// ticket is live and how to get a usable one, and nothing is echoed.
func TestSpaceMintJoinReportsAnUnwritableTempDir(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["TMPDIR"] = filepath.Join(h.base, "does-not-exist")
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false))
	mustContain(t, h.output(), "could not write the join script for 'worker-1'")
	mustContain(t, h.output(), "The ticket was minted before the write failed")
	mustContain(t, h.output(), "valid until 2026-08-07T20:00:00Z")
	mustContain(t, h.output(), "lo kubehz join worker-1")
	mustNotContain(t, h.output(), joinTicket)
}

// The node name becomes part of a filesystem path; the CLI validates it at
// the boundary (the same DNS-label rule the api enforces) instead of
// trusting the caller — before any directory is created.
func TestSpaceMintJoinRejectsAPathShapedNodeName(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["TMPDIR"] = h.base
	h.handle("POST /api/spaces/sp-123/join-token", 201, joinTicketJSON)
	mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "../escaped", false))
	mustContain(t, h.output(), "is not a node name the platform accepts")
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.base), "kubehz-join-escaped.sh")); err == nil {
		t.Fatal("script escaped the temp dir")
	}
	if dirs, _ := filepath.Glob(filepath.Join(h.base, "kubehz-join-*")); len(dirs) != 0 {
		t.Fatalf("a directory was created for a rejected node name: %v", dirs)
	}
}

func TestSpaceMintJoinWithoutScriptKeepsTheGuidePointer(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.handle("POST /api/spaces/sp-123/join-token", 201, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","nodeName":"worker-1","expiresAt":"2026-08-07T20:00:00Z"}}`)
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	mustContain(t, h.output(), "Spaces → Joining nodes")
	// No script: the terminal is the only channel, so the ticket is shown.
	mustContain(t, h.output(), "    "+joinTicket)
	if strings.Contains(h.output(), "Join script") {
		t.Fatalf("script block without a script:\n%s", h.output())
	}
}

// Server strings are scrubbed (terminal control characters dropped) before
// they are shown, and a ticket outside the bootstrap-token shape is
// refused rather than handed to a machine or a terminal.
func TestSpaceMintJoinScrubsServerStringsAndRefusesAnOddTicket(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tmp := filepath.Join(h.base, "tmp")
	_ = os.MkdirAll(tmp, 0o755)
	h.env["TMPDIR"] = tmp
	h.handle("POST /api/spaces/sp-123/join-token", 201, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","expiresAt":"2026\u001b[2J-08-07","endpoint":"https://kkp1\u001b[31m.example:6443","script":"#!/bin/bash\n"}}`)
	mustOK(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-1", false), h.output())
	mustNotContain(t, h.output(), "\x1b[2J")
	mustNotContain(t, h.output(), "\x1b[31m")
	mustContain(t, h.output(), "valid until 2026[2J-08-07")
	mustContain(t, h.output(), "https://kkp1[31m.example:6443")

	h.reset()
	h.handle("POST /api/spaces/sp-123/join-token", 201, `{"ok":true,"data":{"token":"evil\u001b[2Jtoken","expiresAt":"2026-08-07T20:00:00Z","script":"#!/bin/bash\n"}}`)
	mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-2", false))
	mustContain(t, h.output(), "shape this CLI does not recognise")
	mustNotContain(t, h.output(), "\x1b[2J")
	mustNotContain(t, h.output(), "evil")
	if len(joinScripts(t, tmp, "worker-2")) != 0 {
		t.Fatal("a script was written for a refused ticket")
	}

	// A non-2xx envelope's message and help are scrubbed on the way out.
	h.reset()
	h.handle("POST /api/spaces/sp-123/join-token", 422, `{"ok":false,"message":"bad\u001b[2Jnode","help":"try\u0007again"}`)
	mustErr(t, h.ctx.spaceMintJoin(t.Context(), &Config{APIURL: h.apiURL()}, "sp-123", "worker-3", false))
	mustNotContain(t, h.output(), "\x1b[2J")
	mustNotContain(t, h.output(), "\x07")
	mustContain(t, h.output(), "bad[2Jnode")
	mustContain(t, h.output(), "tryagain")
}

// The api's limit refusals reach the terminal as an instruction, not as a
// code. The ACCOUNT's ceiling differs per account and extra resources are
// bought, so the api's own message is the only place the real numbers exist:
// lo prints it, adds the values it sent, and names the next step.
func TestProvisionSharedLimitRefusals(t *testing.T) {
	t.Parallel()
	// The status is the api's own: SPACE_LIMITS_ABOVE_FREE answers 403, the
	// other two answer 400. lo branches on the code, so the status only has
	// to be the real one here, never the thing that selects the message.
	for _, tc := range []struct {
		name    string
		status  int
		code    string
		message string
		help    string
		want    []string
	}{
		{"free", 403, "SPACE_LIMITS_ABOVE_FREE",
			"A free account allows 2 nodes, 1 namespace and an object cap of 256 KiB for one space: maxNodes is 3, the limit is 2",
			"Add a payment method to the account, or lower the values.", []string{
				"kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): A free account allows 2 nodes, 1 namespace and an object cap of 256 KiB for one space: maxNodes is 3, the limit is 2",
				"  Add a payment method to the account, or lower the values.",
				"Decrease the values in spec.kubehz.space.limits, or raise the ceiling",
				"of the account in the kubehz dashboard.",
			}},
		{"shared", 400, "SPACE_LIMITS_ABOVE_SHARED",
			"This account allows 5 nodes, 3 namespaces and an object cap of 512 KiB for one space: maxNodes is 9, the limit is 5",
			"Lower the values, or create a hosted control plane for a larger cluster.", []string{
				"kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): This account allows 5 nodes, 3 namespaces and an object cap of 512 KiB for one space: maxNodes is 9, the limit is 5",
				"  Lower the values, or create a hosted control plane for a larger cluster.",
				"plane: set spec.kubehz.hosting to hosted.",
			}},
		// An api that sends the code alone still gets a usable line. The
		// fallback states NO number: lo does not know the account's ceiling.
		{"free without a message", 403, "SPACE_LIMITS_ABOVE_FREE", "", "", []string{
			"kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): they are above what this account allows",
			"of the account in the kubehz dashboard.",
		}},
		{"shared without a message", 400, "SPACE_LIMITS_ABOVE_SHARED", "", "", []string{
			"kubehz refused the space limits (nodes 3, namespaces 2, object cap 128 KiB): they are above what this account may set on a shared control plane",
			"plane: set spec.kubehz.hosting to hosted.",
		}},
		{"plan retired", 400, "SPACE_PLAN_RETIRED",
			"Space plans are retired. The request sends planId, which the platform no longer accepts.",
			"Send maxNodes, maxNamespaces and maxObjectKiB instead.", []string{
				"kubehz refused the request: Space plans are retired. The request sends planId, which the platform no longer accepts.",
				"  Send maxNodes, maxNamespaces and maxObjectKiB instead.",
				"A space uses spec.kubehz.space.limits: nodes, namespaces and objectCapKiB.",
			}},
		{"plan retired without a message", 400, "SPACE_PLAN_RETIRED", "", "", []string{
			"kubehz refused the request: space plans are retired",
			"A space uses spec.kubehz.space.limits: nodes, namespaces and objectCapKiB.",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			spec := spaceSpec(h, spaceBlock)
			// tc.message and tc.help hold no quote and no backslash.
			body := `{"ok":false,"data":{"code":"` + tc.code + `","message":"` + tc.message + `","help":"` + tc.help + `"}}`
			h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
			h.handle("POST /api/spaces", tc.status, body)
			mustErr(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec))
			for _, want := range tc.want {
				mustContain(t, h.output(), want)
			}
		})
	}
}

// spaceAPIMessageFixture is the hostile refusal message both implementations
// are fed: one real newline, a raw ESC sequence, the six characters an api
// would send to slip an escape sequence through `echo -e`, a BEL, a DEL, and
// enough padding to pass the 256-character clip.
func spaceAPIMessageFixture(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSuffix(
		readFile(t, filepath.Join("testdata", "golden", name)), "\n")
}

// A refusal message is a SERVER string, and the two implementations must
// disarm it identically: scrub, then clip, and (bash only, because error()
// prints through `echo -e`) escape the backslashes last. The golden holds the
// rendered bytes; tests/unit/kubehz_shared_test.bats asserts the bash tree
// against the SAME file, so a drift on either side turns one of them red.
func TestProvisionSharedScrubsTheSharedCeilingMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, spaceBlock)
	body, err := json.Marshal(map[string]any{
		"ok": false,
		"data": map[string]any{
			"code":    "SPACE_LIMITS_ABOVE_SHARED",
			"message": spaceAPIMessageFixture(t, "space-above-shared-message.txt"),
			// The help travels the OTHER print path: a plain echo in bash,
			// which must NOT get the backslash escape error() needs.
			"help": spaceAPIMessageFixture(t, "space-above-shared-help.txt"),
		},
	})
	mustOK(t, err, "marshal")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[]}`)
	h.handle("POST /api/spaces", 400, string(body))
	mustErr(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec))

	got := h.output()
	// No control character may survive into the terminal.
	for _, bad := range []string{"\x1b", "\x07", "\x7f"} {
		mustNotContain(t, got, bad)
	}
	// The characters the api sent stay exactly as many characters, on both
	// print paths: the message reaches error(), the help a plain echo.
	mustContain(t, got, `literal:\033[2J`)
	mustContain(t, got, `literal:\t and \\ stay`)
	if want := golden(t, "space-above-shared.txt", got); got != want {
		t.Fatalf("rendered refusal differs from the golden:\n got: %q\nwant: %q", got, want)
	}
}

// Adoption is read-only, so an edited spec changes nothing server-side. Say
// so instead of letting the edit pass unreported.
func TestProvisionSharedNotesLimitsDrift(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, spaceBlock)
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-7","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256}]}`)
	h.handle("GET /api/spaces/sp-7", 200, `{"ok":true,"data":{"id":"sp-7","status":"Active"}}`)
	h.handle("POST /api/spaces/sp-7/join-token", 201, `{"ok":true,"data":{"token":"a1b2c3.d4e5f6g7h8i9j0k1","expiresAt":"soon"}}`)
	mustOK(t, h.ctx.ProvisionShared(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	mustContain(t, h.output(), "Note: this space keeps the limits it was created with.")
	mustContain(t, h.output(), "The spec asks for nodes 3, namespaces 2, object cap 128 KiB.")
	mustContain(t, h.output(), "Change the limits in the kubehz dashboard.")

	// The same numbers on both sides: no note.
	h2 := newHarness(t)
	spec2 := spaceSpec(h2, "    space:\n      limits:\n        nodes: 2\n        namespaces: 1\n        objectCapKiB: 256\n")
	h2.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-8","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256}]}`)
	h2.handle("GET /api/spaces/sp-8", 200, `{"ok":true,"data":{"id":"sp-8","status":"Active"}}`)
	mustOK(t, h2.ctx.ProvisionShared(t.Context(), &Config{APIURL: h2.apiURL()}, "acme.example.org", spec2), h2.output())
	mustNotContain(t, h2.output(), "keeps the limits it was created with")
}

// A space spec is validated where it is written: lo kubehz register reaches
// validate_config, and a value that is not a whole number of 1 or more stops
// there.
func TestValidateRefusesAMalformedSpaceLimit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "    space:\n      limits:\n        namespaces: 0\n")
	cfg, err := h.ctx.ReadConfig(spec)
	mustOK(t, err, h.output())
	mustErr(t, h.ctx.Validate(cfg, spec))
	mustContain(t, h.output(), "invalid spec.kubehz.space.limits.namespaces: 0 (expected a whole number of 1 or more)")
}

// The same door lets a number the PLATFORM will refuse through: the ceiling
// is the account's, and only the api knows it.
func TestValidateAcceptsANumberAboveThePlatformCeiling(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "    space:\n      limits:\n        namespaces: 4\n        nodes: 6\n        objectCapKiB: 513\n")
	cfg, err := h.ctx.ReadConfig(spec)
	mustOK(t, err, h.output())
	mustOK(t, h.ctx.Validate(cfg, spec), h.output())
}
