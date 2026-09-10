package execx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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

// Run: Env entries are APPENDED to the inherited environment (the parent's
// variables survive), Dir is the child's working directory, and a Name
// carrying a path separator is used as-is instead of resolved.
func TestRunMergesEnvAndSetsDirAndUsesPathsAsIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	t.Setenv("LO_EXECX_INHERITED", "kept")
	var out bytes.Buffer
	err := NewRunner(&config.Paths{Bin: t.TempDir()}).Run(context.Background(), Cmd{
		Name:   "/bin/sh",
		Args:   []string{"-c", `pwd; printf '%s %s\n' "$LO_EXECX_INHERITED" "$LO_EXECX_ADDED"`},
		Dir:    dir,
		Env:    []string{"LO_EXECX_ADDED=added"},
		Stdout: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output: %q", out.String())
	}
	if got, _ := filepath.EvalSymlinks(lines[0]); got != mustEval(t, dir) {
		t.Errorf("pwd = %q, want %q", lines[0], dir)
	}
	if lines[1] != "kept added" {
		t.Errorf("env line = %q, want %q", lines[1], "kept added")
	}
}

// A bare tool name is resolved through Look; a miss is an error naming the
// tool, before anything runs.
func TestRunReportsAnUnresolvableTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := NewRunner(&config.Paths{Bin: t.TempDir()}).Run(context.Background(), Cmd{Name: "lo-execx-absent"})
	if err == nil || !strings.Contains(err.Error(), "lo-execx-absent: executable not found") {
		t.Fatalf("err = %v", err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A cancelled context stops the child with SIGINT so it can clean up and
// report its own exit code; the default SIGKILL would end it at once.
func TestRunCancelSendsInterruptToTheChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRunner(nil)
	var out bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- r.Run(ctx, Cmd{
			Name:   "/bin/sh",
			Args:   []string{"-c", `trap 'echo caught; exit 3' INT; echo up; while :; do sleep 0.05; done`},
			Stdout: &out,
			Stderr: &out,
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "up") {
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errCh:
		if ExitCode(err) != 3 || !strings.Contains(out.String(), "caught") {
			t.Errorf("child was not interrupted gracefully: rc=%d err=%v out=%q", ExitCode(err), err, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("child did not stop after the cancel")
	}
}
