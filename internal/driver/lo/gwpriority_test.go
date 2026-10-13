package lo

// gwpriority_test.go — the default route of a kind node on a shared-registry
// cluster. kind creates the node on the project network; the driver then
// attaches the shared registry network. A container on two networks gets
// its default route from the network with the highest gateway priority, and
// between equal priorities Docker takes the network whose name sorts first.
// Without a priority, a project network named after "lok8s-registries"
// ("ops") lost the default route to the registry network (live on
// lanisce/ops, 2026-10-12: `default via 10.125.200.128 dev eth1` on both
// nodes, Cilium's init container hung on 10.96.0.1:443). The bash twin is
// tests/unit/lo_registry_gw_priority_test.bats.
//
// gwDocker simulates the parts of Docker the fix depends on: the version,
// the endpoints of each node in attach order (eth0, eth1, …) with their
// priorities and gateways, Docker's choice of the default route, and the
// in-node `ip` reads and the `ip route replace` that overrides that choice.
// Like moby, `docker network inspect` reports the gateway of a network
// created without --gateway only from Docker 29 on; the endpoint in
// `docker inspect <node>` always carries it.

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

type gwEndpoint struct {
	network  string
	priority int
	dev      string
}

// gwExit is a failed command with its exit status, like *exec.ExitError.
type gwExit struct {
	code int
	msg  string
}

func (e gwExit) Error() string { return e.msg }
func (e gwExit) ExitCode() int { return e.code }

type gwDocker struct {
	t        *testing.T
	version  string            // `docker version` answer; "" = the command fails
	gateways map[string]string // network → IPv4 gateway
	nodes    []string          // the cluster's nodes in `kind get nodes`
	// lb is kind's HA load balancer ("" = none): `kind get nodes` lists it
	// after the nodes, with the role label external-load-balancer. Its
	// image has no `ip`, so every docker exec in it fails.
	lb       string
	eps      map[string][]gwEndpoint
	override map[string]string // node → the default route `ip route replace` set
	// foreign are other members of lok8s-registries: the registries and
	// another cluster's nodes. They are not in `kind get nodes`.
	foreign []string
	// stopped nodes: State.Running is false, the endpoint has no gateway,
	// and a docker exec fails.
	stopped map[string]bool
	// gone nodes: `kind get nodes` lists them, docker inspect fails.
	gone map[string]bool
	// membersRead, when set, makes the member read of lok8s-registries
	// fail after it printed this partial list.
	membersRead string
	// refuseGwPriority makes `network connect --gw-priority` fail (a
	// client that does not know the flag).
	refuseGwPriority bool
	// noEndpointGateway makes `docker inspect <node>` print an empty
	// endpoint gateway.
	noEndpointGateway bool
	// failExec makes a `docker exec <node> <cmd>` fail when cmd starts
	// with one of these prefixes.
	failExec []string
	// noRouteDev makes `ip -o -4 route get` succeed with no output.
	noRouteDev bool
	log        []string
}

// daemonMajor is the major version of the daemon in g.version, 0 when it
// does not parse.
func (g *gwDocker) daemonMajor() int {
	f := strings.Fields(g.version)
	if len(f) != 2 {
		return 0
	}
	major, _, _ := strings.Cut(f[1], ".")
	n, _ := strconv.Atoi(major)
	return n
}

// networkReportsGateway is moby's behaviour for a network created with
// --subnet and no --gateway: the gateway is in `docker network inspect`
// from Docker 29 on, and empty before.
func (g *gwDocker) networkReportsGateway() bool { return g.daemonMajor() >= 29 }

// restart is a node restart: Docker programs the default route again, and
// the route that `ip route replace` set is gone.
func (g *gwDocker) restart(node string) { delete(g.override, node) }

func newGWDocker(t *testing.T, project string, version string) *gwDocker {
	g := &gwDocker{
		t:       t,
		version: version,
		gateways: map[string]string{
			// The shared network's gateway is the first address of its
			// reserved dynamic range, as Docker assigns it.
			"lok8s-registries": "10.125.200.128",
			project:            "10.125.201.192",
		},
		nodes:    []string{"c-control-plane", "c-worker"},
		eps:      map[string][]gwEndpoint{},
		override: map[string]string{},
		stopped:  map[string]bool{},
		gone:     map[string]bool{},
	}
	// kind creates every node on the project network: eth0, priority 0.
	for _, n := range g.nodes {
		g.eps[n] = []gwEndpoint{{network: project, dev: "eth0"}}
	}
	return g
}

// newHAGWDocker is an HA cluster: three control-plane nodes, a worker and
// kind's load balancer, all on the project network.
func newHAGWDocker(t *testing.T, project string, version string) *gwDocker {
	g := newGWDocker(t, project, version)
	g.nodes = []string{"c-control-plane", "c-control-plane2", "c-control-plane3", "c-worker"}
	g.lb = "c-external-load-balancer"
	for _, n := range append(slices.Clone(g.nodes), g.lb) {
		g.eps[n] = []gwEndpoint{{network: project, dev: "eth0"}}
	}
	return g
}

