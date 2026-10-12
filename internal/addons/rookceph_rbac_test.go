package addons

// rookceph_rbac_test.go — the render-level gate for the least-privilege
// patch in the rook-ceph addon. The addon strips five rules from the Ceph
// CSI node plugin ClusterRoles with guarded JSON patches; nothing else in
// the test matrix renders those patches, so a patch that silently stops
// matching (a renamed target, a reordered chart) would stay green.
//
// TestRookCephNodePluginRBACPatch runs the real `patches:` block of the
// addon's kustomization.yaml over a committed fixture of the chart's node
// plugin ClusterRoles at the pinned ceph-csi-drivers version, through
// internal/render (the pinned kustomize on core, in-process on lo-full),
// and asserts the effective rule sets. The expected role names are derived
// from csi-drivers.yaml the way the chart derives them, so a dropped name
// pin fails the test. TestRookCephNodePluginFixtureMatchesChart re-renders
// the chart itself (khelm, online) and diffs the fixture against it; it
// runs on CI and on request.

import (
	"bufio"
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

// TestMain lets the test binary act as the self-exec kustomize plugins on
// lo-full (the in-process plugin home points at THIS binary, exactly like
// `lo`: when kustomize execs …/chartrenderer/ChartRenderer for the chart
// re-render the binary must answer as khelm). On core DispatchPlugin is a
// no-op and the call falls through.
func TestMain(m *testing.M) {
	if handled, rc := render.DispatchPlugin(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(rc)
	}
	rc := m.Run()
	render.Cleanup()
	os.Exit(rc)
}

const (
	rookCephAddon    = "internal/assets/lok8s/addons/rook-ceph"
	rookCephKMSAddon = "internal/assets/lok8s/addons/rook-ceph-csi-kms"
	rookCephFixture  = "testdata/rook-ceph/nodeplugin-clusterroles.yaml"
	// onlineTestsEnv opts the chart re-render in outside CI (it downloads
	// the pinned chart).
	onlineTestsEnv = "LOK8S_ONLINE_TESTS"
)

// patchedDrivers are the chart driver types whose node plugin role carries
// the five removed rules (nfs carries `nodes get` only).
var patchedDrivers = []string{"rbd", "cephfs", "nvmeof"}

// removedResources are the API resources the patch must strip from every
// node plugin ClusterRole.
var removedResources = []string{"secrets", "configmaps", "serviceaccounts", "serviceaccounts/token", "tokenreviews"}

// keptRules is the effective rule list per driver type after the patch.
var keptRules = map[string][]rbacRule{
	"rbd": {
		{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattachments"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list", "watch", "create", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"get"}},
	},
	"cephfs": {
		{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list", "watch", "create", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"persistentvolumes", "persistentvolumeclaims"}, Verbs: []string{"get"}},
	},
}

func init() { keptRules["nvmeof"] = keptRules["rbd"] }

type rbacRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

func (r rbacRule) String() string {
	return strings.Join(r.APIGroups, ",") + " " + strings.Join(r.Resources, ",") + " " + strings.Join(r.Verbs, ",")
}

type clusterRole struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []rbacRule `yaml:"rules"`
}

// decodeClusterRoles returns the ClusterRoles of a multi-document stream
// whose name ends in -nodeplugin-cr, by name.
func decodeClusterRoles(stream []byte) map[string]clusterRole {
	roles := map[string]clusterRole{}
	dec := yaml.NewDecoder(strings.NewReader(string(stream)))
	for {
		var cr clusterRole
		if err := dec.Decode(&cr); err != nil {
			break
		}
		if cr.Kind == "ClusterRole" && strings.HasSuffix(cr.Metadata.Name, "-nodeplugin-cr") {
			roles[cr.Metadata.Name] = cr
		}
	}
	return roles
}

