package kubehz

// maintenance_test.go ports tests/unit/kubehz_node_maintenance_test.bats:
// `lo kubehz node cordon|uncordon|drain` on the nodes of a Space.

import (
	"strings"
	"testing"
)

const maintSpacesBody = `{"ok":true,"data":[{"id":"sp-5","slug":"acme","status":"Active"}]}`

// maintHarness is a Space domain (hosting: shared) whose space list holds
// the slug acme with the id sp-5.
func maintHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, maintSpacesBody)
	return h
}

// maintRun runs one verb against acme.example.org.
func maintRun(h *harness, action string, o MaintenanceOpts) error {
	ctx := h.t.Context()
	switch action {
	case "cordon":
		return h.ctx.NodeCordon(ctx, "acme.example.org", o)
	case "uncordon":
		return h.ctx.NodeUncordon(ctx, "acme.example.org", o)
	default:
		return h.ctx.NodeDrain(ctx, "acme.example.org", o)
	}
}

// ── preflight ────────────────────────────────────────────

// The platform routes exist for the nodes of a Space only. A hosted or a
// self-hosted domain is refused with the kubectl hint, before any request.
func TestNodeMaintenanceRefusesOtherHosting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ hosting, want, hint string }{
		{"hosted", "'acme.example.org' is hosting: hosted. This command is for the nodes of a Space.",
			"A hosted cluster has its own API server. Use kubectl cordon, uncordon or drain there."},
		{"self", "'acme.example.org' is hosting: self. This command is for the nodes of a Space.",
			"Use kubectl cordon, uncordon or drain against your own API server."},
	} {
		for _, action := range []string{"cordon", "uncordon", "drain"} {
			h := newHarness(t)
			h.writeSpec("acme.example.org", specYAML("KubeOne", "    hosting: "+tc.hosting+"\n    access: managed\n    apiUrl: "+h.apiURL()+"\n"))
			mustErr(t, maintRun(h, action, MaintenanceOpts{Name: "worker-1"}))
			mustContain(t, h.output(), tc.want)
			mustContain(t, h.output(), tc.hint)
			if len(h.reqs()) != 0 {
				t.Fatalf("%s/%s sent %+v", tc.hosting, action, h.reqs())
			}
		}
	}
}

func TestNodeMaintenancePreflightNeedsDomainSpecHTTPSAndToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustErr(t, h.ctx.NodeCordon(t.Context(), "", MaintenanceOpts{Name: "worker-1"}))
	mustContain(t, h.output(), "No active domain. Use: lo use <domain>")

	h = newHarness(t)
	mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
	mustContain(t, h.output(), "No cluster.lok8s.yaml for domain: acme.example.org")

	// The bearer travels on this URL: never over plain HTTP.
	h = newHarness(t)
	h.writeSpec("acme.example.org", "kind: Kubehz\nspec:\n  kubehz:\n    hosting: shared\n    apiUrl: http://api.example.org\n")
	mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
	mustContain(t, h.output(), "spec.kubehz.apiUrl must use HTTPS: http://api.example.org")
	if len(h.reqs()) != 0 {
		t.Fatalf("sent %+v", h.reqs())
	}

	h = maintHarness(t)
	h.env["KUBEHZ_TOKEN"] = ""
	mustErr(t, h.ctx.NodeDrain(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
	mustContain(t, h.output(), "KUBEHZ_TOKEN is required to drain a node.")
	mustContain(t, h.output(), "Mint a clusters:write API token in the dashboard (Access -> API Tokens).")
	if len(h.reqs()) != 0 {
		t.Fatalf("sent %+v", h.reqs())
	}
}

// The node name and the space id are pasted into a URL PATH: each is checked
// before any request is sent.
func TestNodeMaintenanceChecksTheNameAndTheSpaceID(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "Worker-1", "../spaces", "a_b", "-x", strings.Repeat("a", 64)} {
		h := maintHarness(t)
		mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: name}))
		mustContain(t, h.output(), "is not a node name the platform accepts.")
		if len(h.reqs()) != 0 {
			t.Fatalf("name %q sent %+v", name, h.reqs())
		}
	}
	// A typed backslash prints as typed (the bash twin doubles it for the
	// `echo -e` of its error()).
	h := maintHarness(t)
	mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: `\e[2J`}))
	mustContain(t, h.output(), `kubehz: '\e[2J' is not a node name the platform accepts.`)
	for _, id := range []string{"../sp-5", "sp-5/nodes", "sp 5", "sp-5?x=1", "sp-5%2F", `a\\b\e[31m`} {
		h := maintHarness(t)
		mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1", SpaceID: id}))
		mustContain(t, h.output(), "kubehz: '"+id+"' is not a space id.")
		mustContain(t, h.output(), "A space id looks like sp-xxxxxxxx. Read yours in the dashboard.")
		if len(h.reqs()) != 0 {
			t.Fatalf("space id %q sent %+v", id, h.reqs())
		}
	}
}