// containers is the cluster's containers as `kind get nodes` lists them.
func (g *gwDocker) containers() []string {
	if g.lb == "" {
		return slices.Clone(g.nodes)
	}
	return append(slices.Clone(g.nodes), g.lb)
}

// attach adds an endpoint the way an older lok8s did (priority 0).
func (g *gwDocker) attach(node, network string, priority int) {
	g.eps[node] = append(g.eps[node], gwEndpoint{network: network, priority: priority, dev: fmt.Sprintf("eth%d", len(g.eps[node]))})
	delete(g.override, node) // Docker programs the gateway again on a connect
}

func (g *gwDocker) attached(node, network string) bool {
	return slices.ContainsFunc(g.eps[node], func(ep gwEndpoint) bool { return ep.network == network })
}

// dockerDefault is Docker's choice: the highest priority, then the network
// name that sorts first.
func (g *gwDocker) dockerDefault(node string) string {
	eps := slices.Clone(g.eps[node])
	sort.SliceStable(eps, func(i, j int) bool {
		if eps[i].priority != eps[j].priority {
			return eps[i].priority > eps[j].priority
		}
		return eps[i].network < eps[j].network
	})
	return "default via " + g.gateways[eps[0].network] + " dev " + eps[0].dev
}

// defaultRoute is what `ip -4 route show default` prints in the node.
func (g *gwDocker) defaultRoute(node string) string {
	if r, ok := g.override[node]; ok {
		return r
	}
	return g.dockerDefault(node)
}

var gwNetworkField = regexp.MustCompile(`\{\{with index \.NetworkSettings\.Networks "([^"]+)"\}\}\{\{\.(Gateway|GwPriority)\}\}\{\{end\}\}`)

// inspect evaluates the --format of `docker inspect <node>` for the pieces
// the driver uses. .GwPriority before Docker 28 fails the template, as the
// field does not exist there.
func (g *gwDocker) inspect(node, format string) (string, error) {
	if _, ok := g.eps[node]; !ok || g.gone[node] {
		return "", gwExit{1, "Error: No such object: " + node}
	}
	if strings.Contains(format, ".GwPriority") && g.daemonMajor() < 28 {
		return "", gwExit{1, `template: :1: can't evaluate field GwPriority`}
	}
	role := "worker"
	switch {
	case node == g.lb:
		role = "external-load-balancer"
	case strings.Contains(node, "-control-plane"):
		role = "control-plane"
	}
	out := strings.ReplaceAll(format, `{{index .Config.Labels "io.x-k8s.kind.role"}}`, role)
	out = strings.ReplaceAll(out, `{{.State.Running}}`, strconv.FormatBool(!g.stopped[node]))
	out = gwNetworkField.ReplaceAllStringFunc(out, func(m string) string {
		sm := gwNetworkField.FindStringSubmatch(m)
		for _, ep := range g.eps[node] {
			if ep.network != sm[1] {
				continue
			}
			if sm[2] == "GwPriority" {
				return strconv.Itoa(ep.priority)
			}
			if g.noEndpointGateway || g.stopped[node] {
				return ""
			}
			return g.gateways[ep.network]
		}
		return ""
	})
	if strings.Contains(out, "{{") {
		g.t.Errorf("unexpected docker inspect format: %s", format)
	}
	return out, nil
}

