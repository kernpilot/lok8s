package kubehz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// agentHarness: an agent key in the environment, KUBEHZ_API_URL on the TLS
// server, and a token route answering one access token. KUBEHZ_TOKEN is
// unset: the key is the credential.
func agentHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	delete(h.env, "KUBEHZ_TOKEN")
	h.env[EnvAPIURL] = h.apiURL() + "/"
	h.env[EnvAgentClientID] = "kubehz-agent-ak-1a2b3c4d"
	h.env[EnvAgentClientSecret] = "s3cr3t-value"
	h.env[EnvAgentTokenURL] = h.apiURL() + "/oauth/v2/token"
	h.env[EnvAgentScope] = "openid urn:zitadel:iam:org:project:id:P:aud"
	h.handle("POST /oauth/v2/token", 200, `{"access_token":"jwt-agent","token_type":"Bearer","expires_in":43199}`)
	return h
}

// The api's shapes (SpaceSchema, SpaceDetailSchema, ClusterSchema), with
// fields lo does not project, so the projection is what is tested.
const (
	spaceRow = `{"id":"sp-1a2b3c4d","tenantId":"t-1","shardId":"sh-1","name":"Acme Prod","slug":"acme",` +
		`"status":"Active","maxNodes":2,"maxNamespaces":1,"maxObjectKiB":256,"limitsCeiling":{"nodes":5},` +
		`"leaseExpiresAt":"2026-10-04T14:00:00.000Z","createdAt":"2026-10-04T12:00:00.000Z","namespaces":["acme"],"nodeCount":1}`
	oddSpaceRow = `{"id":"sp-9z9z9z9z","name":"Ev\u001b[2Jil \\ na\u2028me\u0085\u009b\u202e\u2066 \u00fc","slug":"yes","status":"Pending",` +
		`"maxNodes":1,"maxNamespaces":3,"maxObjectKiB":null,"leaseExpiresAt":null,"createdAt":"2026-10-04T12:30:00.000Z",` +
		`"namespaces":["yes",7],"nodeCount":0}`
	spaceDetailRow = `{"id":"sp-1a2b3c4d","name":"Acme Prod","slug":"acme","status":"Active","maxNodes":2,"maxNamespaces":1,` +
		`"maxObjectKiB":256,"leaseExpiresAt":"2026-10-04T14:00:00.000Z","createdAt":"2026-10-04T12:00:00.000Z",` +
		`"namespaces":[{"name":"acme","createdAt":"2026-10-04T12:00:00.000Z"}],` +
		`"nodes":[{"name":"worker-1","lane":"metal","status":"Ready"}],"usage":{"nodes":1,"nodesReady":1},` +
		`"endpoint":"https://acme.k8s.kubehz.example"}`
	clusterRow = `{"id":"cl-1a2b3c4d","tenantId":"t-1","domain":"agent.example.org","hosting":"hosted","status":"Running",` +
		`"region":"fsn1","kubernetesVersion":"v1.34.1","controlPlaneReplicas":1,"apiEndpoint":"https://203.0.113.7:6443",` +
		`"health":"healthy","leaseExpiresAt":null,"createdAt":"2026-10-04T12:00:00.000Z","workers":[]}`
)

func okBody(data string) string { return `{"ok":true,"data":` + data + `,"traceId":"tr-1"}` }

func refusalBody(code, message, help string) string {
	return `{"statusCode":400,"data":{"code":"` + code + `","message":"` + message + `","help":"` + help + `"}}`
}

// encode renders a report the way `-o json` does (internal/cli writeOutput).
func encode(t *testing.T, v any) string {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestAgentSpaceListSendsTheMintedBearerAndPrintsTheTable(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces", 200, `{"ok":true,"data":[`+spaceRow+`,`+oddSpaceRow+`,"junk"],"meta":{"pagination":{"total":2}}}`)
	l, err := h.ctx.SpaceList(context.Background())
	mustOK(t, err, h.output())

	reqs := h.reqs()
	if len(reqs) != 2 || reqs[0].Path != "/oauth/v2/token" {
		t.Fatalf("requests = %+v, want the grant, then the list", reqs)
	}
	// The trailing slash of KUBEHZ_API_URL is dropped: a double slash is
	// not canonical, and the api refuses it for a machine credential.
	if r := reqs[1]; r.Method != "GET" || r.Path != "/api/spaces" || r.Query != "perPage=500" || r.Auth != "Bearer jwt-agent" {
		t.Errorf("list request = %+v", r)
	}
	var b bytes.Buffer
	l.WriteText(&b)
	want := "ID           SLUG  NAME              STATUS   NODES  LEASE ENDS\n" +
		"sp-1a2b3c4d  acme  Acme Prod         Active   1/2    2026-10-04T14:00:00.000Z\n" +
		"sp-9z9z9z9z  yes   Ev[2Jil \\ name ü  Pending  0/1    -\n"
	if b.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", b.String(), want)
	}
	if h.errOut.Len() != 0 {
		t.Errorf("stderr = %q, want nothing (the total matches the page)", h.errOut.String())
	}
}

func TestAgentRecordsDropEveryControlCharacterInEveryFormat(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces", 200, okBody(`[`+oddSpaceRow+`]`))
	l, err := h.ctx.SpaceList(context.Background())
	mustOK(t, err, h.output())
	got := encode(t, l)
	want := `{
  "spaces": [
    {
      "id": "sp-9z9z9z9z",
      "name": "Ev[2Jil \\ name ü",
      "slug": "yes",
      "status": "Pending",
      "maxNodes": 1,
      "maxNamespaces": 3,
      "maxObjectKiB": null,
      "nodeCount": 0,
      "namespaces": [
        "yes"
      ],
      "leaseExpiresAt": null,
      "createdAt": "2026-10-04T12:30:00.000Z"
    }
  ]
}
`
	if got != want {
		t.Errorf("json:\n%s\nwant:\n%s", got, want)
	}
	for _, bad := range []string{"\x1b", "\u2028", `\u001b`, `\u2028`, "\u0085", "\u009b", "\u202e", "\u2066", `\u0085`, `\u202e`} {
		if strings.Contains(got, bad) {
			t.Errorf("json output carries %q", bad)
		}
	}
}

// cleanText drops each character that ui.Shown shows as '?' (ui.Unshown),
// but keeps a backslash. For each case of the shared table
// (internal/ui/testdata/shown.json), the input without each character that
// the case's result shows as '?' is what cleanText gives. The bash twin is
// the test "clean: drops each character ..." in kubehz_agent_test.bats.
func TestCleanTextDropsEachUnshownCharacter(t *testing.T) {
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
			if shown[i] != '?' || r == '?' || r == '\\' {
				want = append(want, r)
			}
		}
		got := cleanText(c.In)
		if got != c.In {
			dropped++
		}
		if got != string(want) {
			t.Errorf("%s: cleanText(%q) = %q, want %q", c.Case, c.In, got, string(want))
		}
	}
	// The table drops something: else the test proves nothing.
	if dropped < 15 {
		t.Fatalf("cleanText changed %d cases of the table, want 15 or more", dropped)
	}
	if got := cleanText(`a\b`); got != `a\b` {
		t.Errorf("cleanText dropped the backslash: %q", got)
	}
}

func TestAgentListWarnsWhenTheApiHoldsMoreThanOnePage(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/clusters", 200, `{"ok":true,"data":[`+clusterRow+`],"meta":{"pagination":{"total":612}}}`)
	l, err := h.ctx.ClusterList(context.Background())
	mustOK(t, err, h.output())
	if got := h.errOut.String(); got != "[warn] kubehz cluster list: the list shows 1 of 612 clusters\n" {
		t.Errorf("stderr = %q", got)
	}
	var b bytes.Buffer
	l.WriteText(&b)
	want := "ID           DOMAIN             HOSTING  STATUS   VERSION  LEASE ENDS\n" +
		"cl-1a2b3c4d  agent.example.org  hosted   Running  v1.34.1  -\n"
	if b.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestAgentListRefusesAnAnswerWithoutAList(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces", 200, okBody(spaceRow))
	_, err := h.ctx.SpaceList(context.Background())
	mustErr(t, err)
	if got := h.errOut.String(); got != "[error] kubehz space list: the api answered without a list of spaces\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestAgentSpaceGetProjectsTheDetail(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces/sp-1a2b3c4d", 200, okBody(spaceDetailRow))
	d, err := h.ctx.SpaceGet(context.Background(), "sp-1a2b3c4d")
	mustOK(t, err, h.output())
	var b bytes.Buffer
	d.WriteText(&b)
	wantText := "ID:          sp-1a2b3c4d\n" +
		"Name:        Acme Prod\n" +
		"Slug:        acme\n" +
		"Status:      Active\n" +
		"Nodes:       1 of 2\n" +
		"Namespaces:  acme (limit 1)\n" +
		"Object cap:  256 KiB\n" +
		"Endpoint:    https://acme.k8s.kubehz.example\n" +
		"Lease ends:  2026-10-04T14:00:00.000Z\n" +
		"Created:     2026-10-04T12:00:00.000Z\n"
	if b.String() != wantText {
		t.Errorf("text:\n%s\nwant:\n%s", b.String(), wantText)
	}
	got := encode(t, d)
	for _, want := range []string{
		`"nodeCount": 1,`,
		"\"namespaces\": [\n    \"acme\"\n  ],",
		`"endpoint": "https://acme.k8s.kubehz.example",`,
		"\"nodes\": [\n    {\n      \"name\": \"worker-1\",\n      \"status\": \"Ready\"\n    }\n  ]",
	} {
		mustContain(t, got, want)
	}
	// The record fields come first, the detail fields last (the bash
	// twin's `space + {…}` keeps that order).
	if strings.Index(got, `"createdAt"`) > strings.Index(got, `"endpoint"`) {
		t.Errorf("field order:\n%s", got)
	}
}

