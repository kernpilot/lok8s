package kubehz

// agent.go — `lo kubehz space …` and `lo kubehz cluster …`. An AI agent or
// a pipeline makes these kubehz api calls with an agent key. `lo mcp` turns
// the commands into tools, like every other leaf of the tree. Thus an
// agent that runs `lo mcp` manages spaces and the leases of hosted
// clusters with the key in its environment.
//
// THE CREDENTIAL: the agent key is four variables (KUBEHZ_AGENT_CLIENT_ID,
// KUBEHZ_AGENT_CLIENT_SECRET, KUBEHZ_AGENT_TOKEN_URL, KUBEHZ_AGENT_SCOPE).
// The grant of `lo kubehz token` turns it into an access token, with the
// same cache file. Without a key, KUBEHZ_TOKEN is the bearer.
// KUBEHZ_API_URL names the api. No flag names the URL or holds a
// credential: a model that could set the URL could send the bearer to any
// host.
//
// lo adds no authority. The api enforces the role of the key, its scope,
// its monthly spend cap and the lease. What a key creates ends after two
// hours unless the key extends the lease. lo checks only the shape of what
// it sends: an id that goes into a URL path, and whole numbers.
//
// OUTPUT: -o text prints a table for a list and label lines for one
// record. json and yaml (internal/cli writeOutput) print fixed lowerCamel
// fields. The records are projections, so a new api field changes no
// output. Every string from the api loses its control characters
// (cleanText) in every format.
//
// The bash twin is libs/kubehz/agent. Both print the same bytes:
// hack/parity-kubehz.sh diffs them against an https stub of the api.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kernpilot/lok8s/internal/ui"
)

// EnvAPIURL is the kubehz api base URL of the agent commands.
const EnvAPIURL = "KUBEHZ_API_URL"

const (
	// agentRequestTimeout bounds one api call, the body included. An
	// agent waits on the tool, so a stalled api must end the call (bash:
	// curl --max-time 60).
	agentRequestTimeout = 60 * time.Second
	// agentMaxBody caps what lo reads from one answer.
	agentMaxBody = 16 << 20
	// agentPerPage is the one page a list reads (the api's maximum).
	agentPerPage = 500
	// LeaseMaxHours is the api's longest lease (utils/leases.ts).
	LeaseMaxHours = 720
)

// agentKind is what the commands of one resource share.
type agentKind struct {
	noun   string // space | cluster
	plural string // spaces | clusters
	path   string // the api collection
	prefix string // the id prefix
	id     *regexp.Regexp
}

// The id shapes are the api's own (utils/agent-keys.ts CLUSTER_ID,
// SPACE_ID). An id goes into a URL path, so nothing else may pass: a `../`
// would address another route.
var (
	kindSpace = agentKind{
		noun: "space", plural: "spaces", path: "/api/spaces", prefix: "sp-",
		id: regexp.MustCompile(`^sp-[A-Za-z0-9-]{1,64}$`),
	}
	kindCluster = agentKind{
		noun: "cluster", plural: "clusters", path: "/api/clusters", prefix: "cl-",
		id: regexp.MustCompile(`^cl-[A-Za-z0-9-]{1,64}$`),
	}
)

// apiCodeRe is the shape of an api error code that lo prints. A code in
// another shape is a server string lo does not repeat.
var apiCodeRe = regexp.MustCompile(`^[A-Z0-9_]{1,64}$`)

// ── session: the base URL and the bearer ─────────────────

// agentSession is one command's api access.
type agentSession struct {
	base   string // KUBEHZ_API_URL without a trailing slash
	bearer string
	key    bool // the bearer came from an agent key (else KUBEHZ_TOKEN)
}

