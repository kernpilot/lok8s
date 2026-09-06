package bootstrap

// parse_test.go — the Go port of the _resolve_entries + _parse_entry halves
// of tests/unit/bootstrap_test.bats: entry resolution (per-driver default
// policy) and the entry parser (both schemas, every validation rejection),
// with the bash error strings pinned exactly.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	for _, d := range []string{p.Lok8s + "/addons", p.Clusters + "/test.lok8s.dev"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mkChartAddon lays down the minimal chart addon the bats setup created
// (testcni with chart.yaml), so `values:`/`valueFiles:` are legal on it.
func mkChartAddon(t *testing.T, p *config.Paths, name string) string {
	t.Helper()
	dir := filepath.Join(p.Lok8s, "addons", name)
	writeFile(t, filepath.Join(dir, "chart.yaml"),
		"apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: "+name+"\nvalueFiles:\n  - values.yaml\n")
	return dir
}

// --- bootstrap::_resolve_entries -------------------------------------------

func resolveFromSpec(t *testing.T, spec, kind string) []string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
	writeFile(t, f, spec)
	entries, err := ResolveEntries(f, kind)
	if err != nil {
		t.Fatalf("ResolveEntries: %v", err)
	}
	return entries
}

func TestResolveEntriesExplicitListInOrder(t *testing.T) {
	got := resolveFromSpec(t, "kind: Lo\nspec:\n  bootstrap: [cilium, ./targets/foo, /abs/bar]\n", "lo")
	want := []string{`"cilium"`, `"./targets/foo"`, `"/abs/bar"`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestResolveEntriesDropsCommentsKeepsBlockMaps(t *testing.T) {
	// Comments must not become bogus addon names; a BLOCK-style map entry
	// must survive as ONE compact-JSON element.
	got := resolveFromSpec(t, `kind: Capi
spec:
  # leading comment
  bootstrap:
    - cilium
    # a comment between entries
    - ccm:
        networking:
          enabled: true
`, "capi")
	if len(got) != 2 {
		t.Fatalf("got %d entries: %v", len(got), got)
	}
	if got[0] != `"cilium"` {
		t.Errorf("entry 0 = %q", got[0])
	}
	if got[1] != `{"ccm":{"networking":{"enabled":true}}}` {
		t.Errorf("entry 1 = %q", got[1])
	}
}

func TestResolveEntriesExplicitEmptyOptsOut(t *testing.T) {
	if got := resolveFromSpec(t, "kind: Lo\nspec:\n  bootstrap: []\n", "lo"); len(got) != 0 {
		t.Errorf("explicit [] must opt out, got %v", got)
	}
}

func TestResolveEntriesAbsentDefaultsToCiliumForLo(t *testing.T) {
	got := resolveFromSpec(t, "kind: Lo\nspec:\n  network: {cidr: 10.0.0.0/16}\n", "lo")
	if len(got) != 1 || got[0] != "cilium" {
		t.Errorf("got %v, want [cilium]", got)
	}
}

func TestResolveEntriesAbsentIsEmptyForManagedDrivers(t *testing.T) {
	// KubeOne deploys its own cilium during apply; Capi/Kkp bring their CNI
	// from the management cluster (FRICTION 2026-06-12: the blanket default
	// caused stray cilium applies on managed clusters).
	for _, kind := range []string{"kubeone", "capi", "kkp"} {
		if got := resolveFromSpec(t, "kind: X\nspec:\n  provider: {name: hetzner}\n", kind); len(got) != 0 {
			t.Errorf("kind %s: got %v, want empty", kind, got)
		}
	}
}

// --- bootstrap::_parse_entry ------------------------------------------------

func mustParse(t *testing.T, p *config.Paths, entry string) *Entry {
	t.Helper()
	var errBuf bytes.Buffer
	e, err := ParseEntry(p, &errBuf, "test.lok8s.dev", entry)
	if err != nil {
		t.Fatalf("ParseEntry(%q): %v\nstderr: %s", entry, err, errBuf.String())
	}
	return e
}

func mustFail(t *testing.T, p *config.Paths, entry, wantMsg string) {
	t.Helper()
	var errBuf bytes.Buffer
	_, err := ParseEntry(p, &errBuf, "test.lok8s.dev", entry)
	if err == nil {
		t.Fatalf("ParseEntry(%q) succeeded, want error containing %q", entry, wantMsg)
	}
	if !strings.Contains(errBuf.String(), wantMsg) {
		t.Errorf("ParseEntry(%q) stderr = %q, want substring %q", entry, errBuf.String(), wantMsg)
	}
}

func TestParseEntryBareName(t *testing.T) {
	p := testPaths(t)
	e := mustParse(t, p, `"cilium"`)
	// The fixture holds no cilium: the parser peeks at the embedded copy
	// (temp dir, nothing written into the project) and flags it builtin.
	if e.Name != "cilium" || !e.Builtin || filepath.Base(e.Dir) != "cilium" || !dirExists(e.Dir) || strings.HasPrefix(e.Dir, p.Lok8s) {
		t.Errorf("name/dir/builtin = %q/%q/%v", e.Name, e.Dir, e.Builtin)
	}
	if dirExists(p.Lok8s + "/addons/cilium") {
		t.Error("ParseEntry ejected into the project")
	}
	if e.Inline != "" || e.EnvLines != "" || e.Wait || len(e.Deps) != 0 || e.Explicit {
		t.Errorf("unexpected fields: %+v", e)
	}
	// A name the binary does not ship resolves to its would-be local dir
	// (the bash path), so "not found" keeps its wording.
	e = mustParse(t, p, `"nope"`)
	if !e.Builtin || e.Dir != p.Lok8s+"/addons/nope" {
		t.Errorf("unknown addon dir = %q builtin=%v", e.Dir, e.Builtin)
	}
}

func TestParseEntryBareNameLocalCopyWins(t *testing.T) {
	p := testPaths(t)
	os.MkdirAll(p.Lok8s+"/addons/cilium", 0o755)
	os.WriteFile(p.Lok8s+"/addons/cilium/chart.yaml", []byte("kind: ChartRenderer\n"), 0o644)
	e := mustParse(t, p, `"cilium"`)
	if e.Dir != p.Lok8s+"/addons/cilium" || !e.Builtin {
		t.Errorf("local copy did not win: %q builtin=%v", e.Dir, e.Builtin)
	}
}

func TestParseEntryDefaultBareWordEntry(t *testing.T) {
	// The lo default rides through as the bare word `cilium` (no JSON
	// quoting) — the parser must treat it identically to "cilium".
	p := testPaths(t)
	e := mustParse(t, p, "cilium")
	if e.Name != "cilium" || !e.Builtin || filepath.Base(e.Dir) != "cilium" || !dirExists(e.Dir) {
		t.Errorf("name/dir = %q/%q", e.Name, e.Dir)
	}
}

func TestParseEntryRelativePath(t *testing.T) {
	p := testPaths(t)
	e := mustParse(t, p, `"./targets/foo"`)
	if e.Name != "foo" {
		t.Errorf("name = %q", e.Name)
	}
	// The un-normalized /./ join is the bash contract.
	if e.Dir != p.Clusters+"/test.lok8s.dev/./targets/foo" {
		t.Errorf("dir = %q", e.Dir)
	}
}

func TestParseEntryAbsolutePathUnderBase(t *testing.T) {
	p := testPaths(t)
	e := mustParse(t, p, `"/abs/bar"`)
	if e.Name != "bar" || e.Dir != p.Base+"/abs/bar" {
		t.Errorf("name/dir = %q/%q", e.Name, e.Dir)
	}
}

func TestParseEntryValuesMap(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"values":{"shared_all":"inline","nested":{"k":1}}}}`)
	if e.Name != "testcni" {
		t.Errorf("name = %q", e.Name)
	}
	if !strings.Contains(e.Inline, "shared_all: inline") && !strings.Contains(e.Inline, `shared_all: "inline"`) {
		t.Errorf("inline = %q", e.Inline)
	}
	if !strings.Contains(e.Inline, "k: 1") {
		t.Errorf("inline nested lost: %q", e.Inline)
	}
	if e.EnvLines != "" || e.Wait || len(e.Deps) != 0 {
		t.Errorf("unexpected fields: %+v", e)
	}
}

func TestParseEntryValuesEnvWait(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"values":{"a":1},"env":{"LOK8S_USER_FOO":"bar","LOK8S_USER_BAZ":"qux"},"wait":true}}`)
	if !e.Wait {
		t.Error("wait not parsed")
	}
	for _, want := range []string{"LOK8S_USER_FOO=bar", "LOK8S_USER_BAZ=qux"} {
		found := false
		for _, l := range strings.Split(e.EnvLines, "\n") {
			if l == want {
				found = true
			}
		}
		if !found {
			t.Errorf("env line %q missing from %q", want, e.EnvLines)
		}
	}
}

