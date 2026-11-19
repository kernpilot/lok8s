package addons

// external_dns_rbac_test.go — the gate for the RBAC of the external-dns
// addon. The external-dns chart derives the rules of its ClusterRole from
// the `sources` value (charts/external-dns/templates/clusterrole.yaml), and
// it writes one `--source=<s>` argument per item of the same list
// (templates/deployment.yaml). A source that is enabled with `extraArgs`
// gets the argument but no rules, and external-dns then fails on its first
// list call. So the addon enables a source only through `sources`, and a
// consumer that adds a source sets the whole `sources` list (inline values
// replace a list, they do not append to it).
//
// TestExternalDNSRBAC is offline. It reads two committed fixtures of the
// chart's ClusterRole at the pinned chart version: the addon as shipped
// (the five gateway-* sources), and the addon with `service` added. It
// asserts that the `service` source brings the read-only EndpointSlice rule
// that external-dns needs to find the addresses of a Service, that the
// addon as shipped grants no Service read, and that the values of both
// trees enable no source through `extraArgs`.
// TestExternalDNSFixtureMatchesChart renders the chart itself (khelm,
// online) through Render, the path of a `spec.bootstrap` entry, and diffs
// the fixtures against it; it runs on CI and with LOK8S_ONLINE_TESTS=1.

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	externalDNSAddon  = "internal/assets/lok8s/addons/external-dns"
	externalDNSLegacy = ".lok8s/addons/external-dns"
	// externalDNSName is the chart, the release (the ChartRenderer name),
	// the namespace, and the name of the ClusterRole, the Deployment and its
	// external-dns container.
	externalDNSName           = "external-dns"
	externalDNSDefaultFixture = "testdata/external-dns/clusterrole-default.yaml"
	externalDNSServiceFixture = "testdata/external-dns/clusterrole-service.yaml"
)

// externalDNSSources is the `sources` list of the addon that
// externalDNSDefaultFixture is the render of.
var externalDNSSources = []string{
	"gateway-httproute",
	"gateway-grpcroute",
	"gateway-tlsroute",
	"gateway-tcproute",
	"gateway-udproute",
}

// externalDNSServiceReads are the resources that the `service` source adds
// to the ClusterRole (clusterrole.yaml:12-31 at the chart tag
// external-dns-helm-chart-1.21.1).
var externalDNSServiceReads = []string{"endpointslices", "nodes", "pods", "services"}

// externalDNSValues is the part of the addon's values.yaml the gate reads.
type externalDNSValues struct {
	Sources   []string `yaml:"sources"`
	ExtraArgs any      `yaml:"extraArgs"`
}

func readExternalDNSValues(t *testing.T, path string) externalDNSValues {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v externalDNSValues
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// extraArgsList returns the extraArgs of the values as the chart writes
// them for a list (one argument per item). The chart also takes a map
// (`--<key>=<value>`); the gate does not read that form, so a map fails
// the test until the gate reads it.
func extraArgsList(t *testing.T, path string, v any) []string {
	t.Helper()
	switch x := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(x))
		for _, a := range x {
			s, ok := a.(string)
			if !ok {
				t.Fatalf("%s: extraArgs item %v is not a string", path, a)
			}
			out = append(out, s)
		}
		return out
	default:
		t.Fatalf("%s: extraArgs is a %T: the gate reads the list form only", path, v)
		return nil
	}
}

// sourceFlags returns the sources that an argument list enables: the value
// of each `--source=<s>`, and of each `--source <s>` pair.
func sourceFlags(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--source":
			if i+1 < len(argv) {
				i++
				out = append(out, argv[i])
			} else {
				out = append(out, "")
			}
		case strings.HasPrefix(a, "--source="):
			out = append(out, strings.TrimPrefix(a, "--source="))
		}
	}
	return out
}