func TestAgentClusterGetText(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/clusters/cl-1a2b3c4d", 200, okBody(clusterRow))
	r, err := h.ctx.ClusterGet(context.Background(), "cl-1a2b3c4d")
	mustOK(t, err, h.output())
	var b bytes.Buffer
	r.WriteText(&b)
	want := "ID:          cl-1a2b3c4d\n" +
		"Domain:      agent.example.org\n" +
		"Hosting:     hosted\n" +
		"Status:      Running\n" +
		"Health:      healthy\n" +
		"Region:      fsn1\n" +
		"Version:     v1.34.1\n" +
		"Apiservers:  1\n" +
		"Endpoint:    https://203.0.113.7:6443\n" +
		"Lease ends:  -\n" +
		"Created:     2026-10-04T12:00:00.000Z\n"
	if b.String() != want {
		t.Errorf("text:\n%s\nwant:\n%s", b.String(), want)
	}
	if got := encode(t, r); !strings.HasPrefix(got, "{\n  \"id\": \"cl-1a2b3c4d\",\n  \"domain\": \"agent.example.org\",") ||
		!strings.Contains(got, `"leaseExpiresAt": null,`) || strings.Contains(got, "workers") {
		t.Errorf("json:\n%s", got)
	}
}

func TestAgentCreateSendsOnlyTheNumbersItWasGiven(t *testing.T) {
	h := agentHarness(t)
	h.handle("POST /api/spaces", 201, okBody(spaceRow))
	_, err := h.ctx.SpaceCreate(context.Background(), SpaceCreateOptions{Name: "Acme Prod", Slug: "acme"})
	mustOK(t, err, h.output())
	if r := h.lastReq("POST", "/api/spaces"); r == nil || r.Body != `{"name":"Acme Prod","slug":"acme"}` {
		t.Fatalf("body = %+v", r)
	}

	h.reset()
	r, err := h.ctx.SpaceCreate(context.Background(), SpaceCreateOptions{
		Name: "Acme", Slug: "acme", Nodes: "2", Namespaces: "008", ObjectCapKiB: "256", Region: "fsn1", LeaseHours: "0720",
	})
	mustOK(t, err, h.output())
	var body map[string]any
	if err := json.Unmarshal([]byte(h.lastReq("POST", "/api/spaces").Body), &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "Acme", "slug": "acme", "maxNodes": 2.0, "maxNamespaces": 8.0, "maxObjectKiB": 256.0, "region": "fsn1", "leaseHours": 720.0}
	if len(body) != len(want) {
		t.Errorf("body = %v, want %v", body, want)
	}
	for k, v := range want {
		if body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, body[k], v)
		}
	}
	var b bytes.Buffer
	r.WriteText(&b)
	if strings.Contains(b.String(), "Endpoint:") || !strings.HasPrefix(b.String(), "ID:          sp-1a2b3c4d\n") {
		t.Errorf("create text:\n%s", b.String())
	}
}

func TestAgentCreateRefusesABadNumberBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		o    SpaceCreateOptions
		want string
	}{
		{SpaceCreateOptions{Nodes: "0"}, "--nodes 0 is not valid"},
		{SpaceCreateOptions{Nodes: "-1"}, "--nodes -1 is not valid"},
		{SpaceCreateOptions{Namespaces: "2.5"}, "--namespaces 2.5 is not valid"},
		{SpaceCreateOptions{ObjectCapKiB: "99999999999999999999"}, "--object-cap-kib 99999999999999999999 is not valid"},
		{SpaceCreateOptions{Nodes: "+5"}, "--nodes +5 is not valid"},
		{SpaceCreateOptions{LeaseHours: "721"}, "--lease-hours 721 is not valid"},
	} {
		h := agentHarness(t)
		tc.o.Name, tc.o.Slug = "Acme", "acme"
		_, err := h.ctx.SpaceCreate(context.Background(), tc.o)
		mustErr(t, err)
		mustContain(t, h.errOut.String(), "[error] kubehz space create acme: "+tc.want+"\n")
		if n := len(h.reqs()); n != 0 {
			t.Errorf("%s: %d requests, want none", tc.want, n)
		}
	}
}

func TestAgentLeaseSendsTheHoursAndChecksTheirRange(t *testing.T) {
	h := agentHarness(t)
	h.handle("PATCH /api/spaces/sp-1a2b3c4d/lease", 200, okBody(`{"id":"sp-1a2b3c4d","leaseExpiresAt":"2026-10-05T12:00:00.000Z"}`))
	r, err := h.ctx.SpaceLease(context.Background(), "sp-1a2b3c4d", "024")
	mustOK(t, err, h.output())
	if req := h.lastReq("PATCH", "/api/spaces/sp-1a2b3c4d/lease"); req == nil || req.Body != `{"hours":24}` {
		t.Fatalf("lease request = %+v", req)
	}
	var b bytes.Buffer
	r.WriteText(&b)
	if b.String() != "space sp-1a2b3c4d: the lease ends 2026-10-05T12:00:00.000Z\n" {
		t.Errorf("text = %q", b.String())
	}
	if got := encode(t, r); got != "{\n  \"id\": \"sp-1a2b3c4d\",\n  \"leaseExpiresAt\": \"2026-10-05T12:00:00.000Z\"\n}\n" {
		t.Errorf("json = %q", got)
	}

	for _, hours := range []string{"0", "721", "x", "", "1e3", "99999999999999999999"} {
		h := agentHarness(t)
		_, err := h.ctx.ClusterLease(context.Background(), "cl-1a2b3c4d", hours)
		mustErr(t, err)
		want := "[error] kubehz cluster lease cl-1a2b3c4d: --hours " + hours + " is not valid\n  Use a whole number of hours from 1 to 720.\n"
		if h.errOut.String() != want {
			t.Errorf("hours %q: stderr = %q", hours, h.errOut.String())
		}
		if len(h.reqs()) != 0 {
			t.Errorf("hours %q reached the api", hours)
		}
	}
}

func TestAgentDeleteText(t *testing.T) {
	h := agentHarness(t)
	h.handle("DELETE /api/spaces/sp-1a2b3c4d", 200, okBody(`{"id":"sp-1a2b3c4d","status":"Deleting"}`))
	r, err := h.ctx.SpaceDelete(context.Background(), "sp-1a2b3c4d")
	mustOK(t, err, h.output())
	var b bytes.Buffer
	r.WriteText(&b)
	if b.String() != "space sp-1a2b3c4d: Deleting\n" {
		t.Errorf("text = %q", b.String())
	}
	if got := encode(t, r); got != "{\n  \"id\": \"sp-1a2b3c4d\",\n  \"status\": \"Deleting\"\n}\n" {
		t.Errorf("json = %q", got)
	}
}

