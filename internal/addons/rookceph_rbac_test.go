package addons

// rookceph_rbac_test.go — the render-level gate for the least-privilege
// patches in the rook-ceph addon. The addon strips rules from the Ceph CSI
// node plugin and controller plugin ClusterRoles with guarded JSON patches,
// gives the controller plugin a namespaced Role for the provisioner secrets
// and deletes the COSI ClusterRoleBinding; nothing else in the test matrix
// renders those patches, so a patch that silently stops matching (a renamed
// target, a reordered chart) would stay green.
//
// TestRookCephCSIRBACPatches runs the real `patches:` block of the addon's
// kustomization.yaml over committed fixtures of the chart's ClusterRoles at
// the pinned ceph-csi-drivers version, through internal/render (the pinned
// kustomize on core, in-process on lo-full), and asserts the effective roles
// as whole objects. The expected role names are derived from
// csi-drivers.yaml the way the chart derives them, so a dropped name pin
// fails the test. TestRookCephFixturesMatchChart re-renders the chart itself
// (khelm, online), diffs the fixtures against it and checks the addon as
// shipped; it runs on CI and on request (TESTING.md: LOK8S_ONLINE_TESTS).

import (
	"bufio"
	"context"
	"errors"
	"io"
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
	rookCephNS       = "rook-ceph"
	// onlineTestsEnv opts the chart re-render in outside CI (it downloads
	// the pinned chart).
	onlineTestsEnv = "LOK8S_ONLINE_TESTS"

	ctrlSecretsRole = "rook-ceph-csi-ctrlplugin-secrets"
	kmsRole         = "rook-ceph-csi-nodeplugin-kms-cr"
	// cosiSA is the Ceph COSI driver ServiceAccount the rook-ceph chart
	// binds cluster-wide; cosiFixture is that binding as the chart renders
	// it (header: how to refresh).
	cosiSA      = "objectstorage-provisioner"
	cosiFixture = "testdata/rook-ceph/cosi-clusterrolebinding.yaml"
)

// chartDrivers are the driver types the chart knows.
var chartDrivers = []string{"rbd", "cephfs", "nfs", "nvmeof"}

// nodeForbidden and ctrlForbidden are the API resources the patches strip
// on each side; no narrowed role may grant one of them. The controller
// plugin keeps tokenreviews (and subjectaccessreviews) create: no
// escalation path, and the snapshot-metadata sidecar needs them.
var (
	nodeForbidden = []string{"secrets", "configmaps", "serviceaccounts", "serviceaccounts/token", "tokenreviews"}
	ctrlForbidden = []string{"secrets", "configmaps", "serviceaccounts", "serviceaccounts/token"}
)

