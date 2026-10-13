package lo

// network.go — Docker network lifecycle for Lo clusters
// (.lok8s/drivers/lo/utils/network.sh, minus the heal — see heal.go).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kernpilot/lok8s/internal/ui"
)

// bridgeMaxLen is IFNAMSIZ-1: the longest name a Linux network interface
// can carry (IFNAMSIZ is 16 and counts the NUL terminator).
const bridgeMaxLen = 15

// bridgeName is the host bridge interface for a project network (bash:
// lo::network_bridge_name). A name of at most bridgeMaxLen bytes is used
// as it is, so every existing bridge keeps its name. A longer one becomes
// "lo-<12 hex of its sha256>": Docker hands the option to the kernel
// unchecked, and the kernel answers a 16-byte name with "numerical
// result out of range".
func bridgeName(network string) string {
	if len(network) <= bridgeMaxLen {
		return network
	}
	sum := sha256.Sum256([]byte(network))
	return "lo-" + hex.EncodeToString(sum[:])[:12]
}

// network ensures the project docker bridge exists with the reserved
// dynamic range (bash: lo::network).
func (d *Driver) network(ctx context.Context, errOut io.Writer) error {
	network := getenv("KIND_EXPERIMENTAL_DOCKER_NETWORK")
	subnet := getenv("LOK8S_NETWORK_SUBNET")
	if network == "" {
		fmt.Fprintln(errOut, "error: KIND_EXPERIMENTAL_DOCKER_NETWORK not set (call lo::read_network_config first)")
		return ui.Handled(fmt.Errorf("network name not set"))
	}
	if subnet == "" {
		fmt.Fprintln(errOut, "error: LOK8S_NETWORK_SUBNET not set (call lo::read_network_config first)")
		return ui.Handled(fmt.Errorf("network subnet not set"))
	}

	if d.networkExists(ctx, network) {
		currentSubnet, _ := d.output(ctx, "docker", "network", "inspect", network,
			"--format", "{{range .IPAM.Config}}{{.Subnet}}{{end}}")
		if currentSubnet != subnet {
			_ = d.runQuiet(ctx, "docker", "network", "rm", "-f", network)
		}
	}

	if !d.networkExists(ctx, network) {
		// Reserve the upper quarter for Docker's dynamic allocation (kind
		// nodes), same idea as the shared network's reservation: everything
		// static on the project net — build/cache at .101/.102, non-shared
		// mirrors at .103+, the MetalLB pool at .125-.150 — lives BELOW it,
		// so a node can never squat a registry address or collide with an LB
		// IP after a reboot. Legacy networks (no range) keep working; a
		// squat there fails loudly with the holder named (see registries).
		args := []string{"network", "create", "-d=bridge", "--subnet", subnet}
		if rng, ok := networkDynamicRange(subnet); ok {
			args = append(args, "--ip-range", rng)
		}
		args = append(args,
			"-o", "com.docker.network.bridge.name="+bridgeName(network),
			"-o", "com.docker.network.bridge.enable_ip_masquerade=true",
			"-o", "com.docker.network.bridge.enable_icc=true",
			"-o", "com.docker.network.bridge.host_binding_ipv4=0.0.0.0",
			network)
		return d.run(ctx, "docker", args...)
	}
	return nil
}

func (d *Driver) networkExists(ctx context.Context, network string) bool {
	return d.runQuiet(ctx, "docker", "network", "inspect", network) == nil
}

// networkDynamicRange returns the upper QUARTER of cidr as its own CIDR
// (10.125.125.0/24 → 10.125.125.192/26) — bash: lo::network_dynamic_range.
// The project net needs a smaller dynamic pool than the shared net's upper
// half: its static tenants reach higher (registries .101+, the default
// MetalLB pool up to .150), and ~60 dynamic addresses is far beyond any kind
// cluster's node count.
func networkDynamicRange(cidr string) (string, bool) {
	base, prefixStr, _ := strings.Cut(cidr, "/")
	prefix, err := strconv.Atoi(prefixStr)
	if err != nil || prefix < 1 || prefix > 28 {
		return "", false
	}
	start, ok := ipAdd(base, 3<<(30-prefix))
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s/%d", start, prefix+2), true
}

// registryDynamicRange returns the upper half of cidr as its own CIDR
// (10.125.200.0/24 → 10.125.200.128/25) — bash: lo::registry_dynamic_range.
// Passed as --ip-range so Docker's DYNAMIC allocation (kind nodes attaching
// to the shared network) never hands out the low addresses the registries
// claim statically — without it a node attached before the registries start
// squats their IPs ("Address already in use" on every restart).
func registryDynamicRange(cidr string) (string, bool) {
	base, prefixStr, _ := strings.Cut(cidr, "/")
	prefix, err := strconv.Atoi(prefixStr)
	if err != nil || prefix < 1 || prefix > 30 {
		return "", false
	}
	start, ok := ipAdd(base, 1<<(31-prefix))
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%s/%d", start, prefix+1), true
}

