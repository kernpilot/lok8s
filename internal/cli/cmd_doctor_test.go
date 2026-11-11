package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// TestDoctorReportsPathSecretsAsItIs: the binary defaults PATH_SECRETS to
// nothing (every store is per domain), so doctor says so when the variable
// is unset and prints the directory when it is set (D35).
func TestDoctorReportsPathSecretsAsItIs(t *testing.T) {
	p := synthProject(t)
	t.Setenv("PATH", t.TempDir())
	p.SecretsEnv = ""
	stdout, _, _ := runLo(t, NewRoot(p), "doctor")
	if !strings.Contains(stdout, "ℹ PATH_SECRETS unset (per-domain stores; the flat store is retired)") {
		t.Errorf("unset: no info line:\n%s", stdout)
	}
	if strings.Contains(stdout, ".secrets") {
		t.Errorf("unset: doctor still names a flat store:\n%s", stdout)
	}

	p.SecretsEnv = p.Clusters
	stdout, _, _ = runLo(t, NewRoot(p), "doctor")
	if !strings.Contains(stdout, " PATH_SECRETS="+p.Clusters+"\n") {
		t.Errorf("set: no directory line:\n%s", stdout)
	}
}

// The git worktree case (kubehz.in.net recreate, 2026-10-23): argsh is on
// PATH (the main checkout's .bin), but the project's own .bin/argsh, the one
// every bash child sources, is missing. `lo recover --dry-run` failed in the
// provider plugin. Doctor names the file and the fix, and fails when the
// active domain's provider plugin needs it. The provider section does not
// start a bash child that cannot source argsh.
func TestDoctorNamesTheMissingProjectArgsh(t *testing.T) {
	p := synthProject(t)
	elsewhere := t.TempDir()
	testutil.WriteFile(t, filepath.Join(elsewhere, "argsh"), "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(elsewhere, "argsh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", elsewhere)
	testutil.WriteFile(t, filepath.Join(p.Bin, "b.yaml"), "binaries:\n  argsh: {}\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "metal.dev", "cluster.lok8s.yaml"),
		"kind: KubeOne\nmetadata:\n  name: metal\nspec:\n  provider:\n    name: hetzner\n")

	stdout, _, _ := runLo(t, NewRoot(p), "doctor", "--domain", "metal.dev")
	if !strings.Contains(stdout, "✓ argsh — argsh runtime") {
		t.Fatalf("the fixture has no argsh on PATH:\n%s", stdout)
	}
	want := "✗ bash mode (spec.implementation, provider plugins): not runnable: argsh missing at .bin/argsh; metal.dev uses provider hetzner, a bash plugin (fix: b install)\n"
	if !strings.Contains(stdout, want) {
		t.Errorf("no failure line for the missing .bin/argsh, want %q in:\n%s", want, stdout)
	}
	if !strings.Contains(stdout, "! provider section skipped: argsh missing at .bin/argsh (fix: b install)\n") {
		t.Errorf("the provider section did not name the missing argsh:\n%s", stdout)
	}

	var out strings.Builder
	if doctorBashMode(&out, p, "metal.dev") {
		t.Error("a provider-backed domain without .bin/argsh passes the bash-mode check")
	}
	out.Reset()
	if !doctorBashMode(&out, p, "") || !strings.Contains(out.String(), "! bash mode (spec.implementation, provider plugins): not runnable: argsh missing at .bin/argsh (fix: b install)\n") {
		t.Errorf("without a provider the missing argsh is a warning, got:\n%s", out.String())
	}

	testutil.WriteFile(t, filepath.Join(p.Bin, "argsh"), "#!/usr/bin/env bash\n")
	out.Reset()
	if !doctorBashMode(&out, p, "metal.dev") || strings.Contains(out.String(), "not runnable") {
		t.Errorf("argsh present, got:\n%s", out.String())
	}
}
