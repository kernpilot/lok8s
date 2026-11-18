package kubehz

// shared.go — libs/kubehz/shared: Spaces on the kubehz shared control plane
// (hosting: shared). The platform runs and secures the control plane; you
// bring the machines. `lo provision` creates (or adopts) the Space and mints
// single-node join tickets; `lo destroy` deregisters it.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
// once by the api and kept only in a private file: the join script when
// the api ships one, else a ticket file of its own (both twins). A
// terminal (scrollback, CI logs, a shared screen) is a copy nobody asked
// for, so the ticket goes to stdout only when printToken is set.
// Deviation D21 (Go-only): the join script and the printToken flag.
func (c *Context) spaceMintJoin(ctx context.Context, cfg *Config, spaceID, nodeName string, printToken bool) error {
	// Before the mint: a refused name must not cost a live ticket.
	if err := c.assertNodeName(nodeName); err != nil {
		return err
	}
	// Nor a temp dir that fails the check (joinTempDir printed why). The
	// writer checks again: the directory can change in between. A TMPDIR
	// that cannot be resolved is left to the writer, as before.
	if _, err := c.joinTempDir(); errors.Is(err, errTmpNotPrivate) {
		return ErrHandled
	}
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
	switch {
	case printToken:
		c.echo("    %s", token)
	case script != "":
		c.echo("    (inside the join script below; --print-token shows it here)")
	default:
		// No script: the ticket goes to a private file of its own, as the
		// bash twin writes it (same name, same bytes, same line).
		path, err := c.writeJoinFile(nodeName, ".token", token+"\n")
		if err != nil {
			return c.joinWriteFailed("join ticket", nodeName, expires, err)
		}
		c.echo("    (written to %s, mode 0600)", path)
	}
	// The api ships the join recipe with the ticket (the script the docs
	// point at: containerd + kubelet, the cluster CA read from the control
	// plane and verified against the ticket, bootstrap config, kubelet
	// restart). It carries the ticket, so it goes to a private file, never
	// into the project tree — a script next to cluster.lok8s.yaml is one
	// `git add .` away from a public secret. Nothing runs here: read it,
	// copy it to the machine, run it as root there.
	if script != "" {
		path, err := c.writeJoinFile(nodeName, ".sh", script)
		if err != nil {
			return c.joinWriteFailed("join script", nodeName, expires, err)
		}
		c.echo("  Join script (the ticket is inside; expires with it):")
		c.echo("    %s", path)
		if endpoint != "" {
			c.echo("  It bootstraps the kubelet against %s.", endpoint)
		}
		c.echo("  Read it, then copy it to the machine and run it there as root:")
		c.echo("    scp %s root@<machine>:/root/ && ssh root@<machine> bash /root/%s", path, filepath.Base(path))
		c.echo("  Delete it (and its directory) once the node has joined.")
		c.echo("  The ticket is bound to this node name and expires quickly — mint a")
		c.echo("  fresh one with:")
	} else {
		// Without a script: the lines of the bash twin, which has no script.
		c.echo("  On the machine, follow the node-join guide for your platform")
		c.echo("  (kubehz docs: Spaces → Joining nodes).")
		c.echo("  Delete the ticket file (and its directory) once the node has joined.")
		c.echo("  The ticket is bound to this node name and expires quickly — mint a")
		c.echo("  fresh one with:")
	}
	c.echo("    lo kubehz join %s", nodeName)
	return nil
}

// joinWriteFailed reports a failed write of a join file (what: "join
// script" or "join ticket"). The mint already happened server-side, so the
// ticket is live with no local copy (node.go's mintedSlotNote, for spaces).
// An ErrHandled error (a refused node name) has printed its own message.
func (c *Context) joinWriteFailed(what, nodeName, expires string, err error) error {
	switch {
	case errors.Is(err, ErrHandled):
		return err
	case errors.Is(err, errTmpNotPrivate):
		// joinTempDir printed why, as the bash twin does.
		c.errorf("could not write the %s for '%s'", what, nodeName)
	default:
		c.errorf("could not write the %s for '%s': %v", what, nodeName, err)
	}
	c.echoErr("")
	c.echoErr("  The ticket was minted before the write failed. It stays valid until %s,", expires)
	if left, ok := errors.AsType[*joinLeftError](err); ok {
		c.echoErr("  single use. The cleanup failed too: %s may still hold a part of it;", left.dir)
		c.echoErr("  delete that directory. Mint a fresh one once the temp dir (TMPDIR) is writable:")
	} else {
		c.echoErr("  single use, and no copy was saved. Mint a fresh one once the temp dir")
		c.echoErr("  (TMPDIR) is writable:")
	}
	c.echoErr("    lo kubehz join %s", nodeName)
	return ErrHandled
}

// joinLeftError: the write of a join file failed, and the removal of its
// fresh directory (dir) failed too. The directory may still hold a part of
// the ticket. The error text is the one of the write.
type joinLeftError struct {
	dir string
	err error
}

func (e *joinLeftError) Error() string { return e.err.Error() }
func (e *joinLeftError) Unwrap() error { return e.err }