// rule builds a PolicyRule the way yaml.v3 decodes one into a generic map
// (sequences as []any), so expected and decoded rules compare with
// reflect.DeepEqual.
func rule(apiGroups, resources, verbs []string) map[string]any {
	return map[string]any{"apiGroups": toAny(apiGroups), "resources": toAny(resources), "verbs": toAny(verbs)}
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

var (
	secretsGLW = rule([]string{""}, []string{"secrets"}, []string{"get", "list", "watch"})
	configmaps = rule([]string{""}, []string{"configmaps"}, []string{"get"})
	saGet      = rule([]string{""}, []string{"serviceaccounts"}, []string{"get"})
	saToken    = rule([]string{""}, []string{"serviceaccounts/token"}, []string{"create"})
	tokenRev   = rule([]string{"authentication.k8s.io"}, []string{"tokenreviews"}, []string{"create"})
	kmsFive    = []map[string]any{secretsGLW, configmaps, saGet, saToken, tokenRev}
	kmsFour    = []map[string]any{secretsGLW, configmaps, saGet, saToken}
)

// plugin describes one ClusterRole family the addon patches: the chart's
// name suffix, its fixture, and per driver type the rules the patch must
// remove (nil: the driver is not patched on this side) and the count that
// must remain.
type plugin struct {
	name      string
	suffix    string
	fixture   string
	forbidden []string
	removed   map[string][]map[string]any
	kept      map[string]int
}

var plugins = []plugin{
	{
		name: "nodeplugin", suffix: "-nodeplugin-cr", fixture: "testdata/rook-ceph/nodeplugin-clusterroles.yaml",
		forbidden: nodeForbidden,
		removed:   map[string][]map[string]any{"rbd": kmsFive, "nvmeof": kmsFive, "cephfs": kmsFive, "nfs": nil},
		kept:      map[string]int{"rbd": 5, "nvmeof": 5, "cephfs": 3, "nfs": 1},
	},
	{
		name: "ctrlplugin", suffix: "-ctrlplugin-cr", fixture: "testdata/rook-ceph/ctrlplugin-clusterroles.yaml",
		forbidden: ctrlForbidden,
		removed: map[string][]map[string]any{
			"rbd": kmsFour, "nvmeof": kmsFour, "cephfs": kmsFour,
			"nfs": {rule([]string{""}, []string{"secrets"}, []string{"get"})},
		},
		kept: map[string]int{"rbd": 25, "nvmeof": 25, "cephfs": 23, "nfs": 15},
	},
}

// object is one Kubernetes object as a generic document, metadata reduced
// to name (+ namespace) because the addon's label transformer adds labels
// the chart does not.
type object struct {
	Kind, Name, Namespace string
	Doc                   map[string]any
}

// Rules returns the object's rules as generic maps.
func (o object) Rules() []map[string]any {
	seq, _ := o.Doc["rules"].([]any)
	rules := make([]map[string]any, 0, len(seq))
	for _, r := range seq {
		m, _ := r.(map[string]any)
		rules = append(rules, m)
	}
	return rules
}

// decodeObjects returns every object of a multi-document stream, keyed by
// kind/name. Any decode error other than EOF fails the test: a corrupt
// stream must never read as "nothing there".
func decodeObjects(t *testing.T, stream []byte) map[string]object {
	t.Helper()
	objs := map[string]object{}
	dec := yaml.NewDecoder(strings.NewReader(string(stream)))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding the object stream: %v", err)
		}
		kind, _ := doc["kind"].(string)
		meta, _ := doc["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		if kind == "" || name == "" {
			continue
		}
		m := map[string]any{"name": name}
		if ns != "" {
			m["namespace"] = ns
		}
		doc["metadata"] = m
		objs[kind+"/"+name] = object{Kind: kind, Name: name, Namespace: ns, Doc: doc}
	}
	return objs
}