func (g *gwDocker) handle(c execx.Cmd) error {
	argv := strings.Join(append([]string{c.Name}, c.Args...), " ")
	g.log = append(g.log, argv)
	a := c.Args
	switch {
	case c.Name == "kind" && len(a) >= 2 && a[0] == "get" && a[1] == "nodes":
		writeOut(c, strings.Join(g.containers(), "\n")+"\n")
	case c.Name == "kind" && len(a) >= 2 && a[0] == "get" && a[1] == "clusters":
		writeOut(c, "c\n") // the cluster exists: createKindCluster skips the create
	case c.Name != "docker":
	case len(a) >= 1 && a[0] == "version":
		if g.version == "" {
			return fmt.Errorf("cannot connect to the docker daemon")
		}
		writeOut(c, g.version+"\n")
	case len(a) == 4 && a[0] == "inspect" && a[2] == "--format":
		out, err := g.inspect(a[1], a[3])
		if err != nil {
			return err
		}
		writeOut(c, out+"\n")
	case len(a) >= 3 && a[0] == "network" && a[1] == "inspect":
		format := a[len(a)-1]
		switch {
		case strings.Contains(format, ".Gateway"):
			if g.networkReportsGateway() {
				writeOut(c, g.gateways[a[2]])
			}
			writeOut(c, " \n")
		case strings.Contains(format, ".Containers"):
			if g.membersRead != "" && a[2] == "lok8s-registries" {
				writeOut(c, g.membersRead)
				return gwExit{1, "Error response from daemon: context deadline exceeded"}
			}
			if a[2] == "lok8s-registries" {
				for _, n := range g.foreign {
					writeOut(c, n+" ")
				}
			}
			for _, n := range g.containers() {
				if g.attached(n, a[2]) {
					writeOut(c, n+" ")
				}
			}
			writeOut(c, "\n")
		}
	case len(a) >= 2 && a[0] == "network" && a[1] == "connect":
		rest, priority := a[2:], 0
		if len(rest) >= 2 && rest[0] == "--gw-priority" {
			if g.refuseGwPriority {
				return gwExit{125, "unknown flag: --gw-priority"}
			}
			priority, _ = strconv.Atoi(rest[1])
			rest = rest[2:]
		}
		if g.attached(rest[1], rest[0]) {
			return gwExit{1, "Error response from daemon: endpoint with name " + rest[1] + " already exists in network " + rest[0]}
		}
		g.attach(rest[1], rest[0], priority)
	case len(a) >= 3 && a[0] == "exec":
		node, cmd := a[1], strings.Join(a[2:], " ")
		if node == g.lb {
			return gwExit{127, `OCI runtime exec failed: exec: "ip": executable file not found in $PATH`}
		}
		if g.stopped[node] {
			return gwExit{1, "Error response from daemon: container " + node + " is not running"}
		}
		for _, p := range g.failExec {
			if strings.HasPrefix(cmd, p) {
				return gwExit{2, "ip: failed"}
			}
		}
		switch {
		case cmd == "ip -4 route show default":
			writeOut(c, g.defaultRoute(node)+" \n")
		case strings.HasPrefix(cmd, "ip -o -4 route get "):
			gw := strings.TrimPrefix(cmd, "ip -o -4 route get ")
			if g.noRouteDev {
				break
			}
			for _, ep := range g.eps[node] {
				if g.gateways[ep.network] == gw {
					writeOut(c, gw+" dev "+ep.dev+" src 10.0.0.9 uid 0 \\    cache \n")
				}
			}
		case strings.HasPrefix(cmd, "ip route replace default "):
			g.override[node] = strings.TrimPrefix(cmd, "ip route replace ")
		default:
			g.t.Errorf("unexpected docker exec: %s", argv)
		}
	}
	return nil
}

