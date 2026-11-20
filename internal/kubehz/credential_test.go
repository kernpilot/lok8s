package kubehz

// credential_test.go: each entry point refuses a credential with a control
// character before its first request that carries the credential, with the
// error of http::credential_ok that names the variable. The Go twin of
// tests/unit/kubehz_credential_table_test.bats.
//
// A file with CRLF line ends can leave a CR at the end of a token. The bash
// twin puts a credential on a curl config line, where a control character
// ends the line. The Go client refuses a CR in a header only as a transport
// error, and it sends a tab. A credential in a JSON body goes out with each
// control character escaped. So each row runs twice: with a CR at the end of
// the value, and with a tab in it. Without the check, the tab reaches the
// server.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// credentialValues are the values with a control character. None looks
// like a token. credentialMark is a part of each one: no request and no
// output may hold it.
var credentialValues = []string{"not-a-real-value\r", "not-a\treal-value"}

const credentialMark = "real-value"

// credentialRefusal is the refusal of http::credential_ok for name, as the
// bash twin prints it.
func credentialRefusal(name string) string {
	return "[error] environment variable " + name + " must not contain a control character\n" +
		"  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.\n"
}

// credentialCase is one credential of one entry point.
type credentialCase struct {
	name string
	// site is the function that holds the check and the variable, as
	// credentialSites prints it.
	site     string
	variable string
	// setup writes the spec and the routes. It returns the spec path ("" when
	// the call needs none). Then the variable gets the bad value.
	setup func(h *harness) string
	call  func(ctx context.Context, h *harness, spec string) error
	// wantErr: the entry point fails. Without it, the entry point skips the
	// step of the credential and succeeds, as the bash twin does.
	wantErr bool
	// allowed reports whether a request may go out. nil: no request may. A
	// request that holds the bad value is never allowed.
	allowed func(r recordedReq) bool
	// after is the stderr after the refusal ("" = nothing).
	after string
}

func noRegistryRow(h *harness) { h.handle("GET /api/clusters", 200, `{"ok":true,"data":[]}`) }

