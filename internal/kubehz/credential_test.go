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
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
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

// The values of the other credentials of a row. platformAuth is the
// header of the KUBEHZ_TOKEN of newHarness.
const (
	platformAuth = "Bearer khz_test_token"
	cloudToken   = "hc_test"
	cloudAuth    = "Bearer " + cloudToken
	bindValue    = "bind-value-fixture"
)

// The fall back warnings of the register chain.
const (
	directClaimWarn = "[warn] kubehz direct claim failed for test.kubehz.dev; falling back to the anonymous registration flow\n"
	claimKeyWarn    = "[warn] kubehz claim-key registration failed for test.kubehz.dev; falling back to the legacy fingerprint announce\n"
)

// credentialRefusal is the refusal of http::credential_ok for name, as the
// bash twin prints it.
func credentialRefusal(name string) string {
	return "[error] environment variable " + name + " must not contain a control character\n" +
		"  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.\n"
}

// wantReq is one request of the exact sequence that a row expects.
type wantReq struct {
	route string // "METHOD /path"
	auth  string // the Authorization header, exactly ("" = none)
	lacks string // a part that the body must not hold ("" = no check)
}

// credentialCase is one credential of one entry point.
type credentialCase struct {
	name string
	// site is the function that holds the check and the variable, as
	// credentialSites prints it.
	site     string
	variable string
	// also is a second variable that gets the bad value ("" = none).
	also string
	// setup writes the spec and the routes. It returns the spec path ("" when
	// the call needs none). Then the variable gets the bad value.
	setup func(h *harness) string
	call  func(ctx context.Context, h *harness, spec string) error
	// wantErr: the entry point fails with ErrHandled. Without it, the entry
	// point skips the step of the credential and succeeds, as the bash twin
	// does.
	wantErr bool
	// want is the exact sequence of requests where the entry point goes on
	// without the credential. nil: no request may go out, and no file in the
	// project may change.
	want []wantReq
	// after is the stderr after the refusal ("" = nothing).
	after string
	// check holds more assertions after the call (nil = none).
	check func(t *testing.T, h *harness)
}

func noRegistryRow(h *harness) { h.handle("GET /api/clusters", 200, `{"ok":true,"data":[]}`) }

// cloudSetup gives the hcloud steps a base and a valid token. A row of
// HCLOUD_TOKEN then sets the bad value.
func cloudSetup(h *harness) {
	h.env["HCLOUD_TOKEN"], h.env["HCLOUD_API_BASE"] = cloudToken, h.apiURL()
}