func (g *gwDocker) calls(prefix string) []string {
	var out []string
	for _, l := range g.log {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// gwDriver wires a driver whose registry map names the shared network
// "lok8s-registries" and the given project network, with shared mode on.
func gwDriver(t *testing.T, g *gwDocker, project string) (*Driver, *bytes.Buffer) {
	t.Helper()
	return gwDriverShared(t, g, project, `"shared":true,`)
}

// gwDriverShared is gwDriver with the "shared" member of the registry map
// spelled out: `"shared":true,`, `"shared":false,` or "" (the key omitted).
func gwDriverShared(t *testing.T, g *gwDocker, project, shared string) (*Driver, *bytes.Buffer) {
	t.Helper()
	d, runner, errBuf, _ := testDriver(t)
	jsonPath := filepath.Join(t.TempDir(), ".registries.json")
	testutil.WriteFile(t, jsonPath, `{`+shared+`"tls":false,"port":80,"network":{"name":"lok8s-registries","cidr":"10.125.200.0/24"},"project_network":"`+project+`","registries":[]}`)
	t.Setenv("LOK8S_REGISTRY_JSON", jsonPath)
	runner.handler = g.handle
	return d, errBuf
}

// assertProjectDefault: every node's default route goes through the
// project network (eth0, the project network's gateway).
func assertProjectDefault(t *testing.T, g *gwDocker, project string) {
	t.Helper()
	want := "default via " + g.gateways[project] + " dev eth0"
	for _, n := range g.nodes {
		if got := g.defaultRoute(n); got != want {
			t.Errorf("%s: the default route is %q, want %q (the project network %s)", n, got, want, project)
		}
	}
}

func TestDockerPicksTheRegistryNetworkForAProjectNameAfterIt(t *testing.T) {
	// The bug, reproduced on the simulation (no lok8s code): two endpoints at
	// the same priority, and "lok8s-registries" sorts before "ops".
	g := newGWDocker(t, "ops", "29.6.2 29.6.2")
	g.attach("c-control-plane", "lok8s-registries", 0)
	if got := g.defaultRoute("c-control-plane"); got != "default via 10.125.200.128 dev eth1" {
		t.Fatalf("simulation: %q, want the registry network (the bug on lanisce/ops)", got)
	}
	k := newGWDocker(t, "kubehz", "29.6.2 29.6.2")
	k.attach("c-control-plane", "lok8s-registries", 0)
	if got := k.defaultRoute("c-control-plane"); got != "default via 10.125.201.192 dev eth0" {
		t.Fatalf("simulation: %q, want the project network (kubehz sorts first)", got)
	}
}

func TestConnectNodesSetsGwPriorityOnNewDocker(t *testing.T) {
	for _, project := range []string{"ops", "zz-gwtest", "kubehz"} {
		t.Run(project, func(t *testing.T) {
			g := newGWDocker(t, project, "29.6.2 29.6.2")
			d, errBuf := gwDriver(t, g, project)

			if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
				t.Fatal(err)
			}
			connects := g.calls("docker network connect")
			want := []string{
				"docker network connect --gw-priority -1 lok8s-registries c-control-plane",
				"docker network connect --gw-priority -1 lok8s-registries c-worker",
			}
			if !slices.Equal(connects, want) {
				t.Fatalf("connects:\n%s\nwant:\n%s", strings.Join(connects, "\n"), strings.Join(want, "\n"))
			}
			if r := g.calls("docker exec"); len(r) != 0 {
				t.Fatalf("the route was read or changed although Docker applies the priority:\n%s", strings.Join(r, "\n"))
			}
			if v := g.calls("docker version"); len(v) != 1 {
				t.Fatalf("docker version asked %d times, want once per process", len(v))
			}
			assertProjectDefault(t, g, project)
		})
	}
}

func TestConnectNodesRepairsTheRouteOnOldDocker(t *testing.T) {
	// Docker 27 has no --gw-priority: a plain connect, then the default route
	// moves back to the project network inside the node.
	g := newGWDocker(t, "ops", "27.5.1 27.5.1")
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(g.log, func(l string) bool { return strings.Contains(l, "--gw-priority") }) {
		t.Fatalf("--gw-priority used on Docker 27:\n%s", strings.Join(g.log, "\n"))
	}
	replaces := g.calls("docker exec c-control-plane ip route replace")
	if want := []string{"docker exec c-control-plane ip route replace default via 10.125.201.192 dev eth0"}; !slices.Equal(replaces, want) {
		t.Fatalf("route replace:\n%s\nwant:\n%s", strings.Join(replaces, "\n"), want[0])
	}
	assertProjectDefault(t, g, "ops")
	if !strings.Contains(errBuf.String(), "c-worker: the default route did not go through ops — moved it there (via 10.125.201.192 dev eth0)") {
		t.Fatalf("no warning names the move:\n%s", errBuf.String())
	}
}

func TestConnectNodesLeavesACorrectRouteAloneOnOldDocker(t *testing.T) {
	// "kubehz" sorts before "lok8s-registries": Docker already picks it, so
	// the fallback reads the route and changes nothing.
	g := newGWDocker(t, "kubehz", "27.5.1 27.5.1")
	d, errBuf := gwDriver(t, g, "kubehz")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	if r := g.calls("docker exec c-control-plane ip route replace"); len(r) != 0 {
		t.Fatalf("a correct route was replaced: %v", r)
	}
	if len(g.calls("docker exec c-control-plane ip -4 route show default")) != 1 {
		t.Fatal("the fallback did not read the route")
	}
	assertProjectDefault(t, g, "kubehz")
	if errBuf.Len() != 0 {
		t.Fatalf("output for a correct route:\n%s", errBuf.String())
	}
}

func TestConnectNodesFallsBackWhenTheClientRefusesGwPriority(t *testing.T) {
	// A version read that says 28+ but a connect that refuses the flag: the
	// node must still be attached, and its route repaired.
	g := newGWDocker(t, "ops", "29.6.2 29.6.2")
	g.refuseGwPriority = true
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.log, "docker network connect lok8s-registries c-worker") {
		t.Fatalf("no plain connect after the refused flag:\n%s", strings.Join(g.log, "\n"))
	}
	assertProjectDefault(t, g, "ops")
}

func TestConnectNodesRepairsANodeAnOlderLok8sAttached(t *testing.T) {
	// Already a member, attached without a priority (the state lanisce/ops
	// was in): no second connect, the route is repaired.
	g := newGWDocker(t, "ops", "29.6.2 29.6.2")
	for _, n := range g.nodes {
		g.attach(n, "lok8s-registries", 0)
	}
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	if c := g.calls("docker network connect"); len(c) != 0 {
		t.Fatalf("an attached node was connected again: %v", c)
	}
	assertProjectDefault(t, g, "ops")
}