// --space-id wins over the space list: no list request (a clusters:write
// token without the read scope can still act), and the messages name the id.
func TestNodeMaintenanceSpaceIDSkipsTheLookup(t *testing.T) {
	t.Parallel()
	h := maintHarness(t)
	h.handle("POST /api/spaces/sp-explicit/nodes/worker-1/cordon", 202, `{"ok":true,"data":{"name":"worker-1","action":"cordon","status":"requested","replaced":null}}`)
	mustOK(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1", SpaceID: "sp-explicit"}), h.output())
	if h.anyReq("GET", "/api/spaces") {
		t.Fatalf("--space-id must skip the space list: %+v", h.reqs())
	}
	mustContain(t, h.output(), "kubehz: cordon of node 'worker-1' in space sp-explicit requested.")
}

func TestNodeMaintenanceSpaceLookupFailures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-9","slug":"other"}]}`)
	mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
	mustContain(t, h.output(), "kubehz: no space 'acme' found for acme.example.org.")
	mustContain(t, h.output(), "Create it with 'lo provision', or name the space with:  --space-id sp-xxxxxxxx")
	if h.anyReq("POST", "/cordon") {
		t.Fatal("no space must send no cordon")
	}

	for _, row := range []string{`{"slug":"acme"}`, `{"id":"../x","slug":"acme"}`, `{"id":"","slug":"acme"}`} {
		h = newHarness(t)
		spaceSpec(h, "")
		h.handle("GET /api/spaces", 200, `{"ok":true,"data":[`+row+`]}`)
		mustErr(t, h.ctx.NodeUncordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
		mustContain(t, h.output(), "kubehz: the space row carries no id this CLI can use.")
		if h.anyReq("POST", "/uncordon") {
			t.Fatalf("row %s sent an uncordon", row)
		}
	}

	// A refused list is the api's words.
	h = newHarness(t)
	spaceSpec(h, "")
	h.handle("GET /api/spaces", 403, `{"ok":false,"data":{"code":"TOKEN_SCOPE_MISSING","message":"This endpoint requires the 'read' scope"}}`)
	mustErr(t, h.ctx.NodeDrain(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
	mustContain(t, h.output(), "Failed to list spaces (HTTP 403): This endpoint requires the 'read' scope")
}

// ── drain --timeout ──────────────────────────────────────

func TestNodeDrainTimeoutBounds(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"59", "3601", "0", "00000", "abc", "-60", "60s", " 600", "6e2", "99999", "1٠٠"} {
		h := maintHarness(t)
		mustErr(t, h.ctx.NodeDrain(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1", Timeout: v}))
		mustContain(t, h.output(), "kubehz: --timeout takes a whole number of seconds from 60 to 3600.")
		if len(h.reqs()) != 0 {
			t.Fatalf("timeout %q sent %+v", v, h.reqs())
		}
	}
	for v, want := range map[string]int{"60": 60, "3600": 3600, "0600": 600, "0000060": 60, "900": 900} {
		h := newHarness(t)
		got, err := h.ctx.drainTimeout(v)
		mustOK(t, err, h.output())
		if got != want {
			t.Fatalf("drainTimeout(%q) = %d, want %d", v, got, want)
		}
	}
}

// The timeout is checked before the preflight: a bad value is refused even
// without a spec.
func TestNodeDrainTimeoutBeforePreflight(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustErr(t, h.ctx.NodeDrain(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1", Timeout: "10"}))
	mustContain(t, h.output(), "--timeout takes a whole number")
	mustNotContain(t, h.output(), "No cluster.lok8s.yaml")
}

// ── request ──────────────────────────────────────────────

// Cordon and uncordon send no body; drain sends only the fields it was
// given, in the api's names.
func TestNodeMaintenanceRequestPathAndBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action string
		o      MaintenanceOpts
		body   string
	}{
		{"cordon", MaintenanceOpts{Name: "worker-1"}, ""},
		{"uncordon", MaintenanceOpts{Name: "worker-1"}, ""},
		{"drain", MaintenanceOpts{Name: "worker-1"}, "{}"},
		{"drain", MaintenanceOpts{Name: "worker-1", Timeout: "0600"}, `{"timeoutSeconds":600}`},
		{"drain", MaintenanceOpts{Name: "worker-1", EvictUnmanaged: true}, `{"force":true}`},
		{"drain", MaintenanceOpts{Name: "worker-1", Timeout: "600", EvictUnmanaged: true}, `{"timeoutSeconds":600,"force":true}`},
	} {
		h := maintHarness(t)
		route := "POST /api/spaces/sp-5/nodes/worker-1/" + tc.action
		h.handle(route, 202, `{"ok":true,"data":{"name":"worker-1","action":"`+tc.action+`","status":"requested","replaced":null}}`)
		mustOK(t, maintRun(h, tc.action, tc.o), h.output())
		r := h.lastReq("POST", "/api/spaces/sp-5/nodes/worker-1/"+tc.action)
		if r == nil {
			t.Fatalf("%s: no request: %+v", tc.action, h.reqs())
		}
		if r.Body != tc.body {
			t.Fatalf("%s %+v: body = %q, want %q", tc.action, tc.o, r.Body, tc.body)
		}
		if r.Auth != "Bearer khz_test_token" {
			t.Fatalf("%s: auth = %q", tc.action, r.Auth)
		}
	}
}

// ── answer ───────────────────────────────────────────────

func TestNodeMaintenanceSuccessText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action string
		o      MaintenanceOpts
		want   string
	}{
		{"cordon", MaintenanceOpts{Name: "worker-1"}, "kubehz: cordon of node 'worker-1' in space acme requested.\n" +
			"  The scheduler puts no new pods on the node. The pods on it keep running.\n" +
			"  The change is asynchronous. Read its phase with:  lo kubehz status\n"},
		{"uncordon", MaintenanceOpts{Name: "worker-1"}, "kubehz: uncordon of node 'worker-1' in space acme requested.\n" +
			"  The scheduler can put new pods on the node again. Evicted pods do not move back.\n" +
			"  A cordon that the platform holds (a migration, a node retirement) stays.\n" +
			"  The change is asynchronous. Read its phase with:  lo kubehz status\n"},
		{"drain", MaintenanceOpts{Name: "worker-1"}, "kubehz: drain of node 'worker-1' in space acme requested.\n" +
			"  The platform cordons the node and evicts its pods.\n" +
			"  It tries until the deadline the platform sets by default.\n" +
			"  A pod that no controller recreates stays, and the drain waits as Blocked.\n" +
			"  To evict such pods too, add:  --evict-unmanaged\n" +
			"  The change is asynchronous. Read its phase with:  lo kubehz status\n"},
		{"drain", MaintenanceOpts{Name: "worker-1", Timeout: "600", EvictUnmanaged: true}, "kubehz: drain of node 'worker-1' in space acme requested.\n" +
			"  The platform cordons the node and evicts its pods.\n" +
			"  It tries for at most 600 seconds.\n" +
			"  It also evicts the pods that no controller recreates. Nothing recreates them.\n" +
			"  The change is asynchronous. Read its phase with:  lo kubehz status\n"},
	} {
		h := maintHarness(t)
		h.handle("POST /api/spaces/sp-5/nodes/worker-1/"+tc.action, 202, `{"ok":true,"data":{"replaced":null}}`)
		mustOK(t, maintRun(h, tc.action, tc.o), h.output())
		if got := h.out.String(); got != tc.want {
			t.Fatalf("%s %+v stdout:\n%s\nwant:\n%s", tc.action, tc.o, got, tc.want)
		}
		if h.errOut.Len() != 0 {
			t.Fatalf("%s: stderr %q", tc.action, h.errOut.String())
		}
	}
}

// The latest action wins on the platform; the answer names what it replaced,
// and the CLI says what that means for the node.
func TestNodeMaintenanceReplacedLine(t *testing.T) {
	t.Parallel()
	const (
		keepsDeadline = "  It replaces a pending drain of this node. The first deadline stays if the machine is the same.\n"
		cancelsDrain  = "  It cancels a pending drain. Pods that the drain did not evict stay on the node.\n"
	)
	for _, tc := range []struct {
		action, body, want string
	}{
		{"drain", `{"ok":true,"data":{"replaced":"drain"}}`, keepsDeadline},
		{"cordon", `{"ok":true,"data":{"replaced":"drain"}}`, cancelsDrain},
		{"uncordon", `{"ok":true,"data":{"replaced":"drain"}}`, cancelsDrain},
		{"uncordon", `{"ok":true,"data":{"replaced":"cordon"}}`, "  It replaces a pending cordon of this node.\n"},
		{"drain", `{"ok":true,"data":{"replaced":"uncordon"}}`, "  It replaces a pending uncordon of this node.\n"},
		// A bare (unenveloped) body reads the same.
		{"cordon", `{"replaced":"uncordon"}`, "  It replaces a pending uncordon of this node.\n"},
	} {
		h := maintHarness(t)
		h.handle("POST /api/spaces/sp-5/nodes/worker-1/"+tc.action, 202, tc.body)
		mustOK(t, maintRun(h, tc.action, MaintenanceOpts{Name: "worker-1"}), h.output())
		out := h.out.String()
		mustContain(t, out, tc.want)
		// The replaced line comes after the notes and before the last line.
		if !strings.HasSuffix(out, tc.want+"  The change is asynchronous. Read its phase with:  lo kubehz status\n") {
			t.Fatalf("%s replaced line out of place:\n%s", tc.action, out)
		}
	}
	// Nothing replaced, an unknown value, or a body that is not JSON: no line.
	for _, body := range []string{`{"ok":true,"data":{"replaced":null}}`, `{"ok":true,"data":{"replaced":"reboot"}}`, `<html>ok</html>`, ``} {
		h := maintHarness(t)
		h.handle("POST /api/spaces/sp-5/nodes/worker-1/cordon", 202, body)
		mustOK(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}), h.output())
		mustNotContain(t, h.output(), "replaces")
		mustNotContain(t, h.output(), "cancels")
		mustContain(t, h.output(), "kubehz: cordon of node 'worker-1' in space acme requested.")
	}
}