func TestParseEntryLegacyWholeMapIsHelmValues(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"encryption":{"enabled":true}}}`)
	if !strings.Contains(e.Inline, "enabled: true") {
		t.Errorf("legacy values lost: %q", e.Inline)
	}
	if e.EnvLines != "" || e.Wait || len(e.Deps) != 0 {
		t.Errorf("unexpected fields: %+v", e)
	}
}

func TestParseEntryValuesOnNonChartTargetRejected(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev/targets/raw/kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n")
	mustFail(t, p, `{"./targets/raw":{"values":{"x":1}}}`, "not a chart addon")
}

func TestParseEntryEnvMapValueRejected(t *testing.T) {
	// the ccm-break case: the chart's own env: block left at reserved-key level
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"env":{"ROBOT_ENABLED":{"value":"true"}}}}`, "env: values must be scalars")
}

func TestParseEntryEnvListContainerRejected(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"env":["LOK8S_USER_FOO=bar"]}}`, "env: must be a map")
}

func TestParseEntryEnvScalarContainerRejected(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"env":"LOK8S_USER_FOO=bar"}}`, "env: must be a map")
}

func TestParseEntryMultiKeyMapRejected(t *testing.T) {
	p := testPaths(t)
	mustFail(t, p, `{"testcni":{"wait":true},"other":{"wait":false}}`, "single-key map")
}

