package kubehz

// shared.go — libs/kubehz/shared: Spaces on the kubehz shared control plane
// (hosting: shared). The platform runs and secures the control plane; you
// bring the machines. `lo provision` creates (or adopts) the Space and mints
// single-node join tickets; `lo destroy` deregisters it.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// The three numbers a space is made of, with the bounds the platform
// accepts. A space has no plan: the tenant sets the node ceiling, the
// namespace ceiling and the object cap (the size limit for one Secret or
// ConfigMap in the space's namespaces), under spec.kubehz.space.limits.
const (
	spaceNodesDefault      = 2
	spaceNodesMin          = 1
	spaceNodesMax          = 5
	spaceNamespacesDefault = 1
	spaceNamespacesMin     = 1
	spaceNamespacesMax     = 3
	spaceObjectCapDefault  = 256
	spaceObjectCapMin      = 64
	spaceObjectCapMax      = 512
)

// SpaceConfig is the LOK8S_SPACE_* export set of kubehz::space_config.
type SpaceConfig struct {
	Slug   string
	Name   string
	Region string
	// MaxNodes, MaxNamespaces and MaxObjectKiB are the space's three
	// numbers (spec.kubehz.space.limits.{nodes,namespaces,objectCapKiB}).
	MaxNodes      int
	MaxNamespaces int
	MaxObjectKiB  int
	// Nodes are the machines to mint a join ticket for
	// (spec.kubehz.space.nodes).
	Nodes []string
}

// SpaceConfig ports kubehz::space_config: the slug defaults to the first DNS
// label of the domain, the display name to the slug, and the three limits to
// 2 nodes, 1 namespace and a 256 KiB object cap. A yq PARSE failure is an
// error, never a defaulted slug (destroy would target the WRONG space).
func (c *Context) SpaceConfig(domain, clusterYAML string) (*SpaceConfig, error) {
	doc := loadSpec(clusterYAML)
	if doc.Err != nil {
		c.errorf("cannot parse cluster spec: %s: %v", clusterYAML, doc.Err)
		return nil, ErrHandled
	}
	defaultSlug := domain
	if before, _, ok := strings.Cut(domain, "."); ok {
		defaultSlug = before
	}
	nodes, namespaces, objectCap, err := c.spaceLimits(doc)
	if err != nil {
		return nil, err
	}
	sp := &SpaceConfig{
		Slug:          doc.Or("", "spec", "kubehz", "space", "slug"),
		Name:          doc.Or("", "spec", "kubehz", "space", "name"),
		Region:        doc.Or("", "spec", "kubehz", "space", "region"),
		MaxNodes:      nodes,
		MaxNamespaces: namespaces,
		MaxObjectKiB:  objectCap,
	}
	if sp.Slug == "" {
		sp.Slug = defaultSlug
	}
	if sp.Name == "" {
		sp.Name = sp.Slug
	}
	for _, n := range doc.seqStrings("spec", "kubehz", "space", "nodes") {
		if n != "" && n != "null" {
			sp.Nodes = append(sp.Nodes, n)
		}
	}
	return sp, nil
}

// spaceLimits reads and bounds the three numbers of a space. A value outside
// its range is refused here, before the api sees it: the message names the
// field, the value and the range. A spec without the block takes all three
// defaults.
func (c *Context) spaceLimits(doc specDoc) (nodes, namespaces, objectCap int, err error) {
	// Space plans are retired. A spec that still carries one asks for a
	// shape the platform no longer has, so say so instead of ignoring it.
	if plan := doc.Or("", "spec", "kubehz", "space", "plan"); plan != "" && plan != "null" {
		c.errorf("spec.kubehz.space.plan is not valid: space plans are retired")
		c.echoErr("  Set spec.kubehz.space.limits.nodes, spec.kubehz.space.limits.namespaces")
		c.echoErr("  and spec.kubehz.space.limits.objectCapKiB instead.")
		return 0, 0, 0, ErrHandled
	}
	if nodes, err = c.spaceNumber(doc, "nodes", spaceNodesDefault, spaceNodesMin, spaceNodesMax); err != nil {
		return 0, 0, 0, err
	}
	if namespaces, err = c.spaceNumber(doc, "namespaces", spaceNamespacesDefault, spaceNamespacesMin, spaceNamespacesMax); err != nil {
		return 0, 0, 0, err
	}
	if objectCap, err = c.spaceNumber(doc, "objectCapKiB", spaceObjectCapDefault, spaceObjectCapMin, spaceObjectCapMax); err != nil {
		return 0, 0, 0, err
	}
	return nodes, namespaces, objectCap, nil
}