// registryNetworkCreate is the one place the shared network is created
// (bash: lo::registry_network_create). The reserved dynamic range is what
// makes registry-IP squatting IMPOSSIBLE — statics live below it, dynamic
// attachers (kind nodes) can only ever land inside it.
func (d *Driver) registryNetworkCreate(ctx context.Context, network, subnet, dynamicRange string) error {
	args := []string{"network", "create", "-d=bridge", "--subnet", subnet}
	if dynamicRange != "" {
		args = append(args, "--ip-range", dynamicRange)
	}
	args = append(args,
		"-o", "com.docker.network.bridge.enable_ip_masquerade=true",
		"-o", "com.docker.network.bridge.enable_icc=true",
		"--label", "lok8s.registry=shared",
		network)
	return d.run(ctx, "docker", args...)
}

// registryNetwork ensures the shared registry network exists WITH the
// reserved dynamic range (bash: lo::registry_network).
//
// A network created before the range reservation is force-recreated: the
// attached lok8s mirror containers are removed too, so the registries
// reconcile that follows in the same run recreates them at their static
// addresses on the new network; kind nodes are force-detached and re-attach
// via connectNodesToRegistryNetwork (this run for the active cluster, the
// next `lo up` for any other project). No in-place migration choreography —
// the network and its mirrors are cheap, disposable state.
func (d *Driver) registryNetwork(ctx context.Context, errOut io.Writer) error {
	rf, err := regFile()
	if err != nil {
		return err
	}
	network := rf.Network.Name
	subnet := rf.Network.CIDR

	dynamicRange, _ := registryDynamicRange(subnet)

	if d.networkExists(ctx, network) {
		currentSubnet, _ := d.output(ctx, "docker", "network", "inspect", network,
			"--format", "{{range .IPAM.Config}}{{.Subnet}}{{end}}")
		if currentSubnet != subnet {
			fmt.Fprintf(errOut, "error: registry network '%s' exists with subnet %s, expected %s\n", network, currentSubnet, subnet)
			fmt.Fprintln(errOut, "error: run 'lo registry clean --shared' to recreate, or adjust spec.registries.shared.network.cidr")
			return ui.Handled(fmt.Errorf("registry network %s has wrong subnet", network))
		}

		// THE reserved range — nothing to do. A non-empty range that differs
		// (older tooling, a hand-made network) is NOT safe: dynamic
		// allocation could still overlap the statics — recreate it like the
		// no-range case.
		currentRange, _ := d.output(ctx, "docker", "network", "inspect", network,
			"--format", "{{range .IPAM.Config}}{{.IPRange}}{{end}}")
		if currentRange == dynamicRange || dynamicRange == "" {
			return nil
		}

		// Legacy network without the reserved range: recreate it. Remove OUR
		// mirror containers first (a running mirror whose config-hash still
		// matches would otherwise reconcile as "unchanged" while detached
		// from the new network); their named volumes — the cache — survive.
		has := currentRange
		if has == "" {
			has = "none"
		}
		ui.WarnTo(errOut, "lo: registry network '%s' lacks the reserved dynamic range (has '%s') — recreating it with %s (mirrors + nodes re-attach via the normal reconcile)", network, has, dynamicRange)

		// Serialize the recreate across concurrent `lo` runs (the shared
		// network is host-global). Best-effort, same pattern as the registry
		// reconcile: without the lock a loser can rm the WINNER's
		// freshly-created network.
		_ = os.MkdirAll(registryStateDir(), 0o755)
		release, locked := acquireLock(ctx, filepath.Join(registryStateDir(), network+".netlock"), d.sleep)
		if locked {
			// The winner may have finished the recreate while we waited —
			// re-check against the SAME equality as above, not mere
			// non-emptiness.
			currentRange, _ = d.output(ctx, "docker", "network", "inspect", network,
				"--format", "{{range .IPAM.Config}}{{.IPRange}}{{end}}")
			if currentRange == dynamicRange {
				release()
				return nil
			}
		} else {
			ui.DebugTo(errOut, "registry network %s: lock wait timed out, proceeding unlocked", network)
		}
		// From here to the create the lock must stay HELD: releasing it
		// between the rm and the create re-opens the exact window it exists
		// for — a loser's rm retry removing the winner's freshly-created
		// network.
		defer func() {
			if release != nil {
				release()
			}
		}()

		rmOK := true
		if d.networkExists(ctx, network) {
			members, _ := d.output(ctx, "docker", "network", "inspect", network,
				"-f", `{{range .Containers}}{{.Name}}{{"\n"}}{{end}}`)
			for name := range strings.FieldsSeq(members) {
				if strings.HasPrefix(name, SharedRegistryPrefix) {
					// Removed, not detached: a running mirror with a matching
					// config-hash would reconcile "unchanged" while
					// off-network. Cache volumes survive.
					_ = d.runQuiet(ctx, "docker", "rm", "-f", name)
				} else {
					// `docker network rm` REFUSES a network with active
					// endpoints (even with -f, which only suppresses the
					// not-found error — verified live) — every remaining
					// member must be detached explicitly.
					_ = d.runQuiet(ctx, "docker", "network", "disconnect", "-f", network, name)
				}
			}
			// A just-removed container's endpoint can lag its release (the
			// same lag the registry-start retry absorbs) — one bounded retry
			// before giving up. stderr is held back until the retry also
			// fails: a transiently-lagging first attempt is expected, not
			// something to print.
			rmOK = false
			var rmErr string
			for attempt := 1; attempt <= 2; attempt++ {
				out, err := d.errOutput(ctx, "docker", "network", "rm", network)
				if err == nil {
					rmOK = true
					break
				}
				rmErr = out
				if attempt == 1 && d.sleepSeconds(ctx, 1) != nil {
					break
				}
			}
			if !rmOK && rmErr != "" {
				fmt.Fprintln(errOut, rmErr)
			}
		}
		// else: a prior run removed the network but died before recreating
		// it — fall through and create.

		if !rmOK {
			return ui.Handled(fmt.Errorf("could not remove registry network %s", network))
		}
		return d.registryNetworkCreate(ctx, network, subnet, dynamicRange)
	}

	return d.registryNetworkCreate(ctx, network, subnet, dynamicRange)
}

