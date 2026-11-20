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
// output. Every string from the api loses the characters of ui.Unshown
// (cleanText) in every format.
//
// The bash twin is libs/kubehz/agent. Both print the same bytes:
// hack/parity-kubehz.sh diffs them against an https stub of the api.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

// cleanText drops each character of ui.Unshown: the explicit list of
// characters that can move a terminal, end a line, turn text around or
// hide in a name, which ui.Shown shows as '?'. A backslash stays: it is
// data, and json and yaml escape it. The jq `clean` of the bash twin drops
// the same set (the def unshown, utils/verbose.sh).
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if ui.Unshown(r) {
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
	wide, err := publishKubeconfig(target, res.Body, force)
	if err != nil {
		return nil, c.kubeconfigPublishFailed(action, file, force, err)
	}
	// The exclusive create could not take the group and other bits away:
	// on vfat or exFAT the mount sets the mode.
	if wide != 0 {
		c.warnf("kubehz %s: %s has mode %04o: the mount options of its file system set the mode", action, file, wide)
	}
	return &KubeconfigResult{ID: id, File: file}, nil
}

// kubeconfigPublishFailed prints why publishKubeconfig failed for file,
// and returns ErrHandled. The causes, in this order:
//   - a write that failed after the exclusive create: the path can still
//     hold a part of the file, and "exists" would name the wrong cause;
//   - a last name that names a directory (errKubeconfigNoName): the
//     refusal of a directory;
//   - an open of a directory that exists but that lo cannot read: a hint
//     for the read right. Only the Go twin prints it: the bash twin needs
//     no read right on the directory and writes the file there;
//   - a path that exists, without force;
//   - else "cannot write".
func (c *Context) kubeconfigPublishFailed(action, file string, force bool, err error) error {
	if werr, ok := errors.AsType[*kubeconfigWriteError](err); ok {
		c.errorf("kubehz %s: cannot write %s: %s", action, file, werr.cause())
		if werr.left {
			c.echoErr("  %s can hold a part of the kubeconfig. Remove it before the next run.", file)
		} else {
			c.echoErr("  Check the free space and the file system of the directory, then run the command again.")
		}
		return ErrHandled
	}
	if errors.Is(err, errKubeconfigNoName) {
		return c.kubeconfigDirectory(action, file)
	}
	if oerr, ok := errors.AsType[*kubeconfigOpenError](err); ok && errors.Is(oerr, os.ErrPermission) {
		if fi, serr := os.Stat(oerr.dir); serr == nil && fi.IsDir() {
			c.errorf("kubehz %s: cannot write %s", action, file)
			c.echoErr("  lo also needs the read right on %s. Name a file in a directory that you can read and write to.", oerr.dir)
			return ErrHandled
		}
	}
	if _, lerr := os.Lstat(file); !force && lerr == nil {
		c.kubeconfigExists(action, file)
		return ErrHandled
	}
	return c.kubeconfigUnwritable(action, file)
}