// errTmpNotPrivate: the temp dir of a join file failed the check of
// joinTempDir, which printed why.
var errTmpNotPrivate = errors.New("the temp dir is not private")

// joinTempDir ports kubehz::space_join_tmp_dir: the directory a join file
// goes under. TMPDIR (else the OS default), absolute, with each symlink
// resolved (TMPDIR=/ is /). Each part of the resolved path, from / down to
// the leaf (top-down: see the walk), must belong to root or to this user
// (ownerTrusted: "/" may also show the overflow uid of a user namespace,
// and a user with the overflow uid owns nothing it can prove), and must not
// be writable by others unless it has the sticky bit (/tmp passes). The
// owner of a part could otherwise rename the fresh directory and put a FIFO
// or a link in place of the ticket or the script.
func (c *Context) joinTempDir() (string, error) {
	dir := c.getenv("TMPDIR")
	if dir == "" {
		dir = os.TempDir()
	}
	// No filepath.Abs or Clean before EvalSymlinks: they make "link/.." the
	// directory of the link, where the kernel (and cd -P in the bash twin)
	// goes up from the target of the link. A relative TMPDIR is joined to
	// the working directory as it is; EvalSymlinks resolves each link before
	// the ".." after it.
	if !filepath.IsAbs(dir) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd + string(filepath.Separator) + dir
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	// Top-down, from / to the leaf: each part is read (Lstat, no follow: a
	// link is refused) before any part below it. A part that passes lets
	// no other user rename or replace an entry in it (they cannot write it,
	// or the sticky bit keeps them to their own entries), so each part that
	// was checked stays the same up to the create below it (writeJoinFile:
	// MkdirTemp, then the handle of the fresh directory). A bottom-up walk
	// would let the owner of an ancestor that is not checked yet swap it for
	// a prepared tree after the parts below it passed.
	parts := []string{real}
	for p := real; filepath.Dir(p) != p; p = filepath.Dir(p) {
		parts = append(parts, filepath.Dir(p))
	}
	slices.Reverse(parts)
	for _, part := range parts {
		if why, unmapped := c.notPrivatePart(part); why != "" {
			c.errorf("the temp dir for the join ticket is not safe: %s %s", part, why)
			if unmapped {
				c.echoErr("  The uid of this user is not mapped in the namespace, so the CLI cannot check who owns TMPDIR.")
				c.echoErr("  Run it where the uid of this user is mapped (unshare --map-current-user), or outside the namespace.")
			} else {
				c.echoErr("  Set TMPDIR to a directory of your own that only you can write (mode 0700).")
			}
			return "", errTmpNotPrivate
		}
	}
	return real, nil
}

// notPrivatePart says why one part of a temp dir path fails the check of
// joinTempDir ("" when it passes). unmapped: the part is ours, and this
// user has the overflow uid (a user namespace without a mapping for it).
func (c *Context) notPrivatePart(part string) (why string, unmapped bool) {
	fi, err := c.lstat(part)
	if err != nil {
		return "cannot be read", false
	}
	uid, ok := fileOwner(fi)
	switch {
	case !ok:
		return "cannot be read", false
	case fi.Mode()&fs.ModeSymlink != 0:
		return "is a symlink", false
	case !c.ownerTrusted(part, uid):
		if uid == c.euid() {
			// Ours, and refused: this user has the overflow uid.
			return fmt.Sprintf("cannot be checked (uid %d)", uid), true
		}
		return fmt.Sprintf("belongs to another user (uid %d)", uid), false
	case fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0:
		return "is writable by others and has no sticky bit", false
	}
	return "", false
}

// ownerTrusted: a part of the temp dir path must belong to root, or to this
// user while the uid of this user is not the overflow uid: in a user
// namespace without a mapping for this user, this user and every other
// unmapped owner show that uid, so "ours" proves nothing there. "/" alone
// may also show the overflow uid: in a namespace that does not map root,
// the host's "/" (root's) shows so. Any other part with that owner is
// refused: there it can be any user of the host.
func (c *Context) ownerTrusted(part string, uid int) bool {
	ovf := c.overflowUID()
	return uid == 0 || (uid == c.euid() && c.euid() != ovf) || (part == string(filepath.Separator) && uid == ovf)
}

