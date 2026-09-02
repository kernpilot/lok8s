package kubehz

// cluster.go — the cluster-level `lo kubehz` subcommand bodies from
// libs/kubehz/main: register, deregister, status, join (space ticket),
// assess (+ the pretty-printer), claim-code, claim (mode-3 nonce), and
// re-enroll (agent-identity contract R2).

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Register is `lo kubehz register`: read + validate the active domain's
// config, then announce the cluster.
func (c *Context) Register(ctx context.Context, domain string) error {
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	if err := c.Validate(cfg, cy); err != nil {
		return err
	}
	// A space IS its registration — creating it (lo provision) is the whole act.
	if cfg.Hosting == "shared" {
		c.errorf("hosting: shared has nothing to register separately — 'lo provision' creates the space")
		return ErrHandled
	}
	if cfg.Access == "none" {
		c.errorf("spec.kubehz.access is 'none' — nothing to register")
		return ErrHandled
	}
	return c.RegisterCluster(ctx, cfg, domain, cy)
}

// Deregister is `lo kubehz deregister`. Space-aware: a shared-hosting
// domain's deregistration removes the SPACE — same act as lo destroy.
func (c *Context) Deregister(ctx context.Context, domain string) error {
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	if cfg.Hosting == "shared" {
		return c.DestroyShared(ctx, cfg, domain, cy)
	}
	if cfg.Access == "none" {
		c.errorf("spec.kubehz.access is 'none' — nothing to deregister")
		return ErrHandled
	}
	return c.DeregisterCluster(ctx, cfg, domain, cy)
}

// Status is `lo kubehz status`.
func (c *Context) Status(ctx context.Context, domain string) error {
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	return c.statusReport(ctx, cfg, domain, cy)
}

func (c *Context) statusReport(ctx context.Context, cfg *Config, domain, cy string) error {
	c.echo("Domain:  %s", domain)
	c.echo("Hosting: %s", cfg.Hosting)
	c.echo("Access:  %s", cfg.Access)
	// Which agent the SPEC says owns the heartbeat — intent, not a live probe.
	if cfg.Hosting != "shared" && cfg.Access != "none" {
		agent := cfg.Agent
		if agent == "" {
			agent = "cronjob"
		}
		if agent == "operator" {
			c.echo("Agent:   operator (deployment/kubehz-live-agent — live view + desired state)")
		} else {
			c.echo("Agent:   cronjob (cronjob/kubehz-heartbeat — every 5 minutes)")
		}
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		c.echo("API URL: <not set>")
	} else {
		c.echo("API URL: %s", apiURL)
	}

	// Space-aware: a shared-hosting domain's status IS its space's status.
	if cfg.Hosting == "shared" {
		return c.SpaceStatus(ctx, cfg, domain, cy)
	}
	if cfg.Access == "none" {
		c.echo("Status:  not registered (access: none)")
		return nil
	}

	// Query the tenant registry for the live row: filter the rows
	// client-side and read the ROW's fields (status, lastHeartbeat,
	// connected) — never the pagination envelope's.
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return err
	}
	res, err := c.fetchStatus(ctx, "GET", apiURL+"/api/clusters?perPage=500", withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil {
		c.echo("Status:  unknown (API unreachable: %s)", apiURL)
		return nil
	}
	if !is2xx(res.Status) {
		c.echo("Status:  unknown (HTTP %d — set KUBEHZ_TOKEN to a clusters:write token of the owning tenant)", res.Status)
		return nil
	}

	// Oldest-first: the server binds agent identity to the OLDEST active row.
	var row any
	if v, ok := parseJSON(res.Body); ok {
		if rs := domainRows(v, domain); len(rs) > 0 {
			row = rs[0]
		}
	}
	if row == nil {
		c.echo("Status:  not registered (no cluster for %s in this tenant's registry)", domain)
		return nil
	}

	statusVal := jstrOr(row, "unknown", "status")
	clusterID := jstrOr(row, "-", "id")
	lastBeat := jstrOr(row, "", "lastHeartbeat")
	connected := jstrOr(row, "", "connected")
	c.echo("Status:  %s (id: %s)", statusVal, clusterID)
	if lastBeat != "" {
		switch connected {
		case "true":
			c.echo("Beat:    %s (connected)", lastBeat)
		case "false":
			c.echo("Beat:    %s (stale — outside the reporting window)", lastBeat)
		default:
			c.echo("Beat:    %s", lastBeat)
		}
	} else {
		c.echo("Beat:    none yet — deploy the heartbeat agent (see docs/guide/kubehz.md)")
	}
	return nil
}