func TestParseEntryNonBooleanWaitRejected(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"wait":"yes"}}`, "non-boolean wait")
}

func TestParseEntryNonMapValueRejected(t *testing.T) {
	p := testPaths(t)
	mustFail(t, p, `{"testcni":true}`, "entry value must be a map")
	mustFail(t, p, `{"testcni":[]}`, "entry value must be a map")
}

func TestParseEntryIllegalEnvKeyRejected(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"env":{"MY-VAR":"x"}}}`, "not a valid shell variable name")
	mustFail(t, p, `{"testcni":{"env":{"1X":"y"}}}`, "not a valid shell variable name")
}

// --- dependsOn parse ---------------------------------------------------------

func TestParseEntryDependsOnList(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"dependsOn":["cert-manager","ccm"]}}`)
	if len(e.Deps) != 2 || e.Deps[0] != "cert-manager" || e.Deps[1] != "ccm" {
		t.Errorf("deps = %v", e.Deps)
	}
	// dependsOn alone is the NEW schema — no inline helm values leak.
	if e.Inline != "" || e.Wait {
		t.Errorf("unexpected fields: %+v", e)
	}
}

func TestParseEntryDependsOnScalarRejected(t *testing.T) {
	p := testPaths(t)
	mustFail(t, p, `{"testcni":{"dependsOn":"cert-manager"}}`, "dependsOn: must be a list")
}

func TestParseEntryDependsOnNonScalarElementRejected(t *testing.T) {
	p := testPaths(t)
	mustFail(t, p, `{"testcni":{"dependsOn":[{"name":"x"}]}}`, "must be a scalar entry name")
}

func TestParseEntryDependsOnNullElementRejected(t *testing.T) {
	p := testPaths(t)
	mustFail(t, p, `{"testcni":{"dependsOn":[null]}}`, "null element")
}

// --- name: override ----------------------------------------------------------

func TestParseEntryNameOverridesIdentityNotDir(t *testing.T) {
	p := testPaths(t)
	e := mustParse(t, p, `{"./x":{"name":"bar"}}`)
	if e.Name != "bar" {
		t.Errorf("name = %q", e.Name)
	}
	if e.Dir != p.Clusters+"/test.lok8s.dev/./x" {
		t.Errorf("dir = %q", e.Dir)
	}
	if !e.Explicit {
		t.Error("explicit flag not set")
	}
}

func TestParseEntryNameAloneIsNewSchema(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"name":"renamed"}}`)
	if e.Name != "renamed" || e.Inline != "" || !e.Explicit {
		t.Errorf("fields: %+v", e)
	}
}