// ── refusals ─────────────────────────────────────────────

func TestNodeMaintenanceNotFound(t *testing.T) {
	t.Parallel()
	h := maintHarness(t)
	h.handle("POST /api/spaces/sp-5/nodes/worker-9/drain", 404, `{"ok":false,"data":{"code":"NOT_FOUND","message":"Node not found in this space"}}`)
	mustErr(t, h.ctx.NodeDrain(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-9"}))
	mustContain(t, h.output(), "kubehz: the platform knows no node 'worker-9' in space acme.")
	mustContain(t, h.output(), "Check the node name and the space with:  lo kubehz status")
	mustNotContain(t, h.output(), "requested.")
}

func TestNodeMaintenanceErrorCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		action string
		status int
		code   string
		want   []string
	}{
		{"cordon", 409, "NODE_NOT_JOINED", []string{
			"kubehz: node 'worker-1' has not joined the space yet.",
			"  Nothing changed. Wait until 'lo kubehz status' shows the node Ready, then try again."}},
		{"drain", 409, "NODE_MAINTENANCE_CONFLICT", []string{
			"kubehz: a maintenance request of another space holds the request name of node 'worker-1'.",
			"  Nothing changed. Contact kubehz support."}},
		{"uncordon", 503, "NODE_MAINTENANCE_UNAVAILABLE", []string{
			"kubehz: the uncordon request did not reach the control plane.",
			"  Nothing changed. Try again shortly."}},
		{"cordon", 503, "NODE_MAINTENANCE_AUDIT_UNAVAILABLE", []string{
			"kubehz: the platform cannot record the cordon request, so it did not send it.",
			"  Nothing changed. Try again shortly."}},
		{"drain", 503, "NODE_MAINTENANCE_OUTCOME_UNKNOWN", []string{
			"kubehz: the result of the drain request is unknown. The request can have reached the control plane.",
			"  Read the phase of node 'worker-1' with 'lo kubehz status' before you try again.",
			"  A drain that you send again can start a new deadline."}},
		{"cordon", 403, "TOKEN_SCOPE_MISSING", []string{
			"kubehz: KUBEHZ_TOKEN does not carry the clusters:write scope."}},
		{"drain", 403, "STEP_UP_REQUIRED", []string{
			"kubehz: the platform asks for a fresh, two-factor sign-in."}},
		{"drain", 403, "STEP_UP_MFA_REQUIRED", []string{
			"kubehz: the platform asks for a fresh, two-factor sign-in."}},
		// Any other code prints the api's words with the context.
		{"uncordon", 409, "LEASE_ENDED", []string{
			"Failed to uncordon node 'worker-1' (HTTP 409): The lease of this space ended",
			"  After the end, the api refuses every change except the delete."}},
	} {
		h := maintHarness(t)
		h.handle("POST /api/spaces/sp-5/nodes/worker-1/"+tc.action, tc.status,
			`{"ok":false,"data":{"code":"`+tc.code+`","message":"The lease of this space ended","help":"After the end, the api refuses every change except the delete."}}`)
		mustErr(t, maintRun(h, tc.action, MaintenanceOpts{Name: "worker-1"}))
		for _, w := range tc.want {
			mustContain(t, h.output(), w)
		}
		mustNotContain(t, h.output(), "requested.")
		if h.out.Len() != 0 {
			t.Fatalf("%s: a refusal goes to stderr only, stdout %q", tc.code, h.out.String())
		}
	}
}