// openSession resolves the api URL and the bearer. Every refusal is
// printed here. The bash twin is kubehz::agent_session.
func (c *Context) openSession(ctx context.Context, action string) (*agentSession, error) {
	base := c.getenv(EnvAPIURL)
	if base == "" {
		c.errorf("kubehz %s: %s is not set", action, EnvAPIURL)
		c.echoErr("  Set it to the kubehz api, for example: export %s=https://api.kubehz.cloud", EnvAPIURL)
		return nil, ErrHandled
	}
	if err := c.requireHTTPS(base, EnvAPIURL); err != nil {
		return nil, ErrHandled
	}
	// A double slash in the path is not canonical: the api refuses it for
	// a machine credential (400 PATH_NOT_CANONICAL).
	base = strings.TrimRight(base, "/")

	s := &agentSession{base: base}
	switch {
	case c.getenv(EnvAgentClientID) != "" || c.getenv(EnvAgentClientSecret) != "":
		var missing []string
		for _, name := range []string{EnvAgentClientID, EnvAgentClientSecret, EnvAgentTokenURL, EnvAgentScope} {
			if c.getenv(name) == "" {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			c.errorf("kubehz %s: the agent key needs %s", action, joinAnd(missing))
			c.echoErr("  The answer that created the key holds the four values: clientId, clientSecret, tokenEndpoint and tokenScope.")
			return nil, ErrHandled
		}
		e, err := c.agentAccessToken(ctx, TokenOptions{TokenURL: c.getenv(EnvAgentTokenURL), Scope: c.getenv(EnvAgentScope)})
		if err != nil {
			return nil, err
		}
		s.bearer, s.key = e.AccessToken, true
	case c.getenv("KUBEHZ_TOKEN") != "":
		s.bearer = c.getenv("KUBEHZ_TOKEN")
	default:
		c.errorf("kubehz %s: no credential for the kubehz api", action)
		c.echoErr("  Set %s and %s (an agent key), or KUBEHZ_TOKEN.", EnvAgentClientID, EnvAgentClientSecret)
		return nil, ErrHandled
	}
	// A line break would end the curl config line of the bash twin. The
	// rest would be a curl option, so both twins refuse it.
	if hasControl(s.bearer) {
		c.errorf("kubehz %s: the access token holds a control character", action)
		return nil, ErrHandled
	}
	return s, nil
}

// joinAnd joins names as "a", "a and b" or "a, b and c".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// agentCall is one api request. The bearer goes in the Authorization
// header. No redirect is followed: a 3xx is a refusal. agentRequestTimeout
// bounds the call. err is set only for a transport failure, which is
// already printed. The bash twin is kubehz::agent_api.
func (c *Context) agentCall(ctx context.Context, s *agentSession, action, method, path string, body []byte) (*httpResult, error) {
	ctx, cancel := context.WithTimeout(ctx, agentRequestTimeout)
	defer cancel()
	noAnswer := func() (*httpResult, error) {
		c.errorf("kubehz %s: the api at %s did not answer", action, s.base)
		c.echoErr("  Check %s and the network. Then try again.", EnvAPIURL)
		return nil, ErrHandled
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, rd)
	if err != nil {
		return noAnswer()
	}
	// openSession refuses a URL that is not https. This second check keeps
	// the bearer off any other scheme if that refusal is ever lost. The
	// bash twin has curl --proto =https.
	if req.URL.Scheme != "https" {
		c.errorf("kubehz %s: %s is not an https URL: lo sends no bearer there", action, s.base)
		return nil, ErrHandled
	}
	req.Header.Set("Authorization", "Bearer "+s.bearer)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *c.httpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return noAnswer()
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, agentMaxBody))
	if err != nil {
		return noAnswer()
	}
	return &httpResult{Status: resp.StatusCode, Body: raw}, nil
}

// agentRefused prints a non-2xx answer: what failed, with the status, the
// code and the message of the api. Then it prints the next step. lo prints
// its own hint where it knows more than the api (a flag name, the list
// command). Else it prints the help of the api. Both server strings lose
// every control character and are clipped. A line break goes too, because
// one refusal is two lines. The bash twin is kubehz::agent_refused.
func (c *Context) agentRefused(s *agentSession, k agentKind, action, id string, res *httpResult) {
	code := apiCode(res.Body)
	if !apiCodeRe.MatchString(code) {
		code = ""
	}
	msg := clip(cleanText(apiSaid(res.Body, "message")))
	if msg == "" {
		msg = "no reason given"
	}
	c.errorf("kubehz %s: the api refused the request (HTTP %d%s): %s", action, res.Status, optSuffix(" ", code), msg)
	if hint := agentHint(s, k, id, res.Status, code); hint != "" {
		c.echoErr("  %s", hint)
		return
	}
	if help := clip(cleanText(apiSaid(res.Body, "help"))); help != "" {
		c.echoErr("  %s", help)
	}
}

// apiSaid is the first string of .data.<field> and .<field> in an api
// answer, or "" when neither is a string. A number or an object there
// counts as absent. lo prints what the api said, never its JSON.
func apiSaid(body []byte, field string) string {
	v, ok := parseJSON(body)
	if !ok {
		return ""
	}
	for _, x := range []any{jget(v, "data", field), jget(v, field)} {
		if s, ok := x.(string); ok {
			return s
		}
	}
	return ""
}