// Join is `lo kubehz join <node>`: mint a node join ticket for the active
// domain's space (hosting: shared). The ticket is single-node, single-use
// and short-lived; the plaintext is printed exactly once and never stored.
func (c *Context) Join(ctx context.Context, domain, node string) error {
	if domain == "" {
		c.errorf("No active domain. Use: lo use <domain>")
		return ErrHandled
	}
	if node == "" {
		c.errorf("Node name required: lo kubehz join <node-name>")
		return ErrHandled
	}
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	if cfg.Hosting != "shared" {
		c.errorf("join tickets are for hosting: shared (this domain is hosting: %s)", cfg.Hosting)
		return ErrHandled
	}
	if err := c.Validate(cfg, cy); err != nil {
		return err
	}
	if c.getenv("KUBEHZ_TOKEN") == "" {
		c.errorf("KUBEHZ_TOKEN is required to mint a join ticket")
		return ErrHandled
	}

	sp, err := c.SpaceConfig(domain, cy)
	if err != nil {
		return err
	}
	row, err := c.spaceLookup(ctx, cfg, sp.Slug)
	if err != nil {
		return err
	}
	if row == nil {
		c.errorf("No space '%s' found — run 'lo provision' first", sp.Slug)
		return ErrHandled
	}
	spaceID := jstrOr(row, "", "id")
	if spaceID == "" {
		c.errorf("space row carries no id — refusing to mint a ticket")
		return ErrHandled
	}
	return c.spaceMintJoin(ctx, cfg, spaceID, node)
}

// Assess is `lo kubehz assess`: GET /api/clusters/<id>/assessment with the
// same tenant-token source `lo kubehz status` uses.
func (c *Context) Assess(ctx context.Context, domain string) error {
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	// A space has no control plane to assess.
	if cfg.Hosting == "shared" {
		c.echo("Assessment does not apply to hosting: shared — a space has no control")
		c.echo("plane of its own. Use 'lo kubehz status' for the space + node view.")
		return nil
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		c.errorf("spec.kubehz.apiUrl is not set for %s", domain)
		return ErrHandled
	}
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return err
	}
	clusterID, err := c.ResolveClusterID(ctx, domain, apiURL)
	if err != nil {
		c.errorf("kubehz: no cluster found for %s — is it registered + claimed, and is KUBEHZ_TOKEN set?", domain)
		return ErrHandled
	}
	body, err := c.fetch(ctx, "GET", apiURL+"/api/clusters/"+clusterID+"/assessment", withBearer(c.getenv("KUBEHZ_TOKEN")), nil)
	if err != nil {
		c.errorf("kubehz: could not fetch the assessment for %s (%s)", domain, clusterID)
		return ErrHandled
	}
	return c.RenderAssessment(domain, body)
}

