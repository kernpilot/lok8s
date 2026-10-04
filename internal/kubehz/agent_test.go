package kubehz

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
			"[error] kubehz cluster kubeconfig cl-1a2b3c4d: cannot write " + filepath.Join(dir, "no", "kc.yaml") + "\n  Name a file in a directory that exists and that you can write to.\n"},
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
			dir := t.TempDir()
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

// A link is written through only when the link and its target belong to
// this user: a link that another user planted in a shared directory must
// not move the write to a file of theirs.
func TestAgentKubeconfigWritesThroughOnlyALinkOfOurOwn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns /etc/passwd: no foreign target to point at")
	}
	const want = "[error] kubehz cluster kubeconfig cl-1a2b3c4d: DIR/link is a link, and the link or its target belongs to another user\n" +
		"  lo writes through a link only when you own the link and its target. Name another file.\n"
	for _, tc := range []struct {
		name   string
		target func(dir string) string
		euid   func() int
	}{
		{"the target belongs to another user", func(string) string { return "/etc/passwd" }, nil},
		// The seam makes root the user: the target (root's) passes, the
		// link (ours) does not.
		{"the link belongs to another user", func(string) string { return "/etc/passwd" }, func() int { return 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := agentHarness(t)
			h.handle("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", 200, agentKubeconfigYAML)
			h.ctx.Euid = tc.euid
			dir := t.TempDir()
			must(t, os.Symlink(tc.target(dir), filepath.Join(dir, "link")))
			_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", filepath.Join(dir, "link"), true)
			mustErr(t, err)
			if got := strings.ReplaceAll(h.errOut.String(), dir, "DIR"); got != want {
				t.Errorf("stderr = %q, want %q", got, want)
			}
			if n := len(h.reqs()); n != 0 {
				t.Errorf("%d requests: the refusal must come before the grant", n)
			}
		})
	}
}

// The path rules run again just before the write: a link that is pointed
// at another user's file during the download is refused then.
func TestAgentKubeconfigChecksThePathAgainBeforeTheWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns /etc/passwd: no foreign target to point at")
	}
	h := agentHarness(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	must(t, os.WriteFile(filepath.Join(dir, "real"), []byte("contexts"), 0o600))
	must(t, os.Symlink("real", link))
	h.handleFunc("GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent", func(w http.ResponseWriter, r *http.Request) {
		must(t, os.Remove(link))
		must(t, os.Symlink("/etc/passwd", link))
		_, _ = w.Write([]byte(agentKubeconfigYAML))
	})
	_, err := h.ctx.ClusterKubeconfig(context.Background(), "cl-1a2b3c4d", link, true)
	mustErr(t, err)
	mustContain(t, h.errOut.String(), "is a link, and the link or its target belongs to another user\n")
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