// registryGwPriority is the gateway priority of a kind node's endpoint on
// the shared registry network. kind creates the node on the project network
// with the default priority 0. A container on two networks gets its default
// route from the network with the highest priority; between equal
// priorities Docker takes the network whose name sorts first. Without a
// priority, a project network whose name sorts after "lok8s-registries"
// (for example "ops") lost the default route to the registry network:
// node-to-service traffic left with the registry address, rp_filter dropped
// part of the replies, and the Cilium init container hung on
// https://10.96.0.1:443. With -1 the project network keeps the default
// route for every name.
const registryGwPriority = "-1"

// gwPriorityMinMajor is the first Docker release whose `docker network
// connect` accepts --gw-priority (28.0, API 1.48).
const gwPriorityMinMajor = 28

// gwPrioritySupported reports whether `docker version --format
// '{{.Client.Version}} {{.Server.Version}}'` names a client AND a daemon of
// Docker 28.0 or later: the client parses the flag, the daemon applies it.
// A value that does not parse (another engine, no daemon) reads as false.
func gwPrioritySupported(versions string) bool {
	fields := strings.Fields(versions)
	if len(fields) != 2 {
		return false
	}
	for _, v := range fields {
		major, _, _ := strings.Cut(v, ".")
		n, err := strconv.Atoi(major)
		if err != nil || n < gwPriorityMinMajor {
			return false
		}
	}
	return true
}

// dockerGwPriority is gwPrioritySupported for the docker on PATH, asked once
// per process (bash: lo::docker_gw_priority).
func (d *Driver) dockerGwPriority(ctx context.Context) bool {
	if !d.gwPriority.done {
		versions, err := d.output(ctx, "docker", "version", "--format", "{{.Client.Version}} {{.Server.Version}}")
		d.gwPriority.done = true
		d.gwPriority.ok = err == nil && gwPrioritySupported(versions)
	}
	return d.gwPriority.ok
}

// networkGateway is the first IPv4 gateway of a docker network, "" when
// docker reports none (bash: lo::network_gateway).
func (d *Driver) networkGateway(ctx context.Context, network string) string {
	out, err := d.output(ctx, "docker", "network", "inspect", network,
		"--format", "{{range .IPAM.Config}}{{.Gateway}} {{end}}")
	if err != nil {
		return ""
	}
	for gw := range strings.FieldsSeq(out) {
		if ip := net.ParseIP(gw); ip != nil && ip.To4() != nil {
			return gw
		}
	}
	return ""
}

// routeField returns the word after key in one `ip route` line ("dev",
// "via"), "" when the line does not carry it.
func routeField(line, key string) string {
	f := strings.Fields(line)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == key {
			return f[i+1]
		}
	}
	return ""
}