// The id goes into a URL path: nothing but the api's own shape may reach a
// request, and nothing reaches the token endpoint either.
func TestAgentIDShapeIsCheckedBeforeAnyRequest(t *testing.T) {
	for _, tc := range []struct {
		run  func(c *Context) error
		want string
	}{
		{func(c *Context) error { _, err := c.SpaceGet(context.Background(), "../clusters"); return err },
			"[error] kubehz space get: ../clusters is not a space id\n  A space id is sp- and 1 to 64 letters, digits or dashes. List them: lo kubehz space list\n"},
		{func(c *Context) error { _, err := c.SpaceDelete(context.Background(), "sp-1/../../x"); return err },
			"[error] kubehz space delete: sp-1/../../x is not a space id\n"},
		{func(c *Context) error { _, err := c.ClusterGet(context.Background(), "sp-1a2b3c4d"); return err },
			"[error] kubehz cluster get: sp-1a2b3c4d is not a cluster id\n  A cluster id is cl- and"},
		{func(c *Context) error {
			_, err := c.ClusterLease(context.Background(), "cl-"+strings.Repeat("a", 65), "1")
			return err
		},
			"is not a cluster id"},
		{func(c *Context) error {
			_, err := c.SpaceKubeconfig(context.Background(), "sp-a?b", "kc", false)
			return err
		},
			"[error] kubehz space kubeconfig: sp-a?b is not a space id"},
		{func(c *Context) error { _, err := c.SpaceGet(context.Background(), "sp-\x1b[2Jx"); return err },
			"[error] kubehz space get: sp-[2Jx is not a space id"},
	} {
		h := agentHarness(t)
		mustErr(t, tc.run(h.ctx))
		mustContain(t, h.errOut.String(), tc.want)
		if n := len(h.reqs()); n != 0 {
			t.Errorf("%q: %d requests, want none", tc.want, n)
		}
	}
}

func TestAgentRefusalsNameTheStatusTheCodeAndTheNextStep(t *testing.T) {
	type tc struct {
		name     string
		status   int
		body     string
		tokenEnv bool // KUBEHZ_TOKEN instead of the agent key
		want     string
	}
	for _, c := range []tc{
		{"401 agent key", 401, refusalBody("UNAUTHORIZED", "Invalid or expired agent key", "api help"), false,
			"[error] kubehz space get sp-1a2b3c4d: the api refused the request (HTTP 401 UNAUTHORIZED): Invalid or expired agent key\n" +
				"  The api did not accept the agent key. A revoked or expired key needs a new key from a tenant owner.\n"},
		{"401 KUBEHZ_TOKEN", 401, refusalBody("UNAUTHORIZED", "Invalid or expired API token", "api help"), true,
			"  The api did not accept KUBEHZ_TOKEN. Mint a new token in the kubehz dashboard.\n"},
		{"scope, agent key", 403, refusalBody("TOKEN_SCOPE_MISSING", "This endpoint requires the 'clusters:write' scope", "api help"), false,
			"(HTTP 403 TOKEN_SCOPE_MISSING): This endpoint requires the 'clusters:write' scope\n" +
				"  The agent key can read but not write. Use an agent key with the role editor or admin.\n"},
		// A KUBEHZ_TOKEN can hold clusters:write without read: lo cannot
		// say which scope is missing, the api's help does.
		{"scope, KUBEHZ_TOKEN: the api's help", 403, refusalBody("TOKEN_SCOPE_MISSING", "This endpoint requires the 'read' scope", "Mint a token carrying it."), true,
			"(HTTP 403 TOKEN_SCOPE_MISSING): This endpoint requires the 'read' scope\n  Mint a token carrying it.\n"},
		// The api never answers AGENT_KEY_OUT_OF_SCOPE on these routes (a
		// space outside the reach is a 404); if it does, its help stands.
		{"out of scope: the api's help", 403, refusalBody("AGENT_KEY_OUT_OF_SCOPE", "tenant-wide", "Use an agent key with the scope tenant."), false,
			"(HTTP 403 AGENT_KEY_OUT_OF_SCOPE): tenant-wide\n  Use an agent key with the scope tenant.\n"},
		{"spend cap", 409, refusalBody("AGENT_KEY_SPEND_CAP", "This agent key spent 1200 of its 1000 cents this month", "api help"), false,
			"(HTTP 409 AGENT_KEY_SPEND_CAP): This agent key spent 1200 of its 1000 cents this month\n" +
				"  Delete what the agent key created, or ask a tenant owner for a key with a higher spend cap. The count starts again on the first day of the month (UTC).\n"},
		{"create unconfirmed", 409, refusalBody("CLUSTER_CREATE_UNCONFIRMED", "The create of this cluster is not confirmed yet", "api help"), true,
			"(HTTP 409 CLUSTER_CREATE_UNCONFIRMED): The create of this cluster is not confirmed yet\n" +
				"  The platform is still checking the create of this cluster. Usually, it keeps or removes the cluster in about 3 minutes. When the check fails, it tries again about 15 minutes later. Do not create the cluster again. Run the command again after that.\n"},
		{"not found", 404, refusalBody("NOT_FOUND", "Space not found", "api help"), false,
			"  No space sp-1a2b3c4d exists, or the credential cannot reach it. List what it reaches: lo kubehz space list\n"},
		{"unknown code: the api help", 409, refusalBody("KUBECONFIG_NOT_READY", "not ready", "Poll the space, then retry."), false,
			"(HTTP 409 KUBECONFIG_NOT_READY): not ready\n  Poll the space, then retry.\n"},
		{"no body", 502, ``, false,
			"(HTTP 502): no reason given\n"},
		{"a code in another shape is not repeated", 418, `{"data":{"code":"x; rm","message":"m"}}`, false,
			"(HTTP 418): m\n"},
		// A message or help that is not a string counts as absent: lo
		// never prints a rendering of the api's JSON.
		{"a number as the message, an object as the help", 400, `{"data":{"code":"BAD_REQUEST","message":5,"help":{"a":1}}}`, false,
			"(HTTP 400 BAD_REQUEST): no reason given\n"},
		{"the top-level message when data.message is no string", 400, `{"data":{"message":["x"]},"message":"top"}`, false,
			"(HTTP 400): top\n"},
		{"server strings are cleaned", 400, `{"data":{"code":"BAD_REQUEST","message":"a\u001b[2Jb\nc\td\u009b\u202e\u2069","help":"\u001b]0;t\u0007c\n\u0085"}}`, false,
			"(HTTP 400 BAD_REQUEST): a[2Jbcd\n  ]0;tc\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := agentHarness(t)
			if c.tokenEnv {
				delete(h.env, EnvAgentClientID)
				delete(h.env, EnvAgentClientSecret)
				h.env["KUBEHZ_TOKEN"] = "khzt_x"
			}
			h.handle("GET /api/spaces/sp-1a2b3c4d", c.status, c.body)
			_, err := h.ctx.SpaceGet(context.Background(), "sp-1a2b3c4d")
			mustErr(t, err)
			got := h.errOut.String()
			mustContain(t, got, c.want)
			if strings.Contains(got, "api help") {
				t.Errorf("lo's hint and the api's help both printed:\n%s", got)
			}
			if strings.Count(got, "\n") > 2 {
				t.Errorf("more than two lines:\n%s", got)
			}
		})
	}
}

// A 404 on a list names no id: the api's help is the next step.
func TestAgentListRefusalNamesNoID(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces", 404, refusalBody("NOT_FOUND", "Not found", "Check KUBEHZ_API_URL."))
	_, err := h.ctx.SpaceList(context.Background())
	mustErr(t, err)
	want := "[error] kubehz space list: the api refused the request (HTTP 404 NOT_FOUND): Not found\n  Check KUBEHZ_API_URL.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

func TestAgentCreateHintsNameTheFlags(t *testing.T) {
	for code, want := range map[string]string{
		"SPACE_LIMITS_ABOVE_FREE":   "  Pick a different slug.\n",
		"SPACE_LIMITS_ABOVE_SHARED": "  Lower --nodes, --namespaces or --object-cap-kib. A value you leave out takes the platform default.\n",
		"NO_SHARD_AVAILABLE":        "  This is a platform capacity limit, not an account limit. Try again later, or name another region with --region.\n",
		"SHARD_AT_CAPACITY":         "  This is a platform capacity limit, not an account limit. Try again later, or name another region with --region.\n",
		"SPACE_EXISTS":              "  Pick a different slug.\n",
	} {
		h := agentHarness(t)
		h.handle("POST /api/spaces", 409, refusalBody(code, "refused", "Pick a different slug."))
		_, err := h.ctx.SpaceCreate(context.Background(), SpaceCreateOptions{Name: "A", Slug: "acme"})
		mustErr(t, err)
		mustContain(t, h.errOut.String(), "[error] kubehz space create acme: the api refused the request (HTTP 409 "+code+"): refused\n"+want)
	}
}

func TestAgentSessionRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string // "" deletes the key
		want string
	}{
		{"no url", map[string]string{EnvAPIURL: ""},
			"[error] kubehz space list: KUBEHZ_API_URL is not set\n  Set it to the kubehz api, for example: export KUBEHZ_API_URL=https://api.kubehz.cloud\n"},
		{"no credential", map[string]string{EnvAgentClientID: "", EnvAgentClientSecret: ""},
			"[error] kubehz space list: no credential for the kubehz api\n  Set KUBEHZ_AGENT_CLIENT_ID and KUBEHZ_AGENT_CLIENT_SECRET (an agent key), or KUBEHZ_TOKEN.\n"},
		{"half a key", map[string]string{EnvAgentClientSecret: ""},
			"[error] kubehz space list: the agent key needs KUBEHZ_AGENT_CLIENT_SECRET\n" +
				"  The answer that created the key holds the four values: clientId, clientSecret, tokenEndpoint and tokenScope.\n"},
		{"no token url and scope", map[string]string{EnvAgentTokenURL: "", EnvAgentScope: ""},
			"[error] kubehz space list: the agent key needs KUBEHZ_AGENT_TOKEN_URL and KUBEHZ_AGENT_SCOPE\n"},
		{"three missing", map[string]string{EnvAgentClientID: "", EnvAgentTokenURL: "", EnvAgentScope: ""},
			"[error] kubehz space list: the agent key needs KUBEHZ_AGENT_CLIENT_ID, KUBEHZ_AGENT_TOKEN_URL and KUBEHZ_AGENT_SCOPE\n"},
		{"plain http token url", map[string]string{EnvAgentTokenURL: "http://id.example/t"},
			"[error] Token URL must use HTTPS: http://id.example/t\n"},
		{"a line break in KUBEHZ_TOKEN", map[string]string{EnvAgentClientID: "", EnvAgentClientSecret: "", "KUBEHZ_TOKEN": "a\nb"},
			"[error] kubehz space list: the access token holds a control character\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := agentHarness(t)
			for k, v := range tc.env {
				if v == "" {
					delete(h.env, k)
				} else {
					h.env[k] = v
				}
			}
			_, err := h.ctx.SpaceList(context.Background())
			mustErr(t, err)
			mustContain(t, h.errOut.String(), tc.want)
			for _, r := range h.reqs() {
				if strings.HasPrefix(r.Path, "/api/") {
					t.Errorf("reached the api: %+v", r)
				}
			}
		})
	}
}

