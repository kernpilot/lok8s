package kubehz

// node.go — libs/kubehz/node: machines you bring, joined to a kubehz-HOSTED
// control plane (static pools). The platform composes the whole
// `kubeadm join` line; this file mints the ticket, checks the line, and
// runs it as a kubeadm join config in a private file, so the bootstrap
// token is on no argv. It never builds a join command of its own.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/ui"
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

// urlIDRe is the shape of an id that is pasted into a URL PATH (a cluster
// id, a space id): a `../` in it would address a different route. The bash
// twin is node::url_id.
var urlIDRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// nodeDomainConfig ports node::domain_config, the first half of the two node
// preflights (nodePreflight, spacePreflight): the active domain, its spec
// file and its kubehz block. It returns the config and the spec path.
func (c *Context) nodeDomainConfig(domain string) (*Config, string, error) {
	if domain == "" {
		c.errorf("No active domain. Use: lo use <domain>")
		return nil, "", ErrHandled
	}
	cy := c.clusterYAMLPath(domain)
	if !fsutil.FileExists(cy) {
		c.errorf("No cluster.lok8s.yaml for domain: %s", domain)
		return nil, "", ErrHandled
	}
	cfg, err := c.ReadConfig(cy)
	if err != nil {
		return nil, "", err
	}
	return cfg, cy, nil
}

// nodeAPIAccess ports node::api_access, the part of the two node preflights
// after the hosting check: the api URL, over https only (the tenant bearer
// travels on it), and the tenant token, with no control character in it.
func (c *Context) nodeAPIAccess(cfg *Config, domain, action string) error {
	if cfg.APIURL == "" {
		c.errorf("spec.kubehz.apiUrl is not set for %s", domain)
		return ErrHandled
	}
	if err := c.requireHTTPS(cfg.APIURL, "spec.kubehz.apiUrl"); err != nil {
		return err
	}
	token := c.getenv("KUBEHZ_TOKEN")
	if token == "" {
		c.errorf("KUBEHZ_TOKEN is required to %s.", action)
		c.echoErr("  Mint a clusters:write API token in the dashboard (Access -> API Tokens).")
		return ErrHandled
	}
	// The check of http::credential_ok, before any request. The bash twin
	// puts the bearer on a curl config line, and a control character ends
	// that line. The Go client alone fails on a CR as on a transport error
	// ("unreachable"), and it sends a tab.
	if hasControl(token) {
		c.errorf("environment variable KUBEHZ_TOKEN must not contain a control character")
		c.echoErr("  A file with CRLF line ends can leave a CR at the end of the value. Remove it, then try again.")
		return ErrHandled
	}
	return nil
}

// nodePreflight ports node::preflight: the active domain's kubehz block, the
// api URL, the tenant token, and the platform cluster id.
func (c *Context) nodePreflight(ctx context.Context, domain, action, clusterIDFlag string) (*Config, string, error) {
	cfg, _, err := c.nodeDomainConfig(domain)
	if err != nil {
		return nil, "", err
	}
	switch cfg.Hosting {
	case "hosted":
	case "shared":
		c.errorf("kubehz: '%s' is hosting: shared — its nodes join a Space.", domain)
		c.echoErr("  Mint a Space join ticket with:  lo kubehz join <node-name>")
		c.echoErr("  Read the nodes of the Space and their maintenance phase with:  lo kubehz status")
		return nil, "", ErrHandled
	default:
		c.errorf("kubehz: '%s' is hosting: %s — it runs its own control plane.", domain, cfg.Hosting)
		c.echoErr("  Join nodes with kubeadm against your own API server.")
		return nil, "", ErrHandled
	}
	if err := c.nodeAPIAccess(cfg, domain, action); err != nil {
		return nil, "", err
	}
	apiURL := cfg.APIURL
	// An explicit --cluster-id wins. The value is pasted into a URL PATH.
	if clusterIDFlag != "" {
		if !urlIDRe.MatchString(clusterIDFlag) {
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

// scrub drops each character of ui.Unshown from a server string before it
// is shown, except tab and newline: a status reason or a detail can have
// more than one line. Thus an ESC, a C1 CSI (U+009B), a bidi override or a
// line separator from the api cannot reach the terminal. cleanText (the
// api records) drops the same set, tab and newline too. The bash twin is
// kubehz::scrub (libs/kubehz/shared).
func scrub(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\t' && r != '\n' && ui.Unshown(r) {
			return -1
		}
		return r
	}, s)
}

