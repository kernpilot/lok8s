package lint

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
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

func TestSchemaMissingFields(t *testing.T) {
	t.Parallel()
	l, base, _, errOut := newLinter(t)
	dir := filepath.Join(base, "clusters", "a.dev")
	testutil.WriteFile(t, filepath.Join(dir, "cluster.lok8s.yaml"), "spec: {}\n")

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
	t.Parallel()
	l, base, _, errOut := newLinter(t)
	testutil.WriteFile(t, filepath.Join(base, "clusters", "apex.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	testutil.WriteFile(t, filepath.Join(base, "clusters", "sub.apex.dev", "cluster.lok8s.yaml"), "kind: Lo\n")

	if l.apex() {
		t.Fatal("apex() = ok, want violation")
	}
	want := "cluster 'sub.apex.dev' is a subdomain of cluster 'apex.dev'"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr missing %q; got:\n%s", want, errOut.String())
	}
}

func TestLabelsQueryMultiDocQuirk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Single unlabelled doc → "0" → warns.
	single := filepath.Join(dir, "single.yaml")
	testutil.WriteFile(t, single, "metadata:\n  name: x\n")
	if got := labelsQuery(single); got != "0" {
		t.Fatalf("labelsQuery(single) = %q, want \"0\"", got)
	}

	// Multi-doc whose FIRST doc carries the label: bash captures "1\n0"
	// (yq prints 1, then errors on doc2, then `|| echo 0`) — NOT "0", so no
	// warning. The quirk is the contract.
	multi := filepath.Join(dir, "multi.yaml")
	testutil.WriteFile(t, multi, "metadata:\n  labels:\n    lok8s.dev/name: x\n---\nkind: Foo\n")
	if got := labelsQuery(multi); got != "1\n0" {
		t.Fatalf("labelsQuery(multi) = %q, want \"1\\n0\"", got)
	}

	// Labelled single doc → "1".
	labelled := filepath.Join(dir, "labelled.yaml")
	testutil.WriteFile(t, labelled, "metadata:\n  labels:\n    lok8s.dev/name: x\n")
	if got := labelsQuery(labelled); got != "1" {
		t.Fatalf("labelsQuery(labelled) = %q, want \"1\"", got)
	}
}