// agentHint is the next step of lo for a refusal, or "" when the help of
// the api says it best. A resource-scoped agent key never gets
// AGENT_KEY_OUT_OF_SCOPE on these routes. A space or a cluster outside its
// reach answers 404, the same as one that does not exist (kubehz-api
// classifyScopedAgentRoute).
func agentHint(s *agentSession, k agentKind, id string, status int, code string) string {
	switch {
	case status == http.StatusUnauthorized && s.key:
		return "The api did not accept the agent key. A revoked or expired key needs a new key from a tenant owner."
	case status == http.StatusUnauthorized:
		return "The api did not accept KUBEHZ_TOKEN. Mint a new token in the kubehz dashboard."
	// Only an agent key: its role viewer holds read alone. A KUBEHZ_TOKEN
	// can hold clusters:write without read, and the api's help names the
	// scope it misses.
	case code == "TOKEN_SCOPE_MISSING" && s.key:
		return "The agent key can read but not write. Use an agent key with the role editor or admin."
	case code == "AGENT_KEY_SPEND_CAP":
		return "Delete what the agent key created, or ask a tenant owner for a key with a higher spend cap. The count starts again on the first day of the month (UTC)."
	case code == "SPACE_LIMITS_ABOVE_SHARED":
		return "Lower --nodes, --namespaces or --object-cap-kib. A value you leave out takes the platform default."
	case code == "NO_SHARD_AVAILABLE" || code == "SHARD_AT_CAPACITY":
		return "This is a platform capacity limit, not an account limit. Try again later, or name another region with --region."
	case status == http.StatusNotFound && id != "":
		return fmt.Sprintf("No %s %s exists, or the credential cannot reach it. List what it reaches: lo kubehz %s list", k.noun, id, k.noun)
	}
	return ""
}

// agentCheckID refuses an id that is not the api's shape. The refusal
// names the command without the id: the id is user input, shown once,
// cleaned and clipped.
func (c *Context) agentCheckID(k agentKind, verb, id string) error {
	if k.id.MatchString(id) {
		return nil
	}
	c.errorf("kubehz %s %s: %s is not a %s id", k.noun, verb, clip(cleanText(id)), k.noun)
	c.echoErr("  A %s id is %s and 1 to 64 letters, digits or dashes. List them: lo kubehz %s list", k.noun, k.prefix, k.noun)
	return ErrHandled
}

// agentData is the `data` object of a 2xx answer, or nil.
func agentData(body []byte) map[string]any {
	v, ok := parseJSON(body)
	if !ok {
		return nil
	}
	m, _ := jget(v, "data").(map[string]any)
	return m
}

// ── numbers ──────────────────────────────────────────────

// wholeNumber reads a flag value as a whole number of 1 or more: digits
// only, inside the int64 range. kubehz::space_number_value in the frozen
// tree accepts the same set. "008" is eight.
func wholeNumber(s string) (int64, bool) {
	if !spaceDigits.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// leaseHours reads an hours value: a whole number from 1 to LeaseMaxHours.
func (c *Context) leaseHours(action, flag, s string) (int64, error) {
	if n, ok := wholeNumber(s); ok && n <= LeaseMaxHours {
		return n, nil
	}
	c.errorf("kubehz %s: --%s %s is not valid", action, flag, clip(cleanText(s)))
	c.echoErr("  Use a whole number of hours from 1 to %d.", LeaseMaxHours)
	return 0, ErrHandled
}

// ── records ──────────────────────────────────────────────

// cleanText drops the characters that can move a terminal, end a line or
// turn text around. These are the C0 controls, DEL, the C1 controls
// (U+0080 to U+009F), U+2028 and U+2029. The bidi controls go too (U+202A
// to U+202E, U+2066 to U+2069). The jq `clean` of the bash twin drops the
// same set.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r < 0x20, r >= 0x7f && r <= 0x9f, r == 0x2028, r == 0x2029,
			r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1
		}
		return r
	}, s)
}

// apiText is a string field of an api record, cleaned. It is nil (JSON
// null) for a missing field or another type.
func apiText(v any) any {
	if s, ok := v.(string); ok {
		return cleanText(s)
	}
	return nil
}

// apiNumber is a number field of an api record as written, else nil.
func apiNumber(v any) any {
	if n, ok := v.(json.Number); ok {
		return n
	}
	return nil
}

// apiTexts is a list of strings, cleaned. Other entries are left out.
func apiTexts(v any) []string {
	out := []string{}
	arr, _ := v.([]any)
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, cleanText(s))
		}
	}
	return out
}

// textCell renders a record value for the text form: "-" for null or "".
func textCell(v any) string {
	switch t := v.(type) {
	case string:
		if t != "" {
			return t
		}
	case json.Number:
		return t.String()
	}
	return "-"
}

// SpaceRecord is a space in the agent output (list, create, get).
type SpaceRecord struct {
	ID             any      `json:"id"`
	Name           any      `json:"name"`
	Slug           any      `json:"slug"`
	Status         any      `json:"status"`
	MaxNodes       any      `json:"maxNodes"`
	MaxNamespaces  any      `json:"maxNamespaces"`
	MaxObjectKiB   any      `json:"maxObjectKiB"`
	NodeCount      any      `json:"nodeCount"`
	Namespaces     []string `json:"namespaces"`
	LeaseExpiresAt any      `json:"leaseExpiresAt"`
	CreatedAt      any      `json:"createdAt"`
}

