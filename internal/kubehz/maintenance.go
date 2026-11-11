package kubehz

// maintenance.go — libs/kubehz/node, the Space half: cordon, uncordon and
// drain one node of a Space (hosting: shared) through the platform. A Space
// gives its tenant no rights on the nodes of the shared control plane, so
// the api publishes a request and the platform's operator acts on it. The
// change is asynchronous: `lo kubehz status` shows the last request of each
// node with its phase (SpaceStatus, shared.go).
//
// The auth is the one the hosted node verbs use: a KUBEHZ_TOKEN bearer with
// the clusters:write scope.

import (
	"context"
	"regexp"
	"strconv"
)

// MaintenanceOpts carries the flags of `lo kubehz node cordon|uncordon|drain`.
type MaintenanceOpts struct {
	// SpaceID is --space-id (an explicit id wins over the space list).
	SpaceID string
	// Name is --name, the node to act on.
	Name string
	// Timeout is drain --timeout as typed ("" = the platform default).
	Timeout string
	// EvictUnmanaged is drain --evict-unmanaged (the api's `force`).
	EvictUnmanaged bool
	// GlobalForce is the global --force/-f on a drain, which NodeDrain
	// refuses: it is not `kubectl drain --force`.
	GlobalForce bool
}

// RejectSpaceClusterFlag ports node::reject_space_cluster_flag: the
// --cluster/-s trap of RejectGlobalClusterFlag, with the hint for the verbs
// that act on a Space.
func (c *Context) RejectSpaceClusterFlag() error {
	c.errorf("kubehz: --cluster/-s names the kind cluster lo manages, not the target here.")
	c.echoErr("  Name the space with:  --space-id sp-xxxxxxxx")
	return ErrHandled
}

// spacePreflight ports node::space_preflight: the active domain's kubehz
// block (hosting: shared), the api URL, the tenant token, and the space id.
// It returns the config, the space id and the label the messages use (the
// slug, or the id when --space-id named it).
func (c *Context) spacePreflight(ctx context.Context, domain, action, spaceIDFlag string) (*Config, string, string, error) {
	cfg, cy, err := c.nodeDomainConfig(domain)
	if err != nil {
		return nil, "", "", err
	}
	// Only a Space needs the platform for this. A hosted or a self-hosted
	// cluster has an API server of its own, and kubectl does it there.
	switch cfg.Hosting {
	case "shared":
	case "hosted":
		c.errorf("kubehz: '%s' is hosting: hosted. This command is for the nodes of a Space.", domain)
		c.echoErr("  A hosted cluster has its own API server. Use kubectl cordon, uncordon or drain there.")
		return nil, "", "", ErrHandled
	default:
		c.errorf("kubehz: '%s' is hosting: %s. This command is for the nodes of a Space.", domain, cfg.Hosting)
		c.echoErr("  Use kubectl cordon, uncordon or drain against your own API server.")
		return nil, "", "", ErrHandled
	}
	if err := c.nodeAPIAccess(cfg, domain, action); err != nil {
		return nil, "", "", err
	}
	// The id is pasted into a URL PATH: a `../` in it would address a
	// different route.
	if spaceIDFlag != "" {
		if !urlIDRe.MatchString(spaceIDFlag) {
			c.errorf("kubehz: '%s' is not a space id.", spaceIDFlag)
			c.echoErr("  A space id looks like sp-xxxxxxxx. Read yours in the dashboard.")
			return nil, "", "", ErrHandled
		}
		return cfg, spaceIDFlag, spaceIDFlag, nil
	}
	sp, err := c.SpaceConfig(domain, cy)
	if err != nil {
		return nil, "", "", err
	}
	row, err := c.spaceLookup(ctx, cfg, sp.Slug)
	if err != nil {
		return nil, "", "", err
	}
	if row == nil {
		c.errorf("kubehz: no space '%s' found for %s.", sp.Slug, domain)
		c.echoErr("  Create it with 'lo provision', or name the space with:  --space-id sp-xxxxxxxx")
		return nil, "", "", ErrHandled
	}
	// The id of the row goes into the URL too, so it gets the same check.
	id := jstrOr(row, "", "id")
	if !urlIDRe.MatchString(id) {
		c.errorf("kubehz: the space row carries no id this CLI can use.")
		return nil, "", "", ErrHandled
	}
	return cfg, id, sp.Slug, nil
}

// drainTimeoutRe is node::drain_timeout's shape: digits, leading zeros
// dropped, at most four significant digits.
var drainTimeoutRe = regexp.MustCompile(`^0*([0-9]{1,4})$`)

