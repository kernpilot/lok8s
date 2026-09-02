package kubehz

// shared.go — libs/kubehz/shared: Spaces on the kubehz shared control plane
// (hosting: shared). The platform runs and secures the control plane; you
// bring the machines. `lo provision` creates (or adopts) the Space and mints
// single-node join tickets; `lo destroy` deregisters it.

import (
	"context"
	"strings"
	"time"
)

// SpaceConfig is the LOK8S_SPACE_* export set of kubehz::space_config.
type SpaceConfig struct {
	Slug   string
	Name   string
	Plan   string
	Region string
	Nodes  []string
}

// SpaceConfig ports kubehz::space_config: everything is optional — the slug
// defaults to the first DNS label of the domain, the display name to the
// slug. A yq PARSE failure is an error, never a defaulted slug (destroy
// would target the WRONG space).
func (c *Context) SpaceConfig(domain, clusterYAML string) (*SpaceConfig, error) {
	doc := loadSpec(clusterYAML)
	if doc.err != nil {
		c.errorf("cannot parse cluster spec: %s: %v", clusterYAML, doc.err)
		return nil, ErrHandled
	}
	defaultSlug := domain
	if i := strings.IndexByte(domain, '.'); i >= 0 {
		defaultSlug = domain[:i]
	}
	sp := &SpaceConfig{
		Slug:   doc.or("", "spec", "kubehz", "space", "slug"),
		Name:   doc.or("", "spec", "kubehz", "space", "name"),
		Plan:   doc.or("", "spec", "kubehz", "space", "plan"),
		Region: doc.or("", "spec", "kubehz", "space", "region"),
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
// a non-2xx envelope.
func (c *Context) spaceAPIError(context string, res *httpResult) {
	msg := apiMessage(res.Body)
	help := apiHelp(res.Body)
	c.errorf("%s (HTTP %d)%s", context, res.Status, optSuffix(": ", msg))
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
		return id, nil
	}

	pairs := []jsonPair{{"name", sp.Name}, {"slug", sp.Slug}}
	if sp.Plan != "" {
		pairs = append(pairs, jsonPair{"planId", sp.Plan})
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
		c.sleep(5 * time.Second)
	}
	c.errorf("Timed out waiting for space %s to become Active after %ds", spaceID, timeout)
	return ErrHandled
}

// spaceMintJoin ports kubehz::space_mint_join: mint a single-node join
// ticket and print the join block. The plaintext token is returned exactly
// once by the api and never persisted.
func (c *Context) spaceMintJoin(ctx context.Context, cfg *Config, spaceID, nodeName string) error {
	res, err := c.spaceAPI(ctx, cfg, "POST", "/api/spaces/"+spaceID+"/join-token", compactJSON(jsonPair{"nodeName", nodeName}))
	if err != nil {
		return err
	}
	if !is2xx(res.Status) {
		c.spaceAPIError("Failed to mint a join ticket for '"+nodeName+"'", res)
		return ErrHandled
	}
	v, _ := parseJSON(res.Body)
	token := jstr(jalt("", jget(v, "data", "token"), jget(v, "token")))
	expires := jstr(jalt("", jget(v, "data", "expiresAt"), jget(v, "expiresAt")))
	if token == "" {
		c.errorf("kubehz API did not return a join token for '%s'", nodeName)
		return ErrHandled
	}
	if expires == "" {
		expires = "<unknown>"
	}
	c.echo("")
	c.echo("  Node '%s' — join ticket (valid until %s, single use):", nodeName, expires)
	c.echo("    %s", token)
	c.echo("  On the machine, follow the node-join guide for your platform")
	c.echo("  (kubehz docs: Spaces → Joining nodes). The ticket is bound to this")
	c.echo("  node name and expires quickly — mint a fresh one with:")
	c.echo("    lo kubehz join %s", nodeName)
	return nil
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
		if err := c.spaceMintJoin(ctx, cfg, spaceID, node); err != nil {
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
	c.echo("Plan:    %s", jstrOr(row, "-", "planId"))

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
