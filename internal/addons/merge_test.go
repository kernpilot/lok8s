package addons

// merge_test.go covers the value merge: lists replace, an empty overlay
// keeps the base.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeNodesListsReplace(t *testing.T) {
	t.Parallel()
	// yq `*`: maps deep-merge, LISTS REPLACE (right wins) — the semantics
	// every value stack in the pipeline depends on.
	out, err := MergeYAML("a:\n  - 1\n  - 2\nkeep: x\n", "a:\n  - 3\n")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	yaml.Unmarshal(out, &m)
	list, _ := m["a"].([]any)
	if len(list) != 1 || fmt.Sprint(list[0]) != "3" {
		t.Errorf("list did not replace: %v", m["a"])
	}
	if m["keep"] != "x" {
		t.Errorf("unrelated key lost: %v", m)
	}
}

// TestMergeNodesEmptyOverlayKeepsBase: an empty or comment-only overlay
// file decodes to a zero node. yq reads no document from it, so the value
// stack must come through unchanged instead of collapsing to nothing.
func TestMergeNodesEmptyOverlayKeepsBase(t *testing.T) {
	t.Parallel()
	for name, overlay := range map[string]string{
		"empty":        "",
		"comment-only": "# nothing here\n",
	} {
		t.Run(name, func(t *testing.T) {
			out, err := MergeYAML("a:\n  b: 1\nkeep: x\n", overlay)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			if m["keep"] != "x" {
				t.Errorf("base lost after an empty overlay: %q", out)
			}
			inner, _ := m["a"].(map[string]any)
			if fmt.Sprint(inner["b"]) != "1" {
				t.Errorf("nested base lost after an empty overlay: %q", out)
			}
		})
	}
	// The same through the file path the addon value stack takes.
	dir := t.TempDir()
	base := filepath.Join(dir, "values.yaml")
	empty := filepath.Join(dir, "values.lo.yaml")
	if err := os.WriteFile(base, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, []byte("# overlay left empty on purpose\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := MergeValueFiles(base, empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "a: 1") {
		t.Errorf("MergeValueFiles lost the base behind an empty overlay: %q", out)
	}
}