func TestAgentFallsBackToKubehzToken(t *testing.T) {
	h := agentHarness(t)
	delete(h.env, EnvAgentClientID)
	delete(h.env, EnvAgentClientSecret)
	h.env["KUBEHZ_TOKEN"] = "khzt_tenant"
	h.handle("GET /api/spaces", 200, okBody(`[]`))
	l, err := h.ctx.SpaceList(context.Background())
	mustOK(t, err, h.output())
	reqs := h.reqs()
	if len(reqs) != 1 || reqs[0].Auth != "Bearer khzt_tenant" {
		t.Fatalf("requests = %+v, want one list call with KUBEHZ_TOKEN and no grant", reqs)
	}
	if got := encode(t, l); got != "{\n  \"spaces\": []\n}\n" {
		t.Errorf("empty list json = %q", got)
	}
	var b bytes.Buffer
	l.WriteText(&b)
	if b.String() != "ID  SLUG  NAME  STATUS  NODES  LEASE ENDS\n" {
		t.Errorf("empty table = %q", b.String())
	}
}

// The agent key wins over KUBEHZ_TOKEN: the commands are the agent's.
func TestAgentKeyWinsOverKubehzToken(t *testing.T) {
	h := agentHarness(t)
	h.env["KUBEHZ_TOKEN"] = "khzt_tenant"
	h.handle("GET /api/spaces", 200, okBody(`[]`))
	_, err := h.ctx.SpaceList(context.Background())
	mustOK(t, err, h.output())
	if r := h.lastReq("GET", "/api/spaces"); r == nil || r.Auth != "Bearer jwt-agent" {
		t.Fatalf("list request = %+v", r)
	}
}

// The commands share `lo kubehz token`'s cache: a second command asks the
// token endpoint nothing.
func TestAgentReusesTheTokenCache(t *testing.T) {
	h := agentHarness(t)
	h.env["XDG_CACHE_HOME"] = t.TempDir()
	h.handle("GET /api/clusters", 200, okBody(`[]`))
	for range 2 {
		_, err := h.ctx.ClusterList(context.Background())
		mustOK(t, err, h.output())
	}
	grants := 0
	for _, r := range h.reqs() {
		if r.Path == "/oauth/v2/token" {
			grants++
		}
	}
	if grants != 1 {
		t.Errorf("grants = %d, want 1 (the second call reads the cache)", grants)
	}
	entries, _ := os.ReadDir(filepath.Join(h.env["XDG_CACHE_HOME"], "lok8s", "kubehz-token"))
	if len(entries) != 1 {
		t.Errorf("cache entries = %d, want 1", len(entries))
	}
}

func TestAgentSendsTheAcceptHeaderAndNoBodyOnAGet(t *testing.T) {
	h := agentHarness(t)
	var accept, ctype string
	h.handleFunc("GET /api/spaces", func(w http.ResponseWriter, r *http.Request) {
		accept, ctype = r.Header.Get("Accept"), r.Header.Get("Content-Type")
		_, _ = w.Write([]byte(okBody(`[]`)))
	})
	_, err := h.ctx.SpaceList(context.Background())
	mustOK(t, err, h.output())
	if accept != "application/json" || ctype != "" {
		t.Errorf("Accept = %q, Content-Type = %q", accept, ctype)
	}
}

// A redirect is a refusal: the bearer never follows a Location header.
func TestAgentFollowsNoRedirect(t *testing.T) {
	h := agentHarness(t)
	h.handleFunc("GET /api/spaces/sp-1a2b3c4d", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})
	h.handle("GET /elsewhere", 200, okBody(spaceDetailRow))
	_, err := h.ctx.SpaceGet(context.Background(), "sp-1a2b3c4d")
	mustErr(t, err)
	mustContain(t, h.errOut.String(), "the api refused the request (HTTP 302): no reason given")
	if h.anyReq("GET", "/elsewhere") {
		t.Error("the redirect was followed")
	}
}

func TestAgentNoAnswer(t *testing.T) {
	h := agentHarness(t)
	h.env[EnvAPIURL] = "https://127.0.0.1:1"
	_, err := h.ctx.SpaceGet(context.Background(), "sp-1a2b3c4d")
	mustErr(t, err)
	want := "[error] kubehz space get sp-1a2b3c4d: the api at https://127.0.0.1:1 did not answer\n" +
		"  Check KUBEHZ_API_URL and the network. Then try again.\n"
	if h.errOut.String() != want {
		t.Errorf("stderr = %q", h.errOut.String())
	}
}

func TestAgentRecordRefusesAnAnswerWithoutData(t *testing.T) {
	for _, body := range []string{`<html>gateway</html>`, `{"ok":true,"data":[]}`, `{"id":"sp-1a2b3c4d"}`} {
		h := agentHarness(t)
		h.handle("GET /api/spaces/sp-1a2b3c4d", 200, body)
		_, err := h.ctx.SpaceGet(context.Background(), "sp-1a2b3c4d")
		mustErr(t, err)
		if got := h.errOut.String(); got != "[error] kubehz space get sp-1a2b3c4d: the api answered without a space record\n" {
			t.Errorf("%s: stderr = %q", body, got)
		}
	}
}

const agentKubeconfigYAML = "apiVersion: v1\nkind: Config\nusers:\n- name: kubehz-acme-agent\n  user:\n    exec:\n      command: lo\n      args: [kubehz, token]\n"

func TestAgentKubeconfigWritesAPrivateFileAndPrintsThePath(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	if err := os.WriteFile(file, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := h.ctx.SpaceKubeconfig(context.Background(), "sp-1a2b3c4d", file, true)
	mustOK(t, err, h.output())
	if got := readFile(t, file); got != agentKubeconfigYAML {
		t.Errorf("file = %q", got)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	var b bytes.Buffer
	r.WriteText(&b)
	if b.String() != file+"\n" || h.out.Len() != 0 {
		t.Errorf("text = %q, stdout = %q: the kubeconfig must not reach a stream", b.String(), h.out.String())
	}
	if got := encode(t, r); got != "{\n  \"id\": \"sp-1a2b3c4d\",\n  \"file\": \""+file+"\"\n}\n" {
		t.Errorf("json = %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries: a temporary file was left behind", len(entries))
	}
}

func TestAgentKubeconfigRefusals(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, file string
		status     int
		body, want string
	}{
		{"a missing directory", filepath.Join(dir, "no", "kc.yaml"), 200, agentKubeconfigYAML,
			"[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write " + filepath.Join(dir, "no", "kc.yaml") + ": " + filepath.Join(dir, "no") +
				" does not exist or cannot be read\n  Name a file in a directory that exists and that you can write to.\n"},
		{"an empty answer", filepath.Join(dir, "kc.yaml"), 200, " \n",
			"[error] kubehz cluster kubeconfig cl-1a2b3c4d: the api answered without a kubeconfig\n"},
		{"a refusal", filepath.Join(dir, "kc.yaml"), 409, refusalBody("KUBECONFIG_NOT_READY", "not ready", "Poll, then retry."),
			"(HTTP 409 KUBECONFIG_NOT_READY): not ready\n  Poll, then retry.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := agentHarness(t)
			h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", tc.status, tc.body)
			_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", tc.file, false)
			mustErr(t, err)
			mustContain(t, h.errOut.String(), tc.want)
			if _, err := os.Stat(filepath.Join(dir, "kc.yaml")); err == nil {
				t.Error("a file was written")
			}
		})
	}
}