// defaultRouteRepair decides the default route of a node from two `ip`
// reads in the node: current is `ip -4 route show default`, routeGet is
// `ip -o -4 route get <gw>` for the gateway of the project network. dev is
// the device that reaches gw (the project network's interface); needed
// reports that the default route is not `via <gw> dev <dev>` yet.
func defaultRouteRepair(current, routeGet, gw string) (dev string, needed bool) {
	dev = routeField(routeGet, "dev")
	if dev == "" {
		return "", false
	}
	first, _, _ := strings.Cut(current, "\n")
	return dev, routeField(first, "via") != gw || routeField(first, "dev") != dev
}

// repairDefaultRoute moves the default route of a node back to the project
// network when it goes through another network (bash:
// lo::repair_default_route). It is the fallback for a Docker without
// --gw-priority, and the repair for a node that an older lok8s attached
// without a priority. Docker writes the route again when the node restarts,
// so on such a Docker the next `lo up` repeats the repair. Advisory: a node
// that cannot be read or changed gets a warning, never a failure.
func (d *Driver) repairDefaultRoute(ctx context.Context, node, network, gw string, errOut io.Writer) {
	if gw == "" {
		return
	}
	current, err := d.output(ctx, "docker", "exec", node, "ip", "-4", "route", "show", "default")
	if err != nil {
		return
	}
	routeGet, err := d.output(ctx, "docker", "exec", node, "ip", "-o", "-4", "route", "get", gw)
	if err != nil {
		return
	}
	dev, needed := defaultRouteRepair(current, routeGet, gw)
	if !needed {
		return
	}
	if err := d.runQuiet(ctx, "docker", "exec", node, "ip", "route", "replace", "default", "via", gw, "dev", dev); err != nil {
		ui.WarnTo(errOut, "lo: %s: could not move the default route to %s (via %s dev %s)", node, network, gw, dev)
		return
	}
	ui.WarnTo(errOut, "lo: %s: the default route did not go through %s — moved it there (via %s dev %s)", node, network, gw, dev)
}

// connectNodesToRegistryNetwork attaches the cluster's kind nodes to the
// shared registry network (bash: lo::connect_nodes_to_registry_network).
// Skips nodes that are already attached — a re-run must not error (nor rely
// on error suppression to hide a real failure for the common no-op case).
//
// The registry endpoint gets the gateway priority registryGwPriority, so the
// project network keeps the default route of the node. A Docker without
// --gw-priority (or a connect that refuses the flag) gets a plain connect,
// then repairDefaultRoute. An attached node goes through repairDefaultRoute
// too: it repairs a node that an older lok8s attached without a priority.
func (d *Driver) connectNodesToRegistryNetwork(ctx context.Context, clusterName string, errOut io.Writer) error {
	rf, err := regFile()
	if err != nil {
		return err
	}
	members, _ := d.output(ctx, "docker", "network", "inspect", rf.Network.Name,
		"-f", "{{range .Containers}}{{.Name}} {{end}}")

	nodes, _ := d.output(ctx, "kind", "get", "nodes", "--name", clusterName)
	memberSet := " " + members + " "
	project := rf.ProjectNetwork
	gw, gwRead := "", false
	repair := func(node string) {
		if project == "" || project == rf.Network.Name {
			return
		}
		if !gwRead {
			gw, gwRead = d.networkGateway(ctx, project), true
		}
		d.repairDefaultRoute(ctx, node, project, gw, errOut)
	}
	for node := range strings.FieldsSeq(nodes) {
		if strings.Contains(memberSet, " "+node+" ") {
			repair(node)
			continue
		}
		if d.dockerGwPriority(ctx) &&
			d.runQuiet(ctx, "docker", "network", "connect", "--gw-priority", registryGwPriority, rf.Network.Name, node) == nil {
			continue
		}
		_ = d.runQuiet(ctx, "docker", "network", "connect", rf.Network.Name, node)
		repair(node)
	}
	return nil
}

// nodesOnRegistryNetwork reports whether at least one of the cluster's kind
// nodes is attached to the shared registry network (bash:
// lo::nodes_on_registry_network). The gate for HealNodeIPs: a node can only
// register on the wrong network if it HAS a second network — and after the
// shared→per-project default flip, an already-drifted cluster whose spec
// omits shared.enabled reads "not shared" while its nodes still carry the
// registry NIC from the old default. Gating on membership (not the spec)
// covers exactly that upgrade.
func (d *Driver) nodesOnRegistryNetwork(ctx context.Context, clusterName string) bool {
	rf, err := regFile()
	if err != nil {
		return false
	}
	members, err := d.output(ctx, "docker", "network", "inspect", rf.Network.Name,
		"-f", "{{range .Containers}}{{.Name}} {{end}}")
	if err != nil {
		return false
	}
	nodes, _ := d.output(ctx, "kind", "get", "nodes", "--name", clusterName)
	memberSet := " " + members + " "
	for node := range strings.FieldsSeq(nodes) {
		if strings.Contains(memberSet, " "+node+" ") {
			return true
		}
	}
	return false
}
