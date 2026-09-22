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

// The three numbers a space is made of: the node ceiling, the namespace
// ceiling and the object cap (the size limit for one Secret or ConfigMap in
// the space's namespaces), under spec.kubehz.space.limits.
//
// lo does NOT carry the platform's policy. What an account may ask for
// differs per account, and extra resources are bought, so the platform is
// the only place that knows the ceiling. This CLI checks the SHAPE of a
// value (a whole number, 1 or more) and sends what the spec says; a value
// above the account's ceiling is the api's refusal to make, and the api's
// message carries the account's real numbers. A field the spec leaves out
// is not sent at all, so the api applies its own default.
const spaceNumberMin = 1

// SpaceConfig is the LOK8S_SPACE_* export set of kubehz::space_config.
type SpaceConfig struct {
	Slug   string
	Name   string
	Region string
	// MaxNodes, MaxNamespaces and MaxObjectKiB are the space's three
	// numbers (spec.kubehz.space.limits.{nodes,namespaces,objectCapKiB}).
	// ZERO means the spec names no value: the field is left out of the
	// create body and the api applies its own default. A real value is
	// always 1 or more, so zero cannot collide with one.
	MaxNodes      int
	MaxNamespaces int
	MaxObjectKiB  int
	// Nodes are the machines to mint a join ticket for
	// (spec.kubehz.space.nodes).
	Nodes []string
}

// SpaceConfig ports kubehz::space_config: the slug defaults to the first DNS
// label of the domain and the display name to the slug. A limit the spec
// leaves out stays zero and is not sent, so the api applies its own default.
// A yq PARSE failure is an error, never a defaulted slug (destroy would
// target the WRONG space).
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

// spaceLimits reads the three numbers of a space. Only the SHAPE is checked
// here: a whole number, 1 or more. There is no upper bound, because the
// ceiling belongs to the account and lives on the platform. A field the spec
// leaves out comes back as zero and is not sent.
func (c *Context) spaceLimits(doc specDoc) (nodes, namespaces, objectCap int, err error) {
	// Space plans are retired. A spec that still carries one asks for a
	// shape the platform no longer has, so say so instead of ignoring it.
	// The test is the raw node, not Or(): Or() carries yq's `//` semantics,
	// which read `plan: false` and `plan: ""` as absent and would let those
	// specs through unrefused. Any node that is not null is a plan.
	if !yqsem.IsNull(doc.Lookup("spec", "kubehz", "space", "plan")) {
		c.errorf("spec.kubehz.space.plan is not valid: space plans are retired")
		c.echoErr("  Set spec.kubehz.space.limits.nodes, spec.kubehz.space.limits.namespaces")
		c.echoErr("  and spec.kubehz.space.limits.objectCapKiB instead.")
		return 0, 0, 0, ErrHandled
	}
	if nodes, err = c.spaceNumber(doc, "nodes"); err != nil {
		return 0, 0, 0, err
	}
	if namespaces, err = c.spaceNumber(doc, "namespaces"); err != nil {
		return 0, 0, 0, err
	}
	if objectCap, err = c.spaceNumber(doc, "objectCapKiB"); err != nil {
		return 0, 0, 0, err
	}
	return nodes, namespaces, objectCap, nil
}