// hostedKubeconfigs are the two local kubeconfigs of hostedSpec(h,
// "acme-hosted"): the domain-keyed one and the metadata.name mirror.
func hostedKubeconfigs(h *harness) []string {
	return []string{
		filepath.Join(h.base, ".kubeconfig", "test.kubehz.dev.yaml"),
		filepath.Join(h.base, ".kubeconfig", "acme-hosted.yaml"),
	}
}

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
		// A refused platform step does not stop the claim-key step, which has
		// its own valid token (the bash twin does the same). The result stays
		// a failure.
		name: "deregister, with a cloud token", site: "DeregisterCluster KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			cloudSetup(h)
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			serveClaimKeys(h, claimKeyCases[0])
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.DeregisterCluster(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", "")
		},
		want: []wantReq{{route: "GET /v1/ssh_keys", auth: cloudAuth}, {route: "DELETE /v1/ssh_keys/42", auth: cloudAuth}},
	},
	{
		// The registry has no row, so the platform part succeeds. A refused
		// key step leaves the result as it is (the bash row: status 0).
		name: "deregister, the claim key", site: "DeregisterCluster HCLOUD_TOKEN", variable: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			cloudSetup(h)
			noRegistryRow(h)
			serveClaimKeys(h, claimKeyCases[0])
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.DeregisterCluster(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", "")
		},
		want: []wantReq{{route: "GET /api/clusters", auth: platformAuth}},
	},
	{
		// The platform delete goes out, then the key step is skipped.
		name: "deregister, the claim key, with a registered cluster", site: "DeregisterCluster HCLOUD_TOKEN", variable: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			cloudSetup(h)
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			serveClaimKeys(h, claimKeyCases[0])
			return ""
		},
		call: func(ctx context.Context, h *harness, _ string) error {
			return h.ctx.DeregisterCluster(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", "")
		},
		want: []wantReq{{route: "GET /api/clusters", auth: platformAuth}, {route: "DELETE /api/clusters/cl-old", auth: platformAuth}},
	},
	{
		// No claim-key register: the platform would mint a key that the
		// upload cannot place.
		name: "claim key", site: "ensureClaimKey HCLOUD_TOKEN", variable: "HCLOUD_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			cloudSetup(h)
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
			cloudSetup(h)
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-001","registered":true}`)
			serveClaimKeys(h, claimKeyCases[1])
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.RegisterCluster(ctx, &Config{APIURL: h.apiURL(), Access: "registered"}, "test.kubehz.dev", spec)
		},
		want:  []wantReq{{route: "POST /api/clusters/register", lacks: "claimKey"}},
		after: claimKeyWarn,
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
		want:  []wantReq{{route: "POST /api/clusters/register", lacks: "claimKey"}},
		after: directClaimWarn,
	},
	{
		// register falls back to the claim-key flow, with the valid hcloud
		// token.
		name: "register, the direct claim, with a cloud token", site: "directClaim KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN",
		setup: func(h *harness) string {
			cloudSetup(h)
			h.handle("POST /api/clusters/register", 200, claimRegisterAnswer)
			serveClaimKeys(h, claimKeyCases[1])
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.RegisterCluster(ctx, &Config{APIURL: h.apiURL(), Access: "registered"}, "test.kubehz.dev", spec)
		},
		want: []wantReq{
			{route: "POST /api/clusters/register", lacks: "fingerprint"},
			{route: "GET /v1/ssh_keys", auth: cloudAuth},
			{route: "POST /v1/ssh_keys", auth: cloudAuth},
		},
		after: directClaimWarn,
	},
	{
		// Both steps with a credential are refused: register falls back to
		// the legacy announce.
		name: "register, both tokens", site: "directClaim KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", also: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			cloudSetup(h)
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-001","registered":true}`)
			serveClaimKeys(h, claimKeyCases[1])
			h.handle("POST /v1/ssh_keys", 201, `{"ssh_key":{"id":43}}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.RegisterCluster(ctx, &Config{APIURL: h.apiURL(), Access: "registered"}, "test.kubehz.dev", spec)
		},
		want:  []wantReq{{route: "POST /api/clusters/register", lacks: "claimKey"}},
		after: directClaimWarn + credentialRefusal("HCLOUD_TOKEN") + claimKeyWarn,
	},
	{
		// The register carries only KUBEHZ_TOKEN. The token connect sends
		// HCLOUD_TOKEN in the JSON body: it is skipped (the bash row: status
		// 0, no call to /api/credentials). The bind secret of the register is
		// kept.
		name: "direct claim, the token connect", site: "directClaim HCLOUD_TOKEN", variable: "HCLOUD_TOKEN",
		setup: func(h *harness) string {
			h.handle("POST /api/clusters/register", 200, `{"id":"cl-9","claimed":true,"bindSecret":"`+bindValue+`"}`)
			h.handle("POST /api/credentials", 200, `{"data":{"stored":true,"validation":{"writable":true}}}`)
			return loSpec(h)
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.directClaim(ctx, &Config{ConnectToken: "true"}, "test.kubehz.dev", spec, h.apiURL())
		},
		want:  []wantReq{{route: "POST /api/clusters/register", auth: platformAuth}},
		after: "[warn] kubehz: could not connect the hcloud token (provisioning stays locked until you add it in the dashboard).\n",
		check: func(t *testing.T, h *harness) {
			if got := readFile(t, bindSecretFile(h, "test.kubehz.dev")); got != bindValue {
				t.Errorf("bind secret file = %q, want %q", got, bindValue)
			}
		},
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
		// Both local kubeconfigs stay.
		name: "destroy a hosted cluster", site: "DestroyHosted KUBEHZ_TOKEN", variable: "KUBEHZ_TOKEN", wantErr: true,
		setup: func(h *harness) string {
			h.handle("GET /api/clusters", 200, registryListFixture)
			h.handle("DELETE /api/clusters/cl-old", 200, `{"ok":true}`)
			for _, p := range hostedKubeconfigs(h) {
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					h.t.Fatal(err)
				}
				mustWrite(h.t, p, "apiVersion: v1\nkind: Config\n", 0o600)
			}
			return hostedSpec(h, "acme-hosted")
		},
		call: func(ctx context.Context, h *harness, spec string) error {
			return h.ctx.DestroyHosted(ctx, &Config{APIURL: h.apiURL()}, "test.kubehz.dev", spec)
		},
		check: func(t *testing.T, h *harness) {
			for _, p := range hostedKubeconfigs(h) {
				if _, err := os.Stat(p); err != nil {
					t.Errorf("the kubeconfig %s is gone: %v", p, err)
				}
			}
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
				if tc.also != "" {
					h.env[tc.also] = value
				}
				before := treeSnapshot(t, h.base)
				err := tc.call(t.Context(), h, spec)
				if tc.wantErr && !errors.Is(err, ErrHandled) {
					t.Errorf("error %v, want ErrHandled:\n%s", err, h.output())
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
				reqs := h.reqs()
				for _, r := range reqs {
					if credentialIn(r) {
						t.Errorf("a request carries the credential: %s %s", r.Method, r.Path)
					}
				}
				if got, want := requestLines(reqs), wantLines(tc.want); got != want {
					t.Errorf("requests\n%s\nwant exactly\n%s", got, want)
				}
				for i, w := range tc.want {
					if i < len(reqs) && w.lacks != "" && strings.Contains(reqs[i].Body, w.lacks) {
						t.Errorf("request %d (%s) holds %q: %s", i, w.route, w.lacks, reqs[i].Body)
					}
				}
				if tc.want == nil {
					if after := treeSnapshot(t, h.base); !maps.Equal(before, after) {
						t.Errorf("the refusal changed a file: %s", treeDiff(before, after))
					}
				}
				if len(h.runner.calls) != 0 {
					t.Errorf("a command ran: %q", h.runner.lines())
				}
				if tc.check != nil {
					tc.check(t, h)
				}
			})
		}
	}
}

// requestLines is one line for each recorded request: the route and the
// Authorization header.
func requestLines(reqs []recordedReq) string {
	var b strings.Builder
	for _, r := range reqs {
		b.WriteString(r.Method + " " + r.Path + " auth=" + strconv.Quote(r.Auth) + "\n")
	}
	return b.String()
}

// wantLines is requestLines for the expected sequence.
func wantLines(want []wantReq) string {
	var b strings.Builder
	for _, w := range want {
		b.WriteString(w.route + " auth=" + strconv.Quote(w.auth) + "\n")
	}
	return b.String()
}

// credentialIn reports whether r holds credentialMark in any part, with no
// regard to case: the path, the query (raw and decoded), each header (name
// and value), and the body (raw, URL-decoded, and each key and string of
// its JSON).
func credentialIn(r recordedReq) bool {
	parts := []string{r.Path, r.Body}
	// The query and the headers.
	parts = append(parts, r.Query)
	if q, err := url.QueryUnescape(r.Query); err == nil {
		parts = append(parts, q)
	}
	if vs, err := url.ParseQuery(r.Query); err == nil {
		for k, v := range vs {
			parts = append(parts, k)
			parts = append(parts, v...)
		}
	}
	for k, v := range r.Header {
		parts = append(parts, k)
		parts = append(parts, v...)
	}
	// The body, decoded.
	if b, err := url.QueryUnescape(r.Body); err == nil {
		parts = append(parts, b)
	}
	var v any
	if json.Unmarshal([]byte(r.Body), &v) == nil {
		parts = jsonStrings(v, parts)
	}
	for _, p := range parts {
		if strings.Contains(strings.ToLower(p), credentialMark) {
			return true
		}
	}
	return false
}

// jsonStrings appends each key and each string of a decoded JSON value.
func jsonStrings(v any, out []string) []string {
	switch x := v.(type) {
	case string:
		out = append(out, x)
	case []any:
		for _, e := range x {
			out = jsonStrings(e, out)
		}
	case map[string]any:
		for k, e := range x {
			out = append(out, k)
			out = jsonStrings(e, out)
		}
	}
	return out
}

// credentialIn finds the mark in each part of a request that the fake
// server records, in an encoded form too. A test-local client plants it.
func TestCredentialInFindsAPlantedValue(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	send := func(target, header, body string) recordedReq {
		t.Helper()
		h.reset()
		req, err := http.NewRequestWithContext(t.Context(), "POST", h.apiURL()+target, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if header != "" {
			req.Header.Set(header, "x "+header)
		}
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		reqs := h.reqs()
		if len(reqs) != 1 {
			t.Fatalf("%d requests, want 1", len(reqs))
		}
		return reqs[0]
	}
	planted := []struct{ name, target, header, body string }{
		{"a header", "/p", "X-Debug-" + credentialMark, ""},
		{"the query, encoded", "/p?t=%72eal-value", "", ""},
		{"the body, URL-encoded", "/p", "", "t=%72eal-value"},
		{"the body, a JSON escape", "/p", "", `{"t":"real-value"}`},
		{"a JSON key", "/p", "", `{"real-value":1}`},
	}
	for _, p := range planted {
		if r := send(p.target, p.header, p.body); !credentialIn(r) {
			t.Errorf("%s: credentialIn missed the mark in %+v", p.name, r)
		}
	}
	if r := send("/p?t=x", "X-Debug", `{"t":"x"}`); credentialIn(r) {
		t.Errorf("credentialIn found the mark in a request without it: %+v", r)
	}
}

// treeSnapshot maps each path below root to its type, mode and content
// (a link: its target).
func treeSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		v := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			v += " -> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			v += " " + string(b)
		}
		snap[p] = v
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// treeDiff names each path that differs between two snapshots.
func treeDiff(before, after map[string]string) string {
	var diff []string
	for p, v := range before {
		if w, ok := after[p]; !ok {
			diff = append(diff, "removed "+p)
		} else if w != v {
			diff = append(diff, "changed "+p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			diff = append(diff, "added "+p)
		}
	}
	sort.Strings(diff)
	return strings.Join(diff, ", ")
}

// The checks refuse only a control character. With a valid or an empty
// credential, the first request of the entry point goes out (an empty
// value: the caller decides on that).
func TestEntryPointsSendAValidOrEmptyCredential(t *testing.T) {
	t.Parallel()
	rows := map[string]*credentialCase{}
	for i := range credentialCases {
		rows[credentialCases[i].name] = &credentialCases[i]
	}
	runs := []struct {
		row, value, first string
	}{
		{"status", "khz_test_token", "GET /api/clusters"},
		{"status", "", "GET /api/clusters"},
		{"resolve the cluster id", "khz_test_token", "GET /api/clusters"},
		{"resolve the cluster id", "", "GET /api/clusters"},
		{"wait for a hosted cluster", "khz_test_token", "GET /api/clusters/cl-001"},
		{"wait for a hosted cluster", "", "GET /api/clusters/cl-001"},
		{"status of a space", "khz_test_token", "GET /api/spaces"},
		{"claim key", cloudToken, "POST /api/clusters/register"},
		{"claim key", "", "POST /api/clusters/register"},
	}
	for _, run := range runs {
		t.Run(run.row+"/"+strconv.Quote(run.value), func(t *testing.T) {
			t.Parallel()
			tc := rows[run.row]
			if tc == nil {
				t.Fatalf("no row %q", run.row)
			}
			h := newHarness(t)
			spec := tc.setup(h)
			h.env[tc.variable] = run.value
			_ = tc.call(t.Context(), h, spec)
			if strings.Contains(h.output(), "must not contain a control character") {
				t.Errorf("a refusal:\n%s", h.output())
			}
			reqs := h.reqs()
			if len(reqs) == 0 || reqs[0].Method+" "+reqs[0].Path != run.first {
				t.Errorf("requests\n%s\nwant first %s", requestLines(reqs), run.first)
			}
		})
	}
}

// credentialOK refuses exactly the bytes of [[:cntrl:]] under LC_ALL=C
// (0x00-0x1f and 0x7f), as http::credential_ok does. A byte 0x80-0xff
// alone is not valid UTF-8, and it passes. So does a C1 character in
// UTF-8, and an empty or unset value.
func TestCredentialOKRefusesExactlyTheControlBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	check := func(value string, refuse bool) {
		t.Helper()
		h.reset()
		h.env["TEST_CREDENTIAL"] = value
		ok := h.ctx.credentialOK("TEST_CREDENTIAL")
		want := ""
		if refuse {
			want = credentialRefusal("TEST_CREDENTIAL")
		}
		if ok == refuse || h.errOut.String() != want {
			t.Errorf("%q: credentialOK = %v, stderr %q", value, ok, h.errOut.String())
		}
	}
	for b := 0; b <= 0xff; b++ {
		check("a"+string([]byte{byte(b)})+"b", b <= 0x1f || b == 0x7f)
	}
	for _, v := range []string{"", "a\u0085b", "a\u009bb", "a\xc2b", "\xff\xfe\xfd", "a\xe2\x80b", "\xed\xa0\x80"} {
		check(v, false)
	}
	h.reset()
	delete(h.env, "TEST_CREDENTIAL")
	if !h.ctx.credentialOK("TEST_CREDENTIAL") || h.errOut.Len() != 0 {
		t.Errorf("an unset value: stderr %q", h.errOut.String())
	}
}

// request refuses a bearer with a control character with errHeaderControl:
// no request goes out, requestSent reports nothing sent, and no line is
// printed. An empty bearer, a valid one and none go out.
func TestRequestRefusesAControlCharacterInTheBearer(t *testing.T) {
	t.Parallel()
	for _, value := range credentialValues {
		h := newHarness(t)
		h.handle("GET /api/x", 200, `{"ok":true}`)
		h.handle("POST /api/x", 200, `{"ok":true}`)
		res, err := h.ctx.request(t.Context(), "GET", h.apiURL()+"/api/x", withBearer(value), nil)
		if res != nil || !errors.Is(err, errHeaderControl) {
			t.Errorf("%q: request = %v, %v; want errHeaderControl", value, res, err)
		}
		if err != nil && strings.Contains(err.Error(), credentialMark) {
			t.Errorf("%q: the error holds the value: %v", value, err)
		}
		_, sent, err := h.ctx.requestSent(t.Context(), "POST", h.apiURL()+"/api/x", withBearer(value), []byte(`{}`))
		if sent || !errors.Is(err, errHeaderControl) {
			t.Errorf("%q: requestSent = sent %v, %v; want nothing sent, errHeaderControl", value, sent, err)
		}
		if reqs := h.reqs(); len(reqs) != 0 || h.output() != "" {
			t.Errorf("%q: requests\n%s output %q", value, requestLines(reqs), h.output())
		}
	}
	for _, auth := range []bearer{withBearer(""), withBearer("khz_test_token"), {}} {
		h := newHarness(t)
		h.handle("POST /api/x", 200, `{"ok":true}`)
		res, sent, err := h.ctx.requestSent(t.Context(), "POST", h.apiURL()+"/api/x", auth, []byte(`{}`))
		if err != nil || !sent || res.Status != 200 || len(h.reqs()) != 1 {
			t.Errorf("%+v: sent %v, %v, requests\n%s", auth, sent, err, requestLines(h.reqs()))
		}
	}
}

// The quiet calls of a space (bash: kubehz::space_api with 2>/dev/null)
// refuse such a token too, with no request, no line and no file. Each
// caller has shown the refusal before (spaceAPI, or the node preflight).
func TestSpaceQuietCallsRefuseAControlCharacterInTheToken(t *testing.T) {
	t.Parallel()
	for _, value := range credentialValues {
		h := newHarness(t)
		h.handle("GET /api/spaces/sp-1", 200, `{"ok":true,"data":{"id":"sp-1","status":"Active"}}`)
		h.handle("GET /api/spaces/sp-1/kubeconfig", 200, "apiVersion: v1\nkind: Config\n")
		h.env["KUBEHZ_TOKEN"] = value
		cfg := &Config{APIURL: h.apiURL()}
		before := treeSnapshot(t, h.base)
		if res := h.ctx.spaceAPIQuiet(t.Context(), cfg, "GET", "/api/spaces/sp-1"); res.Status != 0 || len(res.Body) != 0 {
			t.Errorf("%q: spaceAPIQuiet = %d %q, want an empty result", value, res.Status, res.Body)
		}
		if path := h.ctx.spaceKubeconfig(t.Context(), cfg, "acme.example.org", "sp-1"); path != "" {
			t.Errorf("%q: spaceKubeconfig wrote %s", value, path)
		}
		if h.output() != "" || len(h.reqs()) != 0 {
			t.Errorf("%q: output %q, requests %+v", value, h.output(), h.reqs())
		}
		if after := treeSnapshot(t, h.base); !maps.Equal(before, after) {
			t.Errorf("%q: a file changed: %s", value, treeDiff(before, after))
		}
	}
}

// With a valid token, the quiet calls send their request and return the
// answer.
func TestSpaceQuietCallsSendAValidToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const space = `{"ok":true,"data":{"id":"sp-1","status":"Active"}}`
	const kubeconfig = "apiVersion: v1\nkind: Config\n"
	h.handle("GET /api/spaces/sp-1", 200, space)
	h.handle("GET /api/spaces/sp-1/kubeconfig", 200, kubeconfig)
	cfg := &Config{APIURL: h.apiURL()}
	if res := h.ctx.spaceAPIQuiet(t.Context(), cfg, "GET", "/api/spaces/sp-1"); res.Status != 200 || string(res.Body) != space {
		t.Errorf("spaceAPIQuiet = %d %q, want 200 and the answer", res.Status, res.Body)
	}
	path := h.ctx.spaceKubeconfig(t.Context(), cfg, "acme.example.org", "sp-1")
	if want := filepath.Join(h.base, ".kubeconfig", "acme.example.org.yaml"); path != want {
		t.Fatalf("spaceKubeconfig = %q, want %q", path, want)
	}
	if got := readFile(t, path); got != kubeconfig {
		t.Errorf("kubeconfig = %q", got)
	}
	want := "GET /api/spaces/sp-1 auth=" + strconv.Quote(platformAuth) + "\n" +
		"GET /api/spaces/sp-1/kubeconfig auth=" + strconv.Quote(platformAuth) + "\n"
	if got := requestLines(h.reqs()); got != want {
		t.Errorf("requests\n%s\nwant\n%s", got, want)
	}
}

// credentialSitesNotInTable are the check sites of the inventory that no
// row of credentialCases names, and why.
var credentialSitesNotInTable = map[string]string{
	"nodeAPIAccess KUBEHZ_TOKEN":  "the node preflights: TestNodePreflightHTTPSAndToken and TestNodeMaintenanceRefusesAControlCharacterInTheToken",
	"credentialOK hasControl":     "the check itself: TestCredentialOKRefusesExactlyTheControlBytes",
	"openSession hasControl":      "the access token of the agent commands, with its own text (kubehz::agent_session): TestAgentSessionRefusals",
	"agentAccessToken hasControl": "the agent key, with its own text (kubehz::token_access): TestTokenRefusals",
	"spaceAPIQuiet hasControl":    "a quiet call, with no line: TestSpaceQuietCallsRefuseAControlCharacterInTheToken",
	"spaceKubeconfig hasControl":  "a quiet call, with no line: TestSpaceQuietCallsRefuseAControlCharacterInTheToken",
	"request hasControl":          "the second guard of each request, with no line: TestRequestRefusesAControlCharacterInTheBearer",
	"printable isControl":         "no credential: it cleans the error text of an endpoint for the terminal",

	"hasControl isControl: not a direct call": "the definition of hasControl: it gives isControl to strings.ContainsFunc",
}

// A check-site inventory. Each check of a credential in the package has a
// row of credentialCases, or an entry of credentialSitesNotInTable with the
// reason. A function with two checks of one variable needs two rows. Each
// row names a check that exists, and so does each entry.
//
// The inventory does not prove coverage: a new request with no check adds
// no site. The behavior tests above and their mutants prove that. The
// inventory fails on a call shape that it cannot attribute
// (credentialSitesIn).
func TestCredentialCheckSiteInventory(t *testing.T) {
	t.Parallel()
	sites, unattributed := credentialSites(t)
	if len(sites) == 0 {
		t.Fatal("no check found: credentialSites is broken")
	}
	found := map[string]int{}
	for _, s := range sites {
		found[s]++
	}
	for _, u := range unattributed {
		if _, ok := credentialSitesNotInTable[u]; !ok {
			t.Errorf("cannot attribute %q: call the check directly, in a function, with string literals", u)
		}
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
		if found[site] == 0 && !slices.Contains(unattributed, site) {
			t.Errorf("credentialSitesNotInTable: no check %q in the package: remove the entry", site)
		}
	}
}

// credentialSitesIn on a fixture: each call shape, with its function, and
// each shape that it cannot attribute.
func TestCredentialCheckSiteInventoryFixture(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := "package x\n\n" +
		"func (c *C) one() { _ = c.credentialOK(\"A\", \"B\") }\n" +
		"func two(c *C, n string) bool { return c.credentialOK(n) || hasControl(n) }\n" +
		"func three(c *C) { if !c.credentialOK(`C`) { return }; _ = c.credentialOK(\"C\") }\n" +
		"func four(c *C) { check := c.credentialOK; check(\"D\") }\n" +
		"func five(c *C) { (c.credentialOK)(\"E\") }\n" +
		"var six = func(c *C) bool { return c.credentialOK(\"F\") }\n" +
		"func seven(s string) bool { return strings.ContainsFunc(s, isControl) || isControl('x') }\n" +
		"func eight(c *C) { c.credentialOK() }\n" +
		"func nine(c *C) { go func() { _ = c.credentialOK(\"G\") }() }\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\n\nfunc t(c *C) { c.credentialOK(\"T\") }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sites, unattributed := credentialSitesIn(t, dir)
	if got, want := strings.Join(sites, "|"), "nine G|one A|one B|seven isControl|three C|three C|two hasControl"; got != want {
		t.Errorf("sites\n%s\nwant\n%s", got, want)
	}
	want := "(package) credentialOK: in a package-level declaration|" +
		"eight credentialOK: no argument|" +
		"five credentialOK: not a direct call|" +
		"four credentialOK: not a direct call|" +
		"seven isControl: not a direct call|" +
		"two credentialOK: an argument that is not a string literal"
	if got := strings.Join(unattributed, "|"); got != want {
		t.Errorf("unattributed\n%s\nwant\n%s", got, want)
	}
}

// credentialSites is credentialSitesIn for the package directory (a test
// runs in it).
func credentialSites(t *testing.T) (sites, unattributed []string) {
	t.Helper()
	return credentialSitesIn(t, ".")
}

// checkNames are the checks that the inventory counts.
var checkNames = map[string]bool{"credentialOK": true, "hasControl": true, "isControl": true}

// credentialSitesIn is the check-site inventory of the non-test Go files of
// dir. sites holds each direct call of a check in a function body, with the
// function that holds it: "<function> <variable>" for each string literal
// argument of credentialOK, and "<function> hasControl" or "<function>
// isControl". A call in a closure counts for the function around it.
//
// unattributed holds each use that the inventory cannot attribute to a
// variable or a count: a check used as a value (an alias, a parenthesized
// callee, an argument), a credentialOK argument that is not a string
// literal, a credentialOK call with no argument, and any use in a
// package-level declaration. The match is by name, not by type.
func credentialSitesIn(t *testing.T, dir string) (sites, unattributed []string) {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
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
			switch d := d.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					sites, unattributed = inventoryBody(d.Name.Name, d.Body, sites, unattributed)
				}
			case *ast.GenDecl:
				ast.Inspect(d, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok && checkNames[id.Name] {
						unattributed = append(unattributed, "(package) "+id.Name+": in a package-level declaration")
					}
					return true
				})
			}
		}
	}
	sort.Strings(sites)
	sort.Strings(unattributed)
	return sites, unattributed
}

// inventoryBody adds the check sites of the body of the function fn.
func inventoryBody(fn string, body *ast.BlockStmt, sites, unattributed []string) ([]string, []string) {
	called := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			id := calleeIdent(n)
			if id == nil || !checkNames[id.Name] {
				return true
			}
			// ast.Inspect visits the call before its callee.
			called[id] = true
			if id.Name != "credentialOK" {
				sites = append(sites, fn+" "+id.Name)
				return true
			}
			if len(n.Args) == 0 {
				unattributed = append(unattributed, fn+" credentialOK: no argument")
			}
			for _, a := range n.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					unattributed = append(unattributed, fn+" credentialOK: an argument that is not a string literal")
					continue
				}
				v, _ := strconv.Unquote(lit.Value)
				sites = append(sites, fn+" "+v)
			}
		case *ast.Ident:
			if checkNames[n.Name] && !called[n] {
				unattributed = append(unattributed, fn+" "+n.Name+": not a direct call")
			}
		}
		return true
	})
	return sites, unattributed
}

// calleeIdent is the name of the function or method that call calls, when
// the callee is a name or a selector (not parenthesized).
func calleeIdent(call *ast.CallExpr) *ast.Ident {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	}
	return nil
}
