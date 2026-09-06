package kubehz

// manifests_test.go pins the embedded manifest tree to the bash tree
// (.lok8s/libs/kubehz/manifests/**) byte for byte, in BOTH directions: a
// file edited on one side without the other fails here. The digest-pin and
// RBAC least-privilege gates of kubehz_live_agent_test.bats keep running
// against the bash tree, and this equality is what extends them to the
// embedded copy.

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestEmbeddedManifestsMatchBashTree(t *testing.T) {
	bashTree := filepath.Join(repoRoot(t), ".lok8s", "libs", "kubehz", "manifests")
	if _, err := os.Stat(bashTree); err != nil {
		t.Skipf("bash tree not present: %v", err)
	}
	var embedded []string
	if err := fs.WalkDir(Manifests(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			embedded = append(embedded, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	if err := filepath.WalkDir(bashTree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(bashTree, path)
			onDisk = append(onDisk, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(embedded)
	sort.Strings(onDisk)
	if len(embedded) != len(onDisk) {
		t.Fatalf("file lists differ:\n embedded %v\n on disk  %v", embedded, onDisk)
	}
	for i := range embedded {
		if embedded[i] != onDisk[i] {
			t.Fatalf("file lists differ at %d: %s vs %s", i, embedded[i], onDisk[i])
		}
		got, _ := fs.ReadFile(Manifests(), embedded[i])
		want, _ := os.ReadFile(filepath.Join(bashTree, filepath.FromSlash(onDisk[i])))
		if string(got) != string(want) {
			t.Fatalf("%s: embedded bytes differ from .lok8s/libs/kubehz/manifests", embedded[i])
		}
	}
	for _, must := range []string{"agent/cronjob.yaml", "live-agent/UPSTREAM.sha256", "live-agent/managed/rbac-managed.yaml"} {
		if _, err := fs.Stat(Manifests(), must); err != nil {
			t.Fatalf("%s missing from the embed", must)
		}
	}
}

func TestEmbeddedManifestsCarryOnlyTheThreePlaceholders(t *testing.T) {
	want := map[string]int{"KUBEHZ_API_URL_PLACEHOLDER": 2, "CLUSTER_ID_PLACEHOLDER": 2, "HEARTBEAT_OWNER_PLACEHOLDER": 1}
	got := map[string]int{}
	_ = fs.WalkDir(Manifests(), ".", func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		raw, _ := fs.ReadFile(Manifests(), path)
		for token := range want {
			got[token] += countNonComment(string(raw), token)
		}
		return nil
	})
	for token, n := range want {
		if got[token] != n {
			t.Fatalf("%s: %d occurrences, want %d", token, got[token], n)
		}
	}
}

func countNonComment(s, token string) int {
	n := 0
	for _, line := range splitLines(s) {
		trimmed := trimLeftSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '#' {
			continue
		}
		if contains(line, token) {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func trimLeftSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
