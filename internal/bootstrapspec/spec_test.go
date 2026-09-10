package bootstrapspec

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func parseDoc(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	return root.Content[0]
}

func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	return &config.Paths{Base: base, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
}

// CompactJSON is the yq -o=json -I=0 shape, and every string it writes is
// valid JSON: control characters go out as \u00xx (strconv.Quote wrote
// \x01, which no JSON reader accepts), <,>,& stay raw.
func TestCompactJSONWritesValidJSON(t *testing.T) {
	doc := parseDoc(t, "- {\"k\": \"ab\\tc<&>\", n: 1.5, b: true, z: ~}\n")
	// YAML source cannot carry a raw control character; the value node can
	// (a JSON-quoted "\\u0001" arrives that way).
	doc.Content[0].Content[1].Value = "a\x01b\tc<&>"
	got := CompactJSON(doc.Content[0])
	want := `{"k":"a\u0001b\tc<&>","n":1.5,"b":true,"z":null}`
	if got != want {
		t.Errorf("CompactJSON = %s, want %s", got, want)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(got), &back); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if back["k"] != "a\x01b\tc<&>" {
		t.Errorf("round trip lost the control character: %q", back["k"])
	}
}

func TestResolveCases(t *testing.T) {
	for name, tc := range map[string]struct {
		spec string
		kind string
		want []string
	}{
		"explicit list":         {"spec:\n  bootstrap:\n    - cilium\n    - ccm: {wait: true}\n", "kubeone", []string{`"cilium"`, `{"ccm":{"wait":true}}`}},
		"map values (yq []?)":   {"spec:\n  bootstrap:\n    a: cilium\n    b: {ccm: {}}\n", "lo", []string{`"cilium"`, `{"ccm":{}}`}},
		"explicit empty":        {"spec:\n  bootstrap: []\n", "lo", nil},
		"absent, lo default":    {"spec: {}\n", "lo", []string{"cilium"}},
		"absent, other driver":  {"spec: {}\n", "kubeone", nil},
		"no spec at all":        {"kind: Lo\n", "lo", []string{"cilium"}},
		"present but null (lo)": {"spec:\n  bootstrap:\n", "lo", nil},
	} {
		t.Run(name, func(t *testing.T) {
			var got []string
			for _, it := range Resolve(parseDoc(t, tc.spec), tc.kind) {
				got = append(got, it.Raw)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("Resolve = %v, want %v", got, tc.want)
			}
		})
	}
}

// One parse, three callers: the report channel carries the bash message,
// a silent parser (the audit) still fails the entry, and the merge hook
// runs at the bash merge point with the resolved files and the values
// node.
func TestParseReportsThroughTheCaller(t *testing.T) {
	p := testPaths(t)
	chart := filepath.Join(p.Clusters, "d.dev", "targets", "app")
	testutil.WriteFile(t, filepath.Join(chart, "chart.yaml"), "name: app\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "d.dev", "one.yaml"), "a: 1\n")
	doc := parseDoc(t, `
- ./targets/app: {name: web, wait: False, valueFiles: [./one.yaml], values: {b: 2}, env: {K: v, N: 1}, dependsOn: [x, 2]}
- ./targets/app: {wait: maybe}
- ./targets/app: {valueFiles: [./missing.yaml]}
- {a: 1, b: 2}
`)
	items := Resolve(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "spec"},
		{Kind: yaml.MappingNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: "bootstrap"}, doc}},
	}}, "lo")
	if len(items) != 4 {
		t.Fatalf("Resolve = %d items", len(items))
	}

	var reported []string
	var mergedFiles []string
	var mergedValues *yaml.Node
	parser := &Parser{
		Paths:  p,
		Report: func(format string, a ...any) { reported = append(reported, fmt.Sprintf(format, a...)) },
		MergeValueFiles: func(files []string, values *yaml.Node) error {
			mergedFiles, mergedValues = files, values
			return nil
		},
	}

	e, ok := parser.Parse("d.dev", items[0])
	if !ok {
		t.Fatalf("entry 0 failed: %v", reported)
	}
	wantDir := p.Clusters + "/d.dev/./targets/app"
	if e.Name != "web" || !e.Explicit || e.Dir != wantDir || e.Builtin || e.Legacy || e.Wait {
		t.Errorf("entry 0 = %+v", e)
	}
	if len(mergedFiles) != 1 || mergedFiles[0] != p.Clusters+"/d.dev/./one.yaml" || mergedValues == nil {
		t.Errorf("merge hook saw files=%v values=%v", mergedFiles, mergedValues)
	}
	if fmt.Sprint(e.Env) != "[{K v} {N 1}]" || fmt.Sprint(e.Deps) != "[x 2]" {
		t.Errorf("env=%v deps=%v", e.Env, e.Deps)
	}

	if _, ok := parser.Parse("d.dev", items[1]); ok || len(reported) != 1 ||
		reported[0] != "bootstrap: './targets/app' has a non-boolean wait: 'maybe' (use true or false)" {
		t.Errorf("entry 1: ok=%v reported=%v", ok, reported)
	}
	if _, ok := parser.Parse("d.dev", items[2]); ok || !strings.HasSuffix(reported[len(reported)-1], "valueFiles: file not found: "+p.Clusters+"/d.dev/./missing.yaml") {
		t.Errorf("entry 2: ok=%v reported=%v", ok, reported)
	}
	if _, ok := parser.Parse("d.dev", items[3]); ok || reported[len(reported)-1] != `bootstrap: entry must be a single-key map, got 2 keys: {"a":1,"b":2}` {
		t.Errorf("entry 3: ok=%v reported=%v", ok, reported)
	}

	// A silent parser (the audit) fails the same entries without a report.
	silent := &Parser{Paths: p}
	if _, ok := silent.Parse("d.dev", items[1]); ok {
		t.Error("silent parser accepted a non-boolean wait")
	}
	if e, ok := silent.Parse("d.dev", items[0]); !ok || e.Name != "web" {
		t.Errorf("silent parser: ok=%v e=%+v", ok, e)
	}
}

// A merge failure is reported with the bash message and the resolved
// file list.
func TestParseMergeFailureMessage(t *testing.T) {
	p := testPaths(t)
	chart := filepath.Join(p.Clusters, "d.dev", "targets", "app")
	testutil.WriteFile(t, filepath.Join(chart, "chart.yaml"), "name: app\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "d.dev", "bad.yaml"), "a: [\n")
	item := Item{Raw: `{"./targets/app":{"valueFiles":["./bad.yaml"]}}`, Node: parseDoc(t, "./targets/app: {valueFiles: [./bad.yaml]}\n")}
	var got string
	parser := &Parser{
		Paths:           p,
		Report:          func(format string, a ...any) { got = fmt.Sprintf(format, a...) },
		MergeValueFiles: func([]string, *yaml.Node) error { return os.ErrInvalid },
	}
	if _, ok := parser.Parse("d.dev", item); ok {
		t.Fatal("merge failure must fail the entry")
	}
	want := "bootstrap: './targets/app' valueFiles: failed to merge (" + p.Clusters + "/d.dev/./bad.yaml)"
	if got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
}