func TestParseEntryNameCombinesWithDependsOn(t *testing.T) {
	p := testPaths(t)
	e := mustParse(t, p, `{"./targets/rook-ceph":{"name":"rook-ceph-cluster","dependsOn":["rook-ceph"]}}`)
	if e.Name != "rook-ceph-cluster" || e.Dir != p.Clusters+"/test.lok8s.dev/./targets/rook-ceph" {
		t.Errorf("name/dir = %q/%q", e.Name, e.Dir)
	}
	if len(e.Deps) != 1 || e.Deps[0] != "rook-ceph" || !e.Explicit {
		t.Errorf("deps/explicit: %+v", e)
	}
}

func TestParseEntryBareReportsExplicitFalse(t *testing.T) {
	p := testPaths(t)
	if e := mustParse(t, p, `"cilium"`); e.Explicit {
		t.Error("bare entry reports explicit")
	}
}

func TestParseEntryNameValidation(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"name":""}}`, "non-empty string")
	mustFail(t, p, `{"testcni":{"name":"bad/name"}}`, "not a valid entry name")
	mustFail(t, p, `{"testcni":{"name":{"k":"v"}}}`, "non-empty scalar")
	mustFail(t, p, `{"testcni":{"name":null}}`, "non-empty scalar")
	mustFail(t, p, `{"testcni":{"name":["a","b"]}}`, "non-empty scalar")
	// An unquoted YAML bool/int coerces and would slip past the charset
	// check — require a string scalar.
	mustFail(t, p, `{"testcni":{"name":true}}`, "non-empty scalar")
	mustFail(t, p, `{"testcni":{"name":123}}`, "non-empty scalar")
}

func TestParseEntryQuotedBoolLikeNameAccepted(t *testing.T) {
	// name: "true" is !!str — a deliberate string identity, NOT the
	// unquoted-bool mistake.
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"name":"true"}}`)
	if e.Name != "true" || !e.Explicit {
		t.Errorf("fields: %+v", e)
	}
}

// --- valueFiles --------------------------------------------------------------

func writeValueFile(t *testing.T, p *config.Paths, rel, content string) string {
	t.Helper()
	f := filepath.Join(p.Clusters, "test.lok8s.dev", rel)
	writeFile(t, f, content)
	return f
}

func inlineGet(t *testing.T, inline, path string) string {
	t.Helper()
	return yamlPath(t, inline, path)
}

func TestParseEntryValueFilesAlone(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	writeValueFile(t, p, "values/extra.yaml", "marker: \"filevalue\"\nnested:\n  from_file: true\n")
	e := mustParse(t, p, `{"testcni":{"valueFiles":["./values/extra.yaml"]}}`)
	if got := inlineGet(t, e.Inline, "marker"); got != "filevalue" {
		t.Errorf("marker = %q (inline %q)", got, e.Inline)
	}
	if got := inlineGet(t, e.Inline, "nested.from_file"); got != "true" {
		t.Errorf("nested.from_file = %q", got)
	}
	// valueFiles is a RESERVED key, not a legacy whole-map helm value.
	if strings.Contains(e.Inline, "valueFiles") {
		t.Errorf("reserved key leaked: %q", e.Inline)
	}
	if e.EnvLines != "" || e.Wait {
		t.Errorf("unexpected fields: %+v", e)
	}
}