// drainTimeout ports node::drain_timeout: the deadline as the api takes it,
// a whole number of seconds from 60 to 3600. 0600 is 600.
func (c *Context) drainTimeout(value string) (int, error) {
	if m := drainTimeoutRe.FindStringSubmatch(value); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= 60 && n <= 3600 {
			return n, nil
		}
	}
	c.errorf("kubehz: --timeout takes a whole number of seconds from 60 to 3600.")
	return 0, ErrHandled
}

// maintenanceUnknown ports node::maintenance_unknown: the refusal of a
// request whose result is open. The api says so itself
// (NODE_MAINTENANCE_OUTCOME_UNKNOWN), or the answer did not arrive: a
// gateway answered for the api, the connection ended after the send, or a
// 2xx is not the answer of the route. cause says which ("" for the api).
func (c *Context) maintenanceUnknown(action, name, cause string) {
	c.errorf("kubehz: the result of the %s request is unknown%s. Possibly, the request got to the control plane.",
		action, optSuffix(" (", cause)+optIf(cause != "", ")"))
	c.echoErr("  Read the phase of node '%s' with 'lo kubehz status' before you try again.", name)
	c.echoErr("  A drain that you send again can start a new deadline.")
}

// maintenanceError ports node::maintenance_error: the refusals of the three
// maintenance routes, each an instruction; the rest prints the api's words.
func (c *Context) maintenanceError(action, name string, res *httpResult) {
	code := apiCode(res.Body)
	// A gateway in front of the api answers 502 or 504 when the api does
	// not answer in time, and 503 with no code when the connection to the
	// api fails. The api can still act on the request (its publish runs on
	// after the gateway timeout), so the result is open.
	if res.Status == 502 || res.Status == 504 || res.Status == 503 && code == "" {
		c.maintenanceUnknown(action, name, "HTTP "+strconv.Itoa(res.Status))
		return
	}
	switch code {
	case "NODE_NOT_JOINED":
		c.errorf("kubehz: node '%s' has not joined the space yet.", name)
		c.echoErr("  Nothing changed. Wait until 'lo kubehz status' shows the node Ready, then try again.")
	case "NODE_MAINTENANCE_CONFLICT":
		c.errorf("kubehz: a maintenance request of another space holds the request name of node '%s'.", name)
		c.echoErr("  Nothing changed. Contact kubehz support.")
	case "NODE_MAINTENANCE_UNAVAILABLE":
		c.errorf("kubehz: the %s request did not reach the control plane.", action)
		c.echoErr("  Nothing changed. Try again shortly.")
	case "NODE_MAINTENANCE_AUDIT_UNAVAILABLE":
		c.errorf("kubehz: the platform cannot record the %s request, so it did not send it.", action)
		c.echoErr("  Nothing changed. Try again shortly.")
	case "NODE_MAINTENANCE_OUTCOME_UNKNOWN":
		c.maintenanceUnknown(action, name, "")
	case "TOKEN_SCOPE_MISSING", "STEP_UP_REQUIRED", "STEP_UP_MFA_REQUIRED":
		c.nodeAPIError("Failed to "+action+" node '"+name+"'", res)
	default:
		c.spaceAPIError("Failed to "+action+" node '"+name+"'", res)
	}
}

// maintain ports node::maintain: send one request and render the answer. A
// 202 says that the platform took the request, not that the node changed.
// timeout is the drain --timeout in seconds (0: not given, or not a drain).
func (c *Context) maintain(ctx context.Context, cfg *Config, spaceID, label, action, name string, body []byte, timeout int, notes ...string) error {
	path := "/api/spaces/" + spaceID + "/nodes/" + name + "/" + action
	res, sent, err := c.requestSent(ctx, "POST", cfg.APIURL+path, withBearer(c.getenv("KUBEHZ_TOKEN")), body)
	if err != nil {
		// Before the send (DNS, connect, TLS), nothing reached the api.
		// After it, possibly the api took the request: no answer is not
		// a refusal. A Ctrl-C after the send ends here too.
		if !sent {
			c.errorf("kubehz API unreachable (POST %s)", path)
			return ErrHandled
		}
		c.maintenanceUnknown(action, name, "no answer")
		return ErrHandled
	}
	if !is2xx(res.Status) {
		// 404: a space the api does not know, a node the space does not
		// hold, or an api older than these routes.
		if res.Status == 404 {
			c.errorf("kubehz: the platform knows no node '%s' in space %s.", name, label)
			c.echoErr("  Check the node name and the space with:  lo kubehz status")
			return ErrHandled
		}
		c.maintenanceError(action, name, res)
		return ErrHandled
	}
	// The answer of the route carries status "requested". Any other 2xx (a
	// proxy page, an apiUrl that names another server) says nothing about
	// the request, so these destructive verbs do not report a success.
	replaced, status := "", ""
	if v, ok := parseJSON(res.Body); ok {
		replaced = jstrOr(envelope(v), "", "replaced")
		status = jstrOr(envelope(v), "", "status")
	}
	if status != "requested" {
		c.maintenanceUnknown(action, name, "HTTP "+strconv.Itoa(res.Status)+" with an answer this CLI cannot read")
		return ErrHandled
	}
	c.echo("kubehz: %s of node '%s' in space %s requested.", action, name, label)
	for _, n := range notes {
		c.echo("%s", n)
	}
	// The answer names the replaced action only. A drain over a drain of the
	// same machine keeps the first deadline and the first timeout. A new
	// machine under the same node name starts a new one. So the lines say
	// both, and that a new --timeout has no effect then.
	switch replaced {
	case "drain":
		if action == "drain" {
			c.echo("  It replaces a pending drain of this node. The first deadline stays if the machine is the same.")
			if timeout > 0 {
				c.echo("  For the same machine, the first timeout stays too, so --timeout %d has no effect.", timeout)
			}
		} else {
			c.echo("  It cancels a pending drain. Pods that the drain did not evict stay on the node.")
		}
	case "cordon", "uncordon":
		c.echo("  It replaces a pending %s of this node.", replaced)
	}
	c.echo("  The change is asynchronous. Read its phase with:  lo kubehz status")
	return nil
}

