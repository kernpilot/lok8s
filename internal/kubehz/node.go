package kubehz

// node.go — libs/kubehz/node: machines you bring, joined to a kubehz-HOSTED
// control plane (static pools). The platform composes the whole
// `kubeadm join` line; this file mints the ticket, checks the line, and
// runs it. It never builds a join command of its own.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/kernpilot/lok8s/internal/execx"
)

// NodeOpts carries the `lo kubehz node` flags.
type NodeOpts struct {
	// ClusterID is --cluster-id (an explicit id wins over the registry).
	ClusterID string
	// Pool is --pool; Name is --name; NodeIP is --node-ip; KubeletVersion
	// is --kubelet-version; PrintOnly is --print-only.
	Pool           string
	Name           string
	NodeIP         string
	KubeletVersion string
	PrintOnly      bool
}

var clusterIDRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// nodePreflight ports node::preflight: the active domain's kubehz block, the
// api URL, the tenant token, and the platform cluster id.
func (c *Context) nodePreflight(ctx context.Context, domain, action, clusterIDFlag string) (*Config, string, error) {
	if domain == "" {
		c.errorf("No active domain. Use: lo use <domain>")
		return nil, "", ErrHandled
	}
	cy := c.clusterYAMLPath(domain)
	if !fileExists(cy) {
		c.errorf("No cluster.lok8s.yaml for domain: %s", domain)
		return nil, "", ErrHandled
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return nil, "", err
	}
	switch cfg.Hosting {
	case "hosted":
	case "shared":
		c.errorf("kubehz: '%s' is hosting: shared — its nodes join a Space.", domain)
		c.echoErr("  Mint a Space join ticket with:  lo kubehz join <node-name>")
		return nil, "", ErrHandled
	default:
		c.errorf("kubehz: '%s' is hosting: %s — it runs its own control plane.", domain, cfg.Hosting)
		c.echoErr("  Join nodes with kubeadm against your own API server.")
		return nil, "", ErrHandled
	}
	apiURL := cfg.APIURL
	if apiURL == "" {
		c.errorf("spec.kubehz.apiUrl is not set for %s", domain)
		return nil, "", ErrHandled
	}
	if err := c.requireHTTPS(apiURL, "spec.kubehz.apiUrl"); err != nil {
		return nil, "", err
	}
	if c.getenv("KUBEHZ_TOKEN") == "" {
		c.errorf("KUBEHZ_TOKEN is required to %s.", action)
		c.echoErr("  Mint a clusters:write API token in the dashboard (Access -> API Tokens).")
		return nil, "", ErrHandled
	}
	// An explicit --cluster-id wins. The value is pasted into a URL PATH.
	if clusterIDFlag != "" {
		if !clusterIDRe.MatchString(clusterIDFlag) {
			c.errorf("kubehz: '%s' is not a cluster id.", clusterIDFlag)
			c.echoErr("  A cluster id looks like cl-xxxxxxxx. Read yours in the dashboard.")
			return nil, "", ErrHandled
		}
		return cfg, clusterIDFlag, nil
	}
	id, err := c.ResolveClusterID(ctx, domain, apiURL)
	switch {
	case err == nil:
		return cfg, id, nil
	case errors.Is(err, errNotRegistered):
		c.errorf("kubehz: the registry holds no cluster for %s.", domain)
		c.echoErr("  Name the cluster directly with:  --cluster-id cl-xxxxxxxx")
		return nil, "", ErrHandled
	default:
		c.errorf("kubehz: the cluster registry did not answer (%s).", apiURL)
		c.echoErr("  Check KUBEHZ_TOKEN and the network, then try again.")
		return nil, "", ErrHandled
	}
}