var credentialCases = []credentialCase{
	{
		name: "status", site: "statusReport KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			registeredSpec(h)
			h.handle("GET /api/clusters", 200, registryListFixture)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error { return h.ctx.Status(ctx, "test.kubehz.dev") },
	},
	{
		name: "status of a space", site: "spaceAPI KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			spaceSpec(h, "")
			h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-1","slug":"acme"}]}`)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error { return h.ctx.Status(ctx, "acme.example.org") },
	},
	{
		name: "assess", site: "Assess KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			registeredSpec(h)
			h.handle("GET /api/clusters", 200, registryListFixture)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error { return h.ctx.Assess(ctx, "test.kubehz.dev") },
	},
	{
		// Also no kubectl read: the table checks the runner for each row.
		name: "re-enroll", site: "ReEnroll KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string { reEnrollSpec(h, h.apiURL()); kubectlStub(h, false, false); return "" },
		call:  func(ctx context.Context, h *harness, _ string) error { return h.ctx.ReEnroll(ctx, "test.kubehz.dev") },
	},
	{
		name: "resolve the cluster id", site: "ResolveClusterID KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string { h.handle("GET /api/clusters", 200, registryListFixture); return "" },
		call: func(ctx context.Context, h *harness, _ string) error {
			_, err := h.ctx.ResolveClusterID(ctx, "test.kubehz.dev", h.apiURL())
			return err
		},
	},
	{
		name: "deregister", site: "DeregisterCluster KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.DeregisterCluster(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", "")
		},
	},
	{
		// The registry has no row, so the platform part succeeds. A refused
		// key step leaves the result as it is (the bash row: status 0).
		name: "deregister, the claim key", site: "DeregisterCluster HCLOUD_TOKEN", variable: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			h.env["HCLOUD_API_BASE"] = h.apiURL()
			noRegistryRow(h)
			serveClaimKeys(h, claimKeyCases[0])
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.DeregisterCluster(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", "")
		},
		allowed: func(r recordedReq) bool { return r.Method == "GET" && r.Path == "/api/clusters" },
	},
	{
		// No claim-key register: the platform would mint a key that the
		// upload cannot place.
		name: "claim key", site: "ensureClaimKey HCLOUD_TOKEN", variable: "HCLOUD_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.env["HCLOUD_API_BASE"] = h.apiURL()
			h.handle("POST /api/clusters/register", 200, claimRegisterAnswer)
			serveClaimKeys(h, claimKeyCases[1])
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.ensureClaimKey(ctx, "test.kubehz.dev", h.apiURL())
		},
	},
	{
		// register falls back to the legacy announce: it carries no
		// credential and asks for no claim key.
		name: "register, the claim key", site: "ensureClaimKey HCLOUD_TOKEN", variable: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			delete(h.env, "KUBEHZ_TOKEN")
			h.env["HCLOUD_API_BASE"] = h.apiURL()
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-001","registered":true}`)
			serveClaimKeys(h, claimKeyCases[1])
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.RegisterCluster(ctx, &Config{APIURL: h.apiURL(), Access: "registered"}, "test.kubehz.dev", spec)
		},
		allowed: func(r recordedReq) bool {
			return r.Method == "POST" && r.Path == "/api/clusters/register" && r.Auth == "" && !strings.Contains(r.Body, "claimKey")
		},
		after: "[warn] kubehz claim-key registration failed for test.kubehz.dev; falling back to the legacy fingerprint announce\n",
	},
	{
		name: "direct claim", site: "directClaim KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.directClaim(ctx, &Config{}, "test.kubehz.dev", spec, h.apiURL())
		},
	},
	{
		// register falls back to the legacy announce, which has no bearer.
		name: "register, the direct claim", site: "directClaim KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN",
		setup: func(h *harness) string {
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-001","registered":true}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.RegisterCluster(ctx, &Config{APIURL: h.apiURL(), Access: "registered"}, "test.kubehz.dev", spec)
		},
		allowed: func(r recordedReq) bool {
			return r.Method == "POST" && r.Path == "/api/clusters/register" && r.Auth == ""
		},
		after: "[warn] kubehz direct claim failed for test.kubehz.dev; falling back to the anonymous registration flow\n",
	},
	{
		// The register carries only KUBEHZ_TOKEN. The token connect sends
		// HCLOUD_TOKEN in the JSON body: it is skipped (the bash row: status
		// 0, no call to /api/credentials).
		name: "direct claim, the token connect", site: "directClaim HCLOUD_TOKEN", variable: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true}`)
			h.handle("POST /api/credentials", 200, `{"data":{"stored":true,"validation":{"writable":true}}}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.directClaim(ctx, &Config{ConnectToken: "true"}, "test.kubehz.dev", spec, h.apiURL())
		},
		allowed: func(r recordedReq) bool { return r.Method == "POST" && r.Path == "/api/clusters/register" },
		after:   "[warn] kubehz: could not connect the hcloud token (provisioning stays locked until you add it in the dashboard).\n",
	},
	{
		name: "wait for a hosted cluster", site: "waitForCluster KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("GET /api/clusters/cl-001", 200, `{"id":"cl-001","status":"Running"}`)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.waitForCluster(ctx, h.apiURL(), "cl-001", 30)
		},
	},
	{
		name: "provision a hosted cluster", site: "ProvisionHosted KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("POST /api/clusters", 201, `{"ok":true,"data":{"id":"cl-hosted-001","status":"Creating"}}`)
			h.handle("GET /api/clusters/cl-hosted-001", 200, `{"id":"cl-hosted-001","status":"Running"}`)
			h.handle("GET /api/clusters/cl-hosted-001/kubeconfig", 200, "apiVersion: v1\nkind: Config\n")
			return hostedSpec(h, "test.kubehz.dev")
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.ProvisionHosted(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec)
		},
	},
	{
		name: "destroy a hosted cluster", site: "DestroyHosted KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			return hostedSpec(h, "test.kubehz.dev")
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.DestroyHosted(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec)
		},
	},
	{
		name: "provision a space", site: "spaceAPI KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-1","slug":"acme","status":"Active"}]}`)
			return spaceSpec(h, "")
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.ProvisionShared(ctx, &Config{APIURL: h.apiURL()}, "acme.example.org", spec)
		},
	},
	{
		name: "join ticket of a space", site: "spaceAPI KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			spaceSpec(h, "")
			h.handle("GET /api/spaces", 200, `{"ok":true,"data":[{"id":"sp-1","slug":"acme"}]}`)
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.Join(ctx, "acme.example.org", "worker-9", false)
		},
	},
}

func TestEntryPointsRefuseAControlCharacterInACredential(t *testing.T) {
	t.Parallel()
	for _, tc := range credentialCases {
		for _, value := range credentialValues {
			t.Run(tc.name+"/"+strconv.Quote(value), func(t *testing.T) {
				t.Parallel()
				h := newHarness(t)
				spec := tc.setup(h)
				h.env[tc.variable] = value
				err := tc.call(t.Context(), h, spec)
				if tc.wantErr && err == nil {
					t.Errorf("no error:\n%s", h.output())
				}
				if !tc.wantErr && err != nil {
					t.Errorf("error %v, want the step skipped:\n%s", err, h.output())
				}
				if got, want := h.errOut.String(), credentialRefusal(tc.variable)+tc.after; got != want {
					t.Errorf("stderr\n%q\nwant\n%q", got, want)
				}
				if strings.Contains(h.output(), credentialMark) {
					t.Errorf("the output holds the credential:\n%s", h.output())
				}
				for _, r := range h.reqs() {
					if strings.Contains(r.Auth, credentialMark) || strings.Contains(r.Body, credentialMark) {
						t.Errorf("a request carries the credential: %s %s", r.Method, r.Path)
					}
					if tc.allowed == nil || !tc.allowed(r) {
						t.Errorf("a request went out: %s %s?%s body %q", r.Method, r.Path, r.Query, r.Body)
					}
				}
				if len(h.runner.calls) != 0 {
					t.Errorf("a command ran: %q", h.runner.lines())
				}
			})
		}
	}
}

// The quiet calls of a space (bash: kubehz::space_api with 2>/dev/null)
// refuse such a token too, with no request and no line. Each caller has
// shown the refusal before (spaceAPI, or the node preflight).
func TestSpaceQuietCallsRefuseAControlCharacterInTheToken(t *testing.T) {
	t.Parallel()
	for _, value := range credentialValues {
		h := newHarness(t)
		h.handle("GET /api/spaces/sp-1", 200, `{"ok":true,"data":{"id":"sp-1","status":"Active"}}`)
		h.handle("GET /api/spaces/sp-1/kubeconfig", 200, "apiVersion: v1\nkind: Config\n")
		h.env["KUBEHZ_TOKEN"] = value
		cfg := &Config{APIURL: h.apiURL()}
		if res := h.ctx.spaceAPIQuiet(t.Context(), cfg, "GET", "/api/spaces/sp-1"); res.Status != 0 || len(res.Body) != 0 {
			t.Errorf("%q: spaceAPIQuiet = %d %q, want an empty result", value, res.Status, res.Body)
		}
		if path := h.ctx.spaceKubeconfig(t.Context(), cfg, "acme.example.org", "sp-1"); path != "" {
			t.Errorf("%q: spaceKubeconfig wrote %s", value, path)
		}
		if h.output() != "" || len(h.reqs()) != 0 {
			t.Errorf("%q: output %q, requests %+v", value, h.output(), h.reqs())
		}
	}
}

// credentialSitesNotInTable are the calls of credentialOK and hasControl in
// the package that no row of credentialCases names, and why.
var credentialSitesNotInTable = map[string]string{
	"nodeAPIAccess KUBEHZ_TOKEN":  "the node preflights: TestNodePreflightHTTPSAndToken and TestNodeMaintenanceRefusesAControlCharacterInTheToken",
	"credentialOK hasControl":     "the check itself",
	"openSession hasControl":      "the access token of the agent commands, with its own text (kubehz::agent_session): TestAgentSessionRefusals",
	"agentAccessToken hasControl": "the agent key, with its own text (kubehz::token_access): TestTokenRefusals",
	"spaceAPIQuiet hasControl":    "a quiet call, with no line: TestSpaceQuietCallsRefuseAControlCharacterInTheToken",
	"spaceKubeconfig hasControl":  "a quiet call, with no line: TestSpaceQuietCallsRefuseAControlCharacterInTheToken",
}

// Each check of a credential in the package has a row of credentialCases,
// or an entry of credentialSitesNotInTable with the reason. A function with
// two checks of one variable needs two rows. Each row names a check that
// exists, and so does each entry.
func TestCredentialSitesHaveARow(t *testing.T) {
	t.Parallel()
	sites := credentialSites(t)
	if len(sites) == 0 {
		t.Fatal("no check found: credentialSites is broken")
	}
	found := map[string]int{}
	for _, s := range sites {
		found[s]++
	}
	rows := map[string]int{}
	for _, tc := range credentialCases {
		rows[tc.site]++
		if found[tc.site] == 0 {
			t.Errorf("row %q: no check %q in the package", tc.name, tc.site)
		}
		if !strings.HasSuffix(tc.site, " "+tc.variable) {
			t.Errorf("row %q: the site %q does not check %s", tc.name, tc.site, tc.variable)
		}
	}
	for site, n := range found {
		if _, ok := credentialSitesNotInTable[site]; ok {
			continue
		}
		if rows[site] < n {
			t.Errorf("%d checks %q, but %d rows of credentialCases: add a row for each check", n, site, rows[site])
		}
	}
	for site := range credentialSitesNotInTable {
		if found[site] == 0 {
			t.Errorf("credentialSitesNotInTable: no check %q in the package: remove the entry", site)
		}
	}
}

// credentialSites on a fixture: each form of a call, with its function.
func TestCredentialSitesFixture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package x\n\n" +
		"func (c *C) one() { _ = c.credentialOK(\"A\", \"B\") }\n" +
		"func two(c *C, n string) bool { return c.credentialOK(n) || hasControl(n) }\n" +
		"func three(c *C) { if !c.credentialOK(`C`) { return }; _ = c.credentialOK(\"C\") }\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\n\nfunc t(c *C) { c.credentialOK(\"T\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(credentialSitesIn(t, dir), "|")
	want := "one A|one B|three C|three C|two ?|two hasControl"
	if got != want {
		t.Fatalf("sites\n%s\nwant\n%s", got, want)
	}
}

// credentialSites is credentialSitesIn for the package directory (a test
// runs in it).
func credentialSites(t *testing.T) []string {
	t.Helper()
	return credentialSitesIn(t, ".")
}

// credentialSitesIn lists each call of credentialOK and hasControl in the
// non-test Go files of dir, with the function that holds it: "<function>
// <variable>" for each string argument of credentialOK, and "<function>
// hasControl" for each call of hasControl. A credentialOK argument that is
// not a string literal is "<function> ?", and no row can name it.
func credentialSitesIn(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch name := calleeName(call); name {
				case "credentialOK":
					for _, a := range call.Args {
						v := "?"
						if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							v, _ = strconv.Unquote(lit.Value)
						}
						sites = append(sites, fn.Name.Name+" "+v)
					}
				case "hasControl":
					sites = append(sites, fn.Name.Name+" hasControl")
				}
				return true
			})
		}
	}
	sort.Strings(sites)
	return sites
}

// calleeName is the name of the function or method that call calls.
func calleeName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