func sortedNames(roles map[string]clusterRole) []string {
	names := make([]string, 0, len(roles))
	for n := range roles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// fixtureChartVersion reads the `# chart: ceph-csi-drivers <version>` header
// line of the fixture.
func fixtureChartVersion(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	re := regexp.MustCompile(`^# chart: ceph-csi-drivers (\S+)$`)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	t.Fatalf("%s: no `# chart: ceph-csi-drivers <version>` header line", path)
	return ""
}

// csiDrivers is the part of csi-drivers.yaml the test reads.
type csiDrivers struct {
	Chart   string `yaml:"chart"`
	Version string `yaml:"version"`
	Values  struct {
		Drivers map[string]struct {
			Name    string `yaml:"name"`
			Enabled bool   `yaml:"enabled"`
		} `yaml:"drivers"`
	} `yaml:"values"`
}

func readCSIDrivers(t *testing.T, path string) csiDrivers {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spec csiDrivers
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Chart != "ceph-csi-drivers" || spec.Version == "" {
		t.Fatalf("%s: chart=%q version=%q", path, spec.Chart, spec.Version)
	}
	return spec
}

// normalizeDriverName is the chart's helper of the same name
// (_helpers.tpl: lower-case, "." -> "-").
func normalizeDriverName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), ".", "-")
}

// expectedRoleNames derives, per patched driver type, the node plugin
// ClusterRole names the chart renders for this addon: the name pinned in
// csi-drivers.yaml (or the chart default when none is pinned) and the chart
// default itself, which a cluster can keep through an inline override.
func expectedRoleNames(spec csiDrivers) map[string]string {
	names := map[string]string{} // role name -> driver type
	for _, typ := range patchedDrivers {
		def := typ + ".csi.ceph.com"
		pinned := spec.Values.Drivers[typ].Name
		if pinned == "" {
			pinned = def
		}
		for _, n := range []string{pinned, def} {
			names[normalizeDriverName(n)+"-nodeplugin-cr"] = typ
		}
	}
	return names
}

// patchesNode returns the `patches:` mapping value of the addon's
// kustomization.yaml, untouched.
func patchesNode(t *testing.T, path string) *yaml.Node {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "patches" {
			return doc.Content[i+1]
		}
	}
	t.Fatalf("%s: no patches: block", path)
	return nil
}