// The path rules run before any request, so a refused path costs no grant
// and no download: a directory (or a link to one) is never replaced, a
// path that exists needs force, and force writes through a link.
func TestAgentKubeconfigPathRules(t *testing.T) {
	type tc struct {
		name  string
		setup func(dir string) string // returns the --file value
		force bool
		want  string // stderr, "" for success
		check func(t *testing.T, dir string)
	}
	for _, c := range []tc{
		{"a directory", func(dir string) string { return dir }, true,
			"[error] kubehz cluster kubeconfig cl-1a2b3c4d: DIR is a directory\n  Name a file, not a directory.\n", nil},
		{"a link to a directory", func(dir string) string {
			must(t, os.Mkdir(filepath.Join(dir, "d"), 0o755))
			must(t, os.Symlink(filepath.Join(dir, "d"), filepath.Join(dir, "link")))
			return filepath.Join(dir, "link")
		}, true, "[error] kubehz cluster kubeconfig cl-1a2b3c4d: DIR/link is a directory\n", func(t *testing.T, dir string) {
			if fi, err := os.Lstat(filepath.Join(dir, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Error("the link to the directory was replaced")
			}
		}},
		{"a file that exists, no force", func(dir string) string {
			must(t, os.WriteFile(filepath.Join(dir, "config"), []byte("contexts"), 0o600))
			return filepath.Join(dir, "config")
		}, false, "[error] kubehz cluster kubeconfig cl-1a2b3c4d: DIR/config exists\n  Pass --force to replace it, or name a new file.\n",
			func(t *testing.T, dir string) {
				if got := readFile(t, filepath.Join(dir, "config")); got != "contexts" {
					t.Errorf("the file changed: %q", got)
				}
			}},
		{"a link to a file, no force", func(dir string) string {
			must(t, os.WriteFile(filepath.Join(dir, "real"), []byte("contexts"), 0o600))
			must(t, os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")))
			return filepath.Join(dir, "link")
		}, false, "DIR/link exists\n", nil},
		{"a link to nothing, no force", func(dir string) string {
			must(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "link")))
			return filepath.Join(dir, "link")
		}, false, "DIR/link exists\n", nil},
		{"a link to nothing, force", func(dir string) string {
			must(t, os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "link")))
			return filepath.Join(dir, "link")
		}, true, "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write DIR/link\n", nil},
		{"force replaces a file", func(dir string) string {
			must(t, os.WriteFile(filepath.Join(dir, "config"), []byte("contexts"), 0o644))
			return filepath.Join(dir, "config")
		}, true, "", func(t *testing.T, dir string) {
			if got := readFile(t, filepath.Join(dir, "config")); got != agentKubeconfigYAML {
				t.Errorf("file = %q", got)
			}
		}},
		{"force writes through a link", func(dir string) string {
			must(t, os.Mkdir(filepath.Join(dir, "kube"), 0o755))
			must(t, os.WriteFile(filepath.Join(dir, "kube", "real"), []byte("contexts"), 0o644))
			must(t, os.Symlink(filepath.Join("kube", "real"), filepath.Join(dir, "link")))
			return filepath.Join(dir, "link")
		}, true, "", func(t *testing.T, dir string) {
			if fi, err := os.Lstat(filepath.Join(dir, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatal("the link was replaced")
			}
			if got := readFile(t, filepath.Join(dir, "kube", "real")); got != agentKubeconfigYAML {
				t.Errorf("link target = %q", got)
			}
			if entries, _ := os.ReadDir(filepath.Join(dir, "kube")); len(entries) != 1 {
				t.Errorf("kube/ holds %d entries: a temporary file was left behind", len(entries))
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := agentHarness(t)
			h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
			dir := linkTestDir(t)
			file := c.setup(dir)
			_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", file, c.force)
			got := strings.ReplaceAll(h.errOut.String(), dir, "DIR")
			if c.want == "" {
				mustOK(t, err, got)
			} else {
				mustErr(t, err)
				mustContain(t, got, c.want)
				if n := len(h.reqs()); n != 0 {
					t.Errorf("%d requests: a refused path must cost no grant and no download", n)
				}
			}
			if c.check != nil {
				c.check(t, dir)
			}
		})
	}
}

// A link is written through only to a file of this user: a link that
// points at another user's file is refused before any request. The other
// owner and mode rules of the walk are in agent_link_unix_test.go.
func TestAgentKubeconfigWritesThroughOnlyToAFileOfOurOwn(t *testing.T) {
	foreign := foreignFile(t)
	h := agentHarness(t)
	h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	dir := linkTestDir(t)
	must(t, os.Symlink(foreign, filepath.Join(dir, "link")))
	_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", filepath.Join(dir, "link"), true)
	mustErr(t, err)
	real := realDir(t, foreign)
	want := "[error] kubehz cluster kubeconfig cl-1a2b3c4d: DIR/link is a link, and its path is not safe: " + real + " belongs to another user (uid 0)\n" +
		linkNotSafeHint
	if got := strings.ReplaceAll(h.errOut.String(), dir, "DIR"); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if n := len(h.reqs()); n != 0 {
		t.Errorf("%d requests: the refusal must come before the grant", n)
	}
}

// The owner check reads the user from the Euid seam: the parts of ours on
// the way count as another user's when the user is root.
func TestAgentKubeconfigOwnerCheckReadsTheEffectiveUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the test needs a user other than root")
	}
	h := agentHarness(t)
	h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	h.ctx.Euid = func() int { return 0 }
	dir := linkTestDir(t)
	must(t, os.WriteFile(filepath.Join(dir, "real"), []byte("contexts"), 0o600))
	must(t, os.Symlink("real", filepath.Join(dir, "link")))
	_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", filepath.Join(dir, "link"), true)
	mustErr(t, err)
	mustContain(t, h.errOut.String(), "link is a link, and its path is not safe: ")
	mustContain(t, h.errOut.String(), fmt.Sprintf(" belongs to another user (uid %d)\n", os.Geteuid()))
	if got := readFile(t, filepath.Join(dir, "real")); got != "contexts" {
		t.Errorf("real = %q", got)
	}
}

// A path in /proc or /dev, or a path that a link sends there, is refused
// with and without force, before any request. /proc/self resolves against
// lo itself: with force, /proc/self/fd/N named the file open on that
// descriptor (lo's stdout for N=1) and /proc/self/exe the lo binary.
func TestAgentKubeconfigRefusesProcAndDev(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("this system has no /proc")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	must(t, os.WriteFile(out, []byte("stdout"), 0o600))
	f, err := os.OpenFile(out, os.O_WRONLY, 0)
	must(t, err)
	t.Cleanup(func() { _ = f.Close() })
	fd := strconv.Itoa(int(f.Fd()))
	must(t, os.Symlink("/proc/self/fd/"+fd, filepath.Join(dir, "link")))
	must(t, os.Symlink("/proc/self/fd", filepath.Join(dir, "fds")))
	must(t, os.Symlink("fds", filepath.Join(dir, "hop")))
	up := dir + strings.Repeat("/..", strings.Count(dir, "/"))
	for _, tc := range []struct {
		file  string
		force bool
	}{
		{"/proc/self/fd/" + fd, true},
		{"/proc/self/fd/" + fd, false},
		{"/dev/fd/" + fd, true},
		{"/dev", true},
		{up + "/proc/self/fd/" + fd, true},
		{filepath.Join(dir, "link"), true},
		{filepath.Join(dir, "hop", fd), true},
	} {
		t.Run(fmt.Sprintf("%s force=%v", strings.ReplaceAll(tc.file, dir, "DIR"), tc.force), func(t *testing.T) {
			h := agentHarness(t)
			h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
			_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", tc.file, tc.force)
			mustErr(t, err)
			want := "[error] kubehz cluster kubeconfig cl-1a2b3c4d: " + tc.file +
				" is in /proc or /dev, or a link in its path points there\n" +
				"  lo does not write to /proc or /dev. Name a file in another directory.\n"
			if got := h.errOut.String(); got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
			if n := len(h.reqs()); n != 0 {
				t.Errorf("%d requests: the refusal must come before the grant", n)
			}
			if got := readFile(t, out); got != "stdout" {
				t.Errorf("out.txt = %q: the write went through /proc", got)
			}
		})
	}
}

