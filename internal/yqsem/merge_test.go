package yqsem

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The expectations below are what the pinned yq (v4.53) prints for
// `yq --yaml-fix-merge-anchor-to-spec 'explode(.) | .spec.kubernetes.version'`
// on the same input (the bash reader spec::kind_node_version runs that).

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

// versionOf walks spec.kubernetes.version with MapGetMerged.
func versionOf(root *yaml.Node) (string, bool) {
	n := root
	for _, key := range []string{"spec", "kubernetes", "version"} {
		var ok bool
		if n, ok = MapGetMerged(n, key); !ok {
			return "", false
		}
	}
	return Scalar(n), true
}

func TestMapGetMerged(t *testing.T) {
	const anchors = "a: &a {version: va}\nb: &b {version: vb}\n"
	for _, tc := range []struct {
		name, src, want string
		found           bool
	}{
		{"merge supplies the key", "x: &v {version: v1.31.4}\nspec:\n  kubernetes:\n    <<: *v\n", "v1.31.4", true},
		{"a local key after the merge wins", anchors + "spec:\n  kubernetes:\n    <<: *a\n    version: vlocal\n", "vlocal", true},
		{"a local key before the merge wins", anchors + "spec:\n  kubernetes:\n    version: vlocal\n    <<: *a\n", "vlocal", true},
		{"a local null wins over the merge", anchors + "spec:\n  kubernetes:\n    version: ~\n    <<: *a\n", "~", true},
		{"the first mapping of a list wins", anchors + "spec:\n  kubernetes:\n    <<: [*a, *b]\n", "va", true},
		{"a list skips the mappings without the key", "a: &a {other: 1}\nb: &b {version: vb}\nspec:\n  kubernetes:\n    <<: [*a, *b]\n", "vb", true},
		{"the first of two merge keys wins", anchors + "spec:\n  kubernetes:\n    <<: *a\n    <<: *b\n", "va", true},
		{"an inline merge mapping", "spec:\n  kubernetes:\n    <<: {version: vinline}\n", "vinline", true},
		{"a merged mapping resolves its own merge", "a: &a {inner: &i {version: vi}}\nk: &k {<<: *i}\nspec:\n  kubernetes:\n    <<: *k\n", "vi", true},
		{"the last duplicate key wins", "spec:\n  kubernetes:\n    version: va\n    version: vb\n", "vb", true},
		{"an alias on the path", "k: &k\n  kubernetes: {version: vk}\nspec: *k\n", "vk", true},
		{"a merge on the path", "k: &k\n  kubernetes: {version: vk}\nspec:\n  <<: *k\n", "vk", true},
		{"a quoted << is a plain key", "x: &x {version: vx}\nspec:\n  kubernetes:\n    \"<<\": *x\n", "", false},
		{"missing", "spec:\n  kubernetes: {}\n", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := versionOf(parseYAML(t, tc.src))
			if got != tc.want || found != tc.found {
				t.Fatalf("version = %q, found %v; want %q, %v", got, found, tc.want, tc.found)
			}
		})
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
		{"an empty list", "spec:\n  <<: []\n", nil},
		{"an alias cycle without a merge", "a: &a\n  b: *a\nspec: {}\n", nil},
		{"a scalar", "s: &s v1\nspec:\n  kubernetes:\n    <<: *s\n", ErrMergeValue},
		{"a null", "spec:\n  kubernetes:\n    <<: ~\n", ErrMergeValue},
		{"a list with a scalar", "a: &a {x: 1}\nspec:\n  <<: [*a, v1]\n", ErrMergeValue},
		// explode(.) checks the whole document, not only spec.kubernetes.
		{"a bad merge outside the path", "s: &s v1\nmetadata:\n  <<: *s\nspec:\n  kubernetes:\n    version: v1\n", ErrMergeValue},
		{"a merge cycle", "k: &k\n  <<: *k\n", ErrMergeCycle},
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