func TestRepairReadsTheGatewayFromTheNodeEndpoint(t *testing.T) {
	// Up to Docker 28.5, `docker network inspect` reports no gateway for a
	// network created with --subnet and no --gateway, and lo creates the
	// project network that way. The repair takes the gateway from the
	// node's own endpoint. A repair that read the network only skipped
	// every node on the engines that the fallback exists for.
	for _, tc := range []struct {
		name, version string
		attached      bool
	}{
		{"Docker 27: a plain connect", "27.5.1 27.5.1", false},
		{"Docker 28.5: attached at priority 0 by an older lok8s", "28.5.1 28.5.1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGWDocker(t, "ops", tc.version)
			if tc.attached {
				for _, n := range g.nodes {
					g.attach(n, "lok8s-registries", 0)
				}
			}
			if g.networkReportsGateway() {
				t.Fatalf("precondition: %s must report no network gateway", tc.version)
			}
			d, errBuf := gwDriver(t, g, "ops")

			if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
				t.Fatal(err)
			}
			for _, n := range g.nodes {
				read := slices.ContainsFunc(g.calls("docker inspect "+n+" --format "), func(l string) bool {
					return strings.Contains(l, `{{with index .NetworkSettings.Networks "ops"}}{{.Gateway}}{{end}}`)
				})
				if !read {
					t.Errorf("%s: the endpoint gateway was not read:\n%s", n, strings.Join(g.log, "\n"))
				}
				move := "docker exec " + n + " ip route replace default via 10.125.201.192 dev eth0"
				if !slices.Contains(g.log, move) {
					t.Errorf("%s: no %q:\n%s", n, move, strings.Join(g.log, "\n"))
				}
			}
			assertProjectDefault(t, g, "ops")
			if strings.Contains(errBuf.String(), "found no") {
				t.Fatalf("a warning about a missing gateway or interface:\n%s", errBuf.String())
			}
		})
	}
}

func TestRepairWarnsWhenNoGatewayIsKnown(t *testing.T) {
	// No gateway on the endpoint: the route cannot be examined. lo up does
	// not fail, and every node gets a warning that names it and the network.
	g := newGWDocker(t, "ops", "27.5.1 27.5.1")
	g.noEndpointGateway = true
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatalf("a missing gateway failed the connect: %v", err)
	}
	if r := g.calls("docker exec"); len(r) != 0 {
		t.Fatalf("the route was read or changed without a gateway:\n%s", strings.Join(r, "\n"))
	}
	for _, n := range g.nodes {
		want := "lo: " + n + `: found no IPv4 gateway for network ops (docker inspect: ""). lo did not examine the default route. Attach the node to ops (docker network connect ops ` + n + "). Then run lo up again."
		if !strings.Contains(errBuf.String(), want) {
			t.Errorf("no warning for %s:\n%s\nwant:\n%s", n, errBuf.String(), want)
		}
	}
}

func TestRepairWarnsWhenANodeCannotBeRead(t *testing.T) {
	// A failed command shows its exit status, an empty answer shows "". Each
	// warning ends with the action that removes its cause.
	for _, tc := range []struct{ name, fail, want string }{
		{
			"the default route fails", "ip -4 route show default",
			"lo: c-worker: cannot read the default route (docker exec c-worker ip -4 route show default: exit status 2). lo did not change the default route. Make sure that the node has the ip command (iproute2). Then run lo up again.",
		},
		{
			"the interface to the gateway fails", "ip -o -4 route get",
			"lo: c-worker: found no interface to gateway 10.125.201.192 of network ops (ip -o -4 route get 10.125.201.192: exit status 2). lo did not change the default route. Make sure that the node has an address in the subnet of ops (docker exec c-worker ip -4 addr show). Then run lo up again.",
		},
		{
			"the interface to the gateway is not named", "",
			`lo: c-worker: found no interface to gateway 10.125.201.192 of network ops (ip -o -4 route get 10.125.201.192: ""). lo did not change the default route. Make sure that the node has an address in the subnet of ops (docker exec c-worker ip -4 addr show). Then run lo up again.`,
		},
		{
			"the move fails", "ip route replace",
			"lo: c-worker: could not move the default route to ops (ip route replace default via 10.125.201.192 dev eth0: exit status 2). Move the route by hand (docker exec c-worker ip route replace default via 10.125.201.192 dev eth0). Then run lo up again.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGWDocker(t, "ops", "27.5.1 27.5.1")
			if tc.fail != "" {
				g.failExec = []string{tc.fail}
			} else {
				g.noRouteDev = true
			}
			d, errBuf := gwDriver(t, g, "ops")

			if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
				t.Fatalf("a failed read failed the connect: %v", err)
			}
			if r := g.calls("docker exec c-worker ip route replace"); len(r) != 0 && tc.fail != "ip route replace" {
				t.Fatalf("the route was changed after a failed read: %v", r)
			}
			if !strings.Contains(errBuf.String(), tc.want) {
				t.Fatalf("no warning:\n%s\nwant:\n%s", errBuf.String(), tc.want)
			}
		})
	}
}