// spaceRecord projects a SpaceSchema row (GET /api/spaces, the create).
func spaceRecord(m map[string]any) SpaceRecord {
	return SpaceRecord{
		ID: apiText(m["id"]), Name: apiText(m["name"]), Slug: apiText(m["slug"]), Status: apiText(m["status"]),
		MaxNodes: apiNumber(m["maxNodes"]), MaxNamespaces: apiNumber(m["maxNamespaces"]), MaxObjectKiB: apiNumber(m["maxObjectKiB"]),
		NodeCount: apiNumber(m["nodeCount"]), Namespaces: apiTexts(m["namespaces"]),
		LeaseExpiresAt: apiText(m["leaseExpiresAt"]), CreatedAt: apiText(m["createdAt"]),
	}
}

// SpaceNode is a node of a space (get only).
type SpaceNode struct {
	Name   any `json:"name"`
	Status any `json:"status"`
}

// SpaceDetail is `lo kubehz space get`: the record, the stable endpoint and
// the nodes.
type SpaceDetail struct {
	SpaceRecord
	Endpoint any         `json:"endpoint"`
	Nodes    []SpaceNode `json:"nodes"`
}

// spaceDetail projects GET /api/spaces/{id} (SpaceDetailSchema): the
// namespaces are objects there and the node count sits under usage.
func spaceDetail(m map[string]any) *SpaceDetail {
	d := &SpaceDetail{SpaceRecord: spaceRecord(m), Endpoint: apiText(m["endpoint"]), Nodes: []SpaceNode{}}
	d.NodeCount = apiNumber(jget(m, "usage", "nodes"))
	d.Namespaces = []string{}
	if arr, ok := m["namespaces"].([]any); ok {
		for _, e := range arr {
			if s, ok := jget(e, "name").(string); ok {
				d.Namespaces = append(d.Namespaces, cleanText(s))
			}
		}
	}
	if arr, ok := m["nodes"].([]any); ok {
		for _, e := range arr {
			if n, ok := e.(map[string]any); ok {
				d.Nodes = append(d.Nodes, SpaceNode{Name: apiText(n["name"]), Status: apiText(n["status"])})
			}
		}
	}
	return d
}

// labelWidth is the label column of the text form of one record: the
// longest label with its colon ("Namespaces:"). The bash twin's jq `field`
// pads to it and adds the same two spaces.
const labelWidth = 11

// writeLabels prints one record as label lines (ui.Columns: the label
// column, two spaces, the value).
func writeLabels(w io.Writer, lines [][]string) {
	c := ui.NewColumns(w, []string{"", ""}, lines, []int{labelWidth, 0})
	for _, l := range lines {
		c.Row(l...)
	}
}

func (r *SpaceRecord) labels(endpoint any, withEndpoint bool) [][]string {
	ns := strings.Join(r.Namespaces, ", ")
	if ns == "" {
		ns = "-"
	}
	objectCap := "-"
	if r.MaxObjectKiB != nil {
		objectCap = textCell(r.MaxObjectKiB) + " KiB"
	}
	lines := [][]string{
		{"ID:", textCell(r.ID)},
		{"Name:", textCell(r.Name)},
		{"Slug:", textCell(r.Slug)},
		{"Status:", textCell(r.Status)},
		{"Nodes:", textCell(r.NodeCount) + " of " + textCell(r.MaxNodes)},
		{"Namespaces:", ns + " (limit " + textCell(r.MaxNamespaces) + ")"},
		{"Object cap:", objectCap},
	}
	if withEndpoint {
		lines = append(lines, []string{"Endpoint:", textCell(endpoint)})
	}
	return append(lines,
		[]string{"Lease ends:", textCell(r.LeaseExpiresAt)},
		[]string{"Created:", textCell(r.CreatedAt)})
}

// WriteText prints the created space.
func (r *SpaceRecord) WriteText(w io.Writer) { writeLabels(w, r.labels(nil, false)) }

// WriteText prints one space with its endpoint.
func (d *SpaceDetail) WriteText(w io.Writer) { writeLabels(w, d.labels(d.Endpoint, true)) }

// SpaceList is `lo kubehz space list`.
type SpaceList struct {
	Spaces []SpaceRecord `json:"spaces"`
}

// WriteText prints the table.
func (l *SpaceList) WriteText(w io.Writer) {
	rows := [][]string{{"ID", "SLUG", "NAME", "STATUS", "NODES", "LEASE ENDS"}}
	for _, s := range l.Spaces {
		rows = append(rows, []string{textCell(s.ID), textCell(s.Slug), textCell(s.Name), textCell(s.Status),
			textCell(s.NodeCount) + "/" + textCell(s.MaxNodes), textCell(s.LeaseExpiresAt)})
	}
	writeTable(w, rows)
}