// inProcOrDev walks the path itself: only the components /proc and /dev
// count, a link counts by where it points, and a relative path starts at
// the working directory.
func TestInProcOrDev(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	must(t, os.Mkdir("proc", 0o755))
	must(t, os.Symlink("/proc", "p"))
	must(t, os.Symlink("/dev/shm", "shm"))
	must(t, os.Symlink("proc", "here"))
	must(t, os.Symlink("loop", "loop"))
	must(t, os.Mkdir("real", 0o755))
	must(t, os.Symlink("/proc", filepath.Join("real", "x")))
	must(t, os.Symlink("real", "sub"))
	for file, want := range map[string]bool{
		"/proc": true, "/proc/": true, "/proc/1/x": true, "//proc/x": true, "/./dev/x": true,
		"/dev": true, "/dev/null": true, "/tmp/../dev/x": true,
		"/procfs/x": false, "/devx": false, "/device/x": false, "/tmp/proc": false,
		"kc.yaml": false, "proc/kc.yaml": false, "here/kc.yaml": false,
		"p": true, "p/kc.yaml": true, "shm/kc.yaml": true, "proc/../p/x": true, "sub/x/1": true,
		"loop/kc.yaml": false,
	} {
		if got := inProcOrDev(file); got != want {
			t.Errorf("inProcOrDev(%q) = %v, want %v", file, got, want)
		}
	}
}

// The publish step on its own: without force, link(2) refuses a path that
// exists, whatever the checks before saw. The race window after the last
// check is too short to hit through the command.
func TestPublishKubeconfigNeverReplacesWithoutForce(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	must(t, os.WriteFile(file, []byte("planted"), 0o644))
	if _, err := publishKubeconfig(file, []byte("new"), false); err == nil {
		t.Fatal("publish without force replaced a file that exists")
	}
	if got := readFile(t, file); got != "planted" {
		t.Errorf("file = %q", got)
	}
	if _, err := publishKubeconfig(file, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, file); got != "new" {
		t.Errorf("file = %q after force", got)
	}
	fresh := filepath.Join(dir, "fresh.yaml")
	if _, err := publishKubeconfig(fresh, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(fresh); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("dir holds %d entries: a temporary file was left behind", len(entries))
	}
}

// failLink is a linkKubeconfig that fails as link(2) fails with errno. It
// runs hook first (when not nil), and counts its calls in calls.
func failLink(t *testing.T, errno syscall.Errno, calls *int, hook func()) func(*os.Root, string, string) error {
	t.Helper()
	return func(_ *os.Root, oldname, newname string) error {
		*calls++
		if hook != nil {
			hook()
		}
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: errno}
	}
}

// dirNames lists the names in dir: the temporary file must not be one.
func dirNames(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return strings.Join(names, " ")
}

// A link error that is not EEXIST, with no file at the path, takes the
// exclusive create: mode 0600 (no group or other bits, whatever the
// umask), the bytes of the download, and no temporary file stays. A file
// system with no hard links fails the link with EPERM, ENOTSUP,
// EOPNOTSUPP or ENOSYS (a FUSE file system with no link operation). The
// bash twin is in kubehz_agent_test.bats. Not parallel: it replaces
// linkKubeconfig.
func TestPublishKubeconfigWithoutHardLinks(t *testing.T) {
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"EPERM", syscall.EPERM},
		{"ENOSYS", syscall.ENOSYS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "kc.yaml")
			calls := 0
			linkKubeconfig = failLink(t, tc.errno, &calls, nil)
			wide, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
			if err != nil {
				t.Fatalf("publish: %v", err)
			}
			if wide != 0 {
				t.Errorf("wide = %04o, want 0", wide)
			}
			if calls != 1 {
				t.Errorf("the link ran %d times, want 1", calls)
			}
			if got := readFile(t, file); got != agentKubeconfigYAML {
				t.Errorf("file = %q, want %q", got, agentKubeconfigYAML)
			}
			fi, err := os.Lstat(file)
			if err != nil {
				t.Fatal(err)
			}
			if perm := fi.Mode().Perm(); !fi.Mode().IsRegular() || perm&0o077 != 0 || perm&0o600 != 0o600 {
				t.Errorf("mode = %v, want a regular file with 0600", fi.Mode())
			}
			if got := dirNames(t, dir); got != "kc.yaml" {
				t.Errorf("dir holds %q: a temporary file was left behind", got)
			}
		})
	}
}

// failWrite is a writeKubeconfigFile that writes the first 3 bytes, runs
// hook (when not nil) and fails as a full disk fails. It counts its calls
// in calls.
func failWrite(t *testing.T, calls *int, hook func()) func(*os.File, []byte) error {
	t.Helper()
	return func(f *os.File, data []byte) error {
		*calls++
		if _, err := f.Write(data[:3]); err != nil {
			return err
		}
		if hook != nil {
			hook()
		}
		return &os.PathError{Op: "write", Path: f.Name(), Err: syscall.ENOSPC}
	}
}

// A write that fails after the exclusive create leaves a part of the file
// at the path: the publish removes it and fails with a
// kubeconfigWriteError that names the cause. A file that takes the place
// of the made file during the write stays: the publish removes only the
// file that the create made, and left says that the part stays. Not
// parallel: it replaces linkKubeconfig and writeKubeconfigFile.
func TestPublishKubeconfigWriteFailureRemovesTheFile(t *testing.T) {
	link, write := linkKubeconfig, writeKubeconfigFile
	t.Cleanup(func() { linkKubeconfig, writeKubeconfigFile = link, write })
	links := 0
	linkKubeconfig = failLink(t, syscall.EPERM, &links, nil)
	for _, tc := range []struct {
		name string
		swap bool
		want string
	}{
		{name: "the part is removed", want: ""},
		{name: "a file that took its place stays", swap: true, want: "kc.yaml kc.yaml.made"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "kc.yaml")
			writes := 0
			var hook func()
			if tc.swap {
				hook = func() {
					must(t, os.Rename(file, file+".made"))
					must(t, os.WriteFile(file, []byte("other"), 0o600))
				}
			}
			writeKubeconfigFile = failWrite(t, &writes, hook)
			_, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
			var werr *kubeconfigWriteError
			if !errors.As(err, &werr) {
				t.Fatalf("publish: err = %v, want a kubeconfigWriteError", err)
			}
			if got := werr.cause(); got != "no space left on device" {
				t.Errorf("cause = %q", got)
			}
			if werr.left != tc.swap {
				t.Errorf("left = %v, want %v", werr.left, tc.swap)
			}
			if writes != 1 {
				t.Errorf("the write ran %d times, want 1", writes)
			}
			if got := dirNames(t, dir); got != tc.want {
				t.Errorf("dir holds %q, want %q", got, tc.want)
			}
			if tc.swap {
				if got := readFile(t, file); got != "other" {
					t.Errorf("file = %q: the file that took the place of the made file was changed", got)
				}
			}
		})
	}
}

// A link error with a file at the path refuses, and the file stays as it
// was: with EEXIST there is no second write, and with another error the
// exclusive create fails on the file before any write. Not parallel: it
// replaces linkKubeconfig and writeKubeconfigFile.
func TestPublishKubeconfigWithoutHardLinksKeepsAFileThatAppears(t *testing.T) {
	link, write := linkKubeconfig, writeKubeconfigFile
	t.Cleanup(func() { linkKubeconfig, writeKubeconfigFile = link, write })
	writes := 0
	writeKubeconfigFile = func(f *os.File, data []byte) error {
		writes++
		return write(f, data)
	}
	for _, tc := range []struct {
		name  string
		errno syscall.Errno
	}{
		{"EEXIST", syscall.EEXIST},
		{"EPERM", syscall.EPERM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			file := filepath.Join(dir, "kc.yaml")
			calls := 0
			linkKubeconfig = failLink(t, tc.errno, &calls, func() {
				must(t, os.WriteFile(file, []byte("planted"), 0o640))
			})
			_, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("publish: err = %v, want a refusal of the path that exists", err)
			}
			if got := readFile(t, file); got != "planted" {
				t.Errorf("file = %q: the file that appeared was replaced", got)
			}
			if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
				t.Errorf("mode = %v, want 0640 as planted", fi.Mode().Perm())
			}
			if got := dirNames(t, dir); got != "kc.yaml" {
				t.Errorf("dir holds %q: a temporary file was left behind", got)
			}
			if writes != 0 {
				t.Errorf("a second write ran %d times", writes)
			}
		})
	}
}

// EEXIST says that the path exists: the publish fails with the link error
// and writes nothing, also when no file is at the path by then. Not
// parallel: it replaces linkKubeconfig.
func TestPublishKubeconfigEEXISTHasNoFallback(t *testing.T) {
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	calls := 0
	linkKubeconfig = failLink(t, syscall.EEXIST, &calls, nil)
	_, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
	if !errors.Is(err, syscall.EEXIST) {
		t.Fatalf("publish: err = %v, want the link error EEXIST", err)
	}
	if calls != 1 {
		t.Errorf("the link ran %d times, want 1", calls)
	}
	if _, err := os.Lstat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file is at the path (lstat: %v): the publish wrote it after EEXIST", err)
	}
	if got := dirNames(t, dir); got != "" {
		t.Errorf("dir holds %q: a temporary file was left behind", got)
	}
}