// spaceNumber reads one of the three numbers. A missing or null value takes
// the default. spec.kubehz.space.nodes is the machine-name list, so a list
// under limits.nodes is the two fields confused: name both.
func (c *Context) spaceNumber(doc specDoc, field string, def, min, max int) (int, error) {
	n := doc.Lookup("spec", "kubehz", "space", "limits", field)
	if yqsem.IsNull(n) {
		return def, nil
	}
	if n.Kind == yaml.SequenceNode && field == "nodes" {
		c.errorf("spec.kubehz.space.limits.nodes is a list: it is the node ceiling")
		c.echoErr("  Set spec.kubehz.space.limits.nodes to a whole number from %d to %d.", spaceNodesMin, spaceNodesMax)
		c.echoErr("  The machine names stay under spec.kubehz.space.nodes.")
		return 0, ErrHandled
	}
	// Only a list and a map hold no single value to quote back. Every other
	// type is a scalar, so it falls through to the read below and the refusal
	// names what the spec says (2.5, true, two). The bash twin does the same.
	if n.Kind != yaml.ScalarNode {
		c.errorf("invalid spec.kubehz.space.limits.%s: expected a whole number from %d to %d", field, min, max)
		return 0, ErrHandled
	}
	value, err := strconv.Atoi(strings.TrimSpace(n.Value))
	if err != nil || value < min || value > max {
		c.errorf("invalid spec.kubehz.space.limits.%s: %s (expected a whole number from %d to %d)", field, n.Value, min, max)
		return 0, ErrHandled
	}
	return value, nil
}

// spaceAPI ports kubehz::space_api: one call with the envelope contract.
// Returns status + body; err only for a transport failure (already
// rendered, the "unreachable" line). Callers branch on is2xx(status).
func (c *Context) spaceAPI(ctx context.Context, cfg *Config, method, path string, body []byte) (*httpResult, error) {
	res, err := c.fetchStatus(ctx, method, cfg.APIURL+path, withBearer(c.getenv("KUBEHZ_TOKEN")), body)
	if err != nil {
		c.errorf("kubehz API unreachable (%s %s)", method, path)
		return nil, ErrHandled
	}
	return res, nil
}