// ClusterRecord is a cluster in the agent output (list, get).
type ClusterRecord struct {
	ID                   any `json:"id"`
	Domain               any `json:"domain"`
	Hosting              any `json:"hosting"`
	Status               any `json:"status"`
	Region               any `json:"region"`
	KubernetesVersion    any `json:"kubernetesVersion"`
	ControlPlaneReplicas any `json:"controlPlaneReplicas"`
	APIEndpoint          any `json:"apiEndpoint"`
	Health               any `json:"health"`
	LeaseExpiresAt       any `json:"leaseExpiresAt"`
	CreatedAt            any `json:"createdAt"`
}

// clusterRecord projects a ClusterSchema row (the list and the get).
func clusterRecord(m map[string]any) ClusterRecord {
	return ClusterRecord{
		ID: apiText(m["id"]), Domain: apiText(m["domain"]), Hosting: apiText(m["hosting"]), Status: apiText(m["status"]),
		Region: apiText(m["region"]), KubernetesVersion: apiText(m["kubernetesVersion"]),
		ControlPlaneReplicas: apiNumber(m["controlPlaneReplicas"]), APIEndpoint: apiText(m["apiEndpoint"]),
		Health: apiText(m["health"]), LeaseExpiresAt: apiText(m["leaseExpiresAt"]), CreatedAt: apiText(m["createdAt"]),
	}
}

// WriteText prints one cluster.
func (r *ClusterRecord) WriteText(w io.Writer) {
	writeLabels(w, [][]string{
		{"ID:", textCell(r.ID)},
		{"Domain:", textCell(r.Domain)},
		{"Hosting:", textCell(r.Hosting)},
		{"Status:", textCell(r.Status)},
		{"Health:", textCell(r.Health)},
		{"Region:", textCell(r.Region)},
		{"Version:", textCell(r.KubernetesVersion)},
		{"Apiservers:", textCell(r.ControlPlaneReplicas)},
		{"Endpoint:", textCell(r.APIEndpoint)},
		{"Lease ends:", textCell(r.LeaseExpiresAt)},
		{"Created:", textCell(r.CreatedAt)},
	})
}

// ClusterList is `lo kubehz cluster list`.
type ClusterList struct {
	Clusters []ClusterRecord `json:"clusters"`
}

// WriteText prints the table.
func (l *ClusterList) WriteText(w io.Writer) {
	rows := [][]string{{"ID", "DOMAIN", "HOSTING", "STATUS", "VERSION", "LEASE ENDS"}}
	for _, r := range l.Clusters {
		rows = append(rows, []string{textCell(r.ID), textCell(r.Domain), textCell(r.Hosting), textCell(r.Status),
			textCell(r.KubernetesVersion), textCell(r.LeaseExpiresAt)})
	}
	writeTable(w, rows)
}

// writeTable prints the header row and the rows through ui.Columns. Each
// column but the last is padded to its widest cell in code points, as the
// jq `length` of the bash twin counts them. The columns are two spaces
// apart, with no underline.
func writeTable(w io.Writer, rows [][]string) {
	c := ui.NewColumns(w, rows[0], rows[1:], nil)
	for _, row := range rows {
		c.Row(row...)
	}
}

// DeleteResult is `lo kubehz space delete`.
type DeleteResult struct {
	ID     any `json:"id"`
	Status any `json:"status"`
	noun   string
}

// WriteText prints "<noun> <id>: <status>".
func (r *DeleteResult) WriteText(w io.Writer) {
	fmt.Fprintf(w, "%s %s: %s\n", r.noun, textCell(r.ID), textCell(r.Status))
}

// LeaseResult is `lo kubehz space|cluster lease`.
type LeaseResult struct {
	ID             any `json:"id"`
	LeaseExpiresAt any `json:"leaseExpiresAt"`
	noun           string
}

// WriteText prints when the lease ends.
func (r *LeaseResult) WriteText(w io.Writer) {
	fmt.Fprintf(w, "%s %s: the lease ends %s\n", r.noun, textCell(r.ID), textCell(r.LeaseExpiresAt))
}

// KubeconfigResult is `lo kubehz space|cluster kubeconfig`: where the file is.
type KubeconfigResult struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

// WriteText prints the file path.
func (r *KubeconfigResult) WriteText(w io.Writer) { fmt.Fprintln(w, r.File) }

// ── the commands ─────────────────────────────────────────

// agentList reads one page of a collection and returns its rows (objects
// only). A total past the page gets a warning.
func (c *Context) agentList(ctx context.Context, k agentKind) ([]map[string]any, error) {
	action := k.noun + " list"
	s, err := c.openSession(ctx, action)
	if err != nil {
		return nil, err
	}
	res, err := c.agentCall(ctx, s, action, http.MethodGet, k.path+"?perPage="+strconv.Itoa(agentPerPage), nil)
	if err != nil {
		return nil, err
	}
	if !is2xx(res.Status) {
		c.agentRefused(s, k, action, "", res)
		return nil, ErrHandled
	}
	v, _ := parseJSON(res.Body)
	arr, ok := jget(v, "data").([]any)
	if !ok {
		c.errorf("kubehz %s: the api answered without a list of %s", action, k.plural)
		return nil, ErrHandled
	}
	out := []map[string]any{}
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	if total := jnum(jget(v, "meta", "pagination", "total")); total > len(arr) {
		c.warnf("kubehz %s: the list shows %d of %d %s", action, len(arr), total, k.plural)
	}
	return out, nil
}

