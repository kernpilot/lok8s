package kubehz

// agent.go — `lo kubehz space …` and `lo kubehz cluster …`: the kubehz api
// calls an AI agent or a pipeline makes with an agent key. `lo mcp` turns
// these commands into tools like every other leaf of the tree, so an agent
// that runs `lo mcp` locally manages spaces and hosted-cluster leases with
// the key in its environment.
//
// THE CREDENTIAL: the agent key (KUBEHZ_AGENT_CLIENT_ID,
// KUBEHZ_AGENT_CLIENT_SECRET, KUBEHZ_AGENT_TOKEN_URL, KUBEHZ_AGENT_SCOPE),
// turned into an access token by the grant `lo kubehz token` runs (the same
// cache file). Without a key, KUBEHZ_TOKEN is the bearer. The api base URL
// is KUBEHZ_API_URL. No flag names the URL or carries a credential: a model
// that could set the URL could send the bearer to any host.
//
// lo adds no authority. The api enforces the role of the key, its scope
// (tenant or resources), its monthly spend cap and the lease (what a key
// creates ends after two hours unless the key extends it). lo checks only
// the shape of what it sends: an id that goes into a URL path, and whole
// numbers.
//
// OUTPUT: -o text (a table for a list, label lines for one record), json or
// yaml (internal/cli writeOutput) with fixed lowerCamel fields. The records
// are projections, so a new api field changes no output. Every string from
// the api loses its control characters (C0, DEL, U+2028, U+2029) before it
// is printed, in every format.
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
	"unicode/utf8"
)

// EnvAPIURL is the kubehz api base URL of the agent commands.
const EnvAPIURL = "KUBEHZ_API_URL"

const (
	// agentRequestTimeout bounds one api call, the body included: an
	// agent waits on the tool, so a stalled api must end it (bash: curl
	// --max-time 60).
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

// agentSession resolves the api URL and the bearer. Every refusal is
// printed here. The bash twin is kubehz::agent_session.
func (c *Context) agentSession(ctx context.Context, action string) (*agentSession, error) {
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
	// A line break would end the bash twin's curl config line and make
	// the rest a curl option: both refuse it.
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

// agentCall is one api request: the bearer in the Authorization header, no
// redirect followed (a 3xx is a refusal), bounded by agentRequestTimeout.
// err only for a transport failure, already printed. The bash twin is
// kubehz::agent_api.
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

// agentRefused prints a non-2xx answer: what failed with the api's status,
// code and message, then the next step. lo's own hint wins where lo knows
// more than the api (a flag name, the list command); else the api's help.
// Both server strings are scrubbed and clipped. The bash twin is
// kubehz::agent_refused.
func (c *Context) agentRefused(s *agentSession, k agentKind, action, id string, res *httpResult) {
	code := apiCode(res.Body)
	if !apiCodeRe.MatchString(code) {
		code = ""
	}
	msg := clip(scrub(apiMessage(res.Body)))
	if msg == "" {
		msg = "no reason given"
	}
	c.errorf("kubehz %s: the api refused the request (HTTP %d%s): %s", action, res.Status, optSuffix(" ", code), msg)
	if hint := agentHint(s, k, id, res.Status, code); hint != "" {
		c.echoErr("  %s", hint)
		return
	}
	if help := clip(scrub(apiHelp(res.Body))); help != "" {
		c.echoErr("  %s", help)
	}
}

// agentHint is lo's next step for a refusal, "" when the api's help says
// it best.
func agentHint(s *agentSession, k agentKind, id string, status int, code string) string {
	switch {
	case status == http.StatusUnauthorized && s.key:
		return "The api did not accept the agent key. A revoked or expired key needs a new key from a tenant owner."
	case status == http.StatusUnauthorized:
		return "The api did not accept KUBEHZ_TOKEN. Mint a new token in the kubehz dashboard."
	case code == "TOKEN_SCOPE_MISSING":
		return "The credential can read but not write. Use an agent key with the role editor or admin."
	case code == "AGENT_KEY_OUT_OF_SCOPE":
		return "The agent key reaches only its own clusters and spaces. Use a key with the scope tenant."
	case code == "AGENT_KEY_SPEND_CAP":
		return "Delete what the agent key created, or ask a tenant owner for a key with a higher spend cap. The count starts again on the first day of the month (UTC)."
	case code == "SPACE_LIMITS_ABOVE_FREE" || code == "SPACE_LIMITS_ABOVE_SHARED":
		return "Lower --nodes, --namespaces or --object-cap-kib. A value you leave out takes the platform default."
	case code == "NO_SHARD_AVAILABLE" || code == "SHARD_AT_CAPACITY":
		return "This is a platform capacity limit, not an account limit. Try again later."
	case status == http.StatusNotFound && id != "":
		return fmt.Sprintf("No %s %s exists, or the credential cannot reach it. List what it reaches: lo kubehz %s list", k.noun, id, k.noun)
	}
	return ""
}

// agentCheckID refuses an id that is not the api's shape. The refusal
// names the command without the id: the id is user input, shown once,
// scrubbed and clipped.
func (c *Context) agentCheckID(k agentKind, verb, id string) error {
	if k.id.MatchString(id) {
		return nil
	}
	c.errorf("kubehz %s %s: %s is not a %s id", k.noun, verb, clip(scrub(id)), k.noun)
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
// only, inside the int64 range (the frozen tree's kubehz::space_number_value
// accepts the same set; "008" is eight).
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
	c.errorf("kubehz %s: --%s %s is not valid", action, flag, clip(scrub(s)))
	c.echoErr("  Use a whole number of hours from 1 to %d.", LeaseMaxHours)
	return 0, ErrHandled
}

// ── records ──────────────────────────────────────────────

// cleanText drops the characters that can move a terminal or end a line:
// C0 controls, DEL, U+2028 and U+2029. The bash twin's jq `clean` drops the
// same set.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, s)
}

// text is a string field of an api record, cleaned; nil (JSON null) for a
// missing field or another type.
func text(v any) any {
	if s, ok := v.(string); ok {
		return cleanText(s)
	}
	return nil
}

// number is a number field of an api record as written; nil otherwise.
func number(v any) any {
	if n, ok := v.(json.Number); ok {
		return n
	}
	return nil
}

// texts is a list of strings, cleaned; other entries are left out.
func texts(v any) []string {
	out := []string{}
	arr, _ := v.([]any)
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, cleanText(s))
		}
	}
	return out
}