// writeJoinFile stores a join file, the api's join recipe (ext ".sh") or
// the ticket alone (ext ".token"), under the user's temp dir (joinTempDir)
// in a FRESH private directory: <tmp>/kubehz-join-<random>/kubehz-join-<node><ext>.
// The directory comes from os.MkdirTemp (unpredictable name), so there is
// no name to pre-plant in a shared /tmp. The file is created through a
// handle of that directory, with O_EXCL and O_NOFOLLOW: a name that is
// there anyway (a file, a FIFO, a link) fails. The modes are set, not left
// to the umask (a umask such as 0400 would give 0300 and 0200): the
// directory 0700, the file 0600 through its handle. Every mint makes a new
// directory — a fresh ticket does not overwrite an old file; the old one
// expires with its ticket and the user deletes it. The node name is part
// of the path, so it is validated here (the api's DNS-label rule) as well.
func (c *Context) writeJoinFile(nodeName, ext, content string) (string, error) {
	if err := c.assertNodeName(nodeName); err != nil {
		return "", err
	}
	dir, err := c.joinTempDir()
	if err != nil {
		return "", err
	}
	mkdirTemp := c.MkdirTemp
	if mkdirTemp == nil {
		mkdirTemp = os.MkdirTemp
	}
	priv, err := mkdirTemp(dir, "kubehz-join-")
	if err != nil {
		return "", err
	}
	name := "kubehz-join-" + nodeName + ext
	if err := c.writeJoinInto(priv, name, content); err != nil {
		removeAll := c.RemoveAll
		if removeAll == nil {
			removeAll = os.RemoveAll
		}
		if rerr := removeAll(priv); rerr != nil {
			// The directory may still hold a part of the ticket.
			return "", &joinLeftError{dir: priv, err: err}
		}
		return "", err
	}
	return filepath.Join(priv, name), nil
}

// writeJoinInto writes content to the new file name in the fresh directory
// priv (writeJoinFile removes priv when it fails, and reports a failed
// removal).
func (c *Context) writeJoinInto(priv, name, content string) error {
	if err := os.Chmod(priv, 0o700); err != nil { // #nosec G302 -- priv is a directory: the owner needs x to enter it
		return err
	}
	root, err := os.OpenRoot(priv)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat(".")
	if err != nil {
		return err
	}
	if uid, ok := fileOwner(fi); !fi.IsDir() || fi.Mode().Perm() != 0o700 || !ok || uid != c.euid() {
		return errNotPrivate
	}
	f, err := createExclNoFollow(root, name)
	if err != nil {
		return err
	}
	write, closeFile := c.joinWrite, c.joinClose
	if write == nil {
		write = func(f *os.File, s string) error { _, err := f.WriteString(s); return err }
	}
	if closeFile == nil {
		closeFile = (*os.File).Close
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := write(f, content); err != nil {
		_ = f.Close()
		return err
	}
	return closeFile(f)
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
	// D30 (both implementations): the space's kubectl kubeconfig. The api
	// serves a kubelogin file for a shared space. It
	// holds the public endpoint, the shard's sign-in client and the context
	// kubehz-<slug>, and no credential. The file lands beside the hosted
	// path's file. An api without the route, a space still wiring up or one
	// with a dedicated plane keep the older two lines. Nothing fails: the
	// space is Active either way.
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
	// Whole file or nothing: a temp file in the same directory, the mode
	// set on it, then one rename. An existing file keeps nothing of its old
	// mode either (review of #218).
	tmp, err := os.CreateTemp(filepath.Dir(path), ".kubeconfig-*")
	if err != nil {
		c.debugf("space %s: kubeconfig temp: %v", spaceID, err)
		return ""
	}
	_, werr := tmp.Write(res.Body)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp.Name(), 0o600) != nil || os.Rename(tmp.Name(), path) != nil {
		_ = os.Remove(tmp.Name())
		c.debugf("space %s: kubeconfig write failed", spaceID)
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
	// The kubeconfig provision wrote (D30) names a space that is gone.
	_ = os.Remove(filepath.Join(c.Paths.Base, ".kubeconfig", domain+".yaml"))
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
			line := "  " + jstr(jget(n, "name")) + "  " + jstrOr(n, "-", "status") + "  " + jstrOr(n, "-", "lane")
			// One node, one line: a newline or a tab in a server string (a
			// reason, a name) becomes a space, so it cannot print a second
			// node line. Then scrub drops the other unshown characters, as
			// the bash ends its jq with the def scrub.
			c.echo("%s", scrub(nodeLineSpaces.Replace(line+nodeMaintenanceSuffix(n))))
		}
	}
	return nil
}

// nodeLineSpaces turns the two characters that scrub keeps (newline, tab)
// into a space on a Space node line (the bash twin: split and join in jq).
var nodeLineSpaces = strings.NewReplacer("\n", " ", "\t", " ")

// nodeMaintenanceSuffix is what a Space node line shows after the lane:
// `cordoned` while the node is unschedulable (any cordon: the tenant's, a
// platform migration, a node retirement), then the last cordon, uncordon or
// drain request with its phase, and the platform's reason when it gives one
// (`drain Blocked: <reason>`, clipped to 256 runes). The bash twin builds the
// same string in its jq filter (kubehz::space_status).
func nodeMaintenanceSuffix(n any) string {
	s := ""
	if u, ok := jget(n, "unschedulable").(bool); ok && u {
		s += "  cordoned"
	}
	m, ok := jget(n, "maintenance").(map[string]any)
	if !ok {
		return s
	}
	text := func(v any) string {
		if t, ok := v.(string); ok {
			return t
		}
		return "-"
	}
	s += "  " + text(m["action"]) + " " + text(m["phase"])
	if r, ok := m["reason"].(string); ok && r != "" {
		s += ": " + clip(r)
	}
	return s
}
