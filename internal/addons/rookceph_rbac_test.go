package addons

// rookceph_rbac_test.go — the render-level gate for the least-privilege
// patch in the rook-ceph addon. The addon strips five rules from the Ceph
// CSI node plugin ClusterRoles with guarded JSON patches; nothing else in
// the test matrix renders those patches, so a patch that silently stops
// matching (a renamed target, a reordered chart) would stay green. This
// test runs the REAL `patches:` block of the addon's kustomization.yaml
// over a committed fixture of the chart's node plugin ClusterRoles at the
// pinned ceph-csi-drivers version, through internal/render (the pinned
// kustomize on core, in-process on lo-full), and asserts the effective
// rule sets.

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const (
	rookCephAddon   = "internal/assets/lok8s/addons/rook-ceph"
	rookCephFixture = "testdata/rook-ceph/nodeplugin-clusterroles.yaml"
)

// removedResources are the API resources the patch must strip from every
// node plugin ClusterRole.
var removedResources = []string{"secrets", "configmaps", "serviceaccounts", "serviceaccounts/token", "tokenreviews"}

type rbacRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

type clusterRole struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []rbacRule `yaml:"rules"`
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

// pinnedChartVersion reads `version:` from the addon's csi-drivers.yaml.
func pinnedChartVersion(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Chart   string `yaml:"chart"`
		Version string `yaml:"version"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Chart != "ceph-csi-drivers" || spec.Version == "" {
		t.Fatalf("%s: chart=%q version=%q", path, spec.Chart, spec.Version)
	}
	return spec.Version
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

// patchTargetNames lists target.name of every entry in the patches block.
func patchTargetNames(t *testing.T, patches *yaml.Node) []string {
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
	var names []string
	for _, e := range entries {
		if e.Target.Kind != "ClusterRole" {
			t.Fatalf("patch target kind %q, want ClusterRole", e.Target.Kind)
		}
		names = append(names, e.Target.Name)
	}
	return names
}

// renderPatched applies the addon's patches block to the fixture through
// the render pipeline and returns the ClusterRoles by name.
func renderPatched(t *testing.T, root string, patches *yaml.Node) map[string]clusterRole {
	t.Helper()
	if !render.InProcessAvailable() {
		bin := filepath.Join(root, ".bin", "kustomize")
		if info, err := os.Stat(bin); err != nil || info.Mode()&0o111 == 0 {
			what := "pinned kustomize not installed under .bin (b install): the rook-ceph RBAC render gate skipped"
			if os.Getenv("CI") != "" {
				t.Fatalf("%s; on CI the toolchain must be installed before the Go tests (ci.yml: Install toolchain), the gate must not skip", what)
			}
			t.Skip(what)
		}
	}
	dir := t.TempDir()
	fixture, err := os.ReadFile(rookCephFixture)
	if err != nil {
		t.Fatal(err)
	}
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
	rendered, err := render.Build(t.Context(), dir, render.Options{
		Paths:  &config.Paths{Base: root, Bin: filepath.Join(root, ".bin")},
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatalf("render of the patched fixture failed: %v\n%s", err, stderr.String())
	}
	roles := map[string]clusterRole{}
	dec := yaml.NewDecoder(strings.NewReader(string(rendered)))
	for {
		var cr clusterRole
		if err := dec.Decode(&cr); err != nil {
			break
		}
		if cr.Kind == "ClusterRole" {
			roles[cr.Metadata.Name] = cr
		}
	}
	return roles
}

func TestRookCephNodePluginRBACPatch(t *testing.T) {
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, rookCephAddon)

	// The fixture is the chart output at the pinned version. A bump of the
	// pin without a refreshed fixture would test the patch against stale
	// rules, so the two versions must agree.
	if pin, fix := pinnedChartVersion(t, filepath.Join(addon, "csi-drivers.yaml")), fixtureChartVersion(t, rookCephFixture); pin != fix {
		t.Fatalf("csi-drivers.yaml pins ceph-csi-drivers %s but the fixture was rendered from %s: refresh %s (see its header)", pin, fix, rookCephFixture)
	}

	patches := patchesNode(t, filepath.Join(addon, "kustomization.yaml"))
	targets := patchTargetNames(t, patches)

	// Every node plugin ClusterRole the chart renders must have a patch, and
	// every patch must target a role the chart renders. A target that the
	// chart never emits is a silent no-op for kustomize (the cephfs role was
	// one, before the addon pinned the cephfs driver name).
	fixtureRaw, err := os.ReadFile(rookCephFixture)
	if err != nil {
		t.Fatal(err)
	}
	var fixtureNames []string
	dec := yaml.NewDecoder(strings.NewReader(string(fixtureRaw)))
	for {
		var cr clusterRole
		if err := dec.Decode(&cr); err != nil {
			break
		}
		if cr.Kind == "ClusterRole" {
			fixtureNames = append(fixtureNames, cr.Metadata.Name)
		}
	}
	for _, n := range fixtureNames {
		if !slices.Contains(targets, n) {
			t.Errorf("fixture ClusterRole %s has no patch target", n)
		}
	}
	for _, n := range targets {
		if !slices.Contains(fixtureNames, n) {
			t.Errorf("patch target %s matches no ClusterRole in the fixture", n)
		}
	}

	roles := renderPatched(t, root, patches)

	want := map[string][]rbacRule{
		"rook-ceph-rbd-csi-ceph-com-nodeplugin-cr": {
			{APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"storage.k8s.io"}, Resources: []string{"volumeattachments"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list", "watch", "create", "update", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"get"}},
		},
		"rook-ceph-cephfs-csi-ceph-com-nodeplugin-cr": {
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"list", "watch", "create", "update", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"persistentvolumes", "persistentvolumeclaims"}, Verbs: []string{"get"}},
		},
	}
	for name, rules := range want {
		got, ok := roles[name]
		if !ok {
			t.Errorf("%s: missing from the render", name)
			continue
		}
		for _, r := range got.Rules {
			for _, res := range r.Resources {
				if slices.Contains(removedResources, res) {
					t.Errorf("%s still grants %v %v %v", name, r.APIGroups, r.Resources, r.Verbs)
				}
			}
		}
		if !reflect.DeepEqual(got.Rules, rules) {
			t.Errorf("%s: effective rules\n got: %+v\nwant: %+v", name, got.Rules, rules)
		}
	}
}
