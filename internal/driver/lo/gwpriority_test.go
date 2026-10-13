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
// priorities, Docker's choice of the default route, and the in-node `ip`
// reads and the `ip route replace` that overrides that choice.

import (
	"bytes"
	"fmt"
	"path/filepath"
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

type gwDocker struct {
	t        *testing.T
	version  string            // `docker version` answer; "" = the command fails
	gateways map[string]string // network → IPv4 gateway
	nodes    []string          // `kind get nodes`
	eps      map[string][]gwEndpoint
	override map[string]string // node → the default route `ip route replace` set
	// refuseGwPriority makes `network connect --gw-priority` fail (a
	// client that does not know the flag).
	refuseGwPriority bool
	log              []string
}

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
	}
	// kind creates every node on the project network: eth0, priority 0.
	for _, n := range g.nodes {
		g.eps[n] = []gwEndpoint{{network: project, dev: "eth0"}}
	}
	return g
}

// attach adds an endpoint the way an older lok8s did (priority 0).
func (g *gwDocker) attach(node, network string, priority int) {
	g.eps[node] = append(g.eps[node], gwEndpoint{network: network, priority: priority, dev: fmt.Sprintf("eth%d", len(g.eps[node]))})
	delete(g.override, node) // Docker programs the gateway again on a connect
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

func (g *gwDocker) handle(c execx.Cmd) error {
	argv := strings.Join(append([]string{c.Name}, c.Args...), " ")
	g.log = append(g.log, argv)
	a := c.Args
	switch {
	case c.Name == "kind" && len(a) >= 2 && a[0] == "get" && a[1] == "nodes":
		writeOut(c, strings.Join(g.nodes, "\n")+"\n")
	case c.Name != "docker":
	case len(a) >= 1 && a[0] == "version":
		if g.version == "" {
			return fmt.Errorf("cannot connect to the docker daemon")
		}
		writeOut(c, g.version+"\n")
	case len(a) >= 3 && a[0] == "network" && a[1] == "inspect":
		format := a[len(a)-1]
		switch {
		case strings.Contains(format, ".Gateway"):
			writeOut(c, g.gateways[a[2]]+" \n")
		case strings.Contains(format, ".Containers"):
			for _, n := range g.nodes {
				for _, ep := range g.eps[n] {
					if ep.network == a[2] {
						writeOut(c, n+" ")
					}
				}
			}
			writeOut(c, "\n")
		}
	case len(a) >= 2 && a[0] == "network" && a[1] == "connect":
		rest, priority := a[2:], 0
		if len(rest) >= 2 && rest[0] == "--gw-priority" {
			if g.refuseGwPriority {
				return fmt.Errorf("unknown flag: --gw-priority")
			}
			priority, _ = strconv.Atoi(rest[1])
			rest = rest[2:]
		}
		g.attach(rest[1], rest[0], priority)
	case len(a) >= 3 && a[0] == "exec":
		node, cmd := a[1], strings.Join(a[2:], " ")
		switch {
		case cmd == "ip -4 route show default":
			writeOut(c, g.defaultRoute(node)+" \n")
		case strings.HasPrefix(cmd, "ip -o -4 route get "):
			gw := strings.TrimPrefix(cmd, "ip -o -4 route get ")
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
// "lok8s-registries" and the given project network.
func gwDriver(t *testing.T, g *gwDocker, project string) (*Driver, *bytes.Buffer) {
	t.Helper()
	d, runner, errBuf, _ := testDriver(t)
	jsonPath := filepath.Join(t.TempDir(), ".registries.json")
	testutil.WriteFile(t, jsonPath, `{"shared":true,"tls":false,"port":80,"network":{"name":"lok8s-registries","cidr":"10.125.200.0/24"},"project_network":"`+project+`","registries":[]}`)
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
		{"gateway unreachable", "default via 10.125.200.128 dev eth1", "", "", false},
	} {
		dev, needed := defaultRouteRepair(tc.current, tc.routeGet, gw)
		if dev != tc.dev || needed != tc.needed {
			t.Errorf("%s: defaultRouteRepair = %q, %v; want %q, %v", tc.name, dev, needed, tc.dev, tc.needed)
		}
	}
}
