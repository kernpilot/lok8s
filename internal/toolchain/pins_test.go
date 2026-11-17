package toolchain

// pins_test.go — the drift gate. The pins in pins.go must equal what
// go.mod links (lo-full renders through those modules), what the generated
// .bin/b.yaml installs (lo core execs those binaries) and what the
// contributor .bin/b.yaml installs (the render byte-parity tests compare
// those binaries with the linked modules). Bumping any one side alone
// fails here.

import (
	"bytes"
	"cmp"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// goModRequire reads the version go.mod requires for module (direct or
// indirect).
func goModRequire(t *testing.T, module string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(wd, "..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(module) + `\s+(v\S+)`)
	m := re.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("go.mod does not require %s", module)
	}
	return m[1]
}

func TestPinsMatchGoMod(t *testing.T) {
	t.Parallel()
	testutil.Drift{
		Want: testutil.Tree{Name: "go.mod", Files: map[string]string{
			"sigs.k8s.io/kustomize/api":      goModRequire(t, "sigs.k8s.io/kustomize/api"),
			"github.com/mgoltzsche/khelm/v2": goModRequire(t, "github.com/mgoltzsche/khelm/v2"),
			"helm.sh/helm/v3":                goModRequire(t, "helm.sh/helm/v3"),
		}},
		Got: testutil.Tree{Name: "pins.go", Files: map[string]string{
			"sigs.k8s.io/kustomize/api":      KustomizeAPI,
			"github.com/mgoltzsche/khelm/v2": "v" + KhelmVersion,
			"helm.sh/helm/v3":                "v" + HelmVersion,
		}},
		Sync: "bump go.mod and internal/toolchain/pins.go together: kustomizeAPIToCLI and the b.yaml template follow KustomizeAPI, the khelm ChartRenderer binary must be the library release, HelmVersion is khelm's requirement",
	}.Check(t)
}

func TestKustomizeCLIMatchesAPIMapping(t *testing.T) {
	t.Parallel()
	cli, ok := kustomizeAPIToCLI[KustomizeAPI]
	if !ok {
		t.Fatalf("kustomizeAPIToCLI has no entry for api %s — add the CLI release built from it", KustomizeAPI)
	}
	if cli != KustomizeCLI {
		t.Fatalf("KustomizeCLI = %s but api %s maps to %s", KustomizeCLI, KustomizeAPI, cli)
	}
}

// TestTemplateCarriesThePins: the generated b.yaml installs exactly the
// pinned releases, at the plugin paths the exec render resolves.
func TestTemplateCarriesThePins(t *testing.T) {
	t.Parallel()
	tpl := mustTemplate(t, TemplateOptions{Name: "t", LoVersion: "0.3.0", Variant: "core"})
	testutil.Drift{
		Want: testutil.Tree{Name: "pins.go", Files: map[string]string{
			"kustomize":                   KustomizeCLI,
			"github.com/mgoltzsche/khelm": "v" + KhelmVersion,
			"github.com/kernpilot/lok8s":  "v0.3.0",
			"yq":                          YQVersion,
			"kind":                        KindVersion,
		}},
		Got:    testutil.Tree{Name: "the b.yaml template", Files: PinnedEntries("0.3.0")},
		Sync:   "bump internal/toolchain/pins.go and template.go together",
		Subset: true,
	}.Check(t)
	for _, s := range []string{
		"  kustomize:\n    version: " + KustomizeCLI + "\n",
		"  github.com/mgoltzsche/khelm:\n    version: v" + KhelmVersion + "\n    file: ../.kustomize/" + ChartRendererPluginRel + "\n",
		"  github.com/kernpilot/lok8s:\n    version: v0.3.0\n    asset: kustomize-secret-*\n    file: ../.kustomize/" + SecretPluginRel + "\n",
	} {
		if !strings.Contains(tpl, s) {
			t.Errorf("template lacks:\n%s\n--- template:\n%s", s, tpl)
		}
	}
}

// TestContributorBYamlCarriesThePins: the contributor .bin/b.yaml installs
// the pinned kustomize and khelm releases. A floating entry let CI install
// khelm v3.0.0 next to the linked v2.8.0 (2026-10-21), and the chart
// byte-parity test failed on every PR.
func TestContributorBYamlCarriesThePins(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), ".bin", "b.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Binaries map[string]struct {
			Version string `yaml:"version"`
		} `yaml:"binaries"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	testutil.Drift{
		Want: testutil.Tree{Name: "pins.go", Files: map[string]string{
			"kustomize":                   KustomizeCLI,
			"github.com/mgoltzsche/khelm": "v" + KhelmVersion,
			"yq":                          YQVersion,
		}},
		Got: testutil.Tree{Name: "the contributor .bin/b.yaml", Files: map[string]string{
			"kustomize":                   doc.Binaries["kustomize"].Version,
			"github.com/mgoltzsche/khelm": doc.Binaries["github.com/mgoltzsche/khelm"].Version,
			"yq":                          doc.Binaries["yq"].Version,
		}},
		Sync: "bump .bin/b.yaml with internal/toolchain/pins.go, then run b install to refresh .bin/b.lock",
	}.Check(t)
}

// TestYQFallbacksInstallThePin: the other places that install yq install
// the pinned release: the cloud-init fallback of a bare CI VM and mise.toml.
func TestYQFallbacksInstallThePin(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	for _, tc := range []struct{ file, pattern, prefix string }{
		{"internal/assets/lok8s/providers/hetzner/cloud-init/cloud.d/ci/write_files/bootstrap/install-tools.sh", `(?m)^YQ_VERSION="([^"]+)"$`, ""},
		{"mise.toml", `(?m)^yq\s*=\s*"([^"]+)"`, "v"},
	} {
		raw, err := os.ReadFile(filepath.Join(root, tc.file))
		if err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(tc.pattern).FindAllSubmatch(raw, -1)
		if len(m) != 1 {
			t.Errorf("%s: %d yq versions, want one", tc.file, len(m))
			continue
		}
		if got := tc.prefix + string(m[0][1]); got != YQVersion {
			t.Errorf("%s installs yq %s, want the pin %s (internal/toolchain YQVersion)", tc.file, got, YQVersion)
		}
	}
}