func TestReconcileRepairsAttachedNodesWhenSharedIsOff(t *testing.T) {
	// shared.enabled defaulted to true until 2026-08. A cluster created then
	// keeps its registry attachment after the default changed, and its
	// registry map now reads "not shared". The route repair runs for every
	// attached node, also when shared is off (the node-IP healer gates on
	// membership the same way). Through createKindCluster, the call site in
	// Provision: the connect helper alone cannot show the gate. Only the
	// worker is attached: the control plane must stay off the network.
	for name, shared := range map[string]string{
		"shared false":   `"shared":false,`,
		"shared omitted": "",
	} {
		t.Run(name, func(t *testing.T) {
			g := newGWDocker(t, "ops", "28.5.1 28.5.1")
			g.attach("c-worker", "lok8s-registries", 0)
			d, errBuf := gwDriverShared(t, g, "ops", shared)

			if err := d.createKindCluster(t.Context(), "c", "v1.31.4", "ops", "", io.Discard, errBuf); err != nil {
				t.Fatal(err)
			}
			if c := g.calls("docker network connect"); len(c) != 0 {
				t.Fatalf("a cluster that is not shared was connected: %v", c)
			}
			if r := g.calls("docker exec c-control-plane"); len(r) != 0 {
				t.Fatalf("the route of a node that is not attached was read: %v", r)
			}
			assertProjectDefault(t, g, "ops")
			if !strings.Contains(errBuf.String(), "c-worker: the default route did not go through ops") {
				t.Fatalf("no warning names the move:\n%s", errBuf.String())
			}
		})
	}
}

func TestReconcileLeavesNodesAloneWhenSharedIsOffAndNotAttached(t *testing.T) {
	// The common case after the default changed: no node is on the registry
	// network. Nothing is connected, read or changed, and the empty member
	// list ends the walk before `kind get nodes`.
	g := newGWDocker(t, "ops", "29.6.2 29.6.2")
	d, errBuf := gwDriverShared(t, g, "ops", `"shared":false,`)

	if err := d.createKindCluster(t.Context(), "c", "v1.31.4", "ops", "", io.Discard, errBuf); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"docker network connect", "docker exec", "docker inspect", "kind get nodes"} {
		if c := g.calls(prefix); len(c) != 0 {
			t.Errorf("%s called on a cluster that is not shared and not attached: %v", prefix, c)
		}
	}
	if errBuf.Len() != 0 {
		t.Fatalf("output:\n%s", errBuf.String())
	}
}

func TestFallbackRepairRunsAgainAfterANodeRestart(t *testing.T) {
	// The documented contract of the fallback: Docker programs the default
	// route again when a node restarts, so the move is gone. The next
	// `lo up` (a new process) finds the node attached and moves the route
	// back.
	g := newGWDocker(t, "ops", "27.5.1 27.5.1")
	d, errBuf := gwDriver(t, g, "ops")
	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	assertProjectDefault(t, g, "ops")

	for _, n := range g.nodes {
		g.restart(n)
	}
	if got := g.defaultRoute("c-worker"); got != "default via 10.125.200.128 dev eth1" {
		t.Fatalf("simulation: after the restart the route is %q, want the registry network", got)
	}

	d2, errBuf2 := gwDriver(t, g, "ops")
	if err := d2.connectNodesToRegistryNetwork(t.Context(), "c", errBuf2); err != nil {
		t.Fatal(err)
	}
	if c := g.calls("docker network connect"); len(c) != len(g.nodes) {
		t.Fatalf("connects over both runs: %v, want one per node", c)
	}
	for _, n := range g.nodes {
		if r := g.calls("docker exec " + n + " ip route replace"); len(r) != 2 {
			t.Errorf("%s: %d route moves over both runs, want 2 (one per lo up)", n, len(r))
		}
	}
	assertProjectDefault(t, g, "ops")
	if !strings.Contains(errBuf2.String(), "c-worker: the default route did not go through ops — moved it there") {
		t.Fatalf("the second lo up does not report the move:\n%s", errBuf2.String())
	}
}

// gwForeign are members of lok8s-registries that are not this cluster's:
// another cluster's node and a shared registry.
var gwForeign = []string{"other-control-plane", "lok8s-registry-io-docker"}

// assertOnlyClusterTouched: no call names a foreign member, and no docker
// exec reaches the load balancer or a container outside the cluster.
func assertOnlyClusterTouched(t *testing.T, g *gwDocker) {
	t.Helper()
	for _, l := range g.log {
		for _, f := range gwForeign {
			if strings.Contains(l, " "+f) {
				t.Errorf("a foreign member was touched: %s", l)
			}
		}
	}
	for _, l := range g.calls("docker exec ") {
		node := strings.Fields(l)[2]
		if !slices.Contains(g.nodes, node) {
			t.Errorf("docker exec outside the cluster's nodes: %s", l)
		}
	}
}