// NodeCordon ports node::cordon: mark one node of a Space unschedulable.
func (c *Context) NodeCordon(ctx context.Context, domain string, o MaintenanceOpts) error {
	if err := c.assertNodeName(o.Name); err != nil {
		return err
	}
	cfg, id, label, err := c.spacePreflight(ctx, domain, "cordon a node", o.SpaceID)
	if err != nil {
		return err
	}
	return c.maintain(ctx, cfg, id, label, "cordon", o.Name, nil, 0,
		"  The scheduler puts no new pods on the node. The pods on it keep running.")
}

// NodeUncordon ports node::uncordon: make one node of a Space schedulable
// again.
func (c *Context) NodeUncordon(ctx context.Context, domain string, o MaintenanceOpts) error {
	if err := c.assertNodeName(o.Name); err != nil {
		return err
	}
	cfg, id, label, err := c.spacePreflight(ctx, domain, "uncordon a node", o.SpaceID)
	if err != nil {
		return err
	}
	return c.maintain(ctx, cfg, id, label, "uncordon", o.Name, nil, 0,
		"  The scheduler can put new pods on the node again. Evicted pods do not move back.",
		"  A cordon that the platform holds (a migration, a node retirement) stays.")
}

// NodeDrain ports node::drain: cordon one node of a Space and evict its
// pods. The node stays in the space: an uncordon puts it back into service.
func (c *Context) NodeDrain(ctx context.Context, domain string, o MaintenanceOpts) error {
	// A --force that means `kubectl drain --force` is refused, not ignored.
	if o.GlobalForce {
		c.errorf("kubehz: drain does not use the global --force/-f. To also evict the pods that no controller recreates, use --evict-unmanaged.")
		return ErrHandled
	}
	if err := c.assertNodeName(o.Name); err != nil {
		return err
	}
	seconds := 0
	if o.Timeout != "" {
		n, err := c.drainTimeout(o.Timeout)
		if err != nil {
			return err
		}
		seconds = n
	}
	cfg, id, label, err := c.spacePreflight(ctx, domain, "drain a node", o.SpaceID)
	if err != nil {
		return err
	}
	// `force` is the api's name (kubectl drain --force). The flag has its
	// own name because `lo` owns --force/-f globally (see GlobalForce).
	var pairs []jsonPair
	if seconds > 0 {
		pairs = append(pairs, jsonPair{"timeoutSeconds", seconds})
	}
	if o.EvictUnmanaged {
		pairs = append(pairs, jsonPair{"force", true})
	}
	notes := []string{"  The platform cordons the node and evicts its pods."}
	if seconds > 0 {
		notes = append(notes, "  It tries for at most "+strconv.Itoa(seconds)+" seconds.")
	} else {
		notes = append(notes, "  It tries until the deadline the platform sets by default.")
	}
	if o.EvictUnmanaged {
		notes = append(notes, "  It also evicts the pods that no controller recreates. Nothing recreates them.")
	} else {
		notes = append(notes, "  A pod that no controller recreates stays, and the drain waits as Blocked.",
			"  To evict such pods too, add:  --evict-unmanaged")
	}
	return c.maintain(ctx, cfg, id, label, "drain", o.Name, compactJSON(pairs...), seconds, notes...)
}