// cell renders a record value for the text form: "-" for null or "".
func cell(v any) string {
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
		ID: text(m["id"]), Name: text(m["name"]), Slug: text(m["slug"]), Status: text(m["status"]),
		MaxNodes: number(m["maxNodes"]), MaxNamespaces: number(m["maxNamespaces"]), MaxObjectKiB: number(m["maxObjectKiB"]),
		NodeCount: number(m["nodeCount"]), Namespaces: texts(m["namespaces"]),
		LeaseExpiresAt: text(m["leaseExpiresAt"]), CreatedAt: text(m["createdAt"]),
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
	d := &SpaceDetail{SpaceRecord: spaceRecord(m), Endpoint: text(m["endpoint"]), Nodes: []SpaceNode{}}
	d.NodeCount = number(jget(m, "usage", "nodes"))
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
				d.Nodes = append(d.Nodes, SpaceNode{Name: text(n["name"]), Status: text(n["status"])})
			}
		}
	}
	return d
}

// labelLine is one line of the text form of a record.
func labelLine(w io.Writer, label, value string) {
	fmt.Fprintf(w, "%-13s%s\n", label+":", value)
}

func (r *SpaceRecord) writeLabels(w io.Writer, endpoint any, withEndpoint bool) {
	ns := strings.Join(r.Namespaces, ", ")
	if ns == "" {
		ns = "-"
	}
	objectCap := "-"
	if r.MaxObjectKiB != nil {
		objectCap = cell(r.MaxObjectKiB) + " KiB"
	}
	labelLine(w, "ID", cell(r.ID))
	labelLine(w, "Name", cell(r.Name))
	labelLine(w, "Slug", cell(r.Slug))
	labelLine(w, "Status", cell(r.Status))
	labelLine(w, "Nodes", cell(r.NodeCount)+" of "+cell(r.MaxNodes))
	labelLine(w, "Namespaces", ns+" (limit "+cell(r.MaxNamespaces)+")")
	labelLine(w, "Object cap", objectCap)
	if withEndpoint {
		labelLine(w, "Endpoint", cell(endpoint))
	}
	labelLine(w, "Lease ends", cell(r.LeaseExpiresAt))
	labelLine(w, "Created", cell(r.CreatedAt))
}