func TestRepairOnAnHACluster(t *testing.T) {
	// `kind get nodes` lists kind's HA load balancer too. Its envoy image
	// has no `ip`: a docker exec in it fails, and a repair that tried printed
	// a warning on every lo up. The load balancer is attached like a node
	// (the connect did that before the gateway priority existed), but its
	// route is never read. Other members of the registry network (another
	// cluster's node, a registry) are never touched.
	for _, tc := range []struct {
		name, project, version string
		attachedAt             *int // nil: nothing attached yet
		wantExec, wantMoves    bool
	}{
		{"healthy, Docker 29, attached at -1", "ops", "29.6.2 29.6.2", new(-1), false, false},
		{"healthy, Docker 27, a name before lok8s-registries", "kubehz", "27.5.1 27.5.1", new(0), true, false},
		{"Docker 27, attached at 0 by an older lok8s", "ops", "27.5.1 27.5.1", new(0), true, true},
		{"Docker 29, a fresh attach", "ops", "29.6.2 29.6.2", nil, false, false},
		{"Docker 27, a fresh attach", "ops", "27.5.1 27.5.1", nil, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newHAGWDocker(t, tc.project, tc.version)
			g.foreign = gwForeign
			if tc.attachedAt != nil {
				for _, n := range g.containers() {
					g.attach(n, "lok8s-registries", *tc.attachedAt)
				}
			}
			d, errBuf := gwDriver(t, g, tc.project)

			if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
				t.Fatal(err)
			}
			assertOnlyClusterTouched(t, g)
			assertProjectDefault(t, g, tc.project)
			if !g.attached(g.lb, "lok8s-registries") {
				t.Errorf("the load balancer is not attached (the connect attached it before the gateway priority existed)")
			}
			if got := len(g.calls("docker exec ")) > 0; got != tc.wantExec {
				t.Errorf("docker exec called: %v, want %v:\n%s", got, tc.wantExec, strings.Join(g.log, "\n"))
			}
			if strings.Contains(errBuf.String(), g.lb) {
				t.Errorf("a warning names the load balancer:\n%s", errBuf.String())
			}
			moves := strings.Count(errBuf.String(), "moved it there")
			if tc.wantMoves && moves != len(g.nodes) || !tc.wantMoves && errBuf.Len() != 0 {
				t.Errorf("output (want a move per node: %v):\n%s", tc.wantMoves, errBuf.String())
			}
		})
	}
}

func TestRepairSkipsANodeAtANegativePriority(t *testing.T) {
	// Docker 28+: a registry endpoint at a negative gateway priority cannot
	// hold the default route. The repair reads that priority in the same
	// `docker inspect` and skips the three in-node commands. A node at 0 (an
	// older lok8s) is still repaired.
	g := newGWDocker(t, "ops", "28.5.1 28.5.1")
	g.attach("c-control-plane", "lok8s-registries", -1)
	g.attach("c-worker", "lok8s-registries", 0)
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	if r := g.calls("docker exec c-control-plane"); len(r) != 0 {
		t.Fatalf("a node at priority -1 was read or changed: %v", r)
	}
	if r := g.calls("docker exec c-worker ip route replace"); len(r) != 1 {
		t.Fatalf("the node at priority 0 was not repaired:\n%s", strings.Join(g.log, "\n"))
	}
	if n := len(g.calls("docker inspect ")); n != 2 {
		t.Fatalf("%d docker inspect calls, want one per node:\n%s", n, strings.Join(g.log, "\n"))
	}
	assertProjectDefault(t, g, "ops")
	if strings.Contains(errBuf.String(), "c-control-plane") {
		t.Fatalf("output for the node at -1:\n%s", errBuf.String())
	}
}

func TestRepairWarnsForANodeThatDoesNotRun(t *testing.T) {
	// A stopped node has no endpoint gateway, and a docker exec in it fails.
	// The warning says that the node does not run, not that a gateway is
	// missing.
	g := newGWDocker(t, "ops", "27.5.1 27.5.1")
	for _, n := range g.nodes {
		g.attach(n, "lok8s-registries", 0)
	}
	g.stopped["c-worker"] = true
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	if r := g.calls("docker exec c-worker"); len(r) != 0 {
		t.Fatalf("docker exec in a node that does not run: %v", r)
	}
	want := `lo: c-worker: the node is not running (State.Running: "false"). lo did not examine the default route. Start the node (docker start c-worker). Then run lo up again.`
	if !strings.Contains(errBuf.String(), want) {
		t.Fatalf("no warning:\n%s\nwant:\n%s", errBuf.String(), want)
	}
	if strings.Contains(errBuf.String(), "found no IPv4 gateway") {
		t.Fatalf("a stopped node reported as a missing gateway:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "c-control-plane: the default route did not go through ops") {
		t.Fatalf("the running node was not repaired:\n%s", errBuf.String())
	}
}

func TestRepairWarnsForANodeThatCannotBeInspected(t *testing.T) {
	g := newGWDocker(t, "ops", "27.5.1 27.5.1")
	for _, n := range g.nodes {
		g.attach(n, "lok8s-registries", 0)
	}
	g.gone["c-worker"] = true
	d, errBuf := gwDriver(t, g, "ops")

	if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
		t.Fatal(err)
	}
	want := "lo: c-worker: cannot inspect the node (docker inspect c-worker: exit status 1). lo did not examine the default route. Make sure that the node exists. Then run lo up again."
	if !strings.Contains(errBuf.String(), want) {
		t.Fatalf("no warning:\n%s\nwant:\n%s", errBuf.String(), want)
	}
	if r := g.calls("docker exec c-worker"); len(r) != 0 {
		t.Fatalf("docker exec in a node that cannot be inspected: %v", r)
	}
}