// yqMinimumRE finds a statement of the yq minimum: "v4.x.y or later" or
// "v4.x+". Other tools are on a v4 line too (b is v4.18), but the scanned
// trees state no other minimum in these two forms: a match that is not about
// yq fails the test and must be excluded here. A statement of fact such as
// "a yq before v4.47.1" (when a flag came) does not match.
var yqMinimumRE = regexp.MustCompile(`v4\.[0-9]+(\.[0-9]+)?(\+| or later)`)

// TestYQMinimumIsThePin: every yq minimum in the bash tree (its messages),
// the docs (not the release notes, which record their own release) and the
// skills names the pinned release, so the documented minimum is the version
// lok8s installs and tests.
func TestYQMinimumIsThePin(t *testing.T) {
	t.Parallel()
	root := testutil.RepoRoot(t)
	want := YQVersion + " or later"
	// min is the number of statements each tree holds today: the bash
	// messages, the docs and the cluster-spec skill. It is a floor, so a new
	// statement needs no change here. A tree with fewer statements fails: one
	// was removed or reworded, or the walk skips its file. Without the floor a
	// minimum written in another form would not be checked at all.
	for _, tree := range []struct {
		dir string
		min int
	}{{"internal/assets/lok8s", 5}, {"docs", 4}, {"skills", 1}} {
		var found []string
		dir := tree.dir
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "releases", "dist", "cache":
					return filepath.SkipDir
				}
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil || bytes.IndexByte(raw, 0) >= 0 {
				return err
			}
			for i, line := range strings.Split(string(raw), "\n") {
				for _, m := range yqMinimumRE.FindAllString(line, -1) {
					found = append(found, rel+":"+strconv.Itoa(i+1))
					if m != want {
						t.Errorf("%s:%d names the yq minimum %q, want %q (internal/toolchain YQVersion)", rel, i+1, m, want)
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) < tree.min {
			t.Errorf("%s: %d yq minimum statements (found: %s), want at least %d. "+
				"A statement was removed, or reworded so that yqMinimumRE does not find it (write each yq minimum as %q), "+
				"or the walk skips its file. If a statement was removed on purpose, lower the minimum of %s in this test to %d",
				dir, len(found), cmp.Or(strings.Join(found, ", "), "none"), tree.min, want, dir, len(found))
		}
	}
}

// Every entry of the template is pinned, or floatingEntries names it with
// the reason: a new tool cannot float without one. The template is read
// with every group active, so a commented-out entry counts too.
func TestTemplateEntriesArePinnedOrNamedFloating(t *testing.T) {
	t.Parallel()
	tpl := mustTemplate(t, TemplateOptions{Name: "t", LoVersion: "0.3.0", Variant: "core", Groups: []string{GroupCore, GroupLocal, GroupCloud, GroupBash}})
	if problems := floatProblems(tpl, floatingEntries); len(problems) > 0 {
		t.Errorf("the b.yaml template:\n  %s", strings.Join(problems, "\n  "))
	}
	if n := len(entries("0.3.0", "")); n != strings.Count(tpl, "\n    groups: [") {
		t.Errorf("the template with every group active has %d groups: lines, want one per entry (%d)", strings.Count(tpl, "\n    groups: ["), n)
	}
}

// Each rule of floatProblems fails on an edit of the real template or of
// the list.
func TestFloatCheckFindsEachHole(t *testing.T) {
	t.Parallel()
	tpl := mustTemplate(t, TemplateOptions{Name: "t", LoVersion: "0.3.0", Variant: "core", Groups: []string{GroupCore, GroupLocal, GroupCloud, GroupBash}})
	with := func(key, reason string) map[string]string {
		m := map[string]string{}
		maps.Copy(m, floatingEntries)
		m[key] = reason
		return m
	}
	without := func(key string) map[string]string {
		m := with(key, "")
		delete(m, key)
		return m
	}
	const local = "  # ── local: "
	if strings.Count(tpl, local) != 1 {
		t.Fatalf("the template has %d copies of %q, want one: change this test with the template", strings.Count(tpl, local), local)
	}
	cases := []struct {
		name     string
		tpl      string
		floating map[string]string
		want     string
	}{
		{"a new entry with no version",
			strings.Replace(tpl, local, "  newtool:\n    groups: [local]\n"+local, 1), floatingEntries,
			"newtool has no version"},
		{"a floating entry that the list does not name",
			tpl, without("tilt"),
			"tilt has no version"},
		{"a pinned entry that the list names",
			tpl, with("kind", "a reason"),
			"kind is pinned"},
		{"a name in the list that is no entry",
			tpl, with("gone", "a reason"),
			"gone is no entry of the template"},
		{"a floating entry with no reason",
			tpl, with("tilt", " "),
			"tilt floats with no reason"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			problems := floatProblems(c.tpl, c.floating)
			for _, p := range problems {
				if strings.Contains(p, c.want) {
					return
				}
			}
			t.Errorf("the check lets this hole pass, want a problem with %q; it found %q", c.want, problems)
		})
	}
}