// A hostile refusal reaches the terminal without its control characters.
func TestNodeMaintenanceScrubsTheRefusal(t *testing.T) {
	t.Parallel()
	h := maintHarness(t)
	h.handle("POST /api/spaces/sp-5/nodes/worker-1/cordon", 400,
		`{"ok":false,"data":{"code":"BAD_REQUEST","message":"Refused \u001b[2J now","help":"\u001b[HAsk support"}}`)
	mustErr(t, h.ctx.NodeCordon(t.Context(), "acme.example.org", MaintenanceOpts{Name: "worker-1"}))
	mustNotContain(t, h.output(), "\x1b")
	mustContain(t, h.output(), "Failed to cordon node 'worker-1' (HTTP 400): Refused [2J now")
	mustContain(t, h.output(), "  [HAsk support")
}

func TestRejectSpaceClusterFlag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mustErr(t, h.ctx.RejectSpaceClusterFlag())
	mustContain(t, h.output(), "kubehz: --cluster/-s names the kind cluster lo manages, not the target here.")
	mustContain(t, h.output(), "  Name the space with:  --space-id sp-xxxxxxxx")
	mustNotContain(t, h.output(), "--cluster-id")
}

// ── lo kubehz status ─────────────────────────────────────

// A Space node line shows the cordon and the last maintenance request after
// the lane. The reason is the platform's text: clipped to 256 runes, and the
// line loses its control characters.
func TestSpaceStatusShowsNodeMaintenance(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := spaceSpec(h, "")
	h.handle("GET /api/spaces", 200, maintSpacesBody)
	long := strings.Repeat("ü", 300)
	h.handle("GET /api/spaces/sp-5/nodes", 200, `{"ok":true,"data":{"nodes":[`+
		`{"name":"worker-1","status":"Ready","lane":"metal","unschedulable":true,"maintenance":{"action":"drain","phase":"Blocked","reason":"2 pods have no controller \u001b[31mand no force"}},`+
		`{"name":"worker-2","status":"Ready","lane":"hcloud","unschedulable":false,"maintenance":{"action":"uncordon","phase":"Done"}},`+
		`{"name":"worker-3","status":"NotReady","lane":"metal","unschedulable":true,"maintenance":null},`+
		`{"name":"worker-4","status":"Ready","lane":"metal","unschedulable":"true","maintenance":{"phase":7,"reason":"`+long+`"}},`+
		`{"name":"worker-5","status":"Ready","lane":"metal","maintenance":{"action":"cordon","phase":"Pending","reason":""}},`+
		`{"name":"worker-6","status":"Ready","lane":"metal","maintenance":"drain"}`+
		`],"usage":{"nodes":6,"maxNodes":6}}}`)
	mustOK(t, h.ctx.SpaceStatus(t.Context(), &Config{APIURL: h.apiURL()}, "acme.example.org", spec), h.output())
	out := h.out.String()
	for _, want := range []string{
		"  worker-1  Ready  metal  cordoned  drain Blocked: 2 pods have no controller [31mand no force\n",
		"  worker-2  Ready  hcloud  uncordon Done\n",
		"  worker-3  NotReady  metal  cordoned\n",
		"  worker-4  Ready  metal  - -: " + strings.Repeat("ü", 256) + "…\n",
		"  worker-5  Ready  metal  cordon Pending\n",
		"  worker-6  Ready  metal\n",
	} {
		mustContain(t, out, want)
	}
	mustNotContain(t, out, "\x1b")
	mustNotContain(t, out, strings.Repeat("ü", 257))
}