// TestExternalDNSSourceFlags pins sourceFlags to the two flag forms.
func TestExternalDNSSourceFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		argv []string
		want []string
	}{
		{nil, nil},
		{[]string{"--txt-prefix=reg-%{record_type}.", "--exclude-target-net=10.0.0.0/8"}, nil},
		{[]string{"--source=service"}, []string{"service"}},
		{[]string{"--source", "ingress"}, []string{"ingress"}},
		{[]string{"--source=gateway-httproute", "--policy=sync", "--source", "service"}, []string{"gateway-httproute", "service"}},
		{[]string{"--source"}, []string{""}},
		{[]string{"--sources=service", "--source-x=service"}, nil},
	} {
		if got := sourceFlags(tc.argv); !slices.Equal(got, tc.want) {
			t.Errorf("sourceFlags(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

// externalDNSRole returns the ClusterRole of a decoded render or fixture.
func externalDNSRole(t *testing.T, objs map[string]object, from string) object {
	t.Helper()
	role, ok := objs[objKey("ClusterRole", "", externalDNSName)]
	if !ok {
		t.Fatalf("%s: no ClusterRole/%s: %v", from, externalDNSName, sortedKeys(objs))
	}
	return role
}

// checkReadOnly fails on a rule with a verb other than get, list and
// watch, and on a wildcard.
func checkReadOnly(t *testing.T, from string, role object) {
	t.Helper()
	for _, r := range role.Rules() {
		for _, field := range []string{"apiGroups", "resources", "verbs"} {
			if slices.Contains(stringsOf(r[field]), "*") {
				t.Errorf("%s: a wildcard in %s: %s", from, field, canon(t, r))
			}
		}
		for _, v := range stringsOf(r["verbs"]) {
			if !slices.Contains([]string{"get", "list", "watch"}, v) {
				t.Errorf("%s: the verb %q is not a read: %s", from, v, canon(t, r))
			}
		}
	}
}

func TestExternalDNSRBAC(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)

	// The fixtures are the render of the chart version that chart.yaml pins.
	raw, err := os.ReadFile(filepath.Join(root, externalDNSAddon, "chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Chart   string `yaml:"chart"`
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Chart != externalDNSName {
		t.Fatalf("chart.yaml renders the chart %q, want %q", chart.Chart, externalDNSName)
	}
	for _, fx := range []string{externalDNSDefaultFixture, externalDNSServiceFixture} {
		if v := fixtureChartVersion(t, fx, externalDNSName); v != chart.Version {
			t.Errorf("%s is the render of chart %s, but chart.yaml pins %s: refresh the fixture", fx, v, chart.Version)
		}
	}

	// Both trees enable each source through `sources`, never through
	// extraArgs: the chart gives a source in extraArgs no rules.
	for _, tree := range []string{externalDNSAddon, externalDNSLegacy} {
		path := filepath.Join(root, tree, "values.yaml")
		v := readExternalDNSValues(t, path)
		if !slices.Equal(v.Sources, externalDNSSources) {
			t.Errorf("%s: sources is %q, but %s is the render of %q: refresh the fixtures and externalDNSSources", path, v.Sources, externalDNSDefaultFixture, externalDNSSources)
		}
		if s := sourceFlags(extraArgsList(t, path, v.ExtraArgs)); len(s) != 0 {
			t.Errorf("%s: extraArgs enables the sources %q: the chart derives the ClusterRole from `sources` only, so such a source runs without its rules; add it to `sources`", path, s)
		}
	}

	def := externalDNSRole(t, readObjects(t, externalDNSDefaultFixture), externalDNSDefaultFixture)
	svc := externalDNSRole(t, readObjects(t, externalDNSServiceFixture), externalDNSServiceFixture)
	checkReadOnly(t, externalDNSDefaultFixture, def)
	checkReadOnly(t, externalDNSServiceFixture, svc)

	// The addon as shipped reads no Service, Pod, Node or EndpointSlice.
	for _, r := range def.Rules() {
		for _, res := range resourcesOf(r) {
			if slices.Contains(externalDNSServiceReads, res) {
				t.Errorf("%s grants %s without the service source: %s", externalDNSDefaultFixture, res, canon(t, r))
			}
		}
	}

	// The service source adds rules and removes none: the rules of the
	// gateway sources stay, and the new rules name exactly the reads of the
	// service source.
	defRules := ruleSet(t, def.Rules())
	svcRules := ruleSet(t, svc.Rules())
	for _, r := range defRules {
		if !slices.Contains(svcRules, r) {
			t.Errorf("%s lost a rule of %s:\n%s", externalDNSServiceFixture, externalDNSDefaultFixture, r)
		}
	}
	var added []string
	for _, r := range svc.Rules() {
		if !slices.Contains(defRules, canon(t, r)) {
			added = append(added, resourcesOf(r)...)
		}
	}
	slices.Sort(added)
	if !slices.Equal(added, externalDNSServiceReads) {
		t.Errorf("the service source adds rules for %q, want %q", added, externalDNSServiceReads)
	}

	// external-dns resolves the addresses of a Service from its
	// EndpointSlices (discovery.k8s.io): one rule, read-only, with watch for
	// the informer.
	var eps []map[string]any
	for _, r := range svc.Rules() {
		if slices.Contains(resourcesOf(r), "endpointslices") {
			eps = append(eps, r)
		}
	}
	if len(eps) != 1 {
		t.Fatalf("%s: %d rules name endpointslices, want 1", externalDNSServiceFixture, len(eps))
	}
	r := eps[0]
	if g := stringsOf(r["apiGroups"]); !slices.Equal(g, []string{"discovery.k8s.io"}) {
		t.Errorf("the endpointslices rule has the apiGroups %q, want [discovery.k8s.io]", g)
	}
	if res := resourcesOf(r); !slices.Equal(res, []string{"endpointslices"}) {
		t.Errorf("the endpointslices rule has the resources %q, want [endpointslices]", res)
	}
	if verbs := slices.Sorted(slices.Values(stringsOf(r["verbs"]))); !slices.Equal(verbs, []string{"get", "list", "watch"}) {
		t.Errorf("the endpointslices rule has the verbs %q, want get, list and watch", verbs)
	}
}

// TestExternalDNSFixtureMatchesChart renders the pinned chart through
// Render (khelm, so it downloads the chart): the addon as shipped must
// equal externalDNSDefaultFixture, and the addon with `sources` set inline
// to its own list plus `service` must equal externalDNSServiceFixture. In
// both renders the external-dns container must get one `--source` argument
// per item of `sources`.
func TestExternalDNSFixtureMatchesChart(t *testing.T) {
	t.Parallel()
	if os.Getenv("CI") == "" && os.Getenv(onlineTestsEnv) == "" {
		t.Skipf("downloads the pinned chart: runs on CI, or with %s=1", onlineTestsEnv)
	}
	root := testutil.RepoRoot(t)
	requireRenderToolchain(t, root, "the external-dns fixture gate")
	if !render.InProcessAvailable() {
		plugin := filepath.Join(config.KustomizePluginHome(projectPaths(root)), "khelm.mgoltzsche.github.com", "v2", "chartrenderer", "ChartRenderer")
		if _, err := os.Stat(plugin); err != nil {
			t.Fatalf("khelm exec plugin missing at %s (b install): the fixture gate cannot render the chart", plugin)
		}
	}
	addon := filepath.Join(root, externalDNSAddon)
	sources := readExternalDNSValues(t, filepath.Join(addon, "values.yaml")).Sources
	withService := append(slices.Clone(sources), "service")
	inline, err := yaml.Marshal(map[string]any{"sources": withService})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, inline, fixture string
		sources               []string
	}{
		{"as shipped", "", externalDNSDefaultFixture, sources},
		{"with the service source", string(inline), externalDNSServiceFixture, withService},
	} {
		var stderr strings.Builder
		out, err := Render(t.Context(), projectPaths(root), nil, &stderr, addon, "", "", tc.inline, nil)
		if err != nil {
			t.Fatalf("%s: render failed: %v\n%s", tc.name, err, stderr.String())
		}
		objs := decodeObjects(t, []byte(out))
		got := externalDNSRole(t, objs, tc.name)
		want := externalDNSRole(t, readObjects(t, tc.fixture), tc.fixture)
		if !reflect.DeepEqual(got.Doc, want.Doc) {
			t.Errorf("%s: the chart renders\n%s\nbut the fixture has\n%s\nrefresh %s", tc.name, canon(t, got.Doc), canon(t, want.Doc), tc.fixture)
		}

		dep, ok := objs[objKey("Deployment", externalDNSName, externalDNSName)]
		if !ok {
			t.Fatalf("%s: no Deployment %s/%s: %v", tc.name, externalDNSName, externalDNSName, sortedKeys(objs))
		}
		containers, _ := nestedMap(dep.Doc, "spec", "template", "spec")["containers"].([]any)
		var args []string
		found := false
		for _, c := range containers {
			if cm, _ := c.(map[string]any); cm["name"] == externalDNSName {
				args, found = stringsOf(cm["args"]), true
			}
		}
		if !found {
			t.Fatalf("%s: the Deployment has no container %s", tc.name, externalDNSName)
		}
		if s := sourceFlags(args); !slices.Equal(s, tc.sources) {
			t.Errorf("%s: the container enables the sources %q, want %q (args %q)", tc.name, s, tc.sources, args)
		}
	}
}