// floatProblems returns how a rendered template breaks the rule of
// TestTemplateEntriesArePinnedOrNamedFloating against the named floating
// entries. An empty result means the template keeps the rule.
func floatProblems(tpl string, floating map[string]string) []string {
	var doc struct {
		Binaries map[string]struct {
			Version string `yaml:"version"`
		} `yaml:"binaries"`
	}
	if err := yaml.Unmarshal([]byte(tpl), &doc); err != nil {
		return []string{"the template is not YAML: " + err.Error()}
	}
	var problems []string
	for _, key := range slices.Sorted(maps.Keys(doc.Binaries)) {
		reason, named := floating[key]
		switch pinned := doc.Binaries[key].Version != ""; {
		case pinned && named:
			problems = append(problems, key+" is pinned, but floatingEntries names it: remove it from the list")
		case !pinned && !named:
			problems = append(problems, key+" has no version: pin it in pins.go, or name it in floatingEntries with the reason it floats")
		case !pinned && strings.TrimSpace(reason) == "":
			problems = append(problems, key+" floats with no reason: write the reason in floatingEntries")
		}
	}
	for _, key := range slices.Sorted(maps.Keys(floating)) {
		if _, ok := doc.Binaries[key]; !ok {
			problems = append(problems, key+" is no entry of the template: remove it from floatingEntries")
		}
	}
	return problems
}
