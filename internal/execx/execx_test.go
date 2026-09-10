package execx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

// writeExec drops an executable stub named tool into dir.
func writeExec(t *testing.T, dir, tool string) string {
	t.Helper()
	path := filepath.Join(dir, tool)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho "+dir+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Look resolves the project's b-managed .bin BEFORE PATH: the pinned
// toolchain wins over whatever the developer has installed.
func TestLookPrefersTheProjectBinOverPATH(t *testing.T) {
	bin, onPath := t.TempDir(), t.TempDir()
	want := writeExec(t, bin, "tool")
	writeExec(t, onPath, "tool")
	t.Setenv("PATH", onPath)
	got, ok := Look(&config.Paths{Bin: bin}, "tool")
	if !ok || got != want {
		t.Fatalf("Look = %q, %v; want %q", got, ok, want)
	}
}

// A .bin entry that is not an executable file (a directory, a 0644 file) is
// skipped and PATH answers; nothing anywhere reports ok=false.
func TestLookSkipsNonExecutableBinEntriesAndReportsMissing(t *testing.T) {
	bin, onPath := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(bin, "dirtool"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "plain"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fromPath := writeExec(t, onPath, "dirtool")
	writeExec(t, onPath, "plain")
	t.Setenv("PATH", onPath)
	p := &config.Paths{Bin: bin}
	if got, ok := Look(p, "dirtool"); !ok || got != fromPath {
		t.Fatalf("directory in .bin: Look = %q, %v; want the PATH hit %q", got, ok, fromPath)
	}
	if got, ok := Look(p, "plain"); !ok || got != filepath.Join(onPath, "plain") {
		t.Fatalf("0644 in .bin: Look = %q, %v; want the PATH hit", got, ok)
	}
	if got, ok := Look(p, "absent"); ok || got != "" {
		t.Fatalf("absent tool: Look = %q, %v; want \"\", false", got, ok)
	}
}
