package kubehz

// node.go — libs/kubehz/node: machines you bring, joined to a kubehz-HOSTED
// control plane (static pools). The platform composes the whole
// `kubeadm join` line; this file mints the ticket, checks the line, and
// runs it as a kubeadm join config in a private file, so the bootstrap
// token is on no argv. It never builds a join command of its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/interrupt"
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
	// The check of http::credential_ok, before any request.
	if !c.credentialOK("KUBEHZ_TOKEN") {
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
	case "NODE_NOT_REMOVED":
		// A 503 of the remove: the node is still in the cluster, and the
		// same call can succeed after the wait of the body.
		c.apiErrorLine(what, res)
		c.echoErr("  The node is still in the cluster. Run this command again %s.", retryWait(res.Body))
	case "NODE_REMOVE_RESULT_UNKNOWN":
		// A 503 of the remove: the platform does not know if the removal
		// happened, so the node list comes before a retry.
		c.apiErrorLine(what, res)
		c.echoErr("  Read the node list before a retry:  lo kubehz node status")
		c.echoErr("  If the node is still there, run this command again %s.", retryWait(res.Body))
	case "JOIN_TICKET_BUSY":
		// A 503 of a join mint or a node remove: other join changes of the
		// account hold the lock of the platform, and the same call can
		// succeed after the wait of the body.
		c.apiErrorLine(what, res)
		c.echoErr("  The platform is busy with other join changes of this account. Run this command again %s.", retryWait(res.Body))
	case "JOIN_TICKET_NOT_MINTED":
		// A 503 of a join mint: the platform minted no ticket, and the
		// same call can succeed after the wait of the body.
		c.apiErrorLine(what, res)
		c.echoErr("  No join ticket was minted for this node. Run this command again %s.", retryWait(res.Body))
	case "JOIN_TICKET_RESULT_UNKNOWN":
		// A 503 of a join mint: the platform does not know if it kept the
		// ticket. A ticket can exist that lo did not get. The next mint
		// for the name revokes it, so the text says both.
		c.apiErrorLine(what, res)
		c.echoErr("  The platform did not confirm the join ticket. A ticket for this node can exist, but lo did not get it.")
		c.echoErr("  Run this command again %s. The next mint for this node name revokes that ticket, if the platform kept it.", retryWait(res.Body))
	default:
		c.spaceAPIError(what, res)
	}
}

// retryWaitMaxSeconds is the longest wait that retryWait names: an hour.
const retryWaitMaxSeconds = 3600