// scrub ports node::scrub: strip terminal control characters from a server
// string before it is shown. Tab and newline survive.
func scrub(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// nodeAPIError ports node::api_error: the api's refusal codes, each an
// instruction.
func (c *Context) nodeAPIError(context string, res *httpResult) {
	code := apiCode(res.Body)
	msg := scrub(apiMessage(res.Body))
	switch code {
	case "STATIC_POOLS_NOT_ENABLED":
		c.errorf("kubehz: your account may not use static pools yet.")
		c.echoErr("  Static pools roll out account by account. Ask kubehz support to set")
		c.echoErr("  the staticPoolsEnabled flag, then run this command again.")
	case "KUBELET_BELOW_FLOOR":
		c.errorf("kubehz: the kubelet on this machine is too old%s", optSuffix(" — ", msg))
		c.echoErr("  A node must run a kubelet no more than two minor versions below the")
		c.echoErr("  control plane. Upgrade the kubelet, then run this command again.")
	case "QUOTA_EXCEEDED":
		c.errorf("kubehz: this cluster has no free slot for another node%s", optSuffix(" — ", msg))
		c.echoErr("  Free a slot:  lo kubehz node remove --name <node-name>")
		c.echoErr("  Or ask kubehz support to raise the limit.")
	case "CONTROL_PLANE_NOT_READY":
		c.errorf("kubehz: the control plane has not published its join address yet.")
		c.echoErr("  The platform publishes the address and the CA fingerprints after the")
		c.echoErr("  cluster declares its first pool with kind: static.")
		c.echoErr("  Check with:  lo kubehz node status")
	case "NODE_EXISTS":
		c.errorf("kubehz: this cluster already holds a node with that name%s", optSuffix(" — ", msg))
		c.echoErr("  Choose another name with --name, or remove the old node first:")
		c.echoErr("    lo kubehz node remove --name <node-name>")
	case "POOL_NOT_STATIC":
		c.errorf("kubehz: kubehz provisions the machines in that pool%s", optSuffix(" — ", msg))
		c.echoErr("  A machine you bring joins a pool with kind: static. Declare one in the")
		c.echoErr("  dashboard, then join.")
	case "WORKER_POOL_NOT_FOUND":
		c.errorf("kubehz: this cluster has no such pool%s", optSuffix(" — ", msg))
		c.echoErr("  Declare a pool with kind: static in the dashboard, then join.")
	case "NOT_HOSTED":
		c.errorf("kubehz: this cluster runs its own control plane%s", optSuffix(" — ", msg))
		c.echoErr("  Join nodes with kubeadm against your own API server.")
	case "TENANT_SUSPENDED":
		c.errorf("kubehz: your account is suspended, so it may not add nodes.")
		c.echoErr("  Contact kubehz support.")
	case "TOKEN_SCOPE_MISSING":
		c.errorf("kubehz: KUBEHZ_TOKEN does not carry the clusters:write scope.")
		c.echoErr("  Mint a token with clusters:write in the dashboard (Access -> API Tokens).")
	case "STEP_UP_REQUIRED", "STEP_UP_MFA_REQUIRED":
		c.errorf("kubehz: the platform asks for a fresh, two-factor sign-in.")
		c.echoErr("  A browser sign-in cannot happen here. Put a clusters:write API token in")
		c.echoErr("  KUBEHZ_TOKEN instead — minting that token IS the approval.")
	case "HOSTED_BACKEND_ERROR":
		c.errorf("kubehz: the hosted control-plane backend did not answer%s", optSuffix(" — ", msg))
		c.echoErr("  Nothing changed. Try again shortly, or contact kubehz support.")
	default:
		c.spaceAPIError(context, res)
	}
}

var nodeNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// assertNodeName ports node::assert_node_name: a DNS label, 63 at most —
// the value is pasted into a URL PATH.
func (c *Context) assertNodeName(name string) error {
	if !nodeNameRe.MatchString(name) || len(name) > 63 {
		c.errorf("kubehz: '%s' is not a node name the platform accepts.", name)
		c.echoErr("  A node name is a DNS label: lowercase letters, digits and dashes, 63 at most.")
		c.echoErr("  Give one with:  --name <node-name>")
		return ErrHandled
	}
	return nil
}

// RejectGlobalClusterFlag ports node::reject_global_cluster_flag: `lo`
// owns --cluster/-s globally (the kind cluster); on a node verb it would
// parse silently and be ignored, so it is refused by name.
func (c *Context) RejectGlobalClusterFlag() error {
	c.errorf("kubehz: --cluster/-s names the kind cluster lo manages, not the target here.")
	c.echoErr("  Name the platform cluster with:  --cluster-id cl-xxxxxxxx")
	return ErrHandled
}

// defaultNodeName ports node::default_name: the short hostname, lowercased.
func (c *Context) defaultNodeName() string {
	name, err := c.hostname()
	if err != nil {
		name = ""
	}
	name = trimNL(name)
	if name == "" {
		name = c.getenv("HOSTNAME")
	}
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	return strings.ToLower(name)
}

var kubeletVersionRe = regexp.MustCompile(`(v[0-9]+\.[0-9]+\.[0-9]+[^[:space:]]*)`)

// kubeletVersion ports node::kubelet_version: the kubelet on THIS machine,
// "" when there is none to ask.
func (c *Context) kubeletVersion(ctx context.Context) string {
	if !c.lookPath("kubelet") {
		return ""
	}
	out, err := c.capture(ctx, true, "kubelet", "--version")
	if err != nil {
		return ""
	}
	m := kubeletVersionRe.FindStringSubmatch(trimNL(out))
	if m == nil {
		return ""
	}
	return m[1]
}

// inferPool ports node::infer_pool: the one pool every node on the cluster
// already sits in, or "".
func (c *Context) inferPool(ctx context.Context, cfg *Config, clusterID string) string {
	res := c.spaceAPIQuiet(ctx, cfg, "GET", "/api/clusters/"+clusterID+"/nodes")
	if !is2xx(res.Status) {
		return ""
	}
	v, ok := parseJSON(res.Body)
	if !ok {
		return ""
	}
	nodes, _ := jget(envelope(v), "nodes").([]any)
	seen := map[string]bool{}
	var pools []string
	for _, n := range nodes {
		p := jstr(jget(n, "pool"))
		if !seen[p] {
			seen[p] = true
			pools = append(pools, p)
		}
	}
	sort.Strings(pools)
	// `grep -c .` counts non-empty lines.
	var nonEmpty []string
	for _, p := range pools {
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	if len(nonEmpty) != 1 {
		return ""
	}
	return strings.Join(pools, "\n")
}

// nodeJoinAllowedFlags are the join flags this CLI will run, and no others.
var nodeJoinAllowedFlags = []string{"--token", "--discovery-token-ca-cert-hash", "--node-name"}

var (
	joinAlphabetRe = regexp.MustCompile(`^[A-Za-z0-9._:/=+-]+([ \t][A-Za-z0-9._:/=+-]+)*$`)
	joinEndpointRe = regexp.MustCompile(`^[A-Za-z0-9.-]+:[0-9]{1,5}$`)
)

// AssertJoinCommand ports node::assert_join_command: refuse a join line
// whose SEMANTICS this CLI does not recognise — the alphabet check keeps a
// shell metacharacter out, the structural allowlist keeps a dangerous
// kubeadm FLAG out, and at least one CA fingerprint is required.
func (c *Context) AssertJoinCommand(line string) error {
	if !strings.HasPrefix(line, "kubeadm join ") {
		c.errorf("kubehz: the platform returned a command this CLI does not run.")
		c.echoErr("  Expected a 'kubeadm join' line. Nothing ran on this machine.")
		c.echoErr("  Print the command instead and read it:  --print-only")
		return ErrHandled
	}
	if !joinAlphabetRe.MatchString(line) {
		c.errorf("kubehz: the join command carries characters this CLI does not run.")
		c.echoErr("  Nothing ran on this machine. Print the command and read it:  --print-only")
		return ErrHandled
	}
	tok := strings.Fields(line)
	endpoint := ""
	if len(tok) > 2 {
		endpoint = tok[2]
	}
	if !joinEndpointRe.MatchString(endpoint) {
		c.errorf("kubehz: the join command's server address is not a host:port.")
		c.echoErr("  Nothing ran on this machine. Print the command and read it:  --print-only")
		return ErrHandled
	}
	seenCA := false
	for i := 3; i < len(tok); i++ {
		if !strings.HasPrefix(tok[i], "--") {
			continue // a value following an allowed flag
		}
		flag := tok[i]
		if j := strings.IndexByte(flag, '='); j >= 0 {
			flag = flag[:j]
		}
		allowed := false
		for _, cand := range nodeJoinAllowedFlags {
			if flag == cand {
				allowed = true
				break
			}
		}
		if !allowed {
			c.errorf("kubehz: the platform sent a join flag this CLI will not run: %s", flag)
			c.echoErr("  Nothing ran on this machine. This CLI runs only the join, token, CA-pin")
			c.echoErr("  and node-name flags. Print the command and read it:  --print-only")
			return ErrHandled
		}
		if flag == "--discovery-token-ca-cert-hash" {
			seenCA = true
		}
	}
	if !seenCA {
		c.errorf("kubehz: the join command pins no CA fingerprint, so a node could not verify the control plane.")
		c.echoErr("  Nothing ran on this machine. A safe join carries at least one")
		c.echoErr("  --discovery-token-ca-cert-hash. Print the command and read it:  --print-only")
		return ErrHandled
	}
	return nil
}

// mintedSlotNote ports node::minted_slot_note: a post-mint refusal still
// holds a node slot server-side until the ticket expires.
func (c *Context) mintedSlotNote(nodeName string) {
	c.echoErr("")
	c.echoErr("  The mint succeeded, so this attempt holds a node slot until the ticket")
	c.echoErr("  expires (about ten minutes). Free the slot now with:")
	c.echoErr("    lo kubehz node remove --name %s", nodeName)
}

// NodeJoin ports node::join: join THIS machine to a hosted cluster.
func (c *Context) NodeJoin(ctx context.Context, domain string, o NodeOpts) error {
	cfg, clusterID, err := c.nodePreflight(ctx, domain, "join a node", o.ClusterID)
	if err != nil {
		return err
	}
	nodeName := o.Name
	if nodeName == "" {
		nodeName = c.defaultNodeName()
	}
	if err := c.assertNodeName(nodeName); err != nil {
		return err
	}
	nodePool := o.Pool
	if nodePool == "" {
		nodePool = c.inferPool(ctx, cfg, clusterID)
	}
	if nodePool == "" {
		c.errorf("kubehz: name the static pool this machine joins.")
		c.echoErr("  Add:  --pool <pool-name>")
		c.echoErr("  The CLI infers the pool only when every node of the cluster shares one.")
		return ErrHandled
	}
	// Say so BEFORE minting a ticket that would hold a slot for nothing.
	if !o.PrintOnly && !c.lookPath("kubeadm") {
		c.errorf("kubehz: kubeadm is not on this machine, so the join cannot run.")
		c.echoErr("  Install kubeadm, kubelet and a container runtime first — see the")
		c.echoErr("  Kubernetes install guide for your distribution.")
		c.echoErr("  To mint the ticket here and join elsewhere, add:  --print-only")
		return ErrHandled
	}
	if !o.PrintOnly && !c.isRoot() {
		c.errorf("kubehz: kubeadm join writes to /etc/kubernetes, so it must run as root.")
		c.echoErr("  Run it again with 'sudo -E' — the -E keeps KUBEHZ_TOKEN and the PATH_*")
		c.echoErr("  lo environment, which plain sudo would strip.")
		c.echoErr("  Or add --print-only and run the printed command as root yourself.")
		return ErrHandled
	}
	declared := o.KubeletVersion
	if declared == "" {
		declared = c.kubeletVersion(ctx)
	}
	pairs := []jsonPair{{"nodeName", nodeName}, {"pool", nodePool}}
	if declared != "" {
		pairs = append(pairs, jsonPair{"kubeletVersion", declared})
	}
	res, err := c.spaceAPI(ctx, cfg, "POST", "/api/clusters/"+clusterID+"/nodes/join-token", compactJSON(pairs...))
	if err != nil {
		return err
	}
	if !is2xx(res.Status) {
		c.nodeAPIError("Failed to mint a join ticket for '"+nodeName+"'", res)
		return ErrHandled
	}

	// A 2xx body is not a promise of JSON — every read is guarded.
	joinCommand, expires, ready := "", "", ""
	if v, ok := parseJSON(res.Body); ok {
		body := envelope(v)
		joinCommand = jstrOr(body, "", "joinCommand")
		expires = scrub(jstrOr(body, "", "expiresAt"))
		// `.ready` WITHOUT a `//` alternative: an absent key reads "null".
		ready = jstr(jget(body, "ready"))
	}
	if joinCommand == "" {
		c.errorf("kubehz: the platform accepted the mint but returned no join command this CLI could read.")
		c.echoErr("  A ticket may have been minted — if so it holds a node slot for about ten")
		c.echoErr("  minutes. Check for it and free the slot if it is there:")
		c.echoErr("    lo kubehz node status")
		c.echoErr("    lo kubehz node remove --name %s", nodeName)
		return ErrHandled
	}
	if err := c.AssertJoinCommand(joinCommand); err != nil {
		c.mintedSlotNote(nodeName)
		return err
	}
	if ready == "false" {
		c.warnf("kubehz: the platform has not armed this ticket yet — the join may time out.")
		c.warnf("Mint again in a minute if it does.")
	}

	joinArgv := strings.Fields(joinCommand)
	if o.NodeIP != "" {
		joinArgv = append(joinArgv, "--node-ip", o.NodeIP)
	}
	if o.PrintOnly {
		if expires == "" {
			expires = "the ticket expires"
		}
		c.echo("")
		c.echo("  Node '%s' joins pool '%s' of cluster %s.", nodeName, nodePool, clusterID)
		c.echo("  Run this as root on that machine before %s:", expires)
		c.echo("")
		c.echo("    %s", strings.Join(joinArgv, " "))
		c.echo("")
		c.echo("  The ticket is single use and lasts ten minutes. It already holds a node")
		c.echo("  slot. Mint a fresh one with the same command when it expires.")
		return nil
	}

	c.echo("kubehz: joining '%s' to pool '%s' of cluster %s", nodeName, nodePool, clusterID)
	// The ticket is live and holds a slot from here until the join lands: a
	// Ctrl-C mid-join must not leave the caller unaware (bash: trap INT TERM).
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			c.echoErr("")
			c.warnf("kubehz: interrupted after the ticket was minted — it holds a node slot for about ten minutes.")
			c.warnf("Free the slot if you are not retrying:  lo kubehz node remove --name %s", nodeName)
		case <-done:
		}
	}()
	// argv, never a shell: the line was checked above and is passed word by word.
	runErr := c.Runner.Run(ctx, execx.Cmd{Name: joinArgv[0], Args: joinArgv[1:], Stdout: c.out(), Stderr: c.errOut()})
	close(done)
	signal.Stop(sigs)
	if runErr != nil {
		c.errorf("kubehz: kubeadm join failed — read its output above.")
		c.echoErr("  The node keeps its slot until you remove it:")
		c.echoErr("    lo kubehz node remove --name %s", nodeName)
		c.echoErr("  Run 'kubeadm reset' on this machine before you join it again.")
		return ErrHandled
	}
	c.echo("")
	c.echo("kubehz: node '%s' joined cluster %s.", nodeName, clusterID)
	c.echo("  It reports Ready within a few minutes. Watch it with:")
	c.echo("    lo kubehz node status")
	return nil
}