// patchTargets compiles target.name of every entry in the patches block.
// kustomize treats the name as a regular expression; the addon anchors it.
func patchTargets(t *testing.T, patches *yaml.Node) []*regexp.Regexp {
	t.Helper()
	var entries []struct {
		Target struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"target"`
	}
	if err := patches.Decode(&entries); err != nil {
		t.Fatal(err)
	}
	var targets []*regexp.Regexp
	for _, e := range entries {
		if e.Target.Kind != "ClusterRole" {
			t.Fatalf("patch target kind %q, want ClusterRole", e.Target.Kind)
		}
		if !strings.HasPrefix(e.Target.Name, "^") || !strings.HasSuffix(e.Target.Name, "$") {
			t.Fatalf("patch target %q is not anchored", e.Target.Name)
		}
		targets = append(targets, regexp.MustCompile(e.Target.Name))
	}
	return targets
}

// requireRenderToolchain skips (fails on CI) when core would exec a
// kustomize that is not installed.
func requireRenderToolchain(t *testing.T, root string, what string) {
	t.Helper()
	if render.InProcessAvailable() {
		return
	}
	bin := filepath.Join(root, ".bin", "kustomize")
	if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
		msg := "pinned kustomize not installed under .bin (b install): " + what + " skipped"
		if os.Getenv("CI") != "" {
			t.Fatalf("%s; on CI the toolchain must be installed before the Go tests (ci.yml: Install toolchain), the gate must not skip", msg)
		}
		t.Skip(msg)
	}
}

func projectPaths(root string) *config.Paths {
	return &config.Paths{Base: root, Bin: filepath.Join(root, ".bin")}
}

// renderPatched applies the addon's patches block to the fixture through
// the render pipeline and returns the ClusterRoles by name.
func renderPatched(t *testing.T, root string, fixture []byte, patches *yaml.Node) map[string]clusterRole {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clusterroles.yaml"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	kz := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "apiVersion"}, {Kind: yaml.ScalarNode, Value: "kustomize.config.k8s.io/v1beta1"},
		{Kind: yaml.ScalarNode, Value: "kind"}, {Kind: yaml.ScalarNode, Value: "Kustomization"},
		{Kind: yaml.ScalarNode, Value: "resources"}, {Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "clusterroles.yaml"}}},
		{Kind: yaml.ScalarNode, Value: "patches"}, patches,
	}}
	out, err := yaml.Marshal(kz)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	rendered, err := render.Build(context.Background(), dir, render.Options{Paths: projectPaths(root), Stderr: &stderr})
	if err != nil {
		t.Fatalf("render of the patched fixture failed: %v\n%s", err, stderr.String())
	}
	return decodeClusterRoles(rendered)
}

// kmsAddonRules reads the ClusterRole the rook-ceph-csi-kms addon ships.
func kmsAddonRules(t *testing.T, root string) []rbacRule {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rookCephKMSAddon, "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var cr clusterRole
		if err := dec.Decode(&cr); err != nil {
			break
		}
		if cr.Kind == "ClusterRole" {
			return cr.Rules
		}
	}
	t.Fatal("rook-ceph-csi-kms/rbac.yaml: no ClusterRole")
	return nil
}

func ruleSet(rules []rbacRule) []string {
	set := make([]string, 0, len(rules))
	for _, r := range rules {
		set = append(set, r.String())
	}
	sort.Strings(set)
	return set
}

func TestRookCephNodePluginRBACPatch(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, rookCephAddon)
	spec := readCSIDrivers(t, filepath.Join(addon, "csi-drivers.yaml"))

	// The fixture is the chart output at the pinned version. A bump of the
	// pin without a refreshed fixture would test the patch against stale
	// rules, so the two versions must agree.
	if fix := fixtureChartVersion(t, rookCephFixture); spec.Version != fix {
		t.Fatalf("csi-drivers.yaml pins ceph-csi-drivers %s but the fixture was rendered from %s: refresh %s (see its header)", spec.Version, fix, rookCephFixture)
	}
	fixtureRaw, err := os.ReadFile(rookCephFixture)
	if err != nil {
		t.Fatal(err)
	}
	fixture := decodeClusterRoles(fixtureRaw)

	// The role names come from csi-drivers.yaml the way the chart derives
	// them. The fixture must carry exactly those roles, and every one of
	// them must be matched by exactly one patch target: a target the chart
	// never emits is a silent no-op for kustomize (the cephfs role was one,
	// before the addon pinned the cephfs driver name).
	expected := expectedRoleNames(spec)
	var want []string
	for n := range expected {
		want = append(want, n)
	}
	sort.Strings(want)
	if got := sortedNames(fixture); !reflect.DeepEqual(got, want) {
		t.Fatalf("fixture ClusterRoles %v, want (derived from csi-drivers.yaml) %v", got, want)
	}
	patches := patchesNode(t, filepath.Join(addon, "kustomization.yaml"))
	targets := patchTargets(t, patches)
	for _, n := range want {
		matches := 0
		for _, re := range targets {
			if re.MatchString(n) {
				matches++
			}
		}
		if matches != 1 {
			t.Errorf("%s: matched by %d patch targets, want exactly 1", n, matches)
		}
	}
	for _, re := range targets {
		if !slices.ContainsFunc(want, re.MatchString) {
			t.Errorf("patch target %s matches no expected ClusterRole", re)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	requireRenderToolchain(t, root, "the rook-ceph RBAC render gate")
	roles := renderPatched(t, root, fixtureRaw, patches)
	kms := ruleSet(kmsAddonRules(t, root))
	for _, name := range want {
		got, ok := roles[name]
		if !ok {
			t.Errorf("%s: missing from the render", name)
			continue
		}
		for _, r := range got.Rules {
			for _, res := range r.Resources {
				if slices.Contains(removedResources, res) {
					t.Errorf("%s still grants %s", name, r)
				}
			}
		}
		if want := keptRules[expected[name]]; !reflect.DeepEqual(got.Rules, want) {
			t.Errorf("%s: effective rules\n got: %+v\nwant: %+v", name, got.Rules, want)
		}
		// What the patch removed is exactly what rook-ceph-csi-kms gives
		// back: fixture minus kept == the KMS addon's ClusterRole.
		kept := ruleSet(got.Rules)
		var removed []rbacRule
		for _, r := range fixture[name].Rules {
			if !slices.Contains(kept, r.String()) {
				removed = append(removed, r)
			}
		}
		if rs := ruleSet(removed); !reflect.DeepEqual(rs, kms) {
			t.Errorf("%s: removed rules %v differ from rook-ceph-csi-kms %v", name, rs, kms)
		}
	}
}

// TestRookCephNodePluginFixtureMatchesChart re-renders the pinned chart
// through the addon's own generator (khelm, so it downloads the chart) and
// diffs the node plugin ClusterRoles against the committed fixture: the
// version header alone would let a fixture drift from what the chart really
// renders. It follows the fixture header's recipe: the addon with its
// patches removed, the patched drivers enabled, once with the addon's
// pinned driver names and once with the chart defaults.
func TestRookCephNodePluginFixtureMatchesChart(t *testing.T) {
	t.Parallel()
	if os.Getenv("CI") == "" && os.Getenv(onlineTestsEnv) == "" {
		t.Skipf("downloads the pinned chart: runs on CI, or with %s=1", onlineTestsEnv)
	}
	root := testutil.RepoRoot(t)
	requireRenderToolchain(t, root, "the rook-ceph fixture gate")
	if !render.InProcessAvailable() {
		plugin := filepath.Join(config.KustomizePluginHome(projectPaths(root)), "khelm.mgoltzsche.github.com", "v2", "chartrenderer", "ChartRenderer")
		if _, err := os.Stat(plugin); err != nil {
			t.Fatalf("khelm exec plugin missing at %s (b install): the fixture gate cannot render the chart", plugin)
		}
	}
	addon := filepath.Join(root, rookCephAddon)
	fixtureRaw, err := os.ReadFile(rookCephFixture)
	if err != nil {
		t.Fatal(err)
	}
	fixture := decodeClusterRoles(fixtureRaw)

	got := map[string]clusterRole{}
	for _, defaults := range []bool{false, true} {
		dir := t.TempDir()
		if err := copyTree(addon, dir); err != nil {
			t.Fatal(err)
		}
		stripPatches(t, filepath.Join(dir, "kustomization.yaml"))
		enableDrivers(t, filepath.Join(dir, "csi-drivers.yaml"), defaults)
		var stderr strings.Builder
		out, err := render.Build(context.Background(), dir, render.Options{
			Paths:      projectPaths(root),
			EnableExec: true,
			Env:        []string{"KHELM_TRUST_ANY_REPO=true"},
			Stderr:     &stderr,
		})
		if err != nil {
			t.Fatalf("chart render (defaults=%v) failed: %v\n%s", defaults, err, stderr.String())
		}
		maps.Copy(got, decodeClusterRoles(out))
	}
	if !reflect.DeepEqual(sortedNames(got), sortedNames(fixture)) {
		t.Fatalf("chart renders node plugin roles %v, fixture has %v", sortedNames(got), sortedNames(fixture))
	}
	for n, cr := range got {
		if !reflect.DeepEqual(cr.Rules, fixture[n].Rules) {
			t.Errorf("%s: the chart renders\n%+v\nbut the fixture has\n%+v\nrefresh %s", n, cr.Rules, fixture[n].Rules, rookCephFixture)
		}
	}
}

// stripPatches removes the `patches:` key from a kustomization.yaml.
func stripPatches(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	doc := root.Content[0]
	for i := 0; i+1 < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "patches" {
			doc.Content = append(doc.Content[:i], doc.Content[i+2:]...)
			break
		}
	}
	out, err := yaml.Marshal(&root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// enableDrivers sets drivers.<type>.enabled: true for every patched driver
// type in a csi-drivers.yaml; with defaults it also drops the name pins so
// the chart falls back to its default driver names.
func enableDrivers(t *testing.T, path string, defaults bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	drivers := doc["values"].(map[string]any)["drivers"].(map[string]any)
	for _, typ := range patchedDrivers {
		d, _ := drivers[typ].(map[string]any)
		if d == nil {
			d = map[string]any{}
		}
		d["enabled"] = true
		if defaults {
			delete(d, "name")
		}
		drivers[typ] = d
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}
