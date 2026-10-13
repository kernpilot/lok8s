package yqsem

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func parseYAML(t *testing.T, src string) *yaml.Node {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	return &root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// versionOf walks spec.kubernetes.version with MapGetMerged. It renders the
// result as `yq -r` prints it: the value, or "null" when it is missing.
func versionOf(root *yaml.Node) (string, error) {
	n := root
	for _, key := range []string{"spec", "kubernetes", "version"} {
		var found bool
		var err error
		if n, found, err = MapGetMerged(n, key); err != nil || !found {
			return "null", err
		}
	}
	return Scalar(n), nil
}

// mergeCases are pinned to the pinned yq (TestMapGetMergedMatchesYq): what
// `yq --yaml-fix-merge-anchor-to-spec -r '.spec.kubernetes.version'` prints.
// The yq quirks for a merge value that is not a map are pinned too; the
// readers refuse those (CheckMerges), but MapGetMerged still mirrors yq.
var mergeCases = []struct{ name, src, want string }{
	{"merge supplies the key", "x: &v {version: v1.31.4}\nspec:\n  kubernetes:\n    <<: *v\n", "v1.31.4"},
	{"a local key after the merge wins", "a: &a {version: va}\nspec:\n  kubernetes:\n    <<: *a\n    version: vlocal\n", "vlocal"},
	{"a local key before the merge wins", "a: &a {version: va}\nspec:\n  kubernetes:\n    version: vlocal\n    <<: *a\n", "vlocal"},
	{"a local null wins over the merge", "a: &a {version: va}\nspec:\n  kubernetes:\n    version: ~\n    <<: *a\n", "~"},
	{"the first mapping of a list wins", "a: &a {version: va}\nb: &b {version: vb}\nspec:\n  kubernetes:\n    <<: [*a, *b]\n", "va"},
	{"a list skips the mappings without the key", "a: &a {other: 1}\nb: &b {version: vb}\nspec:\n  kubernetes:\n    <<: [*a, *b]\n", "vb"},
	{"the first of two merge keys wins", "a: &a {version: va}\nb: &b {version: vb}\nspec:\n  kubernetes:\n    <<: *a\n    <<: *b\n", "va"},
	{"an inline merge mapping", "spec:\n  kubernetes:\n    <<: {version: vinline}\n", "vinline"},
	{"a merged mapping resolves its own merge", "a: &a {inner: &i {version: vi}}\nk: &k {<<: *i}\nspec:\n  kubernetes:\n    <<: *k\n", "vi"},
	{"the last duplicate key wins", "spec:\n  kubernetes:\n    version: va\n    version: vb\n", "vb"},
	{"an alias on the path", "k: &k\n  kubernetes: {version: vk}\nspec: *k\n", "vk"},
	{"a merge on the path", "k: &k\n  kubernetes: {version: vk}\nspec:\n  <<: *k\n", "vk"},
	{"a redefined anchor", "k: &k {version: v0}\nk2: &k {version: vk}\nspec:\n  kubernetes: *k\nlater: &k {version: v9}\n", "vk"},
	{"a quoted << is a plain key", "x: &x {version: vx}\nspec:\n  kubernetes:\n    \"<<\": *x\n", "null"},
	{"missing", "spec:\n  kubernetes: {}\n", "null"},
	{"a merge cycle off the path", "k: &k\n  <<: *k\nspec:\n  kubernetes:\n    version: v1\n", "v1"},
	{"yq quirk: a scalar merge is skipped", "spec:\n  kubernetes:\n    <<: v9\n    version: vl\n", "vl"},
	{"yq quirk: a null merge is skipped", "spec:\n  kubernetes:\n    <<: ~\n", "null"},
	{"yq quirk: a list stops at a scalar, from its end", "a: &a {version: va}\nspec:\n  kubernetes:\n    <<: [*a, v1]\n", "null"},
	{"yq quirk: the items after the scalar count", "a: &a {version: va}\nspec:\n  kubernetes:\n    <<: [v1, *a]\n", "va"},
	// yq reads an alias used as a key by its own name, and an aliased `<<`
	// is not a merge key.
	{"an alias key is not resolved", "x: &k version\nspec:\n  kubernetes:\n    *k : v1.31.4\n", "null"},
	{"an alias key matches its own name", "x: &version other\nspec:\n  kubernetes:\n    *version : v1.31.4\n", "v1.31.4"},
	{"an aliased << is a plain key", "m: &m <<\na: &a {version: va}\nspec:\n  kubernetes:\n    *m : *a\n", "null"},
}

func TestMapGetMerged(t *testing.T) {
	for _, tc := range mergeCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := versionOf(parseYAML(t, tc.src))
			if err != nil || got != tc.want {
				t.Fatalf("version = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestMapGetMergedMatchesYq runs the pinned yq on every merge case. It needs
// .bin/yq; it skips without it, and fails without it under CI=true.
func TestMapGetMergedMatchesYq(t *testing.T) {
	yq := filepath.Join(testutil.RepoRoot(t), ".bin", "yq")
	if _, err := os.Stat(yq); err != nil {
		if os.Getenv("CI") == "true" {
			t.Fatalf("the pinned yq is missing under CI: %v", err)
		}
		t.Skip("no .bin/yq (b install)")
	}
	dir := t.TempDir()
	for i, tc := range mergeCases {
		path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".yaml")
		writeFile(t, path, tc.src)
		out, err := exec.CommandContext(t.Context(), yq, "--yaml-fix-merge-anchor-to-spec", "-r", ".spec.kubernetes.version", path).Output()
		if err != nil {
			t.Fatalf("case %d (%s): yq: %v", i, tc.name, err)
		}
		if got := strings.TrimSuffix(string(out), "\n"); got != tc.want {
			t.Errorf("%s: yq prints %q, the case says %q", tc.name, got, tc.want)
		}
	}
}

// doublingChain is a<n> merging a<n-1> twice, down to a0 with the version:
// 2^(n+1)-1 map reads for yq, which does not reuse a read.
func doublingChain(n int) string {
	var b strings.Builder
	b.WriteString("a0: &a0 {version: v1.31.4}\n")
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "a%d: &a%d {<<: [*a%d, *a%d]}\n", i, i, i-1, i-1)
	}
	fmt.Fprintf(&b, "spec:\n  kubernetes:\n    <<: *a%d\n", n)
	return b.String()
}

// TestMapGetMergedReusesReads: a mapping that many merge keys share is
// read once, so a doubling chain of 30 mappings (2^31 paths) is read in a
// few hundred map reads, not two billion.
func TestMapGetMergedReusesReads(t *testing.T) {
	root := parseYAML(t, doublingChain(30))
	start := time.Now()
	got, err := versionOf(root)
	if err != nil || got != "v1.31.4" {
		t.Fatalf("version = %q, %v; want v1.31.4", got, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the read took %v: the shared mappings are read again", d)
	}
}

func TestMergeReads(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		limit     int
		want      int
		err       error
	}{
		{"no merge", "spec:\n  kubernetes: {version: v1}\n", 100, 1, nil},
		{"one merge", "a: &a {version: v1}\nspec:\n  kubernetes:\n    <<: *a\n", 100, 2, nil},
		{"a list counts each item", "a: &a {x: 1}\nspec:\n  kubernetes:\n    <<: [*a, *a, {y: 2}]\n", 100, 4, nil},
		{"a nested merge", "a: &a {x: 1}\nb: &b {<<: *a}\nspec:\n  kubernetes:\n    <<: [*b, *a]\n", 100, 4, nil},
		{"a doubling chain of 5 (spec.kubernetes + 63)", doublingChain(5), 100, 64, nil},
		{"a doubling chain of 30 stops at the limit", doublingChain(30), 10000, 10001, nil},
		{"a cycle", "k: &k\n  <<: *k\nspec:\n  kubernetes:\n    <<: *k\n", 100, 0, ErrMergeCycle},
		// The count visits every merge key: a cycle behind a chain that
		// already passed the limit is still a cycle.
		{"a cycle behind the limit", strings.Replace(doublingChain(20), "spec:\n  kubernetes:\n    <<: *a20\n", "k: &k\n  <<: *k\nspec:\n  kubernetes:\n    <<: [*a20, *k]\n", 1), 10, 0, ErrMergeCycle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k8s, _, err := MapGetMerged(Lookup(parseYAML(t, tc.src), "spec"), "kubernetes")
			if err != nil {
				t.Fatal(err)
			}
			got, err := MergeReads(k8s, tc.limit)
			if !errors.Is(err, tc.err) || (tc.err == nil && got != tc.want) {
				t.Fatalf("MergeReads = %d, %v; want %d, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestMapGetMergedCycle(t *testing.T) {
	for _, src := range []string{
		"k: &k\n  <<: *k\nspec:\n  kubernetes:\n    <<: *k\n    version: v1\n",
		"k: &k\n  <<: {<<: *k}\nspec:\n  <<: *k\n",
		"k: &k\n  <<: [*k]\nspec:\n  kubernetes:\n    <<: *k\n",
	} {
		if _, err := versionOf(parseYAML(t, src)); !errors.Is(err, ErrMergeCycle) {
			t.Errorf("%q: err = %v, want ErrMergeCycle", src, err)
		}
	}
}

func TestCheckMerges(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      error
	}{
		{"no merge", "spec: {kubernetes: {version: v1}}\n", nil},
		{"a mapping", "a: &a {version: v1}\nspec:\n  kubernetes:\n    <<: *a\n", nil},
		{"a list of mappings", "a: &a {x: 1}\nb: &b {y: 2}\nspec:\n  <<: [*a, *b]\n", nil},
		{"an alias to a list of mappings", "a: &a {x: 1}\nl: &l [*a, {y: 2}]\nspec:\n  <<: *l\n", nil},
		{"an empty list", "spec:\n  <<: []\n", nil},
		{"an alias cycle without a merge", "a: &a\n  b: *a\nspec: {}\n", nil},
		// A cycle is the read's part (MapGetMerged): one off the path costs
		// nothing and is not an error.
		{"a merge cycle", "k: &k\n  <<: *k\n", nil},
		{"a scalar", "s: &s v1\nspec:\n  kubernetes:\n    <<: *s\n", ErrMergeValue},
		{"an inline scalar", "spec:\n  kubernetes:\n    <<: v1\n", ErrMergeValue},
		{"a null", "spec:\n  kubernetes:\n    <<: ~\n", ErrMergeValue},
		{"a list with a scalar", "a: &a {x: 1}\nspec:\n  <<: [*a, v1]\n", ErrMergeValue},
		{"a list with a list", "a: &a {x: 1}\nspec:\n  <<: [*a, [1]]\n", ErrMergeValue},
		// The readers refuse a merge key that yq would skip anywhere in the file.
		{"a bad merge outside the path", "s: &s v1\nmetadata:\n  <<: *s\nspec:\n  kubernetes:\n    version: v1\n", ErrMergeValue},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CheckMerges(parseYAML(t, tc.src)); !errors.Is(got, tc.want) {
				t.Fatalf("CheckMerges = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLoadDocsCountsLikeYq: the document count that `yq ea '[.] | length'`
// prints, for every input with more than one document.
func TestLoadDocsCountsLikeYq(t *testing.T) {
	for _, tc := range []struct {
		src  string
		want int
	}{
		{"", 0}, // yq: 1 (one empty document); both are "not more than one"
		{"# only a comment\n", 0},
		{"a: 1\n", 1},
		{"---\na: 1\n", 1},
		{"a: 1\n...\n", 1},
		{"a: 1\n---\n", 2},
		{"a: 1\n---\n# a comment\n", 2},
		{"a: 1\n---\nb: 2\n", 2},
		{"---\n---\na: 1\n", 2},
	} {
		path := filepath.Join(t.TempDir(), "f.yaml")
		writeFile(t, path, tc.src)
		docs, err := LoadDocs(path)
		if err != nil || len(docs) != tc.want {
			t.Errorf("%q: %d documents, %v; want %d", tc.src, len(docs), err, tc.want)
		}
	}
	if _, err := LoadDocs(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("a missing file loaded")
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	writeFile(t, bad, "a: 1\n---\nb: *nope\n")
	if _, err := LoadDocs(bad); err == nil {
		t.Error("a parse error in the second document loaded")
	}
}