// kubeconfigDirectory prints the refusal of a path that names a directory.
func (c *Context) kubeconfigDirectory(action, file string) error {
	c.errorf("kubehz %s: %s is a directory", action, file)
	c.echoErr("  Name a file, not a directory.")
	return ErrHandled
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
//   - Each name of the path that a "/" follows must be a directory that
//     exists, as the kernel requires (see kubeconfigPathParts). Thus
//     real/, real/., missing/../real and file/../real are refused before
//     any request, not at the write.
//   - A path that exists (a file or a link) is refused without force. Thus
//     --file ~/.kube/config cannot lose its contexts by accident.
//   - With force, lo writes through a link: the target becomes a new file
//     (owned by this user, mode 0600) and the link stays. A hard link to
//     the old target keeps the old content.
//   - lo writes through a link only when no other user can send the write
//     somewhere else (see linkTarget). lo resolves the link itself and
//     renames onto the target, so the kernel rule fs.protected_symlinks
//     does not apply. A link that another user put in a shared directory
//     (/tmp/kc.yaml) must not send the write to a file of this user, such
//     as ~/.bashrc. A link to nothing is refused.
//
// A plain file and a path that does not exist yet keep the rules above
// only: the walk of linkTarget is for a link. A plain path that exists must
// be a regular file, as the target of a link must be: a FIFO, a socket or a
// device is refused (kubeconfigNotRegular).
//
// The bash twin is kubehz::agent_target.
func (c *Context) kubeconfigTarget(action, file string, force bool) (string, error) {
	if inProcOrDev(file) {
		c.errorf("kubehz %s: %s is in /proc or /dev, or a link in its path points there", action, file)
		c.echoErr("  lo does not write to /proc or /dev. Name a file in another directory.")
		return "", ErrHandled
	}
	if fi, err := os.Stat(file); err == nil && fi.IsDir() {
		return "", c.kubeconfigDirectory(action, file)
	}
	if err := c.kubeconfigPathParts(action, file); err != nil {
		return "", err
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
		// A FIFO, a socket or a device is not a kubeconfig (a directory
		// was refused above): the rename would replace it.
		if !li.Mode().IsRegular() {
			return "", c.kubeconfigNotRegular(action, file, "")
		}
		return file, nil
	}
	return c.linkTarget(action, file)
}

// kubeconfigPathParts checks each name of file that a "/" follows, as the
// kernel does when it opens the path: it must be a directory that exists.
// The check stats each part as written (os.Stat follows a link, and ".."
// goes up from where a link points, as in the kernel). A trailing "/" makes
// the last name such a part too. The first part that fails is refused
// (kubeconfigBadPart).
//
// The bash twin is kubehz::agent_path_parts.
func (c *Context) kubeconfigPathParts(action, file string) error {
	for i := 1; i < len(file); i++ {
		if file[i] != '/' || file[i-1] == '/' {
			continue
		}
		part := file[:i]
		fi, err := os.Stat(part)
		switch {
		case err != nil:
			return c.kubeconfigBadPart(action, file, part, "does not exist or cannot be read")
		case !fi.IsDir():
			return c.kubeconfigBadPart(action, file, part, "is not a directory")
		}
	}
	return nil
}

// maxLinkHops is the most links that linkTarget follows on the way to the
// target. A loop, or a longer chain, is refused.
const maxLinkHops = 8

// linkTarget resolves the link file hop by hop and returns the target. The
// walk reads each name of the path itself, as inProcOrDev does, and counts
// each link that it follows: a link as the last name, and a link to a
// directory on the way. For each link, each part of the directory that
// holds it, from / down, must pass the check of notPrivatePart: it belongs
// to root or to this user, and it is not writable by its group or by
// others unless it has the sticky bit. The link itself must belong to root
// or to this user. The directory of the target gets the same check, and
// the target must be a regular file of this user. Thus only root and this
// user can change a link or a directory on the way, and the target stays
// the file that the check saw. A refusal names the part that failed and
// why. Nothing is written.
//
// The walk keeps the rules of the kernel: a name that a "/" follows (also
// in the target of a link, also a trailing "/") must be a directory that
// exists before "." or ".." applies to it. So real/, real/.,
// missing/../real and file/../real are refused, as the kernel refuses
// them. ".." goes up from where a link to a directory points. A relative
// path starts at the working directory as the kernel knows it (the
// physical path), not at $PWD.
//
// The bash twin is kubehz::agent_link_target.
func (c *Context) linkTarget(action, file string) (string, error) {
	rest := file
	if !strings.HasPrefix(rest, "/") {
		// getcwd, as pwd -P in the bash twin. os.Getwd gives $PWD when it
		// names the same directory, also through a link: the walk would
		// then check links and directories that the kernel does not pass.
		wd, err := syscall.Getwd()
		if err != nil {
			return "", c.kubeconfigNoWorkDir(action, file)
		}
		rest = wd + "/" + rest
	}
	walked, links := "", 0
	for rest != "" {
		name, after, dirNext := strings.Cut(rest, "/")
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
		li, err := c.lstat(next)
		if err != nil || li.Mode()&os.ModeSymlink == 0 {
			if dirNext {
				switch {
				case err != nil:
					return "", c.kubeconfigBadPart(action, file, next, "does not exist or cannot be read")
				case !li.IsDir():
					return "", c.kubeconfigBadPart(action, file, next, "is not a directory")
				}
			}
			walked = next
			continue
		}
		if links++; links > maxLinkHops {
			c.errorf("kubehz %s: %s is a link, and its path is not safe: %s is link %d on the way, and lo follows at most %d (a loop, or a chain that is too long)",
				action, file, next, links, maxLinkHops)
			c.echoErr("  Point the link at the file, or name the file itself.")
			return "", ErrHandled
		}
		if err := c.linkDirSafe(action, file, rootIfEmpty(walked)); err != nil {
			return "", err
		}
		if uid, ok := fileOwner(li); !ok || !c.ownerTrusted(next, uid) {
			why, unmapped := ownerWhy(uid, ok, c.euid())
			return "", c.linkNotSafe(action, file, next, why, unmapped)
		}
		dest, err := os.Readlink(next)
		if err != nil {
			return "", c.kubeconfigUnwritable(action, file)
		}
		if strings.HasPrefix(dest, "/") {
			walked = ""
		}
		// A "/" after the link (also a trailing one) stays: the target of
		// the link must then be a directory.
		if dirNext {
			dest += "/" + rest
		}
		rest = dest
	}
	target := rootIfEmpty(walked)
	if err := c.linkDirSafe(action, file, filepath.Dir(target)); err != nil {
		return "", err
	}
	ti, err := c.lstat(target)
	if err != nil || ti.Mode()&os.ModeSymlink != 0 {
		return "", c.kubeconfigUnwritable(action, file)
	}
	// A FIFO, a socket, a device or a directory is not a kubeconfig: the
	// rename would replace it.
	if !ti.Mode().IsRegular() {
		return "", c.kubeconfigNotRegular(action, file, target)
	}
	if uid, ok := fileOwner(ti); !ok || uid != c.euid() || c.euid() == c.overflowUID() {
		why, unmapped := ownerWhy(uid, ok, c.euid())
		return "", c.linkNotSafe(action, file, target, why, unmapped)
	}
	return target, nil
}

// linkDirSafe checks each part of dir (a path that linkTarget resolved),
// from / down, with notPrivatePart. The first part that fails is refused
// (linkNotSafe).
func (c *Context) linkDirSafe(action, file, dir string) error {
	parts := []string{dir}
	for p := dir; filepath.Dir(p) != p; p = filepath.Dir(p) {
		parts = append(parts, filepath.Dir(p))
	}
	slices.Reverse(parts)
	for _, part := range parts {
		if why, unmapped := c.notPrivatePart(part); why != "" {
			return c.linkNotSafe(action, file, part, why, unmapped)
		}
	}
	return nil
}

// ownerWhy says why an owner fails the check of a link or a target, in the
// words of notPrivatePart. unmapped: the entry is ours, and this user has
// the overflow uid (a user namespace without a mapping for it).
func ownerWhy(uid int, ok bool, euid int) (why string, unmapped bool) {
	switch {
	case !ok:
		return "cannot be read", false
	case uid == euid:
		return fmt.Sprintf("cannot be checked (uid %d)", uid), true
	}
	return fmt.Sprintf("belongs to another user (uid %d)", uid), false
}

// linkNotSafe prints the refusal of a link whose part failed the check of
// linkTarget, with why.
func (c *Context) linkNotSafe(action, file, part, why string, unmapped bool) error {
	c.errorf("kubehz %s: %s is a link, and its path is not safe: %s %s", action, file, part, why)
	if unmapped {
		c.echoErr("  The uid of this user is not mapped in the namespace, so lo cannot check who owns the path.")
		c.echoErr("  Run it where the uid of this user is mapped (unshare --map-current-user), or name a file that is not a link.")
		return ErrHandled
	}
	c.echoErr("  lo writes through a link only to a file of yours. Each link and each directory on the way must belong to root or to you,")
	c.echoErr("  and must not be writable by its group or by others unless it has the sticky bit. Name another file.")
	return ErrHandled
}

// kubeconfigNotRegular prints the refusal of a path that exists and is not
// a regular file: file itself (target ""), or target, where the link file
// points. One rule and one hint for both.
func (c *Context) kubeconfigNotRegular(action, file, target string) error {
	if target == "" {
		c.errorf("kubehz %s: %s is not a regular file", action, file)
	} else {
		c.errorf("kubehz %s: %s is a link, and its target %s is not a regular file", action, file, target)
	}
	c.echoErr("  lo replaces only a regular file. Name another file.")
	return ErrHandled
}

// kubeconfigUnwritable prints the refusal of a path that lo cannot write.
func (c *Context) kubeconfigUnwritable(action, file string) error {
	c.errorf("kubehz %s: cannot write %s", action, file)
	c.echoErr("  Name a file in a directory that exists and that you can write to.")
	return ErrHandled
}

// kubeconfigBadPart prints the refusal of a path with a part that is not
// a directory that exists, with why.
func (c *Context) kubeconfigBadPart(action, file, part, why string) error {
	c.errorf("kubehz %s: cannot write %s: %s %s", action, file, part, why)
	c.echoErr("  Name a file in a directory that exists and that you can write to.")
	return ErrHandled
}

// kubeconfigNoWorkDir prints the refusal of a relative path when the
// working directory cannot be read (it was removed, for example).
func (c *Context) kubeconfigNoWorkDir(action, file string) error {
	c.errorf("kubehz %s: cannot write %s: the working directory cannot be read", action, file)
	c.echoErr("  Run lo in a directory that exists, or name the file with its full path.")
	return ErrHandled
}

// rootIfEmpty is p, or "/" for the empty path of a walk at the root.
func rootIfEmpty(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// inProcOrDev reports whether file is in /proc or /dev, or whether a link
// on its path points there. The kernel resolves /proc/self against the
// process that asks. Thus for lo, /proc/self/exe is the lo binary and
// /proc/self/fd/1 is the file that stdout goes to, and a resolved path
// looks like a usual file. The walk reads each link itself, hop by hop, and
// checks each component before it reads a link there. ".." goes up from
// the resolved path, as in the kernel. A relative path starts at the
// working directory as the kernel knows it (getcwd, see linkTarget): with
// PWD=/proc/self/cwd, os.Getwd would start the walk in /proc. After 40
// links the walk stops with false: the write then fails anyway.
//
// The bash twin is kubehz::agent_in_proc_or_dev.
func inProcOrDev(file string) bool {
	rest := file
	if !strings.HasPrefix(rest, "/") {
		wd, err := syscall.Getwd()
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

// publishKubeconfig writes data to target through a temporary file next to
// it (mode 0600). It opens the directory of target one time, as an os.Root
// (openKubeconfigRoot). After a successful open, each step works relative
// to that directory: the create of the temporary file, the rename, the
// link, the exclusive create, each check of a name and the cleanup. The
// names are one component each. Thus when a directory of the path changes
// after the open (another directory, or a link put in its place), no step
// goes to another directory. A process that can change the entries of that
// directory can still interfere: it can put another file in place of the
// temporary file before the link or the rename, or put a file at a name
// between a check and the remove after it. The open needs the read right
// on the directory (os.OpenRoot opens it read-only), so a directory with
// only the write and the search rights (mode 0300) fails
// (kubeconfigOpenError). A last name that is empty, "." or ".." names no
// file in the directory: the publish refuses it before the open
// (errKubeconfigNoName).
//
// With force, a rename replaces the target. Without force, a hard link
// publishes the file. link(2) fails when the name exists, so a file that
// appeared during the download stays as it is. A link that fails with
// EEXIST is the error, with no second write. Any other link error takes an
// exclusive create (createKubeconfig), which also fails when the name
// exists and replaces nothing. A file system with no hard links (vfat,
// exFAT, some FUSE and SMB mounts) fails the link with EPERM, ENOTSUP,
// EOPNOTSUPP or ENOSYS, and the texts and the numbers differ between
// systems. An error that the create cannot get past (EACCES, EROFS,
// ENOSPC) fails the create too. The fallback also needs the temporary file
// at its name, the file that it wrote (os.SameFile of its Stat and an
// Lstat of the name). Else another process moved it, and that is the link
// error too. On each path except a rename that worked, the cleanup makes
// one attempt to remove the temporary file (removeKubeconfigTemp). For a
// link, target is the file that linkTarget checked: no other user can
// change a directory from / down to it, so the path names that file up to
// the open. The open takes the directory of target as written
// (kubeconfigTempDir), as the kernel resolves it for target. wide is the
// mode of the file that the fallback made when it has group or other bits
// (vfat, exFAT), else 0.
//
// The bash twin is kubehz::agent_write. It does each step by path, so
// there a change of a directory of the path during the write can still
// send a step to another directory. It needs no read right on the
// directory, and it removes the temporary file by path with no check.
func publishKubeconfig(target string, data []byte, force bool) (wide os.FileMode, err error) {
	dir, base := kubeconfigTempDir(target)
	if base == "" || base == "." || base == ".." {
		return 0, errKubeconfigNoName
	}
	root, err := openKubeconfigRoot(dir)
	if err != nil {
		return 0, &kubeconfigOpenError{dir: dir, err: err}
	}
	defer func() { _ = root.Close() }()
	tmp, name, err := createKubeconfigTemp(root)
	if err != nil {
		return 0, err
	}
	_, err = tmp.Write(data)
	wrote, serr := tmp.Stat()
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && force {
		if err = root.Rename(name, base); err == nil {
			return 0, nil
		}
	} else if err == nil {
		err = linkKubeconfig(root, name, base)
		if err != nil && !errors.Is(err, os.ErrExist) && serr == nil && kubeconfigPathNames(root, name, wrote) {
			wide, err = createKubeconfig(root, base, data)
		}
	}
	removeKubeconfigTemp(root, name, wrote, serr)
	return wide, err
}

// removeKubeconfigTemp makes the one attempt to remove the temporary file
// name in root. When the Stat of the temporary file worked (serr is nil),
// it removes the name only while the name names that file
// (removeKubeconfigPart): another process can have put another file there.
// The check and the remove are two steps, so a file that another process
// puts at the name between them goes. When the Stat failed, it removes the
// name with no check: a temporary file that stays holds the kubeconfig,
// and that is worse.
func removeKubeconfigTemp(root *os.Root, name string, wrote os.FileInfo, serr error) {
	if serr != nil {
		_ = root.Remove(name)
		return
	}
	_ = removeKubeconfigPart(root, name, wrote)
}

// errKubeconfigNoName: the last name of the target is empty, "." or "..",
// so the target names a directory, not a file in it. kubeconfigTarget
// refuses such a path first (a directory, or a "/" at the end); the
// publish checks again before it makes the temporary file.
var errKubeconfigNoName = errors.New("the path names a directory, not a file in it")

// kubeconfigOpenError is an open of the directory of the target that
// failed (openKubeconfigRoot). dir is the directory as written.
type kubeconfigOpenError struct {
	dir string
	err error
}

func (e *kubeconfigOpenError) Error() string { return e.err.Error() }

func (e *kubeconfigOpenError) Unwrap() error { return e.err }

// openKubeconfigRoot is os.OpenRoot. A test replaces it, to change a
// directory of the path after the open and before the first step.
var openKubeconfigRoot = os.OpenRoot

// createKubeconfigTemp makes the temporary file in root: a new name
// (".kubeconfig-" and 26 random characters of crypto/rand), mode 0600,
// with an exclusive create (O_EXCL). os.Root has no CreateTemp. The name
// has 130 random bits, so the create does not try a second name: a file
// with that name already there is an error.
func createKubeconfigTemp(root *os.Root) (*os.File, string, error) {
	name := ".kubeconfig-" + rand.Text()
	f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	return f, name, err
}

// kubeconfigPathNames reports whether name in root names the file of fi
// itself, not a link to it.
func kubeconfigPathNames(root *os.Root, name string, fi os.FileInfo) bool {
	now, err := root.Lstat(name)
	return err == nil && os.SameFile(fi, now)
}

// linkKubeconfig makes newname a hard link to oldname, both in root. A
// test replaces it, to make the link fail as it fails on a file system
// with no hard links.
var linkKubeconfig = func(root *os.Root, oldname, newname string) error {
	return root.Link(oldname, newname)
}

// createKubeconfig writes data to a new file name in root, mode 0600, with
// an exclusive create (O_EXCL). The create fails when the name exists, a
// link too, so a file that appeared during the download stays as it is.
// Unlike the hard link, a reader can see the file before all of it is
// written. The steps before the first byte, as in the bash twin (bash
// also checks descriptor 3 and refuses under a default ACL):
//   - chmod 0600 on the open file. A default ACL of the directory replaces
//     the umask; the create asks for 0600, but the bash twin asks for 0666.
//     vfat can refuse the chmod, so the mode that the Stat reads back
//     decides.
//   - the name must still name the file of f (kubeconfigPathNames), and
//     the Stat must read its mode. Else nothing is written or removed.
//
// When the write or the close fails after the create (a short write,
// ENOSPC, EIO), the name holds a part of the file: createKubeconfig removes
// it while the name names the file of f, and returns a
// kubeconfigWriteError. Its left is true when the part was not removed.
// The check and the remove are two steps. A failed create removes nothing.
// wide is the mode read back when it has group or other bits, else 0.
func createKubeconfig(root *os.Root, name string, data []byte) (wide os.FileMode, err error) {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	_ = chmodKubeconfig(f)
	made, err := f.Stat()
	if err == nil && !kubeconfigPathNames(root, name, made) {
		err = errKubeconfigMoved
	}
	if err != nil {
		_ = f.Close()
		return 0, err
	}
	if err = writeKubeconfigFile(f, data); err != nil {
		// While f is open, so that the inode is not used again.
		left := !removeKubeconfigPart(root, name, made)
		_ = f.Close()
		return 0, &kubeconfigWriteError{err: err, left: left}
	}
	if err = closeKubeconfigFile(f); err != nil {
		return 0, &kubeconfigWriteError{err: err, left: !removeKubeconfigPart(root, name, made)}
	}
	if perm := made.Mode().Perm(); perm&0o077 != 0 {
		return perm, nil
	}
	return 0, nil
}

// removeKubeconfigPart removes name in root while it names the file of
// made, and reports whether it did.
func removeKubeconfigPart(root *os.Root, name string, made os.FileInfo) bool {
	return kubeconfigPathNames(root, name, made) && root.Remove(name) == nil
}

// errKubeconfigMoved: after the exclusive create, the name no longer names
// the file that the create made.
var errKubeconfigMoved = errors.New("the path no longer names the new file")

// chmodKubeconfig sets mode 0600 on the file that the exclusive create
// made. A test replaces it, to give the file the mode that a vfat mount
// sets.
var chmodKubeconfig = func(f *os.File) error { return f.Chmod(0o600) }

// writeKubeconfigFile writes data to f. A test replaces it, to make the
// write fail after the exclusive create.
var writeKubeconfigFile = func(f *os.File, data []byte) error {
	_, err := f.Write(data)
	return err
}

// closeKubeconfigFile closes the file of the exclusive create after the
// write. A test replaces it, to make the close fail as it can fail on a
// network file system (EIO).
var closeKubeconfigFile = func(f *os.File) error { return f.Close() }

// kubeconfigWriteError is a write or a close that failed after the
// exclusive create made the file (createKubeconfig). left is true when the
// part of the file was not removed.
type kubeconfigWriteError struct {
	err  error
	left bool
}

func (e *kubeconfigWriteError) Error() string { return e.err.Error() }

func (e *kubeconfigWriteError) Unwrap() error { return e.err }

// cause is the text of the system error without the operation and the
// path ("no space left on device"). The bash twin prints the C-locale
// text of the shell with its first letter in lower case.
func (e *kubeconfigWriteError) cause() string {
	if pe, ok := errors.AsType[*os.PathError](e.err); ok {
		return pe.Err.Error()
	}
	return e.err.Error()
}

// kubeconfigTempDir splits target at its last "/". dir is the directory of
// target as written: the part before the last "/" ("/" for a name in /,
// "." for a name without a "/"). name is the part after it (empty when
// target ends in "/"). One split gives both, so the directory that
// publishKubeconfig opens and the name that it writes there come from the
// same "/". dir is not cleaned: filepath.Dir makes "alias/../config" a
// file of the directory that holds alias, but the kernel goes up from
// where alias points. The kernel resolves the part as it resolves target,
// so publishKubeconfig opens the directory of target, and the temporary
// file and the rename stay in it. The bash twin is in kubehz::agent_write.
func kubeconfigTempDir(target string) (dir, name string) {
	i := strings.LastIndex(target, "/")
	switch {
	case i < 0:
		return ".", target
	case i == 0:
		return "/", target[1:]
	}
	return target[:i], target[i+1:]
}