// WriteText prints the created space.
func (r *SpaceRecord) WriteText(w io.Writer) { r.writeLabels(w, nil, false) }

// WriteText prints one space with its endpoint.
func (d *SpaceDetail) WriteText(w io.Writer) { d.writeLabels(w, d.Endpoint, true) }

// SpaceList is `lo kubehz space list`.
type SpaceList struct {
	Spaces []SpaceRecord `json:"spaces"`
}

// WriteText prints the table.
func (l *SpaceList) WriteText(w io.Writer) {
	rows := [][]string{{"ID", "SLUG", "NAME", "STATUS", "NODES", "LEASE ENDS"}}
	for _, s := range l.Spaces {
		rows = append(rows, []string{cell(s.ID), cell(s.Slug), cell(s.Name), cell(s.Status),
			cell(s.NodeCount) + "/" + cell(s.MaxNodes), cell(s.LeaseExpiresAt)})
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
func clusterRecord(m map[string]any) *ClusterRecord {
	return &ClusterRecord{
		ID: text(m["id"]), Domain: text(m["domain"]), Hosting: text(m["hosting"]), Status: text(m["status"]),
		Region: text(m["region"]), KubernetesVersion: text(m["kubernetesVersion"]),
		ControlPlaneReplicas: number(m["controlPlaneReplicas"]), APIEndpoint: text(m["apiEndpoint"]),
		Health: text(m["health"]), LeaseExpiresAt: text(m["leaseExpiresAt"]), CreatedAt: text(m["createdAt"]),
	}
}

// WriteText prints one cluster.
func (r *ClusterRecord) WriteText(w io.Writer) {
	labelLine(w, "ID", cell(r.ID))
	labelLine(w, "Domain", cell(r.Domain))
	labelLine(w, "Hosting", cell(r.Hosting))
	labelLine(w, "Status", cell(r.Status))
	labelLine(w, "Health", cell(r.Health))
	labelLine(w, "Region", cell(r.Region))
	labelLine(w, "Version", cell(r.KubernetesVersion))
	labelLine(w, "Apiservers", cell(r.ControlPlaneReplicas))
	labelLine(w, "Endpoint", cell(r.APIEndpoint))
	labelLine(w, "Lease ends", cell(r.LeaseExpiresAt))
	labelLine(w, "Created", cell(r.CreatedAt))
}

// ClusterList is `lo kubehz cluster list`.
type ClusterList struct {
	Clusters []ClusterRecord `json:"clusters"`
}

// WriteText prints the table.
func (l *ClusterList) WriteText(w io.Writer) {
	rows := [][]string{{"ID", "DOMAIN", "HOSTING", "STATUS", "VERSION", "LEASE ENDS"}}
	for _, r := range l.Clusters {
		rows = append(rows, []string{cell(r.ID), cell(r.Domain), cell(r.Hosting), cell(r.Status),
			cell(r.KubernetesVersion), cell(r.LeaseExpiresAt)})
	}
	writeTable(w, rows)
}

// writeTable pads every column but the last to its widest cell, in
// characters (code points, as jq's `length` in the bash twin counts them),
// two spaces apart.
func writeTable(w io.Writer, rows [][]string) {
	widths := make([]int, len(rows[0]))
	for _, row := range rows {
		for i, c := range row {
			widths[i] = max(widths[i], utf8.RuneCountInString(c))
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for i, c := range row {
			if i == len(row)-1 {
				b.WriteString(c)
				break
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+2))
		}
		fmt.Fprintln(w, b.String())
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
	fmt.Fprintf(w, "%s %s: %s\n", r.noun, cell(r.ID), cell(r.Status))
}

// LeaseResult is `lo kubehz space|cluster lease`.
type LeaseResult struct {
	ID             any `json:"id"`
	LeaseExpiresAt any `json:"leaseExpiresAt"`
	noun           string
}

// WriteText prints when the lease ends.
func (r *LeaseResult) WriteText(w io.Writer) {
	fmt.Fprintf(w, "%s %s: the lease ends %s\n", r.noun, cell(r.ID), cell(r.LeaseExpiresAt))
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
	s, err := c.agentSession(ctx, action)
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
	s, err := c.agentSession(ctx, action)
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

// SpaceCreateOptions are the flags of `lo kubehz space create`; "" is a
// flag not given (the api applies its own default).
type SpaceCreateOptions struct {
	Name, Slug                                  string
	Nodes, Namespaces, ObjectCapKiB, LeaseHours string
	Region                                      string
}

// SpaceCreate creates a space. A number the flags leave out is not sent.
func (c *Context) SpaceCreate(ctx context.Context, o SpaceCreateOptions) (*SpaceRecord, error) {
	action := "space create " + clip(scrub(o.Slug))
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
			c.errorf("kubehz %s: --%s %s is not valid", action, f.flag, clip(scrub(f.value)))
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
	return &DeleteResult{ID: text(m["id"]), Status: text(m["status"]), noun: "space"}, nil
}

// SpaceLease sets the lease of a space: hours from now.
func (c *Context) SpaceLease(ctx context.Context, id, hours string) (*LeaseResult, error) {
	return c.agentLease(ctx, kindSpace, id, hours)
}

// SpaceKubeconfig writes the agent kubeconfig of a space to file.
func (c *Context) SpaceKubeconfig(ctx context.Context, id, file string) (*KubeconfigResult, error) {
	return c.agentKubeconfig(ctx, kindSpace, id, file)
}

// ClusterList lists the clusters the credential reaches.
func (c *Context) ClusterList(ctx context.Context) (*ClusterList, error) {
	rows, err := c.agentList(ctx, kindCluster)
	if err != nil {
		return nil, err
	}
	l := &ClusterList{Clusters: []ClusterRecord{}}
	for _, m := range rows {
		l.Clusters = append(l.Clusters, *clusterRecord(m))
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
	return clusterRecord(m), nil
}

// ClusterLease sets the lease of a hosted cluster: hours from now.
func (c *Context) ClusterLease(ctx context.Context, id, hours string) (*LeaseResult, error) {
	return c.agentLease(ctx, kindCluster, id, hours)
}

// ClusterKubeconfig writes the agent kubeconfig of a hosted cluster to file.
func (c *Context) ClusterKubeconfig(ctx context.Context, id, file string) (*KubeconfigResult, error) {
	return c.agentKubeconfig(ctx, kindCluster, id, file)
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
	return &LeaseResult{ID: text(m["id"]), LeaseExpiresAt: text(m["leaseExpiresAt"]), noun: k.noun}, nil
}

// agentKubeconfig downloads <collection>/<id>/kubeconfig/agent and writes it
// to file (0600, through a temporary file in the same directory). The file
// holds no secret: its exec stanza runs `lo kubehz token`. The command still
// counts as credential output, so `lo mcp` never offers it.
func (c *Context) agentKubeconfig(ctx context.Context, k agentKind, id, file string) (*KubeconfigResult, error) {
	if err := c.agentCheckID(k, "kubeconfig", id); err != nil {
		return nil, err
	}
	action := k.noun + " kubeconfig " + id
	s, err := c.agentSession(ctx, action)
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
	if err := writePrivateFile(file, res.Body); err != nil {
		c.errorf("kubehz %s: cannot write %s", action, file)
		c.echoErr("  Name a file in a directory that exists and that you can write to.")
		return nil, ErrHandled
	}
	return &KubeconfigResult{ID: id, File: file}, nil
}

// writePrivateFile replaces file with data, mode 0600, through a temporary
// file next to it. A directory is never replaced: rename(2) refuses to put
// a file over a directory (EISDIR). The bash twin checks with -d first,
// because mv would move the file into the directory.
func writePrivateFile(file string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".kubeconfig-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, file)
	}
	if err != nil {
		_ = os.Remove(name)
	}
	return err
}