// clusterRoles filters decoded objects to the ClusterRoles with a suffix.
func clusterRoles(objs map[string]object, suffix string) map[string]object {
	out := map[string]object{}
	for _, o := range objs {
		if o.Kind == "ClusterRole" && strings.HasSuffix(o.Name, suffix) {
			out[o.Name] = o
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// canon is the canonical text of a generic document (yaml.v3 sorts map
// keys), used to compare rules as sets and to print diffs.
func canon(t *testing.T, doc any) string {
	t.Helper()
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func ruleSet(t *testing.T, rules []map[string]any) []string {
	t.Helper()
	set := make([]string, 0, len(rules))
	for _, r := range rules {
		set = append(set, canon(t, r))
	}
	sort.Strings(set)
	return set
}

// fixtureChartVersion reads the `# chart: <chart> <version>` header line
// of a fixture.
func fixtureChartVersion(t *testing.T, path, chart string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	re := regexp.MustCompile(`^# chart: ` + regexp.QuoteMeta(chart) + ` (\S+)$`)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	t.Fatalf("%s: no `# chart: %s <version>` header line", path, chart)
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

func readCSIDrivers(t *testing.T, addon string) csiDrivers {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(addon, "csi-drivers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var spec csiDrivers
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Chart != "ceph-csi-drivers" || spec.Version == "" {
		t.Fatalf("csi-drivers.yaml: chart=%q version=%q", spec.Chart, spec.Version)
	}
	return spec
}

// rookChartVersion reads `version:` from the addon's chart.yaml (the
// rook-ceph operator chart).
func rookChartVersion(t *testing.T, addon string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(addon, "chart.yaml"))
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
	if spec.Chart != "rook-ceph" || spec.Version == "" {
		t.Fatalf("chart.yaml: chart=%q version=%q", spec.Chart, spec.Version)
	}
	return spec.Version
}

// normalizeDriverName is the chart's helper of the same name
// (_helpers.tpl: lower-case, "." -> "-").
func normalizeDriverName(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), ".", "-")
}

// expectedNames derives, per chart driver type, the normalized driver names
// the chart renders for this addon: the name pinned in csi-drivers.yaml (or
// the chart default when none is pinned) and the chart default itself, which
// a fork of the addon can keep. The chart appends -nodeplugin-cr,
// -ctrlplugin-cr, -nodeplugin-sa, -ctrlplugin-sa to them.
func expectedNames(spec csiDrivers) map[string]string {
	names := map[string]string{} // normalized name -> driver type
	for _, typ := range chartDrivers {
		def := typ + ".csi.ceph.com"
		pinned := spec.Values.Drivers[typ].Name
		if pinned == "" {
			pinned = def
		}
		for _, n := range []string{pinned, def} {
			names[normalizeDriverName(n)] = typ
		}
	}
	return names
}

// withSuffix maps expectedNames to object names.
func withSuffix(names map[string]string, suffix string) map[string]string {
	out := map[string]string{}
	for n, typ := range names {
		out[n+suffix] = typ
	}
	return out
}

// patchEntry is one entry of the kustomization's patches block.
type patchEntry struct {
	Target struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"target"`
	Patch string `yaml:"patch"`
}

// patchesNode returns the `patches:` mapping value of the addon's
// kustomization.yaml, untouched, and its decoded entries.
func patchesNode(t *testing.T, addon string) (*yaml.Node, []patchEntry) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(addon, "kustomization.yaml"))
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
			var entries []patchEntry
			if err := doc.Content[i+1].Decode(&entries); err != nil {
				t.Fatal(err)
			}
			return doc.Content[i+1], entries
		}
	}
	t.Fatal("kustomization.yaml: no patches: block")
	return nil, nil
}

// clusterRoleTargets compiles the ClusterRole targets (kustomize treats the
// name as a regular expression; the addon anchors it) and returns the
// `$patch: delete` entries by kind/name.
func clusterRoleTargets(t *testing.T, entries []patchEntry) (targets []*regexp.Regexp, deletes map[string]bool) {
	t.Helper()
	deletes = map[string]bool{}
	for _, e := range entries {
		if strings.Contains(e.Patch, "$patch: delete") {
			// A delete needs a target: without one kustomize fails when the
			// object is absent from the render. The body must name the same
			// object.
			var obj struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name string `yaml:"name"`
				} `yaml:"metadata"`
			}
			if err := yaml.Unmarshal([]byte(e.Patch), &obj); err != nil {
				t.Fatal(err)
			}
			if e.Target.Kind != obj.Kind || e.Target.Name != "^"+obj.Metadata.Name+"$" {
				t.Fatalf("$patch: delete for %s/%s has target %+v, want the same object, anchored", obj.Kind, obj.Metadata.Name, e.Target)
			}
			deletes[obj.Kind+"/"+obj.Metadata.Name] = true
			continue
		}
		if e.Target.Kind != "ClusterRole" {
			t.Fatalf("patch target kind %q, want ClusterRole", e.Target.Kind)
		}
		if !strings.HasPrefix(e.Target.Name, "^") || !strings.HasSuffix(e.Target.Name, "$") {
			t.Fatalf("patch target %q is not anchored", e.Target.Name)
		}
		targets = append(targets, regexp.MustCompile(e.Target.Name))
	}
	return targets, deletes
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

// renderPatched applies the addon's patches block to the fixtures through
// the render pipeline and returns the objects by kind/name.
func renderPatched(t *testing.T, root string, fixtures [][]byte, patches *yaml.Node) map[string]object {
	t.Helper()
	dir := t.TempDir()
	var resources []*yaml.Node
	for i, fx := range fixtures {
		name := "fixture" + string(rune('a'+i)) + ".yaml"
		if err := os.WriteFile(filepath.Join(dir, name), fx, 0o644); err != nil {
			t.Fatal(err)
		}
		resources = append(resources, &yaml.Node{Kind: yaml.ScalarNode, Value: name})
	}
	kz := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "apiVersion"}, {Kind: yaml.ScalarNode, Value: "kustomize.config.k8s.io/v1beta1"},
		{Kind: yaml.ScalarNode, Value: "kind"}, {Kind: yaml.ScalarNode, Value: "Kustomization"},
		{Kind: yaml.ScalarNode, Value: "resources"}, {Kind: yaml.SequenceNode, Content: resources},
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
		t.Fatalf("render of the patched fixtures failed: %v\n%s", err, stderr.String())
	}
	return decodeObjects(t, rendered)
}

// readObjects decodes one YAML file of the addon.
func readObjects(t *testing.T, path string) map[string]object {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return decodeObjects(t, raw)
}

// resourcesOf returns a rule's resources.
func resourcesOf(r map[string]any) []string {
	seq, _ := r["resources"].([]any)
	out := make([]string, 0, len(seq))
	for _, s := range seq {
		if str, ok := s.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// grantsForbidden reports whether a rule grants one of the forbidden
// resources.
func grantsForbidden(r map[string]any, forbidden []string) bool {
	return slices.ContainsFunc(resourcesOf(r), func(res string) bool { return slices.Contains(forbidden, res) })
}

// cosiBindings returns every ClusterRoleBinding whose subjects name the
// COSI ServiceAccount in the Rook namespace, by subject and not by name:
// an upstream rename must not slip past the delete patch.
func cosiBindings(objs map[string]object) []object {
	var out []object
	for _, k := range sortedKeys(objs) {
		o := objs[k]
		if o.Kind != "ClusterRoleBinding" {
			continue
		}
		seq, _ := o.Doc["subjects"].([]any)
		for _, s := range seq {
			m, _ := s.(map[string]any)
			if m["kind"] == "ServiceAccount" && m["name"] == cosiSA && m["namespace"] == rookCephNS {
				out = append(out, o)
				break
			}
		}
	}
	return out
}

// roleRefName returns a binding's roleRef name.
func roleRefName(b object) string {
	ref, _ := b.Doc["roleRef"].(map[string]any)
	n, _ := ref["name"].(string)
	return n
}

// subjectNames returns the ServiceAccount subject names of a binding, all
// of which must live in the Rook namespace.
func subjectNames(t *testing.T, b object) []string {
	t.Helper()
	seq, _ := b.Doc["subjects"].([]any)
	var names []string
	for _, s := range seq {
		m, _ := s.(map[string]any)
		if m["kind"] != "ServiceAccount" || m["namespace"] != rookCephNS {
			t.Errorf("%s/%s: subject %v is not a ServiceAccount in %s", b.Kind, b.Name, m, rookCephNS)
		}
		n, _ := m["name"].(string)
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ctrlplugin SA names for every expected driver name, sorted.
func ctrlpluginSANames(names map[string]string) []string {
	var out []string
	for n := range names {
		out = append(out, n+"-ctrlplugin-sa")
	}
	sort.Strings(out)
	return out
}

// checkPluginRoles asserts, for one plugin family, that roles (the effective
// render) equals the fixture with exactly the expected rules removed.
func checkPluginRoles(t *testing.T, p plugin, names map[string]string, fixture, roles map[string]object, kms []string) {
	t.Helper()
	expected := withSuffix(names, p.suffix)
	for _, name := range sortedKeys(expected) {
		typ := expected[name]
		got, ok := roles[name]
		if !ok {
			t.Errorf("%s: missing from the render", name)
			continue
		}
		for _, r := range got.Rules() {
			if grantsForbidden(r, p.forbidden) {
				t.Errorf("%s still grants %s", name, canon(t, r))
			}
		}
		// The effective role is the fixture role with exactly the expected
		// rules removed, order and every other field untouched.
		removedSet := ruleSet(t, p.removed[typ])
		var kept []any
		var removed []map[string]any
		for _, r := range fixture[name].Rules() {
			if slices.Contains(removedSet, canon(t, r)) {
				removed = append(removed, r)
			} else {
				kept = append(kept, r)
			}
		}
		if rs := ruleSet(t, removed); !reflect.DeepEqual(rs, removedSet) {
			t.Errorf("%s: the fixture carries %d of the %d rules the patch must remove: %v", name, len(removed), len(p.removed[typ]), rs)
		}
		if len(kept) != p.kept[typ] {
			t.Errorf("%s: %d rules kept, want %d", name, len(kept), p.kept[typ])
		}
		wantDoc := maps.Clone(fixture[name].Doc)
		wantDoc["rules"] = kept
		if !reflect.DeepEqual(got.Doc, wantDoc) {
			t.Errorf("%s: effective ClusterRole\n got: %s\nwant: %s", name, canon(t, got.Doc), canon(t, wantDoc))
		}
		// What the patch removed is what rook-ceph-csi-kms gives back: the
		// node plugin's five rules exactly; the controller plugin's four
		// (it keeps tokenreviews). Not for nfs: it has no KMS, and its
		// cluster-wide `secrets get` is replaced by the namespaced Role.
		if p.removed[typ] == nil || typ == "nfs" {
			continue
		}
		for _, r := range removedSet {
			if !slices.Contains(kms, r) {
				t.Errorf("%s: removed rule is not restored by rook-ceph-csi-kms: %s", name, r)
			}
		}
	}
}

func TestRookCephCSIRBACPatches(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	addon := filepath.Join(root, rookCephAddon)
	spec := readCSIDrivers(t, addon)
	names := expectedNames(spec)
	patches, entries := patchesNode(t, addon)
	targets, deletes := clusterRoleTargets(t, entries)

	// The chart binds the COSI ServiceAccount cluster-wide (the fixture
	// proves the unpatched render carries such a binding); the patches
	// must leave no ClusterRoleBinding with that subject, whatever its
	// name. The delete patch itself is checked for shape only.
	cosiRaw, err := os.ReadFile(cosiFixture)
	if err != nil {
		t.Fatal(err)
	}
	cosiFx := cosiBindings(decodeObjects(t, cosiRaw))
	if len(cosiFx) == 0 {
		t.Fatalf("%s: no ClusterRoleBinding binds %s/%s", cosiFixture, rookCephNS, cosiSA)
	}
	if pin, fix := rookChartVersion(t, addon), fixtureChartVersion(t, cosiFixture, "rook-ceph"); pin != fix {
		t.Fatalf("chart.yaml pins rook-ceph %s but %s was rendered from %s: refresh it (see its header)", pin, cosiFixture, fix)
	}
	if len(deletes) == 0 {
		t.Errorf("no $patch: delete entry in the patches block")
	}

	fixtures := map[string]map[string]object{}
	raws := [][]byte{cosiRaw}
	for _, p := range plugins {
		// The fixture is the chart output at the pinned version. A bump of
		// the pin without a refreshed fixture would test the patch against
		// stale rules, so the two versions must agree.
		if fix := fixtureChartVersion(t, p.fixture, "ceph-csi-drivers"); spec.Version != fix {
			t.Fatalf("csi-drivers.yaml pins ceph-csi-drivers %s but %s was rendered from %s: refresh it (see its header)", spec.Version, p.fixture, fix)
		}
		raw, err := os.ReadFile(p.fixture)
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, raw)
		fixtures[p.name] = clusterRoles(decodeObjects(t, raw), p.suffix)

		// The role names come from csi-drivers.yaml the way the chart
		// derives them. The fixture must carry exactly those roles. Every
		// role the patch covers on this side must be matched by exactly
		// one target (a target the chart never emits is a silent no-op for
		// kustomize), every other role by none.
		expected := withSuffix(names, p.suffix)
		if got, want := sortedKeys(fixtures[p.name]), sortedKeys(expected); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s fixture ClusterRoles %v, want (derived from csi-drivers.yaml) %v", p.name, got, want)
		}
		for _, n := range sortedKeys(expected) {
			matches := 0
			for _, re := range targets {
				if re.MatchString(n) {
					matches++
				}
			}
			wantMatches := 0
			if p.removed[expected[n]] != nil {
				wantMatches = 1
			}
			if matches != wantMatches {
				t.Errorf("%s: matched by %d patch targets, want %d", n, matches, wantMatches)
			}
		}
	}
	allNames := map[string]bool{}
	for _, p := range plugins {
		for n := range withSuffix(names, p.suffix) {
			allNames[n] = true
		}
	}
	for _, re := range targets {
		if !slices.ContainsFunc(sortedKeys(allNames), re.MatchString) {
			t.Errorf("patch target %s matches no expected ClusterRole", re)
		}
	}

	// The controller plugin's namespaced Secret access: `get` on Rook's
	// provisioner secrets by name, bound to every expected ctrlplugin
	// ServiceAccount.
	rbac := readObjects(t, filepath.Join(addon, "ctrlplugin-secrets-rbac.yaml"))
	role, ok := rbac["Role/"+ctrlSecretsRole]
	if !ok || role.Namespace != rookCephNS {
		t.Fatalf("Role %s missing or not in %s: %v", ctrlSecretsRole, rookCephNS, sortedKeys(rbac))
	}
	wantRule := map[string]any{
		"apiGroups": toAny([]string{""}), "resources": toAny([]string{"secrets"}),
		"resourceNames": toAny([]string{"rook-csi-rbd-provisioner", "rook-csi-cephfs-provisioner"}),
		"verbs":         toAny([]string{"get"}),
	}
	if rules := role.Rules(); len(rules) != 1 || !reflect.DeepEqual(rules[0], wantRule) {
		t.Errorf("Role %s rules\n got: %s\nwant: %s", ctrlSecretsRole, canon(t, rules), canon(t, []any{wantRule}))
	}
	binding, ok := rbac["RoleBinding/"+ctrlSecretsRole]
	if !ok || binding.Namespace != rookCephNS {
		t.Fatalf("RoleBinding %s missing or not in %s", ctrlSecretsRole, rookCephNS)
	}
	if ref, _ := binding.Doc["roleRef"].(map[string]any); ref["kind"] != "Role" || ref["name"] != ctrlSecretsRole {
		t.Errorf("RoleBinding roleRef = %v", ref)
	}
	if got, want := subjectNames(t, binding), ctrlpluginSANames(names); !reflect.DeepEqual(got, want) {
		t.Errorf("RoleBinding subjects %v, want (derived from csi-drivers.yaml) %v", got, want)
	}
	if raw, err := os.ReadFile(filepath.Join(addon, "kustomization.yaml")); err != nil || !strings.Contains(string(raw), "- ctrlplugin-secrets-rbac.yaml") {
		t.Errorf("kustomization.yaml does not list ctrlplugin-secrets-rbac.yaml as a resource")
	}

	// rook-ceph-csi-kms binds its ClusterRole to the node plugin AND the
	// controller plugin ServiceAccount of the rbd driver.
	kmsObjs := readObjects(t, filepath.Join(root, rookCephKMSAddon, "rbac.yaml"))
	kmsCR, ok := kmsObjs["ClusterRole/"+kmsRole]
	if !ok {
		t.Fatalf("rook-ceph-csi-kms: ClusterRole %s missing", kmsRole)
	}
	kms := ruleSet(t, kmsCR.Rules())
	if !reflect.DeepEqual(kms, ruleSet(t, kmsFive)) {
		t.Errorf("rook-ceph-csi-kms rules %v, want the five node plugin rules", kms)
	}
	var kmsCRB object
	for _, o := range kmsObjs {
		if o.Kind == "ClusterRoleBinding" {
			kmsCRB = o
		}
	}
	if got, want := subjectNames(t, kmsCRB), []string{"rook-ceph-rbd-csi-ceph-com-ctrlplugin-sa", "rook-ceph-rbd-csi-ceph-com-nodeplugin-sa"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rook-ceph-csi-kms subjects %v, want %v", got, want)
	}
	if t.Failed() {
		t.FailNow()
	}

	requireRenderToolchain(t, root, "the rook-ceph RBAC render gate")
	rendered := renderPatched(t, root, raws, patches)
	for _, p := range plugins {
		checkPluginRoles(t, p, names, fixtures[p.name], clusterRoles(rendered, p.suffix), kms)
	}
	for _, b := range cosiBindings(rendered) {
		t.Errorf("ClusterRoleBinding/%s still binds %s/%s to ClusterRole %s after the patches", b.Name, rookCephNS, cosiSA, roleRefName(b))
	}
}

// TestRookCephFixturesMatchChart re-renders the pinned chart through the
// addon's own generator (khelm, so it downloads the chart) and diffs both
// fixtures against it as whole objects: the version header alone would let
// a fixture drift from what the chart really renders. It follows the fixture
// headers' recipe: the addon with its patches removed, every chart driver
// enabled, once with the addon's pinned driver names and once with the
// chart defaults. A third render is the addon exactly as shipped, which
// proves the COSI binding is gone, the Role and RoleBinding are in, and the
// rbd roles are narrowed end to end.
func TestRookCephFixturesMatchChart(t *testing.T) {
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
	names := expectedNames(readCSIDrivers(t, addon))

	renderCopy := func(mutate func(dir string)) map[string]object {
		dir := t.TempDir()
		if err := copyTree(addon, dir); err != nil {
			t.Fatal(err)
		}
		mutate(dir)
		var stderr strings.Builder
		out, err := render.Build(context.Background(), dir, render.Options{
			Paths:      projectPaths(root),
			EnableExec: true,
			Env:        []string{"KHELM_TRUST_ANY_REPO=true"},
			Stderr:     &stderr,
		})
		if err != nil {
			t.Fatalf("chart render failed: %v\n%s", err, stderr.String())
		}
		return decodeObjects(t, out)
	}

	chart := map[string]object{}
	for _, defaults := range []bool{false, true} {
		maps.Copy(chart, renderCopy(func(dir string) {
			stripPatches(t, filepath.Join(dir, "kustomization.yaml"))
			enableDrivers(t, filepath.Join(dir, "csi-drivers.yaml"), defaults)
		}))
	}
	for _, p := range plugins {
		raw, err := os.ReadFile(p.fixture)
		if err != nil {
			t.Fatal(err)
		}
		fixture := clusterRoles(decodeObjects(t, raw), p.suffix)
		got := clusterRoles(chart, p.suffix)
		if !reflect.DeepEqual(sortedKeys(got), sortedKeys(fixture)) {
			t.Fatalf("%s: chart renders %v, fixture has %v", p.name, sortedKeys(got), sortedKeys(fixture))
		}
		expected := withSuffix(names, p.suffix)
		for n, cr := range got {
			if !reflect.DeepEqual(cr.Doc, fixture[n].Doc) {
				t.Errorf("%s: the chart renders\n%s\nbut the fixture has\n%s\nrefresh %s", n, canon(t, cr.Doc), canon(t, fixture[n].Doc), p.fixture)
			}
			// A chart bump must not grow an unpatched role into the removed
			// set unnoticed.
			if p.removed[expected[n]] != nil {
				continue
			}
			for _, r := range cr.Rules() {
				if grantsForbidden(r, p.forbidden) {
					t.Errorf("%s: the chart now grants %s to a driver the addon does not patch", n, canon(t, r))
				}
			}
		}
	}

	// The COSI binding: the unpatched chart renders exactly the fixture's.
	cosiRaw, err := os.ReadFile(cosiFixture)
	if err != nil {
		t.Fatal(err)
	}
	cosiFx := cosiBindings(decodeObjects(t, cosiRaw))
	if got := cosiBindings(chart); !reflect.DeepEqual(got, cosiFx) {
		t.Errorf("the chart binds %s/%s with\n%s\nbut the fixture has\n%s\nrefresh %s", rookCephNS, cosiSA, canon(t, got), canon(t, cosiFx), cosiFixture)
	}

	// The addon as shipped: no binding of the COSI ServiceAccount, by
	// subject; the ClusterRole it referred to stays (only the binding goes).
	shipped := renderCopy(func(string) {})
	for _, b := range cosiBindings(shipped) {
		t.Errorf("the shipped render still binds %s/%s through ClusterRoleBinding/%s (roleRef %s)", rookCephNS, cosiSA, b.Name, roleRefName(b))
	}
	for _, b := range cosiFx {
		if _, ok := shipped["ClusterRole/"+roleRefName(b)]; !ok {
			t.Errorf("the shipped render lost ClusterRole/%s (only the binding is deleted)", roleRefName(b))
		}
	}
	for _, k := range []string{"Role/" + ctrlSecretsRole, "RoleBinding/" + ctrlSecretsRole} {
		if o, ok := shipped[k]; !ok || o.Namespace != rookCephNS {
			t.Errorf("the shipped render lacks %s in %s", k, rookCephNS)
		}
	}
	for _, p := range plugins {
		name := "rook-ceph-rbd-csi-ceph-com" + p.suffix
		got, ok := shipped["ClusterRole/"+name]
		if !ok {
			t.Errorf("the shipped render lacks ClusterRole/%s", name)
			continue
		}
		if n := len(got.Rules()); n != p.kept["rbd"] {
			t.Errorf("%s: %d rules in the shipped render, want %d", name, n, p.kept["rbd"])
		}
		for _, r := range got.Rules() {
			if grantsForbidden(r, p.forbidden) {
				t.Errorf("%s still grants %s in the shipped render", name, canon(t, r))
			}
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

// enableDrivers sets drivers.<type>.enabled: true for every chart driver
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
	for _, typ := range chartDrivers {
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