// agentOne runs one call that answers a record: the session, the call, the
// refusal, the `data` object. The caller checked the id.
func (c *Context) agentOne(ctx context.Context, k agentKind, action, id, method, path string, body []byte) (map[string]any, error) {
	s, err := c.openSession(ctx, action)
	if err != nil {
		return nil, err
	}
	res, err := c.agentCall(ctx, s, action, method, path, body)
	if err != nil {
		return nil, err
	}
	if !is2xx(res.Status) {
		c.agentRefused(s, k, action, id, res)
		return nil, ErrHandled
	}
	m := agentData(res.Body)
	if m == nil {
		c.errorf("kubehz %s: the api answered without a %s record", action, k.noun)
		return nil, ErrHandled
	}
	return m, nil
}

// SpaceList lists the spaces the credential reaches.
func (c *Context) SpaceList(ctx context.Context) (*SpaceList, error) {
	rows, err := c.agentList(ctx, kindSpace)
	if err != nil {
		return nil, err
	}
	l := &SpaceList{Spaces: []SpaceRecord{}}
	for _, m := range rows {
		l.Spaces = append(l.Spaces, spaceRecord(m))
	}
	return l, nil
}

// SpaceGet reads one space.
func (c *Context) SpaceGet(ctx context.Context, id string) (*SpaceDetail, error) {
	if err := c.agentCheckID(kindSpace, "get", id); err != nil {
		return nil, err
	}
	m, err := c.agentOne(ctx, kindSpace, "space get "+id, id, http.MethodGet, kindSpace.path+"/"+id, nil)
	if err != nil {
		return nil, err
	}
	return spaceDetail(m), nil
}

// SpaceCreateOptions are the flags of `lo kubehz space create`. "" is a
// flag that was not given: the api applies its own default.
type SpaceCreateOptions struct {
	Name, Slug                                  string
	Nodes, Namespaces, ObjectCapKiB, LeaseHours string
	Region                                      string
}

// SpaceCreate creates a space. A number the flags leave out is not sent.
func (c *Context) SpaceCreate(ctx context.Context, o SpaceCreateOptions) (*SpaceRecord, error) {
	action := "space create " + clip(cleanText(o.Slug))
	pairs := []jsonPair{{"name", o.Name}, {"slug", o.Slug}}
	for _, f := range []struct{ flag, field, value string }{
		{"nodes", "maxNodes", o.Nodes},
		{"namespaces", "maxNamespaces", o.Namespaces},
		{"object-cap-kib", "maxObjectKiB", o.ObjectCapKiB},
	} {
		if f.value == "" {
			continue
		}
		n, ok := wholeNumber(f.value)
		if !ok {
			c.errorf("kubehz %s: --%s %s is not valid", action, f.flag, clip(cleanText(f.value)))
			c.echoErr("  Use a whole number of 1 or more, or leave the flag out for the platform default.")
			return nil, ErrHandled
		}
		pairs = append(pairs, jsonPair{f.field, n})
	}
	if o.Region != "" {
		pairs = append(pairs, jsonPair{"region", o.Region})
	}
	if o.LeaseHours != "" {
		n, err := c.leaseHours(action, "lease-hours", o.LeaseHours)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, jsonPair{"leaseHours", n})
	}
	m, err := c.agentOne(ctx, kindSpace, action, "", http.MethodPost, kindSpace.path, compactJSON(pairs...))
	if err != nil {
		return nil, err
	}
	r := spaceRecord(m)
	return &r, nil
}

// SpaceDelete deletes a space (the platform drains it: status Deleting).
func (c *Context) SpaceDelete(ctx context.Context, id string) (*DeleteResult, error) {
	if err := c.agentCheckID(kindSpace, "delete", id); err != nil {
		return nil, err
	}
	m, err := c.agentOne(ctx, kindSpace, "space delete "+id, id, http.MethodDelete, kindSpace.path+"/"+id, nil)
	if err != nil {
		return nil, err
	}
	return &DeleteResult{ID: apiText(m["id"]), Status: apiText(m["status"]), noun: "space"}, nil
}

// SpaceLease sets the lease of a space: hours from now.
func (c *Context) SpaceLease(ctx context.Context, id, hours string) (*LeaseResult, error) {
	return c.agentLease(ctx, kindSpace, id, hours)
}

