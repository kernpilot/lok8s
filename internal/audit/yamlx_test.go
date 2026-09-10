package audit

// yamlx_test.go covers the yq-shaped YAML helpers: the merge semantics
// and the literal-preserving render.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

func TestMergeYAMLDocsSemantics(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f1 := filepath.Join(dir, "1.yaml")
	f2 := filepath.Join(dir, "2.yaml")
	writeFileT(t, f1, "a: true\nlist: [a, b]\nm:\n  x: 1\n")
	writeFileT(t, f2, "a: false\nlist: [c]\nm:\n  y: 2\n")
	merged, err := mergeYAMLDocs([]string{f1, f2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v := yqRenderNode(yqsem.Lookup(merged, "a")); v != "false" {
		t.Errorf("scalar replace: %q", v)
	}
	if v := yqRenderNode(yqsem.Lookup(merged, "m", "x")); v != "1" {
		t.Errorf("maps must deep-merge: %q", v)
	}
	if v := yqRenderNode(yqsem.Lookup(merged, "m", "y")); v != "2" {
		t.Errorf("maps must deep-merge: %q", v)
	}
	if n := yqsem.Lookup(merged, "list"); n == nil || len(n.Content) != 1 {
		t.Errorf("lists must REPLACE, not concatenate")
	}

	// A scalar document is a merge ERROR (yq: cannot multiply !!map with
	// !!str) — the caller renders unknown, never a fall-through pass.
	f3 := filepath.Join(dir, "3.yaml")
	writeFileT(t, f3, "just a string\n")
	if _, err := mergeYAMLDocs([]string{f1, f3}, nil); err == nil {
		t.Error("scalar doc must fail the merge")
	}

	// A null document is a no-op.
	f4 := filepath.Join(dir, "4.yaml")
	writeFileT(t, f4, "null\n")
	merged, err = mergeYAMLDocs([]string{f1, f4}, nil)
	if err != nil || yqRenderNode(yqsem.Lookup(merged, "a")) != "true" {
		t.Errorf("null doc must be a no-op (err=%v)", err)
	}
}

func TestYqRenderPreservesLiterals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "s.yaml")
	writeFileT(t, f, "a: ~\nb: True\nc: \"true\"\nd: 1.0\n")
	doc := firstDocNode(f)
	for k, want := range map[string]string{"a": "~", "b": "True", "c": "true", "d": "1.0"} {
		if got := yqRenderNode(yqsem.Lookup(doc, k)); got != want {
			t.Errorf("%s renders %q, want %q (yq preserves scalar style)", k, got, want)
		}
	}
	if got := yqRenderNode(yqsem.Lookup(doc, "missing")); got != "null" {
		t.Errorf("absent key renders %q, want null", got)
	}
	if strings.TrimSpace(altNode(yqsem.Lookup(doc, "a"), "def")) != "def" {
		t.Errorf("`//` must treat null as empty")
	}
}