// nodeAPIError ports node::api_error: the api's refusal codes, each an
// instruction.
func (c *Context) nodeAPIError(what string, res *httpResult) {
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
		c.spaceAPIError(what, res)
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
	name = execx.TrimNewlines(name)
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
	m := kubeletVersionRe.FindStringSubmatch(execx.TrimNewlines(out))
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
		allowed := slices.Contains(nodeJoinAllowedFlags, flag)
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
	// The address goes into the join config: its shape is checked before
	// the api is asked anything.
	if o.NodeIP != "" && !nodeIPRe.MatchString(o.NodeIP) {
		c.errorf("kubehz: --node-ip must be one IP address, or an IPv4 and an IPv6 address with a comma: %s", clip(scrub(o.NodeIP)))
		c.echoErr("  Example:  --node-ip 203.0.113.7")
		return ErrHandled
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
	// Before the mint: the config version that this kubeadm reads, and a
	// temp dir that passes the check (joinTempDir printed why). A refusal
	// here costs no ticket. The writer checks the temp dir again: it can
	// change in between.
	configVersion := ""
	if !o.PrintOnly {
		if configVersion, err = c.joinConfigVersion(ctx); err != nil {
			return err
		}
		if _, err := c.joinTempDir(); err != nil {
			if !errors.Is(err, errTmpNotPrivate) {
				c.errorf("kubehz: the temp dir (TMPDIR) cannot be resolved, so the join config cannot be written.")
				c.echoErr("  Set TMPDIR to a directory of your own that only you can write (mode 0700).")
			}
			return ErrHandled
		}
	}
	ticket, err := c.mintTicket(ctx, cfg, clusterID, nodeName, nodePool, o.KubeletVersion)
	if err != nil {
		return err
	}
	if o.PrintOnly {
		joinArgv := strings.Fields(ticket.joinCommand)
		if o.NodeIP != "" {
			joinArgv = append(joinArgv, "--node-ip", o.NodeIP)
		}
		c.printJoin(nodeName, nodePool, clusterID, ticket.expires, joinArgv)
		return nil
	}
	config, err := c.joinConfig(ticket.joinCommand, configVersion, o.NodeIP)
	if err != nil {
		c.mintedSlotNote(nodeName)
		return err
	}
	path, err := c.writeJoinFile(nodeName, ".yaml", config)
	if err != nil {
		return c.joinConfigWriteFailed(nodeName, err)
	}
	return c.runJoin(ctx, nodeName, nodePool, clusterID, path)
}

var (
	// nodeIPRe is the shape of --node-ip: one address, or an IPv4 and an
	// IPv6 address with a comma (the kubelet checks the addresses).
	nodeIPRe = regexp.MustCompile(`^[0-9A-Fa-f.:]+(,[0-9A-Fa-f.:]+)?$`)
	// kubeadmVersionRe reads `kubeadm version -o short` (v1.36.0); group 1
	// is the minor version.
	kubeadmVersionRe = regexp.MustCompile(`^v1\.([0-9]{1,3})\.[0-9]+([-+][0-9A-Za-z.+-]*)?$`)
)

// joinConfigVersion returns the JoinConfiguration API version that the
// kubeadm on this machine reads: v1beta3 for kubeadm 1.22 to 1.30, v1beta4
// from 1.31 on (kubeadm 1.31 and later read v1beta3 too, with a deprecation
// warning). Another answer, or none, is refused before the mint.
func (c *Context) joinConfigVersion(ctx context.Context) (string, error) {
	out, err := c.capture(ctx, true, "kubeadm", "version", "-o", "short")
	if err != nil {
		out = ""
	}
	v := execx.TrimNewlines(out)
	if m := kubeadmVersionRe.FindStringSubmatch(v); m != nil {
		minor, _ := strconv.Atoi(m[1])
		switch {
		case minor >= 31:
			return "v1beta4", nil
		case minor >= 22:
			return "v1beta3", nil
		}
	}
	c.errorf("kubehz: this CLI cannot write a join config for the kubeadm on this machine: '%s'", clip(scrub(v)))
	c.echoErr("  It supports kubeadm 1.22 and later. Nothing was minted.")
	c.echoErr("  Check the output of:  kubeadm version -o short")
	return "", ErrHandled
}

// joinConfig reads the join line that AssertJoinCommand passed into a
// kubeadm JoinConfiguration of the API version apiVersion: the endpoint,
// the one bootstrap token, the CA pins and the node name of the line, and
// nodeIP as the kubelet's node-ip. kubeadm takes the TLS bootstrap token
// from the bootstrap token (its defaults, in both versions), as it does for
// --token. The values come from the join alphabet (no quote, no backslash),
// so a double-quoted YAML scalar holds each of them as it is. The bash twin
// (node::join_config) prints the same bytes.
func (c *Context) joinConfig(line, apiVersion, nodeIP string) (string, error) {
	refuse := func(format string, a ...any) (string, error) {
		c.errorf(format, a...)
		c.echoErr("  Nothing ran on this machine. Print the command and read it:  --print-only")
		return "", ErrHandled
	}
	tok := strings.Fields(line)
	endpoint := tok[2]
	var token, name string
	var hashes []string
	for i := 3; i < len(tok); i++ {
		flag, value, inline := strings.Cut(tok[i], "=")
		if !strings.HasPrefix(flag, "--") {
			return refuse("kubehz: the join command has a word that follows no flag.")
		}
		if !inline && i+1 < len(tok) {
			i++
			value = tok[i]
		}
		if value == "" || strings.HasPrefix(value, "-") {
			return refuse("kubehz: the join command has a flag without a value: %s", flag)
		}
		switch flag {
		case "--token":
			if token != "" {
				return refuse("kubehz: the join command must carry exactly one bootstrap token (--token).")
			}
			token = value
		case "--discovery-token-ca-cert-hash":
			hashes = append(hashes, value)
		case "--node-name":
			if name != "" {
				return refuse("kubehz: the join command names the node more than once (--node-name).")
			}
			name = value
		default:
			return refuse("kubehz: the platform sent a join flag this CLI will not run: %s", flag)
		}
	}
	if token == "" {
		return refuse("kubehz: the join command must carry exactly one bootstrap token (--token).")
	}
	if !joinTokenRe.MatchString(token) {
		return refuse("kubehz: the bootstrap token of the join command does not have the kubeadm token shape.")
	}
	var b strings.Builder
	b.WriteString("apiVersion: kubeadm.k8s.io/" + apiVersion + "\n")
	b.WriteString("kind: JoinConfiguration\n")
	b.WriteString("discovery:\n")
	b.WriteString("  bootstrapToken:\n")
	b.WriteString("    apiServerEndpoint: \"" + endpoint + "\"\n")
	b.WriteString("    token: \"" + token + "\"\n")
	b.WriteString("    caCertHashes:\n")
	for _, h := range hashes {
		b.WriteString("      - \"" + h + "\"\n")
	}
	if name != "" || nodeIP != "" {
		b.WriteString("nodeRegistration:\n")
	}
	if name != "" {
		b.WriteString("  name: \"" + name + "\"\n")
	}
	if nodeIP != "" {
		b.WriteString("  kubeletExtraArgs:\n")
		// v1beta3: a map; v1beta4: a list of name/value pairs.
		if apiVersion == "v1beta3" {
			b.WriteString("    node-ip: \"" + nodeIP + "\"\n")
		} else {
			b.WriteString("    - name: \"node-ip\"\n")
			b.WriteString("      value: \"" + nodeIP + "\"\n")
		}
	}
	return b.String(), nil
}

// joinConfigWriteFailed reports a failed write of the join config. The
// mint happened, so the ticket holds a node slot (mintedSlotNote). An
// ErrHandled error (a refused node name) printed its own message, and
// errTmpNotPrivate printed why (joinTempDir).
func (c *Context) joinConfigWriteFailed(nodeName string, err error) error {
	switch {
	case errors.Is(err, ErrHandled):
	case errors.Is(err, errTmpNotPrivate):
		c.errorf("kubehz: could not write the join config for '%s'", nodeName)
	default:
		c.errorf("kubehz: could not write the join config for '%s': %v", nodeName, err)
	}
	if left, ok := errors.AsType[*joinLeftError](err); ok {
		c.echoErr("  The cleanup failed too: %s may still hold the join ticket. Delete that directory.", left.dir)
	}
	c.mintedSlotNote(nodeName)
	return ErrHandled
}

// removeJoinConfig removes the private directory of the join config at
// path. runJoin defers it, so it runs on each way out: a join that landed,
// a join that failed, and a join that a signal stopped (the signal cancels
// the context, kubeadm stops, and the run returns).
func (c *Context) removeJoinConfig(path string) {
	removeAll := c.RemoveAll
	if removeAll == nil {
		removeAll = os.RemoveAll
	}
	dir := filepath.Dir(path)
	// The text of the bash twin (node::remove_join_config), which has no
	// cause to name.
	if err := removeAll(dir); err != nil {
		c.warnf("kubehz: could not remove %s", dir)
		c.warnf("It holds the join ticket until the ticket expires. Delete that directory.")
	}
}

// mintedTicket is a minted join ticket as the platform returned it.
type mintedTicket struct {
	joinCommand string
	expires     string
}

// mintTicket asks the platform for a join ticket and checks the join
// command it returned. A 2xx body is not a promise of JSON — every read
// is guarded.
func (c *Context) mintTicket(ctx context.Context, cfg *Config, clusterID, nodeName, nodePool, kubeletVersion string) (mintedTicket, error) {
	declared := kubeletVersion
	if declared == "" {
		declared = c.kubeletVersion(ctx)
	}
	pairs := []jsonPair{{"nodeName", nodeName}, {"pool", nodePool}}
	if declared != "" {
		pairs = append(pairs, jsonPair{"kubeletVersion", declared})
	}
	res, err := c.spaceAPI(ctx, cfg, "POST", "/api/clusters/"+clusterID+"/nodes/join-token", compactJSON(pairs...))
	if err != nil {
		return mintedTicket{}, err
	}
	if !is2xx(res.Status) {
		c.nodeAPIError("Failed to mint a join ticket for '"+nodeName+"'", res)
		return mintedTicket{}, ErrHandled
	}

	var t mintedTicket
	ready := ""
	if v, ok := parseJSON(res.Body); ok {
		body := envelope(v)
		t.joinCommand = jstrOr(body, "", "joinCommand")
		t.expires = scrub(jstrOr(body, "", "expiresAt"))
		// `.ready` WITHOUT a `//` alternative: an absent key reads "null".
		ready = jstr(jget(body, "ready"))
	}
	if t.joinCommand == "" {
		c.errorf("kubehz: the platform accepted the mint but returned no join command this CLI could read.")
		c.echoErr("  A ticket may have been minted — if so it holds a node slot for about ten")
		c.echoErr("  minutes. Check for it and free the slot if it is there:")
		c.echoErr("    lo kubehz node status")
		c.echoErr("    lo kubehz node remove --name %s", nodeName)
		return mintedTicket{}, ErrHandled
	}
	if err := c.AssertJoinCommand(t.joinCommand); err != nil {
		c.mintedSlotNote(nodeName)
		return mintedTicket{}, err
	}
	if ready == "false" {
		c.warnf("kubehz: the platform has not armed this ticket yet — the join may time out.")
		c.warnf("Mint again in a minute if it does.")
	}
	return t, nil
}

// printJoin is --print-only: the join command for another machine.
func (c *Context) printJoin(nodeName, nodePool, clusterID, expires string, joinArgv []string) {
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
}

// runJoin runs kubeadm join here with the join config at configPath, and
// removes the config on each way out. The ticket is live and holds a slot
// from here until the join lands: a Ctrl-C mid-join must not leave the
// caller unaware (bash: trap INT TERM HUP). A hangup is caught too: by
// default it would end lo before the config is removed.
func (c *Context) runJoin(ctx context.Context, nodeName, nodePool, clusterID, configPath string) error {
	defer c.removeJoinConfig(configPath)
	c.echo("kubehz: joining '%s' to pool '%s' of cluster %s", nodeName, nodePool, clusterID)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
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
	// argv, never a shell. The token is in the config file, not on argv.
	runErr := c.Runner.Run(ctx, execx.Cmd{Name: "kubeadm", Args: []string{"join", "--config", configPath}, Stdout: c.out(), Stderr: c.errOut()})
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
	used, maxNodes, discoveryReady := "0", "-", "false"
	var nodes []any
	if v, ok := parseJSON(res.Body); ok {
		body := envelope(v)
		used = jstrOr(body, "0", "usage", "nodes")
		maxNodes = jstrOr(body, "-", "usage", "maxStaticNodes")
		discoveryReady = jstrOr(body, "false", "discoveryReady")
		nodes, _ = jget(body, "nodes").([]any)
	}
	used, maxNodes = scrub(used), scrub(maxNodes)

	c.echo("Cluster: %s (%s)", clusterID, domain)
	c.echo("Nodes:   %s/%s", used, maxNodes)

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