func TestMemberReadThatFailsWithPartialOutput(t *testing.T) {
	// The member read of lok8s-registries fails after it printed a partial
	// list: another cluster's node, and c-worker, which is NOT attached. A
	// failed read is no membership. The connect still attaches every node
	// (Docker refuses a second endpoint for c-control-plane, which an older
	// lok8s attached, and the repair moves its route). The repair-only walk
	// touches nothing.
	partial := "other-control-plane c-worker "

	t.Run("connect", func(t *testing.T) {
		g := newGWDocker(t, "ops", "29.6.2 29.6.2")
		g.foreign = gwForeign
		g.attach("c-control-plane", "lok8s-registries", 0)
		g.membersRead = partial
		d, errBuf := gwDriver(t, g, "ops")

		if err := d.connectNodesToRegistryNetwork(t.Context(), "c", errBuf); err != nil {
			t.Fatal(err)
		}
		for _, n := range g.nodes {
			if !g.attached(n, "lok8s-registries") {
				t.Errorf("%s is not attached:\n%s", n, strings.Join(g.log, "\n"))
			}
		}
		if n := len(g.eps["c-control-plane"]); n != 2 {
			t.Errorf("c-control-plane has %d endpoints, want 2", n)
		}
		assertOnlyClusterTouched(t, g)
		assertProjectDefault(t, g, "ops")
	})
	t.Run("repair only", func(t *testing.T) {
		g := newGWDocker(t, "ops", "29.6.2 29.6.2")
		g.foreign = gwForeign
		g.attach("c-worker", "lok8s-registries", 0)
		g.membersRead = partial
		d, errBuf := gwDriverShared(t, g, "ops", `"shared":false,`)

		if err := d.createKindCluster(t.Context(), "c", "v1.31.4", "ops", "", io.Discard, errBuf); err != nil {
			t.Fatal(err)
		}
		for _, prefix := range []string{"docker network connect", "docker exec", "docker inspect", "kind get nodes"} {
			if c := g.calls(prefix); len(c) != 0 {
				t.Errorf("%s after a failed member read: %v", prefix, c)
			}
		}
		if errBuf.Len() != 0 {
			t.Fatalf("output:\n%s", errBuf.String())
		}
	})
}

func TestGwPrioritySupported(t *testing.T) {
	t.Parallel()
	for versions, want := range map[string]bool{
		"29.6.2 29.6.2":           true,
		"28.0.0 28.0.0":           true,
		"28.0.0-rc.1 28.0.0-rc.1": true,
		"27.5.1 27.5.1":           false,
		"27.5.1 29.6.2":           false, // the client does not know the flag
		"29.6.2 27.5.1":           false, // the daemon does not apply it
		"29.6.2":                  false, // no daemon answered
		"":                        false,
		"5.2.0 5.2.0":             false, // another engine behind the docker CLI
		"dev dev":                 false,
	} {
		if got := gwPrioritySupported(versions); got != want {
			t.Errorf("gwPrioritySupported(%q) = %v, want %v", versions, got, want)
		}
	}
}

func TestDefaultRouteRepair(t *testing.T) {
	t.Parallel()
	const gw = "10.125.201.192"
	routeGet := gw + " dev eth0 src 10.125.201.193 uid 0 \\    cache "
	for _, tc := range []struct {
		name, current, routeGet string
		dev                     string
		needed                  bool
	}{
		{"ops: through the registry network", "default via 10.125.200.128 dev eth1 ", routeGet, "eth0", true},
		{"already through the project network", "default via " + gw + " dev eth0 ", routeGet, "eth0", false},
		{"no default route", "", routeGet, "eth0", true},
		{"right gateway, wrong device", "default via " + gw + " dev eth1", routeGet, "eth0", true},
		{"two default routes: the first one counts", "default via " + gw + " dev eth0\ndefault via 10.125.200.128 dev eth1", routeGet, "eth0", false},
		{"no interface reaches the gateway", "default via 10.125.200.128 dev eth1", "", "", false},
	} {
		dev, needed := defaultRouteRepair(tc.current, tc.routeGet, gw)
		if dev != tc.dev || needed != tc.needed {
			t.Errorf("%s: defaultRouteRepair = %q, %v; want %q, %v", tc.name, dev, needed, tc.dev, tc.needed)
		}
	}
}
