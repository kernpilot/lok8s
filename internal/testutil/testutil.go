// Package testutil holds the fixture helpers the package tests share: the
// repo root (for tests that read the checked-in tree) and the
// mkdir-then-write fixture writer. Test-only by convention — nothing outside
// *_test.go imports it.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

// RepoRoot walks up from the test's working directory to the repository
// root — the directory that carries the frozen bash tree (`.lok8s/lo`).
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; {
		if _, err := os.Stat(filepath.Join(d, ".lok8s", "lo")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatalf("repo root not found from %s", dir)
		}
		d = parent
	}
}

// WriteFile writes a fixture, creating its directory first; any failure
// fails the test.
func WriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