// The fallback needs the temporary file at its name, the file that the
// publish wrote. When another process moves the temporary file during the
// link, the link source is missing (ENOENT): the publish fails with the
// link error and writes no file. Here the link step moves the temporary
// file away. Not parallel: it replaces linkKubeconfig.
func TestPublishKubeconfigNoFallbackWithoutTheTemporaryFile(t *testing.T) {
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	moved := filepath.Join(dir, "moved")
	calls := 0
	linkKubeconfig = func(_ *os.Root, oldname, newname string) error {
		calls++
		must(t, os.Rename(filepath.Join(dir, oldname), moved))
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOENT}
	}
	_, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("publish: err = %v, want the link error ENOENT", err)
	}
	if calls != 1 {
		t.Errorf("the link ran %d times, want 1", calls)
	}
	if _, err := os.Lstat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file is at the path (lstat: %v): the fallback ran without the temporary file", err)
	}
	if got := dirNames(t, dir); got != "moved" {
		t.Errorf("dir holds %q, want only the moved temporary file", got)
	}
}

// After the create, the path must still name the file of the create: else
// nothing is written or removed. Here the chmod step moves the new file
// away and puts another file at the path. Not parallel: it replaces
// linkKubeconfig, chmodKubeconfig and writeKubeconfigFile.
func TestPublishKubeconfigWritesNothingWhenThePathChangesAfterTheCreate(t *testing.T) {
	link, chmod, write := linkKubeconfig, chmodKubeconfig, writeKubeconfigFile
	t.Cleanup(func() { linkKubeconfig, chmodKubeconfig, writeKubeconfigFile = link, chmod, write })
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	calls, writes := 0, 0
	linkKubeconfig = failLink(t, syscall.EPERM, &calls, nil)
	chmodKubeconfig = func(f *os.File) error {
		must(t, os.Rename(file, file+".made"))
		must(t, os.WriteFile(file, []byte("other"), 0o600))
		return chmod(f)
	}
	writeKubeconfigFile = func(f *os.File, data []byte) error {
		writes++
		return write(f, data)
	}
	_, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
	if !errors.Is(err, errKubeconfigMoved) {
		t.Fatalf("publish: err = %v, want errKubeconfigMoved", err)
	}
	if writes != 0 {
		t.Errorf("the write ran %d times, want 0", writes)
	}
	if got := readFile(t, file); got != "other" {
		t.Errorf("file = %q: the file at the path was changed", got)
	}
	if got := readFile(t, file+".made"); got != "" {
		t.Errorf("the made file holds %q, want nothing", got)
	}
	if got := dirNames(t, dir); got != "kc.yaml kc.yaml.made" {
		t.Errorf("dir holds %q", got)
	}
}

// aclRights lists the entries of a getfacl listing that give a group or
// another user an effective right: the rights after "#effective:" when the
// mask cuts the entry, else the rights of the entry. The entry of the
// owner ("user::") does not count.
func aclRights(listing string) string {
	var bad []string
	for line := range strings.SplitSeq(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "user::") {
			continue
		}
		entry, effective, cut := strings.Cut(line, "#effective:")
		rights := strings.TrimSpace(effective)
		if !cut {
			rights = entry[strings.LastIndex(entry, ":")+1:]
		}
		if rights != "---" {
			bad = append(bad, line)
		}
	}
	return strings.Join(bad, "; ")
}

// A default ACL of the directory replaces the umask. Under one that gives
// the group and others read, the file of the fallback has mode 0600, and
// no entry of its ACL gives a group or another user an effective right.
// The Go create asks for 0600, so the file has no group or other bits
// already before the chmod: the chmod step reads the mode of the open file
// first. The bash twin asks for 0666, so its new file is open to others
// until the chmod, and it refuses there (kubehz_agent_test.bats). Skips
// without setfacl or getfacl, and on a file system of the test dir with
// no ACLs. Not parallel: it replaces linkKubeconfig and chmodKubeconfig.
func TestPublishKubeconfigUnderADefaultACL(t *testing.T) {
	setfacl, serr := exec.LookPath("setfacl")
	getfacl, gerr := exec.LookPath("getfacl")
	if serr != nil || gerr != nil {
		t.Skip("setfacl or getfacl is missing")
	}
	link, chmod := linkKubeconfig, chmodKubeconfig
	t.Cleanup(func() { linkKubeconfig, chmodKubeconfig = link, chmod })
	dir := t.TempDir()
	if out, err := exec.Command(setfacl, "-d", "-m", "u::rw,g::r,o::r", dir).CombinedOutput(); err != nil {
		t.Skipf("no ACLs on the file system of the test dir: %v: %s", err, out)
	}
	// The default ACL applies: a file that asks for 0666 gets 0644,
	// whatever the umask.
	probe := filepath.Join(dir, "probe")
	must(t, os.WriteFile(probe, nil, 0o666))
	if fi, err := os.Stat(probe); err != nil || fi.Mode().Perm() != 0o644 {
		t.Skipf("the default ACL does not apply on the file system of the test dir (stat: %v, %v)", fi, err)
	}
	must(t, os.Remove(probe))
	file := filepath.Join(dir, "kc.yaml")
	calls := 0
	linkKubeconfig = failLink(t, syscall.EPERM, &calls, nil)
	// The mode that the open gave, read before the chmod.
	var opened os.FileMode
	read := false
	chmodKubeconfig = func(f *os.File) error {
		if fi, err := f.Stat(); err == nil {
			opened, read = fi.Mode().Perm(), true
		}
		return f.Chmod(0o600)
	}
	wide, err := publishKubeconfig(file, []byte(agentKubeconfigYAML), false)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !read {
		t.Fatal("the chmod step did not read the mode of the open file")
	}
	if opened&0o077 != 0 {
		t.Errorf("the open gave mode %04o, want no group or other bits", opened)
	}
	if wide != 0 {
		t.Errorf("wide = %04o, want 0", wide)
	}
	fi, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %04o, want 0600", perm)
	}
	out, err := exec.Command(getfacl, "-cp", "--", file).Output()
	if err != nil {
		t.Fatalf("getfacl: %v", err)
	}
	if bad := aclRights(string(out)); bad != "" {
		t.Errorf("ACL entries with an effective right: %s (getfacl %q)", bad, out)
	}
}

// The command on a file system with no hard links: a new file is written
// and the command prints no error. A file that appears after the last path
// check (in the link step) stays, with the refusal of a path that exists.
// Not parallel: it replaces linkKubeconfig.
func TestAgentKubeconfigWithoutHardLinks(t *testing.T) {
	link := linkKubeconfig
	t.Cleanup(func() { linkKubeconfig = link })
	calls := 0
	linkKubeconfig = failLink(t, syscall.EPERM, &calls, nil)
	h := agentHarness(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	h.handle("GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	res, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", file, false)
	if err != nil {
		t.Fatalf("kubeconfig: %v (stderr %q)", err, h.errOut.String())
	}
	if res.File != file || h.errOut.String() != "" {
		t.Errorf("result file = %q, stderr = %q", res.File, h.errOut.String())
	}
	if got := readFile(t, file); got != agentKubeconfigYAML {
		t.Errorf("file = %q", got)
	}
	if calls != 1 {
		t.Errorf("the link ran %d times, want 1", calls)
	}

	race := filepath.Join(dir, "race.yaml")
	linkKubeconfig = failLink(t, syscall.EPERM, &calls, func() {
		must(t, os.WriteFile(race, []byte("planted"), 0o600))
	})
	_, err = h.ctx.SpaceKubeconfig(context.Background(), "sp-1a2b3c4d", race, false)
	mustErr(t, err)
	if calls != 2 {
		t.Errorf("the link ran %d times in all, want 2", calls)
	}
	want := "[error] kubehz space kubeconfig sp-1a2b3c4d: " + race + " exists\n  Pass --force to replace it, or name a new file.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := readFile(t, race); got != "planted" {
		t.Errorf("file = %q: the file that appeared was replaced", got)
	}
	if got := dirNames(t, dir); got != "kc.yaml race.yaml" {
		t.Errorf("dir holds %q: a temporary file was left behind", got)
	}

	// A write that fails after the create: the part is removed, and the
	// error names the cause, not a file that exists.
	write := writeKubeconfigFile
	t.Cleanup(func() { writeKubeconfigFile = write })
	writes := 0
	writeKubeconfigFile = failWrite(t, &writes, nil)
	linkKubeconfig = failLink(t, syscall.EPERM, &calls, nil)
	h.errOut.Reset()
	full := filepath.Join(dir, "full.yaml")
	_, err = h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", full, false)
	mustErr(t, err)
	want = "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write " + full + ": no space left on device\n" +
		"  Check the free space and the file system of the directory, then run the command again.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := dirNames(t, dir); got != "kc.yaml race.yaml" {
		t.Errorf("dir holds %q: the part of full.yaml or a temporary file was left behind", got)
	}

	// A file that takes the place of the made file during the write stays,
	// so the part is not removed: the error names the file to remove.
	swap := filepath.Join(dir, "swap.yaml")
	writeKubeconfigFile = failWrite(t, &writes, func() {
		must(t, os.Rename(swap, swap+".made"))
		must(t, os.WriteFile(swap, []byte("other"), 0o600))
	})
	h.errOut.Reset()
	_, err = h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", swap, false)
	mustErr(t, err)
	want = "[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write " + swap + ": no space left on device\n" +
		"  " + swap + " can hold a part of the kubeconfig. Remove it before the next run.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := dirNames(t, dir); got != "kc.yaml race.yaml swap.yaml swap.yaml.made" {
		t.Errorf("dir holds %q", got)
	}
}