// SpaceKubeconfig writes the agent kubeconfig of a space to file. force
// replaces a file that exists.
func (c *Context) SpaceKubeconfig(ctx context.Context, id, file string, force bool) (*KubeconfigResult, error) {
	return c.agentKubeconfig(ctx, kindSpace, id, file, force)
}

// ClusterList lists the clusters the credential reaches.
func (c *Context) ClusterList(ctx context.Context) (*ClusterList, error) {
	rows, err := c.agentList(ctx, kindCluster)
	if err != nil {
		return nil, err
	}
	l := &ClusterList{Clusters: []ClusterRecord{}}
	for _, m := range rows {
		l.Clusters = append(l.Clusters, clusterRecord(m))
	}
	return l, nil
}

// ClusterGet reads one cluster.
func (c *Context) ClusterGet(ctx context.Context, id string) (*ClusterRecord, error) {
	if err := c.agentCheckID(kindCluster, "get", id); err != nil {
		return nil, err
	}
	m, err := c.agentOne(ctx, kindCluster, "cluster get "+id, id, http.MethodGet, kindCluster.path+"/"+id, nil)
	if err != nil {
		return nil, err
	}
	r := clusterRecord(m)
	return &r, nil
}

// ClusterLease sets the lease of a hosted cluster: hours from now.
func (c *Context) ClusterLease(ctx context.Context, id, hours string) (*LeaseResult, error) {
	return c.agentLease(ctx, kindCluster, id, hours)
}

// ClusterKubeconfig writes the agent kubeconfig of a hosted cluster to
// file. force replaces a file that exists.
func (c *Context) ClusterKubeconfig(ctx context.Context, id, file string, force bool) (*KubeconfigResult, error) {
	return c.agentKubeconfig(ctx, kindCluster, id, file, force)
}

// agentLease is PATCH <collection>/<id>/lease {hours}. lo always sends a
// number: removing a lease is a person's action in the dashboard.
func (c *Context) agentLease(ctx context.Context, k agentKind, id, hours string) (*LeaseResult, error) {
	if err := c.agentCheckID(k, "lease", id); err != nil {
		return nil, err
	}
	action := k.noun + " lease " + id
	n, err := c.leaseHours(action, "hours", hours)
	if err != nil {
		return nil, err
	}
	m, err := c.agentOne(ctx, k, action, id, http.MethodPatch, k.path+"/"+id+"/lease", compactJSON(jsonPair{"hours", n}))
	if err != nil {
		return nil, err
	}
	return &LeaseResult{ID: apiText(m["id"]), LeaseExpiresAt: apiText(m["leaseExpiresAt"]), noun: k.noun}, nil
}

// agentKubeconfig downloads <collection>/<id>/kubeconfig/agent and writes it
// to file (0600, through a temporary file in the same directory). The file
// holds no secret: its exec stanza runs `lo kubehz token`. The command still
// counts as credential output, so `lo mcp` never offers it.
func (c *Context) agentKubeconfig(ctx context.Context, k agentKind, id, file string, force bool) (*KubeconfigResult, error) {
	if err := c.agentCheckID(k, "kubeconfig", id); err != nil {
		return nil, err
	}
	action := k.noun + " kubeconfig " + id
	// First before any request: a refused path costs no grant and no
	// download.
	if _, err := c.kubeconfigTarget(action, file, force); err != nil {
		return nil, err
	}
	s, err := c.openSession(ctx, action)
	if err != nil {
		return nil, err
	}
	res, err := c.agentCall(ctx, s, action, http.MethodGet, k.path+"/"+id+"/kubeconfig/agent", nil)
	if err != nil {
		return nil, err
	}
	if !is2xx(res.Status) {
		c.agentRefused(s, k, action, id, res)
		return nil, ErrHandled
	}
	if len(bytes.TrimSpace(res.Body)) == 0 {
		c.errorf("kubehz %s: the api answered without a kubeconfig", action)
		return nil, ErrHandled
	}
	// Again just before the write: the path can change during the grant
	// and the download.
	target, err := c.kubeconfigTarget(action, file, force)
	if err != nil {
		return nil, err
	}
	if err := publishKubeconfig(target, res.Body, force); err != nil {
		if _, lerr := os.Lstat(file); !force && lerr == nil {
			c.kubeconfigExists(action, file)
			return nil, ErrHandled
		}
		c.errorf("kubehz %s: cannot write %s", action, file)
		c.echoErr("  Name a file in a directory that exists and that you can write to.")
		return nil, ErrHandled
	}
	return &KubeconfigResult{ID: id, File: file}, nil
}

// kubeconfigExists prints the refusal for a path that exists without
// --force.
func (c *Context) kubeconfigExists(action, file string) {
	c.errorf("kubehz %s: %s exists", action, file)
	c.echoErr("  Pass --force to replace it, or name a new file.")
}