// retryWait is the wait of a refusal body for a help line: "in N seconds"
// ("in 1 second") from `.data.retryAfterSeconds // .retryAfterSeconds`,
// when that is a JSON number in digits only, from 1 to
// retryWaitMaxSeconds. Else it is "shortly": a string, a fraction, 0 or a
// number above an hour names no number. Atoi and the range check are
// enough here: a JSON number has no "+" and no leading zero, a minus sign
// gives a number below 1, and Atoi refuses a fraction, an exponent and a
// number that does not fit. The bash twin is node::retry_wait.
func retryWait(body []byte) string {
	v, ok := parseJSON(body)
	if !ok {
		return "shortly"
	}
	n, ok := jalt(nil, jget(v, "data", "retryAfterSeconds"), jget(v, "retryAfterSeconds")).(json.Number)
	if !ok {
		return "shortly"
	}
	s, err := strconv.Atoi(string(n))
	switch {
	case err != nil || s < 1 || s > retryWaitMaxSeconds:
		return "shortly"
	case s == 1:
		return "in 1 second"
	}
	return "in " + strconv.Itoa(s) + " seconds"
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
// "" when there is none to ask. A node join asks while it holds the
// signals (holdJoinSignals): a signal ends ctx, and the runner stops the
// kubelet as it stops kubeadm (a SIGINT, then a SIGKILL after
// joinStopDelay). Without the bound, a kubelet that ignores the SIGINT
// would keep the join waiting, and a second signal could not end it.
func (c *Context) kubeletVersion(ctx context.Context) string {
	if !c.lookPath("kubelet") {
		return ""
	}
	var out bytes.Buffer
	err := c.Runner.Run(ctx, execx.Cmd{
		Name: "kubelet", Args: []string{"--version"},
		Stdout: &out, Stderr: io.Discard,
		WaitDelay: joinStopDelay,
	})
	if err != nil {
		return ""
	}
	m := kubeletVersionRe.FindStringSubmatch(execx.TrimNewlines(out.String()))
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
	// joinAlphabetRe: the words of the join alphabet, with one space
	// between two words (no tab, no other control character).
	joinAlphabetRe = regexp.MustCompile(`^[A-Za-z0-9._:/=+-]+( [A-Za-z0-9._:/=+-]+)*$`)
	joinEndpointRe = regexp.MustCompile(`^[A-Za-z0-9.-]+:[0-9]{1,5}$`)
)

// isJoinControl reports a control character: U+0000 to U+001F and U+007F.
func isJoinControl(r rune) bool { return r < 0x20 || r == 0x7f }

// AssertJoinCommand ports node::assert_join_command: refuse a join line
// whose SEMANTICS this CLI does not recognise — the alphabet check keeps a
// shell metacharacter out, the structural allowlist keeps a dangerous
// kubeadm FLAG out, and at least one CA fingerprint is required. No
// refusal prints a word of the line: a word can hold the ticket, so a
// refusal names the number of the word (the first word is 1).
func (c *Context) AssertJoinCommand(line string) error {
	// A control character first: the bash twin refuses it as it reads the
	// response (NODE_JOIN_LINE_JQ), before any other check, with this text.
	if strings.ContainsFunc(line, isJoinControl) {
		c.errorf("kubehz: the join command carries characters this CLI does not run.")
		c.echoErr("  Nothing ran on this machine. Print the command and read it:  --print-only")
		return ErrHandled
	}
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
			c.errorf("kubehz: word %d of the join command is a flag this CLI will not run.", i+1)
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
	// kubeadm join has no --node-ip flag: only the join config sets the
	// node-ip of the kubelet, and --print-only prints a line, not a config.
	if o.PrintOnly && o.NodeIP != "" {
		c.errorf("kubehz: --print-only cannot carry --node-ip: kubeadm join has no such flag.")
		c.echoErr("  Nothing was minted. Run it without --print-only: the join config then sets the node-ip of the kubelet.")
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
	// The ticket holds a slot from the mint until the join lands. The hold
	// comes before the mint (bash: the traps of node::join, set before its
	// mint): a signal during the mint or just after it ends ctx, and the
	// join ends with a note. Then kubeadm stops or does not start, the
	// config is removed, and the slot note goes out.
	ctx, release := holdJoinSignals(ctx)
	defer release()
	ticket, err := c.mintTicket(ctx, cfg, clusterID, nodeName, nodePool, o.KubeletVersion)
	if err != nil {
		return err
	}
	if c.joinMinted != nil {
		c.joinMinted()
	}
	// The strict read of the line, before --print-only too: the same
	// verdicts. An empty version checks the line and makes no config.
	if _, err := c.joinConfig(ticket.joinCommand, "", ""); err != nil {
		c.mintedSlotNote(nodeName)
		return err
	}
	// A signal so far: nothing is printed, and nothing is made.
	if ctx.Err() != nil {
		c.interruptedSlotNote(nodeName)
		return ErrHandled
	}
	if o.PrintOnly {
		c.printJoin(nodeName, nodePool, clusterID, ticket.expires, ticket.joinCommand)
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
	// joinPinRe is the shape of a CA pin of the join line: sha256: and 64
	// hex digits, either case (the bash twin's pin_re).
	joinPinRe = regexp.MustCompile(`^sha256:[0-9A-Fa-f]{64}$`)
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
// (node::join_config) prints the same bytes. With an empty apiVersion it
// checks the line and returns no config (the check before --print-only
// prints the line). No refusal prints a word of the line (see
// AssertJoinCommand).
func (c *Context) joinConfig(line, apiVersion, nodeIP string) (string, error) {
	refuse := func(format string, a ...any) (string, error) {
		c.errorf(format, a...)
		c.echoErr("  Nothing ran on this machine. Print the command and read it:  --print-only")
		return "", ErrHandled
	}
	tok := strings.Fields(line)
	// AssertJoinCommand passed the line, so it has three words or more; a
	// shorter line carries no token, as the bash twin reads it.
	endpoint := ""
	if len(tok) > 2 {
		endpoint = tok[2]
	}
	var token, name string
	var hashes []string
	for i := 3; i < len(tok); i++ {
		at := i + 1 // the number of the word of the flag
		flag, value, inline := strings.Cut(tok[i], "=")
		if !strings.HasPrefix(flag, "--") {
			return refuse("kubehz: word %d of the join command follows no flag.", at)
		}
		if !inline && i+1 < len(tok) {
			i++
			value = tok[i]
		}
		if value == "" || strings.HasPrefix(value, "-") {
			return refuse("kubehz: word %d of the join command is a flag without a value.", at)
		}
		switch flag {
		case "--token":
			if token != "" {
				return refuse("kubehz: the join command must carry exactly one bootstrap token (--token).")
			}
			token = value
		case "--discovery-token-ca-cert-hash":
			if !joinPinRe.MatchString(value) {
				return refuse("kubehz: word %d of the join command is a CA pin that is not sha256: and 64 hex digits.", at)
			}
			hashes = append(hashes, value)
		case "--node-name":
			if name != "" {
				return refuse("kubehz: the join command names the node more than once (--node-name).")
			}
			name = value
		default:
			return refuse("kubehz: word %d of the join command is a flag this CLI will not run.", at)
		}
	}
	if token == "" {
		return refuse("kubehz: the join command must carry exactly one bootstrap token (--token).")
	}
	if !joinTokenRe.MatchString(token) {
		return refuse("kubehz: the bootstrap token of the join command does not have the kubeadm token shape.")
	}
	if len(hashes) == 0 {
		return refuse("kubehz: the join command pins no CA fingerprint, so a node could not verify the control plane.")
	}
	if apiVersion == "" {
		return "", nil
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

// removeJoinConfig removes the join config at path, then its private
// directory (removeJoinFile, as node::remove_join_config): a directory
// that is not empty stays, and the warning names it. runJoin runs it on
// each way out: a join that landed, a join that failed, and a join that a
// signal stopped (the signal ends the context, kubeadm stops, and the run
// returns).
func (c *Context) removeJoinConfig(path string) {
	dir := filepath.Dir(path)
	// The text of the bash twin (node::remove_join_config), which has no
	// cause to name.
	if err := c.removeJoinFile(dir, path); err != nil {
		c.warnf("kubehz: could not remove %s", dir)
		c.warnf("It holds the join ticket until the ticket expires. Delete that directory.")
	}
}

// mintedTicket is a minted join ticket as the platform returned it.
type mintedTicket struct {
	joinCommand string
	expires     string
}

// mintMaxTime bounds the mint request of a node join, the answer
// included: the bound of curl --max-time in the bash twin
// (kubehz::space_api_mint_max_time). With the dial bound (30 s) and the
// response header bound (2 minutes) of the client, a mint that gets no
// answer ends. A variable, so that a test can make it short.
var mintMaxTime = 150 * time.Second

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
	// The mint ends within mintMaxTime, the answer included (bash: curl
	// --max-time). The dialer of the client gives up a connect after 30
	// seconds (bash: curl --connect-timeout 30).
	mintCtx, cancel := context.WithTimeout(ctx, mintMaxTime)
	res, err := c.spaceAPI(mintCtx, cfg, "POST", "/api/clusters/"+clusterID+"/nodes/join-token", compactJSON(pairs...))
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			c.interruptedMintNote(nodeName)
		}
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

// printJoin is --print-only: the join command for another machine. The
// line passed the alphabet check: its words are split by one space.
func (c *Context) printJoin(nodeName, nodePool, clusterID, expires, line string) {
	if expires == "" {
		expires = "the ticket expires"
	}
	c.echo("")
	c.echo("  Node '%s' joins pool '%s' of cluster %s.", nodeName, nodePool, clusterID)
	c.echo("  Run this as root on that machine before %s:", expires)
	c.echo("")
	c.echo("    %s", line)
	c.echo("")
	c.echo("  The ticket is single use and lasts ten minutes. It already holds a node")
	c.echo("  slot. Mint a fresh one with the same command when it expires.")
}

// joinStopDelay bounds the stop of an interrupted kubeadm join: the runner
// sends it a SIGINT when ctx ends, and a SIGKILL after this delay. A
// variable, so that a test can make it short.
var joinStopDelay = 10 * time.Second

// holdJoinSignals holds SIGINT, SIGTERM and SIGHUP for a node join: from
// before the mint until release, which NodeJoin defers, so that it runs
// after runJoin removed the config. A signal cancels the returned context:
// SIGINT and SIGTERM also through the Watch of main (cli.WatchInterrupt),
// a SIGHUP also through CatchHangup (which ends the command context too,
// so main ends lo with the SIGHUP). The Watch and the catch end with their
// first signal: a second one would take its default action, and end lo
// before the config is removed and before WaitDelay stops a kubeadm that
// ignores the first signal. The hold takes each later signal and only
// cancels the context again. Thus a second Ctrl-C does not end lo during a
// join: the join ends within joinStopDelay and the removal of the config,
// and SIGKILL stays the way to force it. A signal that is ignored when the
// hold starts (nohup lo …) stays ignored: signal.Notify would undo the
// ignore.
func holdJoinSignals(ctx context.Context) (context.Context, func()) {
	ctx, stopHangup := interrupt.CatchHangup(ctx)
	ctx, cancel := context.WithCancel(ctx)
	var held []os.Signal
	for _, s := range []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		if !signal.Ignored(s) {
			held = append(held, s)
		}
	}
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	finished := make(chan struct{})
	if len(held) > 0 {
		signal.Notify(ch, held...)
	}
	go func() {
		defer close(finished)
		for {
			select {
			case <-ch:
				cancel()
			case <-done:
				return
			}
		}
	}()
	return ctx, func() {
		// The hold ends first: a SIGHUP after it still reaches the catch,
		// which records it for main.
		signal.Stop(ch)
		close(done)
		<-finished
		stopHangup()
		cancel()
	}
}

// interruptedMintNote ports node::interrupted_mint_note: a signal stopped
// the mint request, and the platform may have minted the ticket.
func (c *Context) interruptedMintNote(nodeName string) {
	c.echoErr("")
	c.warnf("kubehz: interrupted during the mint. If the platform minted the ticket, it holds a node slot for about ten minutes.")
	c.warnf("Look for it with 'lo kubehz node status', and free it with:  lo kubehz node remove --name %s", nodeName)
}

// interruptedSlotNote ports node::interrupted_slot_note: a signal stopped
// the join after the mint, and the ticket holds a slot.
func (c *Context) interruptedSlotNote(nodeName string) {
	c.echoErr("")
	c.warnf("kubehz: interrupted after the ticket was minted — it holds a node slot for about ten minutes.")
	c.warnf("Free the slot if you are not retrying:  lo kubehz node remove --name %s", nodeName)
}

// runJoin runs kubeadm join here with the join config at configPath, and
// removes the config on each way out, before it prints the result. When
// ctx ends (a signal, see NodeJoin), kubeadm does not start, or the
// runner stops it (a SIGINT, then a SIGKILL after joinStopDelay); the
// slot note then goes out. A kubeadm that ended with 0, before the signal
// or after the stop signal, counts as a join that landed (as in the bash
// twin): the success text goes out, and main ends lo with 128 + the
// signal.
func (c *Context) runJoin(ctx context.Context, nodeName, nodePool, clusterID, configPath string) error {
	removed := false
	remove := func() {
		if !removed {
			removed = true
			c.removeJoinConfig(configPath)
		}
	}
	defer remove()
	if ctx.Err() != nil {
		remove()
		c.interruptedSlotNote(nodeName)
		return ErrHandled
	}
	c.echo("kubehz: joining '%s' to pool '%s' of cluster %s", nodeName, nodePool, clusterID)
	// argv, never a shell. The token is in the config file, not on argv.
	var state *os.ProcessState
	runErr := c.Runner.Run(ctx, execx.Cmd{
		Name:      "kubeadm",
		Args:      []string{"join", "--config", configPath},
		Stdout:    c.out(),
		Stderr:    c.errOut(),
		WaitDelay: joinStopDelay,
		Exited:    func(s *os.ProcessState) { state = s },
	})
	remove()
	// A kubeadm that ended with 0 is a join that landed, also when a
	// signal came: the runner then returns the error of ctx, and the state
	// of kubeadm tells (bash: the status that node::_join_stop reads).
	if runErr != nil && state != nil && state.Success() {
		runErr = nil
	}
	if runErr != nil && ctx.Err() != nil {
		c.interruptedSlotNote(nodeName)
		return ErrHandled
	}
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
