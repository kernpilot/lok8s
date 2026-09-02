package lint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

// newLinter builds a Linter over a synthetic project root with capture
// buffers for both streams.
func newLinter(t *testing.T) (*Linter, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "clusters"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	l := &Linter{
		Paths: &config.Paths{
			Base:     base,
			Bin:      filepath.Join(base, ".bin"),
			Lok8s:    filepath.Join(base, ".lok8s"),
			Clusters: filepath.Join(base, "clusters"),
		},
		Out:    out,
		ErrOut: errOut,
	}
	return l, base, out, errOut
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

func TestSchemaMissingFields(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	writeFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "spec: {}\n")

	if got := l.schema(dir, filepath.Join(dir, "cluster.lok8s.yaml")); got != 3 {
		t.Fatalf("schema errors = %d, want 3", got)
	}
	for _, want := range []string{
		"  Missing required field: kind",
		"  Missing required field: apiVersion",
		"  Missing required field: metadata.name",
		"  Missing spec.kind (cluster runtime type)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
		}
	}
}

func TestApexSubdomainViolation(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	writeFile(t, filepath.Join(base, "clusters", "apex.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	writeFile(t, filepath.Join(base, "clusters", "sub.apex.dev", "cluster.lok8s.yaml"), "kind: Lo\n")

	if l.apex() {
		t.Fatal("apex() = ok, want violation")
	}
	want := "cluster 'sub.apex.dev' is a subdomain of cluster 'apex.dev'"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
	}
}

func TestBootstrapEntryNotFound(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	writeFile(t, spec, "kind: Lo\nspec:\n  bootstrap:\n    - nope\n    - ccm:\n        wait: true\n")

	if got := l.bootstrap(dir, spec, "a.dev"); got != 2 {
		t.Fatalf("bootstrap errors = %d, want 2\nstderr:\n%s", got, errOut.String())
	}
	// The scalar entry is reported in its yq-JSON form (quoted), the map
	// entry as compact JSON — both with the verbatim resolved dir.
	for _, want := range []string{
		`spec.bootstrap entry not found: "nope" (resolved to ` + l.Paths.Lok8s + "/addons/nope)",
		`spec.bootstrap entry not found: {"ccm":{"wait":true}} (resolved to ` + l.Paths.Lok8s + "/addons/ccm)",
	} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
		}
	}
}

func TestBootstrapDefaultCilium(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	spec := filepath.Join(dir, "cluster.lok8s.yaml")
	// Absent spec.bootstrap on a Lo cluster → the per-driver default entry
	// "cilium" (BARE, not JSON-quoted — it comes from an echo, not yq).
	writeFile(t, spec, "kind: Lo\n")

	if got := l.bootstrap(dir, spec, "a.dev"); got != 1 {
		t.Fatalf("bootstrap errors = %d, want 1", got)
	}
	want := "spec.bootstrap entry not found: cilium (resolved to " + l.Paths.Lok8s + "/addons/cilium)"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
	}

	// Explicit empty list = authoritative opt-out: no default, no error.
	writeFile(t, spec, "kind: Lo\nspec:\n  bootstrap: []\n")
	if got := l.bootstrap(dir, spec, "a.dev"); got != 0 {
		t.Fatalf("bootstrap errors with empty list = %d, want 0", got)
	}
}

func TestLabelsQueryMultiDocQuirk(t *testing.T) {
	dir := t.TempDir()

	// Single unlabelled doc → "0" → warns.
	single := filepath.Join(dir, "single.yaml")
	writeFile(t, single, "metadata:\n  name: x\n")
	if got := labelsQuery(single); got != "0" {
		t.Fatalf("labelsQuery(single) = %q, want \"0\"", got)
	}

	// Multi-doc whose FIRST doc carries the label: bash captures "1\n0"
	// (yq prints 1, then errors on doc2, then `|| echo 0`) — NOT "0", so no
	// warning. The quirk is the contract.
	multi := filepath.Join(dir, "multi.yaml")
	writeFile(t, multi, "metadata:\n  labels:\n    lok8s.dev/name: x\n---\nkind: Foo\n")
	if got := labelsQuery(multi); got != "1\n0" {
		t.Fatalf("labelsQuery(multi) = %q, want \"1\\n0\"", got)
	}

	// Labelled single doc → "1".
	labelled := filepath.Join(dir, "labelled.yaml")
	writeFile(t, labelled, "metadata:\n  labels:\n    lok8s.dev/name: x\n")
	if got := labelsQuery(labelled); got != "1" {
		t.Fatalf("labelsQuery(labelled) = %q, want \"1\"", got)
	}
}

func TestServicesImageRegistryExclusive(t *testing.T) {
	l, base, _, errOut := newLinter(t)
	writeFile(t, filepath.Join(base, "services.yaml"),
		"services:\n  app:\n    image: pinned:1\n    registry:\n      endpoint: r\n")

	if l.services() {
		t.Fatal("services() = ok, want error")
	}
	want := "  services.yaml: services.app: 'image' and 'registry' are mutually exclusive"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
	}
}

func TestCompactJSONMatchesYq(t *testing.T) {
	// Rendering contract spots that appear in error messages.
	docs := parseDocs([]byte(`- cilium
- ccm:
    wait: true
    dependsOn: [a, b]
- x: {n: 1.5, s: "q<&>"}
`))
	if len(docs) != 1 {
		t.Fatal("fixture parse failed")
	}
	items := seqItems(docs[0])
	for i, want := range []string{
		`"cilium"`,
		`{"ccm":{"wait":true,"dependsOn":["a","b"]}}`,
		`{"x":{"n":1.5,"s":"q<&>"}}`,
	} {
		if got := compactJSON(items[i]); got != want {
			t.Errorf("compactJSON[%d] = %s, want %s", i, got, want)
		}
	}
}