func TestParseEntryValueFilesMergeOrder(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	writeValueFile(t, p, "vf/a.yaml", "shared: \"first\"\nnested:\n  overridden: \"first\"\n  from_first: true\n")
	writeValueFile(t, p, "vf/b.yaml", "shared: \"second\"\nnested:\n  overridden: \"second\"\n  from_second: true\n")
	e := mustParse(t, p, `{"testcni":{"valueFiles":["./vf/a.yaml","./vf/b.yaml"]}}`)
	for path, want := range map[string]string{
		"shared": "second", "nested.overridden": "second",
		"nested.from_first": "true", "nested.from_second": "true",
	} {
		if got := inlineGet(t, e.Inline, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestParseEntryInlineValuesWinOverValueFiles(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	writeValueFile(t, p, "vf/base.yaml", "shared: \"file\"\nnested:\n  overridden: \"file\"\n  from_file: true\n")
	e := mustParse(t, p, `{"testcni":{"valueFiles":["./vf/base.yaml"],"values":{"shared":"inline","nested":{"overridden":"inline"}}}}`)
	for path, want := range map[string]string{
		"shared": "inline", "nested.overridden": "inline", "nested.from_file": "true",
	} {
		if got := inlineGet(t, e.Inline, path); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestParseEntryAbsoluteValueFilePassesThrough(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	abs := filepath.Join(t.TempDir(), "abs-values.yaml")
	writeFile(t, abs, "marker: \"absolute\"\n")
	e := mustParse(t, p, `{"testcni":{"valueFiles":["`+abs+`"]}}`)
	if got := inlineGet(t, e.Inline, "marker"); got != "absolute" {
		t.Errorf("marker = %q", got)
	}
}

func TestParseEntryValueFilesMissingFileHardError(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	var errBuf bytes.Buffer
	_, err := ParseEntry(p, &errBuf, "test.lok8s.dev", `{"testcni":{"valueFiles":["./vf/does-not-exist.yaml"]}}`)
	if err == nil {
		t.Fatal("missing file must be a hard error")
	}
	if !strings.Contains(errBuf.String(), "valueFiles: file not found") {
		t.Errorf("stderr = %q", errBuf.String())
	}
	// The error names the RESOLVED path (cluster-dir base), not the raw ref.
	if !strings.Contains(errBuf.String(), p.Clusters+"/test.lok8s.dev/./vf/does-not-exist.yaml") {
		t.Errorf("resolved path not named: %q", errBuf.String())
	}
}

func TestParseEntryValueFilesContainerTypeRejected(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"valueFiles":"./x.yaml"}}`, "valueFiles: must be a list of file paths (got str)")
	mustFail(t, p, `{"testcni":{"valueFiles":{"f":"./x.yaml"}}}`, "valueFiles: must be a list of file paths (got map)")
}

func TestParseEntryValueFilesElementTypeRejected(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"valueFiles":[null]}}`, "each element must be a file path string (got null)")
	mustFail(t, p, `{"testcni":{"valueFiles":[{"f":"./x.yaml"}]}}`, "each element must be a file path string (got map)")
}

func TestParseEntryEmptyValueFilesListIsNoop(t *testing.T) {
	// `valueFiles: []` is vacuous config, not an error — inline values pass
	// through untouched. Pinned so the behavior is a decision, not an
	// accident.
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	e := mustParse(t, p, `{"testcni":{"valueFiles":[],"values":{"kept":"yes"}}}`)
	if got := inlineGet(t, e.Inline, "kept"); got != "yes" {
		t.Errorf("kept = %q", got)
	}
}

func TestParseEntryEmptyStringValueFileElementHardError(t *testing.T) {
	// "" is !!str so it survives the tag check — but it is not a path. The
	// fail-fast contract is "never render with half the values".
	p := testPaths(t)
	mkChartAddon(t, p, "testcni")
	mustFail(t, p, `{"testcni":{"valueFiles":[""]}}`, "valueFiles: empty element")
}

func TestParseEntryValueFilesOnNonChartTargetRejected(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev/targets/rawvf/kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n")
	writeValueFile(t, p, "vf/x.yaml", "marker: x\n")
	mustFail(t, p, `{"./targets/rawvf":{"valueFiles":["./vf/x.yaml"]}}`, "not a chart addon")
}

// --- bootstrap::inline_values -----------------------------------------------

func TestInlineValuesMatchesFrameworkEntryNotTarget(t *testing.T) {
	p := testPaths(t)
	mkChartAddon(t, p, "cilium")
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, `kind: KubeOne
spec:
  bootstrap:
    - ./targets/cilium
    - cilium:
        values: {kubeProxyReplacement: true}
`)
	var errBuf bytes.Buffer
	got, err := InlineValues(p, &errBuf, "test.lok8s.dev", spec, "cilium")
	if err != nil {
		t.Fatalf("InlineValues: %v", err)
	}
	// A same-basename target must not shadow the framework entry's values.
	if !strings.Contains(got, "kubeProxyReplacement: true") {
		t.Errorf("inline = %q", got)
	}
}

func TestInlineValuesMissingSpecIsHardError(t *testing.T) {
	p := testPaths(t)
	var errBuf bytes.Buffer
	if _, err := InlineValues(p, &errBuf, "test.lok8s.dev", "/nope/cluster.yaml", "cilium"); err == nil {
		t.Fatal("missing cluster yaml must error")
	}
	if !strings.Contains(errBuf.String(), "inline_values: cluster yaml not found") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

// yamlPath resolves a dotted path in a YAML doc, stringifying the scalar.
func yamlPath(t *testing.T, doc, path string) string {
	t.Helper()
	return yqLike(t, doc, path)
}
