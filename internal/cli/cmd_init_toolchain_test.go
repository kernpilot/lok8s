package cli

// cmd_init_toolchain_test.go — `lo init toolchain` resolves its default
// project from where the user stands, never from the ambient PATH_BASE
// (the leak class `lo init project` closed). Dry-run only: nothing is
// written, downloaded or run.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitToolchainDefaultsToTheProjectAboveCwdNotPathBase(t *testing.T) {
	// The ambient project (what an exported PATH_BASE resolves to).
	ambient := synthProject(t)
	t.Setenv("PATH_BASE", ambient.Base)

	// The project the user stands in: a kind: Project marker above a
	// nested working directory.
	project := t.TempDir()
	writeFile(t, filepath.Join(project, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: standing\n")
	nested := filepath.Join(project, "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	stdout, stderr, err := runLo(t, NewRoot(ambient), "init", "toolchain", "--dry-run")
	if err != nil {
		t.Fatalf("init toolchain --dry-run: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "lo init toolchain — "+project+" (") {
		t.Errorf("did not resolve the project above cwd:\n%s", stdout)
	}
	if strings.Contains(stdout, ambient.Base) {
		t.Errorf("ambient PATH_BASE leaked into the target:\n%s", stdout)
	}
	if !strings.Contains(stdout, "would ensure .gitignore entries in "+filepath.Join(project, ".gitignore")) {
		t.Errorf("dry-run steps not against the standing project:\n%s", stdout)
	}
	for _, dir := range []string{project, ambient.Base} {
		if _, err := os.Stat(filepath.Join(dir, ".bin", "b.yaml")); err == nil {
			t.Errorf("dry run wrote %s/.bin/b.yaml", dir)
		}
	}
}

func TestInitToolchainPathFlagWins(t *testing.T) {
	ambient := synthProject(t)
	t.Setenv("PATH_BASE", ambient.Base)
	standing := t.TempDir()
	writeFile(t, filepath.Join(standing, "lok8s.yaml"), "kind: Project\n")
	t.Chdir(standing)
	target := t.TempDir()

	stdout, stderr, err := runLo(t, NewRoot(ambient), "init", "toolchain", "--dry-run", "--path", target)
	if err != nil {
		t.Fatalf("init toolchain --path: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "lo init toolchain — "+target+" (") {
		t.Errorf("--path did not win:\n%s", stdout)
	}
}

// Outside any project the default is the working directory itself.
func TestInitToolchainFallsBackToCwd(t *testing.T) {
	ambient := synthProject(t)
	t.Setenv("PATH_BASE", ambient.Base)
	bare := t.TempDir()
	t.Chdir(bare)
	stdout, _, err := runLo(t, NewRoot(ambient), "init", "toolchain", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "lo init toolchain — "+bare+" (") {
		t.Errorf("no-marker fallback is not cwd:\n%s", stdout)
	}
}