// kubeconfigTarget decides where the kubeconfig goes. It runs before any
// request and again just before the write:
//   - A path in /proc or /dev, or a path that a link sends there, is
//     refused (see inProcOrDev).
//   - A directory, or a link to one, is refused.
//   - A path that exists (a file or a link) is refused without force. Thus
//     --file ~/.kube/config cannot lose its contexts by accident.
//   - With force, lo writes through a link: the target becomes a new file
//     (owned by this user, mode 0600) and the link stays. A hard link to
//     the old target keeps the old content.
//   - lo writes through a link only when the link and its target belong to
//     this user. lo resolves the link itself and renames onto the target,
//     so the kernel rule fs.protected_symlinks does not apply. A link that
//     another user put in a shared directory (/tmp/kc.yaml) must not send
//     the write to a file of this user, such as ~/.bashrc. A link to
//     nothing is refused.
//
// A short race stays. Between the last check and the rename, a user who
// can write the directory of a link can point the link somewhere else. The
// check just before the write keeps that window as short as lo can.
//
// The bash twin is kubehz::agent_target.
func (c *Context) kubeconfigTarget(action, file string, force bool) (string, error) {
	if inProcOrDev(file) {
		c.errorf("kubehz %s: %s is in /proc or /dev, or a link in its path points there", action, file)
		c.echoErr("  lo does not write to /proc or /dev. Name a file in another directory.")
		return "", ErrHandled
	}
	if fi, err := os.Stat(file); err == nil && fi.IsDir() {
		c.errorf("kubehz %s: %s is a directory", action, file)
		c.echoErr("  Name a file, not a directory.")
		return "", ErrHandled
	}
	li, err := os.Lstat(file)
	if err != nil {
		return file, nil
	}
	if !force {
		c.kubeconfigExists(action, file)
		return "", ErrHandled
	}
	if li.Mode()&os.ModeSymlink == 0 {
		return file, nil
	}
	target, err := filepath.EvalSymlinks(file)
	if err != nil {
		c.errorf("kubehz %s: cannot write %s", action, file)
		c.echoErr("  Name a file in a directory that exists and that you can write to.")
		return "", ErrHandled
	}
	ti, err := os.Stat(target)
	if err != nil || !c.ownedByMe(li) || !c.ownedByMe(ti) {
		c.errorf("kubehz %s: %s is a link, and the link or its target belongs to another user", action, file)
		c.echoErr("  lo writes through a link only when you own the link and its target. Name another file.")
		return "", ErrHandled
	}
	return target, nil
}

// inProcOrDev reports whether file is in /proc or /dev, or whether a link
// on its path points there. The kernel resolves /proc/self against the
// process that asks. Thus for lo, /proc/self/exe is the lo binary and
// /proc/self/fd/1 is the file that stdout goes to, and a resolved path
// looks like a usual file. The walk reads each link itself, hop by hop, and
// checks each component before it reads a link there. ".." goes up from
// the resolved path, as in the kernel. A relative path starts at the
// working directory. After 40 links the walk stops with false: the write
// then fails anyway.
//
// The bash twin is kubehz::agent_in_proc_or_dev.
func inProcOrDev(file string) bool {
	rest := file
	if !strings.HasPrefix(rest, "/") {
		wd, err := os.Getwd()
		if err != nil {
			return false
		}
		rest = wd + "/" + rest
	}
	walked, links := "", 0
	for rest != "" {
		name, after, _ := strings.Cut(rest, "/")
		rest = after
		switch name {
		case "", ".":
			continue
		case "..":
			if i := strings.LastIndex(walked, "/"); i >= 0 {
				walked = walked[:i]
			}
			continue
		}
		next := walked + "/" + name
		if next == "/proc" || next == "/dev" {
			return true
		}
		li, err := os.Lstat(next)
		if err != nil || li.Mode()&os.ModeSymlink == 0 {
			walked = next
			continue
		}
		if links++; links > 40 {
			return false
		}
		dest, err := os.Readlink(next)
		if err != nil {
			return false
		}
		if strings.HasPrefix(dest, "/") {
			walked = ""
		}
		if rest != "" {
			dest += "/" + rest
		}
		rest = dest
	}
	return false
}

// ownedByMe reports whether the effective user owns the entry.
func (c *Context) ownedByMe(info os.FileInfo) bool {
	uid, ok := fileOwner(info)
	return ok && uid == c.euid()
}

// publishKubeconfig writes data to target through a temporary file next to
// it (mode 0600). With force, a rename replaces the target. Without force,
// a hard link publishes the file. link(2) fails when the path exists, so a
// file that appeared during the download stays as it is. The temporary
// file goes in both cases.
func publishKubeconfig(target string, data []byte, force bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".kubeconfig-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && force {
		err = os.Rename(name, target)
	} else if err == nil {
		err = os.Link(name, target)
		_ = os.Remove(name)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
