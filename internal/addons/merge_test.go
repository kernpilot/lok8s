package addons

// merge_test.go covers the value merge: lists replace, an empty overlay
// keeps the base, and the document-level rules match the pinned yq.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
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

// mergeCase is one `yq eval-all '. as $item ireduce ({}; . * $item)'` run
// over the given streams, with the result the pinned .bin/yq (v4.53.3)
// printed for it. want is compared as YAML data (yq and yaml.v3 format the
// same data differently); wantErr is yq's own error line.
type mergeCase struct {
	name    string
	streams []string
	want    string
	wantErr string
}

const (
	mergeBase  = "a:\n  b: 1\nc: [1, 2]\n"
	mergeMulti = "c: [9]\n---\na:\n  d: 2\n"
)

var mergeCases = []mergeCase{
	// A whole-document null on the right is a no-op.
	{name: "null overlay", streams: []string{mergeBase, "null\n"}, want: mergeBase},
	{name: "tilde overlay", streams: []string{mergeBase, "~\n"}, want: mergeBase},
	{name: "empty document overlay", streams: []string{"a: 1\n---\n"}, want: "a: 1\n"},
	// A null on the left of the fold ({} * null = {}) changes nothing.
	{name: "null first", streams: []string{"null\n", mergeBase}, want: mergeBase},
	{name: "null alone", streams: []string{"null\n"}, want: "{}\n"},
	// A non-map document on either side is yq's multiply error.
	{name: "seq overlay", streams: []string{mergeBase, "- 9\n"}, wantErr: "cannot multiply !!map with !!seq"},
	{name: "seq first", streams: []string{"- 9\n", mergeBase}, wantErr: "cannot multiply !!map with !!seq"},
	{name: "seq alone", streams: []string{"- 9\n"}, wantErr: "cannot multiply !!map with !!seq"},
	{name: "string overlay", streams: []string{mergeBase, "x\n"}, wantErr: "cannot multiply !!map with !!str"},
	{name: "bool overlay", streams: []string{mergeBase, "true\n"}, wantErr: "cannot multiply !!map with !!bool"},
	{name: "int overlay", streams: []string{mergeBase, "3\n"}, wantErr: "cannot multiply !!map with !!int"},
	// eval-all merges EVERY document of a stream, in order.
	{name: "multi-document overlay", streams: []string{mergeBase, mergeMulti}, want: "a:\n  b: 1\n  d: 2\nc: [9]\n"},
	{name: "multi-document alone", streams: []string{mergeMulti}, want: "c: [9]\na:\n  d: 2\n"},
	{name: "null document in the middle", streams: []string{"a: {b: 1}\n---\nnull\n---\nc: 2\n"}, want: "a:\n  b: 1\nc: 2\n"},
	// The nested rules stay: a nested null or sequence REPLACES.
	{name: "nested null replaces", streams: []string{mergeBase, "a: null\n"}, want: "a: null\nc: [1, 2]\n"},
	{name: "nested seq replaces", streams: []string{mergeBase, "a: [7]\n"}, want: "a: [7]\nc: [1, 2]\n"},
	{name: "nested scalar replaces", streams: []string{mergeBase, "a: 5\n"}, want: "a: 5\nc: [1, 2]\n"},
}

// yamlData decodes YAML into plain data for a formatting-independent
// comparison.
func yamlData(t *testing.T, raw []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return v
}

// TestMergeMatchesPinnedYQ pins the document-level rules of the yq merge
// idiom: a null document is a no-op, a non-map document is an error, every
// document of a stream takes part. The results are the pinned yq's; with
// the toolchain installed the same runs go through .bin/yq as well.
func TestMergeMatchesPinnedYQ(t *testing.T) {
	t.Parallel()
	yq := filepath.Join(testutil.RepoRoot(t), ".bin", "yq")
	if info, err := os.Stat(yq); err != nil || info.Mode()&0o111 == 0 {
		if os.Getenv("CI") != "" {
			t.Fatal(".bin/yq is not installed; on CI the toolchain must be installed before the Go tests")
		}
		t.Log(".bin/yq is not installed (b install); the pinned results stand alone")
		yq = ""
	}
	for _, tc := range mergeCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			paths := make([]string, 0, len(tc.streams))
			for i, s := range tc.streams {
				p := filepath.Join(dir, fmt.Sprintf("%d.yaml", i))
				if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
					t.Fatal(err)
				}
				paths = append(paths, p)
			}
			viaFiles, errFiles := MergeValueFiles(paths...)
			viaStrings, errStrings := MergeYAML(tc.streams...)
			for name, got := range map[string]struct {
				out []byte
				err error
			}{"MergeValueFiles": {viaFiles, errFiles}, "MergeYAML": {viaStrings, errStrings}} {
				if tc.wantErr != "" {
					if got.err == nil || !strings.Contains(got.err.Error(), tc.wantErr) {
						t.Errorf("%s: err = %v, want %q", name, got.err, tc.wantErr)
					}
					continue
				}
				if got.err != nil {
					t.Fatalf("%s: %v", name, got.err)
				}
				if want, have := yamlData(t, []byte(tc.want)), yamlData(t, got.out); !reflect.DeepEqual(want, have) {
					t.Errorf("%s = %q, want %q", name, got.out, tc.want)
				}
			}
			if yq == "" {
				return
			}
			// The same run through the pinned binary, the way the bash did it.
			args := append([]string{"eval-all", ". as $item ireduce ({}; . * $item)"}, paths...)
			cmd := exec.Command(yq, args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(stderr.String(), "Error: "+tc.wantErr) {
					t.Errorf("yq: err = %v, stderr = %q, want %q", err, stderr.String(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("yq: %v: %s", err, stderr.String())
			}
			if want, have := yamlData(t, []byte(tc.want)), yamlData(t, out); !reflect.DeepEqual(want, have) {
				t.Errorf("yq printed %q, the pinned result is %q", out, tc.want)
			}
		})
	}
}