// spaceNumber reads one of the three numbers. A missing or null value returns
// zero: the field is left out of the request and the api applies its own
// default. spec.kubehz.space.nodes is the machine-name list, so a list under
// limits.nodes is the two fields confused: name both.
func (c *Context) spaceNumber(doc specDoc, field string) (int, error) {
	n := doc.Lookup("spec", "kubehz", "space", "limits", field)
	if yqsem.IsNull(n) {
		return 0, nil
	}
	if n.Kind == yaml.SequenceNode && field == "nodes" {
		c.errorf("spec.kubehz.space.limits.nodes is a list: it is the node ceiling")
		c.echoErr("  Set spec.kubehz.space.limits.nodes to a whole number of 1 or more.")
		c.echoErr("  The machine names stay under spec.kubehz.space.nodes.")
		return 0, ErrHandled
	}
	// Only a list and a map hold no single value to quote back. Every other
	// type is a scalar, so it falls through to the read below and the refusal
	// names what the spec says (2.5, true, two). The bash twin does the same.
	if n.Kind != yaml.ScalarNode {
		c.errorf("invalid spec.kubehz.space.limits.%s: expected a whole number of 1 or more", field)
		return 0, ErrHandled
	}
	// No upper bound on the CEILING: a number the account may not have is the
	// api's refusal to make. The shape below is the BASH twin's contract, not
	// strconv's: strconv alone would take "+5" and, after a TrimSpace, " 5 "
	// as well, and the frozen tree takes neither. Bash wins on a divergence.
	if !spaceDigits.MatchString(n.Value) {
		c.errorf("invalid spec.kubehz.space.limits.%s: %s (expected a whole number of 1 or more)", field, n.Value)
		return 0, ErrHandled
	}
	// Leading zeros drop out here, so "008" is eight and never octal, and a
	// value past the strconv range is refused on both sides.
	value, err := strconv.Atoi(n.Value)
	if err != nil || value < spaceNumberMin {
		c.errorf("invalid spec.kubehz.space.limits.%s: %s (expected a whole number of 1 or more)", field, n.Value)
		return 0, ErrHandled
	}
	return value, nil
}