// RenderAssessment ports kubehz::render_assessment: one aligned line per
// probe with kapply-style markers (the `lo registry status` idiom).
func (c *Context) RenderAssessment(domain string, response []byte) error {
	cOK, cWarn, cDim, cOff := "", "", "", ""
	if c.isTTY() {
		cOK, cWarn, cDim, cOff = "\033[32m", "\033[33m", "\033[2m", "\033[0m"
	}
	v, ok := parseJSON(response)
	if !ok {
		c.errorf("kubehz: unexpected assessment response (not JSON)")
		return ErrHandled
	}
	body := envelope(v)
	a := jget(body, "assessment")
	if a == nil {
		c.echo("No assessment recorded for %s yet.", domain)
		c.echo("The in-cluster agent sends one at most every 24h — request a refresh in the dashboard, or wait for the next heartbeat.")
		return nil
	}
	assessed := jstrOr(body, "", "assessedAt")
	collected := jstrOr(a, "", "collectedAt")
	c.echo("kubehz assessment — %s%s%s", domain, optSuffix(" · collected ", collected), optSuffix(" · stored ", assessed))
	c.echo("")

	line := func(marker, name, note string) {
		fmt.Fprintf(c.out(), "%s %-14s %s\n", marker, name, note)
	}
	okMark := cOK + "✓" + cOff
	warnMark := cWarn + "⚠" + cOff

	line(okMark, "kubernetes", jstrOr(a, "unknown", "k8sVersion"))

	// datastore — the restore/graft hinge.
	datastore := jstrOr(a, "unknown", "datastore")
	reachable := jstrOr(a, "false", "etcdReachable")
	switch {
	case datastore == "etcd" && reachable == "true":
		line(okMark, "datastore", "etcd · reachable")
	case datastore == "etcd":
		line(warnMark, "datastore", "etcd · NOT reachable")
	default:
		line(warnMark, "datastore", datastore)
	}

	if jstrOr(a, "false", "capiManaged") == "true" {
		line(warnMark, "capi", "Cluster API CRDs present — pause the CAPI controllers before handover")
	} else {
		line(okMark, "capi", "not CAPI-managed")
	}

	line(okMark, "cni", jstrOr(a, "unknown", "cni"))
	line(okMark, "networks", "pods "+jstrOr(a, "?", "podCidr")+" · services "+jstrOr(a, "?", "serviceCidr"))

	scCount := 0
	if sc, ok := jget(a, "storageClasses").([]any); ok {
		scCount = len(sc)
	}
	line(okMark, "storage", strconv.Itoa(scCount)+" classes · "+jstrOr(a, "0", "pvSummary", "count")+" PVs · "+jstrOr(a, "0", "pvSummary", "totalGi")+"Gi")
	// Per-provisioner detail (dimmed) — this drives the data-move plan.
	if by, ok := jget(a, "pvSummary", "byProvisioner").(map[string]any); ok {
		for _, prov := range sortedKeys(by) {
			entry := by[prov]
			fmt.Fprintf(c.out(), "      %s%s — %s PVs · %sGi%s\n", cDim, prov,
				jstr(jget(entry, "count")), jstr(jget(entry, "totalGi")), cOff)
		}
	}

	line(okMark, "loadbalancers", jstrOr(a, "0", "loadBalancers")+" LoadBalancer service(s)")
	line(okMark, "webhooks", jstrOr(a, "0", "webhooks", "validating")+" validating · "+jstrOr(a, "0", "webhooks", "mutating")+" mutating")
	line(okMark, "nodes", jstrOr(a, "0", "cpUsage", "nodes")+" total · "+jstrOr(a, "0", "cpUsage", "cpNodes")+" control-plane")

	if path := jstrOr(body, "", "feasibility", "path"); path != "" {
		c.echo("")
		c.echo("Feasibility: %s", path)
		for _, r := range jstrings(jget(body, "feasibility", "reasons")) {
			if r == "" {
				continue
			}
			fmt.Fprintf(c.out(), "  %s·%s %s\n", cDim, cOff, r)
		}
		for _, w := range jstrings(jget(body, "feasibility", "warnings")) {
			if w == "" {
				continue
			}
			fmt.Fprintf(c.out(), "  %s⚠%s %s\n", cWarn, cOff, w)
		}
	}
	return nil
}

// jstrings is `<path> // [] | .[]` rendered -r.
func jstrings(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		out = append(out, jstr(e))
	}
	return out
}