// The mode read back decides. When the file keeps group or other bits
// after the chmod (vfat and exFAT set the mode at the mount, and can
// refuse the chmod), the command writes the file and prints one warning
// that names the mode. Not parallel: it replaces linkKubeconfig and
// chmodKubeconfig.
func TestAgentKubeconfigWarnsWhenTheModeStaysWide(t *testing.T) {
	link, chmod := linkKubeconfig, chmodKubeconfig
	t.Cleanup(func() { linkKubeconfig, chmodKubeconfig = link, chmod })
	calls := 0
	linkKubeconfig = failLink(t, syscall.EPERM, &calls, nil)
	chmodKubeconfig = func(f *os.File) error {
		// As a mount with fmask=0133: the mode is 0644, the chmod fails.
		must(t, f.Chmod(0o644))
		return syscall.EPERM
	}
	h := agentHarness(t)
	file := filepath.Join(t.TempDir(), "kc.yaml")
	h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
	res, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", file, false)
	if err != nil {
		t.Fatalf("kubeconfig: %v (stderr %q)", err, h.errOut.String())
	}
	want := "[warn] kubehz cluster kubeconfig cl-1a2b3c4d: " + file + " has mode 0644: the mount options of its file system set the mode\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if res.File != file {
		t.Errorf("result file = %q", res.File)
	}
	if got := readFile(t, file); got != agentKubeconfigYAML {
		t.Errorf("file = %q", got)
	}
}

// The path rules run again just before the write: a link that is pointed
// at another user's file during the download is refused then.
func TestAgentKubeconfigChecksThePathAgainBeforeTheWrite(t *testing.T) {
	foreign := foreignFile(t)
	h := agentHarness(t)
	dir := linkTestDir(t)
	link := filepath.Join(dir, "link")
	must(t, os.WriteFile(filepath.Join(dir, "real"), []byte("contexts"), 0o600))
	must(t, os.Symlink("real", link))
	h.handleFunc("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", func(w http.ResponseWriter, r *http.Request) {
		must(t, os.Remove(link))
		must(t, os.Symlink(foreign, link))
		_, _ = w.Write([]byte(agentKubeconfigYAML))
	})
	_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", link, true)
	mustErr(t, err)
	mustContain(t, h.errOut.String(), "link is a link, and its path is not safe: "+realDir(t, foreign)+" belongs to another user (uid 0)\n")
	if got := readFile(t, filepath.Join(dir, "real")); got != "contexts" {
		t.Errorf("real = %q", got)
	}
}

// Without force, a file that appears during the download stays as it is:
// a hard link publishes the kubeconfig, and link(2) fails on a path that
// exists.
func TestAgentKubeconfigKeepsAFileThatAppearsDuringTheDownload(t *testing.T) {
	h := agentHarness(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "kc.yaml")
	h.handleFunc("GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent", func(w http.ResponseWriter, r *http.Request) {
		must(t, os.WriteFile(file, []byte("planted"), 0o600))
		_, _ = w.Write([]byte(agentKubeconfigYAML))
	})
	_, err := h.ctx.SpaceKubeconfig(context.Background(), "sp-1a2b3c4d", file, false)
	mustErr(t, err)
	want := "[error] kubehz space kubeconfig sp-1a2b3c4d: " + file + " exists\n  Pass --force to replace it, or name a new file.\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	if got := readFile(t, file); got != "planted" {
		t.Errorf("file = %q: the file that appeared was replaced", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("dir holds %d entries: a temporary file was left behind", len(entries))
	}
}

// foreignFile is a regular file that root owns, for a link that points at
// another user's file. A container can bind-mount an /etc/passwd of the
// test user, so the first candidate is not always one.
func foreignFile(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the test needs a user other than root")
	}
	sh, _ := filepath.EvalSymlinks("/bin/sh")
	for _, f := range []string{"/etc/passwd", sh} {
		if fi, err := os.Stat(f); err == nil && fi.Mode().IsRegular() {
			if uid, ok := fileOwner(fi); ok && uid == 0 {
				return f
			}
		}
	}
	t.Skip("no file of root to point at")
	return ""
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A plain-http KUBEHZ_API_URL reaches no server: neither the api nor the
// token endpoint gets a request, and only the session's refusal prints.
func TestAgentPlainHTTPReachesNoServer(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer plain.Close()
	h := agentHarness(t)
	h.env[EnvAPIURL] = plain.URL
	_, err := h.ctx.SpaceList(context.Background())
	mustErr(t, err)
	if n := hits.Load(); n != 0 {
		t.Errorf("the plain-http server got %d requests", n)
	}
	if n := len(h.reqs()); n != 0 {
		t.Errorf("%d requests reached the token endpoint: the refusal must stop before the grant", n)
	}
	want := "[error] KUBEHZ_API_URL must use HTTPS: " + plain.URL + "\n[error] Plain HTTP is not allowed for security reasons\n"
	if got := h.errOut.String(); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// The second guard: agentCall itself sends no bearer to a base that is not
// https, whatever the session holds.
func TestAgentCallRefusesAPlainHTTPBase(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer plain.Close()
	h := agentHarness(t)
	h.ctx.HTTP = plain.Client()
	_, err := h.ctx.agentCall(context.Background(), &agentSession{base: plain.URL, bearer: "jwt-agent", key: true},
		"space list", http.MethodGet, "/api/spaces", nil)
	mustErr(t, err)
	if n := hits.Load(); n != 0 {
		t.Errorf("the plain-http server got %d requests", n)
	}
	mustContain(t, h.errOut.String(), "[error] kubehz space list: "+plain.URL+" is not an https URL: lo sends no bearer there\n")
}

// The grant follows no redirect: the Basic client secret and the form
// stay with the token endpoint the key names.
func TestTokenGrantFollowsNoRedirect(t *testing.T) {
	for _, viaAgent := range []bool{false, true} {
		h := agentHarness(t)
		h.handleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/elsewhere/token", http.StatusTemporaryRedirect)
		})
		h.handle("POST /elsewhere/token", 200, `{"access_token":"jwt-agent","token_type":"Bearer","expires_in":43199}`)
		h.handle("GET /api/spaces", 200, okBody(`[]`))
		var err error
		if viaAgent {
			_, err = h.ctx.SpaceList(context.Background())
		} else {
			err = h.ctx.Token(context.Background(), TokenOptions{TokenURL: h.apiURL() + "/oauth/v2/token", Scope: "s"})
		}
		mustErr(t, err)
		if h.anyReq("POST", "/elsewhere/token") {
			t.Errorf("viaAgent=%v: the redirect target got the grant", viaAgent)
		}
		if got := h.errOut.String(); got != "[error] token request refused: HTTP 307: no reason given\n" {
			t.Errorf("viaAgent=%v: stderr = %q", viaAgent, got)
		}
	}
}

func TestAgentSecretNeverReachesAStream(t *testing.T) {
	h := agentHarness(t)
	h.handle("GET /api/spaces/sp-1a2b3c4d", 401, refusalBody("UNAUTHORIZED", "no", "no"))
	_, _ = h.ctx.SpaceGet(context.Background(), "sp-1a2b3c4d")
	h.handle("GET /api/spaces", 200, okBody(`[`+spaceRow+`]`))
	_, _ = h.ctx.SpaceList(context.Background())
	if out := h.output(); strings.Contains(out, "s3cr3t-value") || strings.Contains(out, "jwt-agent") {
		t.Errorf("a credential reached a stream:\n%s", out)
	}
}