// NodeRemove ports node::remove: take one node out of a hosted cluster.
func (c *Context) NodeRemove(ctx context.Context, domain string, o NodeOpts) error {
	if err := c.assertNodeName(o.Name); err != nil {
		return err
	}
	cfg, clusterID, err := c.nodePreflight(ctx, domain, "remove a node", o.ClusterID)
	if err != nil {
		return err
	}
	res, err := c.spaceAPI(ctx, cfg, "DELETE", "/api/clusters/"+clusterID+"/nodes/"+o.Name, nil)
	if err != nil {
		return err
	}
	if !is2xx(res.Status) {
		if res.Status == 404 {
			c.errorf("kubehz: cluster %s holds no node named '%s'.", clusterID, o.Name)
			c.echoErr("  List the nodes with:  lo kubehz node status")
			return ErrHandled
		}
		c.nodeAPIError("Failed to remove node '"+o.Name+"'", res)
		return ErrHandled
	}
	removedPool := ""
	if v, ok := parseJSON(res.Body); ok {
		removedPool = scrub(jstrOr(envelope(v), "", "pool"))
	}
	c.echo("kubehz: node '%s' is draining%s.", o.Name, optSuffix(" (pool ", removedPool)+optIf(removedPool != "", ")"))
	c.echo("  The platform cordons the node, then deletes it from the cluster.")
	c.echo("  Its slot is free now, so another machine can take it.")
	c.echo("  Pods on the machine are NOT evicted first — the machine is yours, and")
	c.echo("  kubehz takes the membership, never the hardware.")
	c.echo("  Run 'kubeadm reset' on the machine before you join it anywhere again.")
	return nil
}

func optIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

// NodeStatus ports node::status: the nodes brought to a hosted cluster.
func (c *Context) NodeStatus(ctx context.Context, domain string, o NodeOpts) error {
	cfg, clusterID, err := c.nodePreflight(ctx, domain, "list nodes", o.ClusterID)
	if err != nil {
		return err
	}
	res, err := c.spaceAPI(ctx, cfg, "GET", "/api/clusters/"+clusterID+"/nodes", nil)
	if err != nil {
		return err
	}
	if !is2xx(res.Status) {
		c.nodeAPIError("Failed to list the nodes of "+clusterID, res)
		return ErrHandled
	}
	used, max, discoveryReady := "0", "-", "false"
	var nodes []any
	if v, ok := parseJSON(res.Body); ok {
		body := envelope(v)
		used = jstrOr(body, "0", "usage", "nodes")
		max = jstrOr(body, "-", "usage", "maxStaticNodes")
		discoveryReady = jstrOr(body, "false", "discoveryReady")
		nodes, _ = jget(body, "nodes").([]any)
	}
	used, max = scrub(used), scrub(max)

	c.echo("Cluster: %s (%s)", clusterID, domain)
	c.echo("Nodes:   %s/%s", used, max)

	if len(nodes) > 0 {
		c.echo("")
		fmt.Fprintf(c.out(), "  %-24s %-16s %-10s %s\n", "NAME", "POOL", "STATUS", "JOINED")
		for _, n := range nodes {
			name := scrub(jstr(jget(n, "name")))
			if name == "" {
				continue
			}
			fmt.Fprintf(c.out(), "  %-24s %-16s %-10s %s\n", name,
				scrub(jstrOr(n, "-", "pool")), scrub(jstrOr(n, "-", "status")), scrub(jstrOr(n, "-", "joinedAt")))
		}
	} else {
		c.echo("")
		c.echo("  No nodes yet. Join this machine with:  lo kubehz node join --pool <pool-name>")
	}
	if discoveryReady != "true" {
		c.echo("")
		c.warnf("The control plane has not published its join address yet — a join is refused until it does.")
		c.warnf("The platform publishes it after the cluster declares its first pool with kind: static.")
	}
	return nil
}