// sortedKeys mirrors jq's to_entries order over an object (keys sorted —
// jq keeps insertion order for objects it parsed, but Go maps do not; the
// stable choice is sorted keys).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ── claim-code / claim / re-enroll (against the AMBIENT kubeconfig) ──

const (
	agentNamespace  = "kubehz-system"
	agentSecret     = "kubehz-agent"
	agentConfigName = "kubehz-agent-config"
)

// ClaimCode is `lo kubehz claim-code`: print the one-time claim code (C)
// from the in-cluster kubehz-agent Secret via the ACTIVE kubeconfig.
// SECURITY: the agent-token (A) is never printed by any command — only
// `.data.claim-code` is read.
func (c *Context) ClaimCode(ctx context.Context) error {
	if err := c.runQuiet(ctx, "kubectl", "-n", agentNamespace, "get", "secret", agentSecret); err != nil {
		c.errorf("secret/%s not found in %s. Deploy the heartbeat agent first (see docs/guide/kubehz.md) and point your kubeconfig at the cluster.", agentSecret, agentNamespace)
		return ErrHandled
	}
	raw, _ := c.capture(ctx, true, "kubectl", "-n", agentNamespace, "get", "secret", agentSecret, "-o", "jsonpath={.data.claim-code}")
	code := base64Decode(raw)
	if code == "" {
		c.errorf("secret/%s has no claim-code yet — the agent mints it on its first run. Wait for one heartbeat, then retry.", agentSecret)
		return ErrHandled
	}
	// C on stdout (pipe-friendly); the human hint on stderr.
	c.echo("%s", code)
	c.echoErr("")
	c.echoErr("Paste this one-time claim code at the dashboard /claim page while signed in")
	c.echoErr("to attribute this cluster to your account. It is single-use — do not share it.")
	return nil
}

// base64Decode mirrors `| base64 -d` on a `$(…)` capture: trailing
// newlines stripped, a decode failure yields "".
func base64Decode(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return trimNL(string(out))
}

var claimNonceRe = regexp.MustCompile(`^khzn_[A-Za-z0-9_-]{15,195}$`)