// spaceDigits is the shape a limit must have: digits only. No sign, no
// spaces, no underscores — the frozen tree's regexp, character for
// character.
var spaceDigits = regexp.MustCompile(`^[0-9]+$`)

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
	// Clipped as well as scrubbed: this is the FIRST api call lo makes, and
	// an unbounded server string here is a terminal full of whatever the api
	// chose. The bash twin bounds both halves the same way.
	msg := clip(scrub(apiMessage(res.Body)))
	help := clip(scrub(apiHelp(res.Body)))
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

	// A number the spec does not name is LEFT OUT of the body. Sending a
	// locally chosen default would make this CLI the author of a policy that
	// belongs to the platform, and would pin a value the account may later
	// have more of. The api's schema makes all three optional and applies
	// its own default to whatever is absent.
	pairs := []jsonPair{
		{"name", sp.Name},
		{"slug", sp.Slug},
	}
	if sp.MaxNodes > 0 {
		pairs = append(pairs, jsonPair{"maxNodes", sp.MaxNodes})
	}
	if sp.MaxNamespaces > 0 {
		pairs = append(pairs, jsonPair{"maxNamespaces", sp.MaxNamespaces})
	}
	if sp.MaxObjectKiB > 0 {
		pairs = append(pairs, jsonPair{"maxObjectKiB", sp.MaxObjectKiB})
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
		// The three space-limit codes all render the api's own words. The
		// account's ceiling differs per account and extra resources are
		// bought, so the platform holds the only true numbers; lo adds what
		// the spec asked for and the local next step.
		case "SPACE_LIMITS_ABOVE_FREE":
			c.spaceLimitsRefused(sp, res, "they are above what this account allows")
			c.echoErr("  %s the values in spec.kubehz.space.limits, or raise the ceiling", sp.limitsVerb())
			c.echoErr("  of the account in the kubehz dashboard.")
			return "", ErrHandled
		case "SPACE_LIMITS_ABOVE_SHARED":
			c.spaceLimitsRefused(sp, res, "they are above what this account may set on a shared control plane")
			c.echoErr("  %s the values in spec.kubehz.space.limits, or use a hosted control", sp.limitsVerb())
			c.echoErr("  plane: set spec.kubehz.hosting to hosted.")
			return "", ErrHandled
		case "SPACE_PLAN_RETIRED":
			c.spaceAPIReason("kubehz refused the request", res, "space plans are retired")
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

// spaceAPIReason prints "<what>: <the api's message>", then the api's help.
// The fallback stands in for an api that sends the code alone, and states no
// number of its own. Both server strings are scrubbed and bounded.
func (c *Context) spaceAPIReason(what string, res *httpResult, fallback string) {
	reason := clip(scrub(apiMessage(res.Body)))
	if reason == "" {
		reason = fallback
	}
	c.errorf("%s: %s", what, reason)
	if help := clip(scrub(apiHelp(res.Body))); help != "" {
		c.echoErr("  %s", help)
	}
}

// spaceLimitsRefused names what the spec asked for, then lets the api say why
// that is too much.
func (c *Context) spaceLimitsRefused(sp *SpaceConfig, res *httpResult, fallback string) {
	c.spaceAPIReason("kubehz refused the space limits ("+sp.limitsLine()+")", res, fallback)
}

// limitsVerb is the local next step: a spec that names numbers gets them
// lowered, a spec that names none gets them set. Telling a reader to decrease
// values that are not there reads as nonsense.
func (sp *SpaceConfig) limitsVerb() string {
	if sp.MaxNodes == 0 && sp.MaxNamespaces == 0 && sp.MaxObjectKiB == 0 {
		return "Set"
	}
	return "Decrease"
}

// limitsLine renders the numbers the SPEC names, in a fixed order. A number
// the spec leaves out is not shown, because it was never sent.
func (sp *SpaceConfig) limitsLine() string {
	var parts []string
	if sp.MaxNodes > 0 {
		parts = append(parts, "nodes "+strconv.Itoa(sp.MaxNodes))
	}
	if sp.MaxNamespaces > 0 {
		parts = append(parts, "namespaces "+strconv.Itoa(sp.MaxNamespaces))
	}
	if sp.MaxObjectKiB > 0 {
		parts = append(parts, "object cap "+strconv.Itoa(sp.MaxObjectKiB)+" KiB")
	}
	if len(parts) == 0 {
		return "the spec sets none"
	}
	return strings.Join(parts, ", ")
}

// noteLimitsDrift reports a space whose limits differ from the spec. Adoption
// stays read-only: lo creates a space with the three numbers, and never
// changes the numbers of a space that exists. Without this note the edit in
// the spec would do nothing and say nothing.
func (c *Context) noteLimitsDrift(row any, sp *SpaceConfig) {
	// Only a number the SPEC names can drift. A field the spec leaves out
	// was never sent, so whatever the space holds for it is the api's own
	// value and is not a disagreement.
	want := []struct {
		field string
		value int
	}{
		{"maxNodes", sp.MaxNodes},
		{"maxNamespaces", sp.MaxNamespaces},
		{"maxObjectKiB", sp.MaxObjectKiB},
	}
	drift := false
	for _, w := range want {
		if w.value == 0 {
			continue
		}
		live := jstrOr(row, "", w.field)
		// An api that does not report the number gives nothing to compare.
		if live == "" {
			return
		}
		if live != strconv.Itoa(w.value) {
			drift = true
		}
	}
	if !drift {
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
	// Go-only (D30): the space's kubectl kubeconfig. The api serves a kubelogin
	// file for a shared space (kubehz-api #127, B221): the public endpoint, the
	// shard's sign-in client, the context kubehz-<slug>. No credential inside.
	// Written beside the hosted path's file; an api without the route (404),
	// a space still wiring up (503) or one with a dedicated plane (409) keep
	// the older two lines, and nothing fails: the space is Active either way.
	if path := c.spaceKubeconfig(ctx, cfg, domain, spaceID); path != "" {
		c.echo("  Access: kubectl --context kubehz-%s -n %s get pods", sp.Slug, sp.Slug)
		c.echo("  Kubeconfig: %s (kubectl signs you in with your kubehz account)", path)
	} else {
		c.echo("  Access: sign in with your kubehz account (OIDC) — the control plane")
		c.echo("  itself is operated by the platform and is not directly accessible.")
	}
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

// spaceKubeconfig fetches the shared space's kubectl kubeconfig and writes it
// to .kubeconfig/<domain>.yaml (0600, the hosted path's location). Returns
// the path, or "" when the api has no file for this space yet (see the
// caller). The file holds no credential; the mode matches the hosted one.
func (c *Context) spaceKubeconfig(ctx context.Context, cfg *Config, domain, spaceID string) string {
	res, err := c.fetchStatus(ctx, "GET", cfg.APIURL+"/api/spaces/"+spaceID+"/kubeconfig", withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil || !is2xx(res.Status) || len(res.Body) == 0 {
		if err == nil {
			c.debugf("space %s: no kubeconfig from the api (HTTP %d)", spaceID, res.Status)
		}
		return ""
	}
	path := filepath.Join(c.Paths.Base, ".kubeconfig", domain+".yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		c.debugf("space %s: kubeconfig dir: %v", spaceID, err)
		return ""
	}
	if err := os.WriteFile(path, res.Body, 0o600); err != nil {
		c.debugf("space %s: kubeconfig write: %v", spaceID, err)
		return ""
	}
	return path
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