// spaceAPIQuiet is `kubehz::space_api … 2>/dev/null || true`: a transport
// failure yields an empty result instead of an error line.
func (c *Context) spaceAPIQuiet(ctx context.Context, cfg *Config, method, path string) *httpResult {
	res, err := c.fetchStatus(ctx, method, cfg.APIURL+path, withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil {
		return &httpResult{}
	}
	return res
}

// spaceAPIError ports kubehz::space_api_error: the message + help halves of
// a non-2xx envelope. Both are server strings: scrubbed before they reach
// the terminal (node.go does the same for every api string it shows).
func (c *Context) spaceAPIError(what string, res *httpResult) {
	msg := scrub(apiMessage(res.Body))
	help := scrub(apiHelp(res.Body))
	c.errorf("%s (HTTP %d)%s", what, res.Status, optSuffix(": ", msg))
	if help != "" {
		c.echoErr("  %s", help)
	}
}

// spaceLookup ports kubehz::space_lookup: the Space row by slug (nil when
// absent).
func (c *Context) spaceLookup(ctx context.Context, cfg *Config, slug string) (any, error) {
	res, err := c.spaceAPI(ctx, cfg, "GET", "/api/spaces", nil)
	if err != nil {
		return nil, err
	}
	if !is2xx(res.Status) {
		c.spaceAPIError("Failed to list spaces", res)
		return nil, ErrHandled
	}
	v, _ := parseJSON(res.Body)
	for _, r := range rows(v) {
		if s, ok := jget(r, "slug").(string); ok && s == slug {
			return r, nil
		}
	}
	return nil, nil
}

// spaceEnsure ports kubehz::space_ensure: create the Space, or adopt it
// when it already exists (idempotent re-provision). Returns the space id.
func (c *Context) spaceEnsure(ctx context.Context, cfg *Config, sp *SpaceConfig) (string, error) {
	row, err := c.spaceLookup(ctx, cfg, sp.Slug)
	if err != nil {
		return "", err
	}
	if row != nil {
		c.debugf("Space '%s' already exists — adopting", sp.Slug)
		id := jstrOr(row, "", "id")
		if id == "" {
			c.errorf("space row carries no id — refusing to continue")
			return "", ErrHandled
		}
		c.noteLimitsDrift(row, sp)
		return id, nil
	}

	pairs := []jsonPair{
		{"name", sp.Name},
		{"slug", sp.Slug},
		{"maxNodes", sp.MaxNodes},
		{"maxNamespaces", sp.MaxNamespaces},
		{"maxObjectKiB", sp.MaxObjectKiB},
	}
	if sp.Region != "" {
		pairs = append(pairs, jsonPair{"region", sp.Region})
	}
	res, err := c.spaceAPI(ctx, cfg, "POST", "/api/spaces", compactJSON(pairs...))
	if err != nil {
		return "", err
	}
	if !is2xx(res.Status) {
		switch apiCode(res.Body) {
		case "NO_SHARD_AVAILABLE":
			c.errorf("kubehz: no shared control plane has room for a new space right now.")
			c.echoErr("  Capacity frees as spaces are removed and as new planes come online. You can:")
			c.echoErr("    • retry later")
			c.echoErr("    • run your own cluster meanwhile (spec.kubehz.hosting: self)")
			return "", ErrHandled
		case "SPACE_LIMITS_ABOVE_FREE":
			// All three numbers on both sides: what the spec asks for, and
			// what a free account allows. Two of the three left the reader
			// guessing which number was too large.
			c.errorf("kubehz refused the space limits (%s): they are above the free allowance", sp.limitsLine())
			c.echoErr("  A free account gets 1 space with 2 nodes, 1 namespace and a 256 KiB object cap.")
			c.echoErr("  Decrease the values in spec.kubehz.space.limits, or upgrade the account in")
			c.echoErr("  the kubehz dashboard.")
			return "", ErrHandled
		case "SPACE_LIMITS_ABOVE_SHARED":
			// The ceiling moves with the account, so the api's own message
			// carries it. The fixed text is the fallback for an api that
			// sends the code alone.
			reason := clip(scrub(apiMessage(res.Body)))
			if reason == "" {
				reason = "they are above the maximum of a shared control plane"
			}
			c.errorf("kubehz refused the space limits (%s): %s", sp.limitsLine(), reason)
			c.echoErr("  Decrease the values in spec.kubehz.space.limits, or use a hosted control")
			c.echoErr("  plane: set spec.kubehz.hosting to hosted.")
			return "", ErrHandled
		case "SPACE_PLAN_RETIRED":
			c.errorf("kubehz refused the request: space plans are retired")
			c.echoErr("  A space uses spec.kubehz.space.limits: nodes, namespaces and objectCapKiB.")
			c.echoErr("  Remove spec.kubehz.space.plan from the cluster spec. Then run")
			c.echoErr("  lo provision again.")
			return "", ErrHandled
		default:
			// Lost a create race? The adopt path answers it — retry the lookup once.
			if res.Status == 409 {
				row, _ := c.spaceLookup(ctx, cfg, sp.Slug)
				if row != nil {
					c.debugf("Space '%s' appeared concurrently — adopting", sp.Slug)
					id := jstrOr(row, "", "id")
					if id == "" {
						c.errorf("space row carries no id — refusing to continue")
						return "", ErrHandled
					}
					return id, nil
				}
			}
			c.spaceAPIError("Failed to create the space", res)
			return "", ErrHandled
		}
	}
	v, _ := parseJSON(res.Body)
	return jstr(jalt(nil, jget(v, "data", "id"), jget(v, "id"))), nil
}

// limitsLine renders the three numbers for one terminal line.
func (sp *SpaceConfig) limitsLine() string {
	return "nodes " + strconv.Itoa(sp.MaxNodes) +
		", namespaces " + strconv.Itoa(sp.MaxNamespaces) +
		", object cap " + strconv.Itoa(sp.MaxObjectKiB) + " KiB"
}

// noteLimitsDrift reports a space whose limits differ from the spec. Adoption
// stays read-only: lo creates a space with the three numbers, and never
// changes the numbers of a space that exists. Without this note the edit in
// the spec would do nothing and say nothing.
func (c *Context) noteLimitsDrift(row any, sp *SpaceConfig) {
	liveNodes := jstrOr(row, "", "maxNodes")
	liveNamespaces := jstrOr(row, "", "maxNamespaces")
	liveObjectCap := jstrOr(row, "", "maxObjectKiB")
	// An api that does not report the three numbers gives nothing to compare.
	if liveNodes == "" || liveNamespaces == "" || liveObjectCap == "" {
		return
	}
	if liveNodes == strconv.Itoa(sp.MaxNodes) &&
		liveNamespaces == strconv.Itoa(sp.MaxNamespaces) &&
		liveObjectCap == strconv.Itoa(sp.MaxObjectKiB) {
		return
	}
	c.echo("  Note: this space keeps the limits it was created with.")
	c.echo("  The spec asks for %s.", sp.limitsLine())
	c.echo("  lo does not change them. Change the limits in the kubehz dashboard.")
}

// spaceWaitActive ports kubehz::space_wait_active: wait for the Active
// phase, failing FAST on 401/403 (an expired token stays refused) and 404
// (the space vanished).
func (c *Context) spaceWaitActive(ctx context.Context, cfg *Config, spaceID string, timeout int) error {
	for elapsed := 0; elapsed < timeout; elapsed += 5 {
		res := c.spaceAPIQuiet(ctx, cfg, "GET", "/api/spaces/"+spaceID)
		switch res.Status {
		case 401, 403:
			c.errorf("kubehz API refused the token while waiting for space %s (HTTP %d)", spaceID, res.Status)
			return ErrHandled
		case 404:
			c.errorf("space %s vanished while waiting for it to become Active", spaceID)
			return ErrHandled
		}
		// The api serves the observed `status` overlay, never `phase`.
		phase := "Unknown"
		if v, ok := parseJSON(res.Body); ok {
			phase = jstr(jalt("Unknown", jget(v, "data", "status"), jget(v, "status")))
		}
		switch phase {
		case "Active":
			c.debugf("Space %s is Active", spaceID)
			return nil
		case "Failed", "Error":
			c.errorf("Space %s failed: phase=%s", spaceID, phase)
			return ErrHandled
		}
		c.debugf("Space %s phase: %s (%ds / %ds)", spaceID, phase, elapsed, timeout)
		if err := c.sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
	c.errorf("Timed out waiting for space %s to become Active after %ds", spaceID, timeout)
	return ErrHandled
}

// clip bounds a server string headed for one terminal line: a timestamp or
// an endpoint is a few dozen runes; past 256 the rest is not information.
func clip(s string) string {
	const maxRunes = 256
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// joinTokenRe is the kubeadm bootstrap-token shape the platform mints
// (`<6 id>.<16 secret>`, lowercase alphanumerics). Anything else is not a
// ticket this CLI hands to a machine — and, as a server string headed for
// the terminal, would be an injection vector.
var joinTokenRe = regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)

// spaceMintJoin ports kubehz::space_mint_join: mint a single-node join
// ticket and print the join block. The plaintext token is returned exactly
// once by the api and never persisted beyond the join script. Deviation
// D21 (Go-only): when the api ships a join script the ticket is NOT echoed
// to the terminal unless printToken is set — the script carries it, and a
// terminal (scrollback, CI logs, a shared screen) is a second copy nobody
// asked for. Without a script the terminal is the only channel, so the
// ticket is printed as before.
func (c *Context) spaceMintJoin(ctx context.Context, cfg *Config, spaceID, nodeName string, printToken bool) error {
	res, err := c.spaceAPI(ctx, cfg, "POST", "/api/spaces/"+spaceID+"/join-token", compactJSON(jsonPair{"nodeName", nodeName}))
	if err != nil {
		return err
	}
	if !is2xx(res.Status) {
		c.spaceAPIError("Failed to mint a join ticket for '"+nodeName+"'", res)
		return ErrHandled
	}
	// Every string below comes from the server: scrubbed and bounded (or
	// shape-checked) before it reaches the terminal.
	v, _ := parseJSON(res.Body)
	token := jstr(jalt("", jget(v, "data", "token"), jget(v, "token")))
	expires := clip(scrub(jstr(jalt("", jget(v, "data", "expiresAt"), jget(v, "expiresAt")))))
	script := jstr(jalt("", jget(v, "data", "script"), jget(v, "script")))
	endpoint := clip(scrub(jstr(jalt("", jget(v, "data", "endpoint"), jget(v, "endpoint")))))
	if token == "" {
		c.errorf("kubehz API did not return a join token for '%s'", nodeName)
		return ErrHandled
	}
	if !joinTokenRe.MatchString(token) {
		c.errorf("kubehz API returned a join ticket for '%s' in a shape this CLI does not recognise; nothing was written", nodeName)
		return ErrHandled
	}
	if expires == "" {
		expires = "<unknown>"
	}
	c.echo("")
	c.echo("  Node '%s' — join ticket (valid until %s, single use):", nodeName, expires)
	if script == "" || printToken {
		c.echo("    %s", token)
	} else {
		c.echo("    (inside the join script below; --print-token shows it here)")
	}
	// The api ships the join recipe with the ticket (the script the docs
	// point at: containerd + kubelet, the cluster CA read from the control
	// plane and verified against the ticket, bootstrap config, kubelet
	// restart). It carries the ticket, so it goes to a private file, never
	// into the project tree — a script next to cluster.lok8s.yaml is one
	// `git add .` away from a public secret. Nothing runs here: read it,
	// copy it to the machine, run it as root there.
	if script != "" {
		path, err := c.writeJoinScript(nodeName, script)
		if errors.Is(err, ErrHandled) {
			return err
		}
		if err != nil {
			c.errorf("could not write the join script for '%s': %v", nodeName, err)
			// The mint already happened server-side; the ticket is live
			// with no local copy (node.go's mintedSlotNote, for spaces).
			c.echoErr("")
			c.echoErr("  The ticket was minted before the write failed. It stays valid until %s,", expires)
			c.echoErr("  single use, and no copy was saved. Mint a fresh one once the temp dir")
			c.echoErr("  (TMPDIR) is writable:")
			c.echoErr("    lo kubehz join %s", nodeName)
			return ErrHandled
		}
		c.echo("  Join script (the ticket is inside; expires with it):")
		c.echo("    %s", path)
		if endpoint != "" {
			c.echo("  It bootstraps the kubelet against %s.", endpoint)
		}
		c.echo("  Read it, then copy it to the machine and run it there as root:")
		c.echo("    scp %s root@<machine>:/root/ && ssh root@<machine> bash /root/%s", path, filepath.Base(path))
		c.echo("  Delete it (and its directory) once the node has joined.")
	} else {
		c.echo("  On the machine, follow the node-join guide for your platform")
		c.echo("  (kubehz docs: Spaces → Joining nodes).")
	}
	c.echo("  The ticket is bound to this node name and expires quickly — mint a")
	c.echo("  fresh one with:")
	c.echo("    lo kubehz join %s", nodeName)
	return nil
}

// writeJoinScript stores the api's join recipe under the user's temp dir
// (TMPDIR, else the OS default) in a FRESH private directory:
// <tmp>/kubehz-join-<random>/kubehz-join-<node>.sh. The directory comes
// from os.MkdirTemp (0700, unpredictable name), so there is no name to
// pre-plant in a shared /tmp: nothing a foreign user creates can collide
// with it, and the O_EXCL create inside is belt and braces. Every mint
// makes a new directory — a fresh ticket does not overwrite an old file;
// the old one expires with its ticket and the user deletes it. The node
// name is part of the path, so it is validated here (the api's DNS-label
// rule) as well.
func (c *Context) writeJoinScript(nodeName, script string) (string, error) {
	if err := c.assertNodeName(nodeName); err != nil {
		return "", err
	}
	dir := c.getenv("TMPDIR")
	if dir == "" {
		dir = os.TempDir()
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	priv, err := os.MkdirTemp(dir, "kubehz-join-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(priv, "kubehz-join-"+nodeName+".sh")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(priv)
		return "", err
	}
	if _, err := f.WriteString(script); err != nil {
		_ = f.Close()
		_ = os.RemoveAll(priv)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.RemoveAll(priv)
		return "", err
	}
	return path, nil
}

// ProvisionShared ports kubehz::provision_shared: the full provision arc for
// hosting: shared.
func (c *Context) ProvisionShared(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	if c.getenv("KUBEHZ_TOKEN") == "" {
		c.errorf("KUBEHZ_TOKEN is required to provision a space (get one from your kubehz account)")
		return ErrHandled
	}
	sp, err := c.SpaceConfig(domain, clusterYAML)
	if err != nil {
		return err
	}
	spaceID, err := c.spaceEnsure(ctx, cfg, sp)
	if err != nil {
		return err
	}
	if spaceID == "" || spaceID == "null" {
		c.errorf("kubehz API did not return a space ID")
		return ErrHandled
	}
	if err := c.spaceWaitActive(ctx, cfg, spaceID, 300); err != nil {
		return err
	}
	c.echo("Space '%s' is Active (id: %s)", sp.Slug, spaceID)
	c.echo("  Namespace: %s", sp.Slug)
	c.echo("  Access: sign in with your kubehz account (OIDC) — the control plane")
	c.echo("  itself is operated by the platform and is not directly accessible.")
	for _, node := range sp.Nodes {
		if err := c.spaceMintJoin(ctx, cfg, spaceID, node, false); err != nil {
			return err
		}
	}
	if len(sp.Nodes) == 0 {
		c.echo("")
		c.echo("  No nodes declared under spec.kubehz.space.nodes — mint a join")
		c.echo("  ticket any time with: lo kubehz join <node-name>")
	}
	return nil
}

// DestroyShared ports kubehz::destroy_shared: deregister the Space.
func (c *Context) DestroyShared(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	sp, err := c.SpaceConfig(domain, clusterYAML)
	if err != nil {
		return err
	}
	row, err := c.spaceLookup(ctx, cfg, sp.Slug)
	if err != nil {
		return err
	}
	if row == nil {
		c.echo("No space '%s' found — nothing to destroy", sp.Slug)
		return nil
	}
	spaceID := jstrOr(row, "", "id")
	if spaceID == "" {
		c.errorf("space row carries no id — refusing to continue")
		return ErrHandled
	}
	res, err := c.spaceAPI(ctx, cfg, "DELETE", "/api/spaces/"+spaceID, nil)
	if err != nil {
		return err
	}
	if !is2xx(res.Status) {
		c.spaceAPIError("Failed to remove space '"+sp.Slug+"'", res)
		return ErrHandled
	}
	c.echo("Space '%s' removed (id: %s)", sp.Slug, spaceID)
	return nil
}

// SpaceStatus ports kubehz::space_status: the Space + its registered nodes.
func (c *Context) SpaceStatus(ctx context.Context, cfg *Config, domain, clusterYAML string) error {
	sp, err := c.SpaceConfig(domain, clusterYAML)
	if err != nil {
		return err
	}
	row, err := c.spaceLookup(ctx, cfg, sp.Slug)
	if err != nil {
		return err
	}
	if row == nil {
		c.echo("Space:   '%s' not found (not provisioned yet?)", sp.Slug)
		return nil
	}
	spaceID := jstrOr(row, "", "id")
	if spaceID == "" {
		c.errorf("space row carries no id — refusing to continue")
		return ErrHandled
	}
	c.echo("Space:   %s (id: %s)", sp.Slug, spaceID)
	c.echo("Phase:   %s", jstrOr(row, "Unknown", "status"))
	c.echo("Limits:  nodes %s, namespaces %s, object cap %s KiB",
		jstrOr(row, "-", "maxNodes"), jstrOr(row, "-", "maxNamespaces"), jstrOr(row, "-", "maxObjectKiB"))

	res := c.spaceAPIQuiet(ctx, cfg, "GET", "/api/spaces/"+spaceID+"/nodes")
	if !is2xx(res.Status) {
		c.echo("Nodes:   unknown (API unreachable)")
		return nil
	}
	// The route answers {nodes:[{name,lane,status,…}], usage:{nodes,maxNodes}}.
	v, _ := parseJSON(res.Body)
	body := envelope(v)
	c.echo("Nodes:   %s/%s", jstrOr(body, "0", "usage", "nodes"), jstrOr(body, "-", "usage", "maxNodes"))
	if nodes, ok := jget(body, "nodes").([]any); ok {
		for _, n := range nodes {
			c.echo("  %s  %s  %s", jstr(jget(n, "name")), jstrOr(n, "-", "status"), jstrOr(n, "-", "lane"))
		}
	}
	return nil
}