// Claim is `lo kubehz claim --nonce <value>`: place a dashboard-minted
// claim-challenge nonce where the heartbeat agent can echo it (mode 3).
// SECURITY: the nonce is a claim ticket — never printed, not even on
// rejection; only its presence/shape is reported.
func (c *Context) Claim(ctx context.Context, nonce string) error {
	// Shape check BEFORE any cluster call: khzn_ + 43 base64url chars, whole
	// value bounded at 20..200 chars by the heartbeat schema.
	if !claimNonceRe.MatchString(nonce) {
		c.errorf("invalid claim nonce: expected a dashboard-minted khzn_… value (20-200 chars); nothing was placed")
		return ErrHandled
	}
	if err := c.runQuiet(ctx, "kubectl", "-n", agentNamespace, "get", "configmap", agentConfigName); err != nil {
		c.errorf("configmap/%s not found in %s. Deploy the heartbeat agent first (see docs/guide/kubehz.md) and point your kubeconfig at the cluster.", agentConfigName, agentNamespace)
		return ErrHandled
	}
	// One call, both annotations: the value and its placement stamp.
	stamp := strconv.FormatInt(c.now().Unix(), 10)
	if _, err := c.capture(ctx, false, "kubectl", "-n", agentNamespace, "annotate", "--overwrite", "configmap", agentConfigName,
		"kubehz.cloud/claim-nonce="+nonce, "kubehz.cloud/claim-nonce-placed="+stamp); err != nil {
		c.errorf("could not annotate configmap/%s — check your kubeconfig permissions", agentConfigName)
		return ErrHandled
	}
	c.echo("kubehz: claim nonce placed for the agent.")
	c.echo("The next heartbeat (within 5 minutes) echoes it to the platform; on success the")
	c.echo("dashboard shows the cluster claimed and the agent clears the nonce. The challenge")
	c.echo("expires 15 minutes after minting — mint and place a fresh one if it lapses.")
	return nil
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReEnroll is `lo kubehz re-enroll`: re-enroll a REGENERATED in-cluster
// agent token with the platform (POST /api/clusters/{id}/agent-token with
// the USER's bearer). The token plaintext never leaves this machine and is
// never printed; only its sha256 is sent, HTTPS-only.
func (c *Context) ReEnroll(ctx context.Context, domain string) error {
	cy, err := c.requireDomainSpec(domain)
	if err != nil {
		return err
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return err
	}
	if cfg.Hosting == "shared" {
		c.errorf("hosting: shared has no heartbeat agent to re-enroll (a Space has no agent)")
		return ErrHandled
	}
	if cfg.Access == "none" {
		c.errorf("spec.kubehz.access is 'none' — no agent identity to re-enroll")
		return ErrHandled
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		c.errorf("spec.kubehz.apiUrl is not set for %s", domain)
		return ErrHandled
	}
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return err
	}
	token := c.getenv("KUBEHZ_TOKEN")
	if token == "" {
		c.errorf("KUBEHZ_TOKEN is required to re-enroll (a clusters:write API token from the dashboard's Access -> API Tokens)")
		return ErrHandled
	}

	// The CURRENT in-cluster token — read via the ambient kubeconfig. Never
	// printed.
	raw, _ := c.capture(ctx, true, "kubectl", "-n", agentNamespace, "get", "secret", agentSecret, "-o", "jsonpath={.data.agent-token}")
	agentToken := base64Decode(raw)
	if agentToken == "" {
		c.errorf("secret/kubehz-agent has no agent-token in kubehz-system — deploy the heartbeat agent and wait one tick (it mints the token on its first run)")
		return ErrHandled
	}
	// sha256, lowercase hex — the exact verifier shape the route stores.
	// Only the hash leaves this function; the token string is not reused.
	sum := sha256.Sum256([]byte(agentToken))
	tokenHash := hex.EncodeToString(sum[:])
	if !sha256HexRe.MatchString(tokenHash) {
		c.errorf("could not hash the agent token (need sha256sum or openssl)")
		return ErrHandled
	}

	clusterID, err := c.ResolveClusterID(ctx, domain, apiURL)
	if err != nil {
		c.errorf("kubehz: no cluster found for %s — is it registered + claimed, and does KUBEHZ_TOKEN belong to the owning tenant?", domain)
		return ErrHandled
	}

	res, err := c.fetchStatus(ctx, "POST", apiURL+"/api/clusters/"+clusterID+"/agent-token", withBearer(token),
		compactJSON(jsonPair{"agentTokenHash", tokenHash}))
	if err != nil {
		c.errorf("kubehz: agent-token request failed (network) — retry once %s is reachable", apiURL)
		return ErrHandled
	}
	if res.Status != 200 {
		v, _ := parseJSON(res.Body)
		code := jstrOr(v, "", "data", "code")
		msg := jstrOr(v, "", "data", "message")
		c.errorf("kubehz: re-enroll refused (HTTP %d%s)%s", res.Status, optSuffix(" ", code), optSuffix(": ", msg))
		return ErrHandled
	}
	v, _ := parseJSON(res.Body)
	if jstrOr(v, "false", "rotated") == "true" {
		c.echo("kubehz: agent token re-enrolled for %s (%s).", domain, clusterID)
		c.echo("The platform revoked the previous token and armed the in-cluster one;")
		c.echo("heartbeats resume on the agent's next tick (within 5 minutes).")
	} else {
		c.echo("kubehz: the in-cluster agent token is already the live one for %s (%s) — nothing rotated.", domain, clusterID)
		c.echo("If heartbeats still fail, check the agent's logs (kubehz-system, CronJob kubehz-heartbeat).")
	}
	return nil
}
