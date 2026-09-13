package cli

// cmd_toolchain_test.go — `lo toolchain install` resolves its default
// project from where the user stands, never from the ambient PATH_BASE
// (the leak class `lo init project` closed); `lo init toolchain` is its
// hidden alias for one release; `lo toolchain doctor` is the pinned-tools
// section on its own. Dry-run only: nothing is written, downloaded or run.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

func TestToolchainInstallDefaultsToTheProjectAboveCwdNotPathBase(t *testing.T) {
	// The ambient project (what an exported PATH_BASE resolves to).
	ambient := synthProject(t)
	t.Setenv("PATH_BASE", ambient.Base)

	// The project the user stands in: a kind: Project marker above a
	// nested working directory.
	project := t.TempDir()
	testutil.WriteFile(t, filepath.Join(project, "lok8s.yaml"), "apiVersion: lok8s.dev/v1\nkind: Project\nmetadata:\n  name: standing\n")
	nested := filepath.Join(project, "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)

	stdout, stderr, err := runLo(t, NewRoot(ambient), "toolchain", "install", "--dry-run")
	if err != nil {
		t.Fatalf("toolchain install --dry-run: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "lo toolchain install — "+project+" (") {
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
	if stderr != "" {
		t.Errorf("the real command printed on stderr: %q", stderr)
	}
}

// The bash group (argsh, yq, jq, envsubst, sops, ssh-to-age) is carried
// commented out and selectable through --groups; there is no flag of its
// own (the project config routes to bash, WP8).
func TestToolchainInstallBashGroupThroughGroups(t *testing.T) {
	project := synthProject(t)
	t.Chdir(project.Base)
	stdout, stderr, err := runLo(t, NewRoot(project), "toolchain", "install", "--dry-run", "--groups", "core,local,bash")
	if err != nil {
		t.Fatalf("toolchain install --groups bash: %v\n%s", err, stderr)
	}
	if _, _, err := runLo(t, NewRoot(project), "toolchain", "install", "--dry-run", "--with-bash"); err == nil {
		t.Fatal("--with-bash accepted; the bash runtime is selected by the project config, not a flag")
	}
	if !strings.Contains(stdout, "groups: core,local,bash)") {
		t.Errorf("bash group not selected:\n%s", stdout)
	}
	tpl, err := toolchainTemplate("p", []string{"core", "local", "bash"})
	if err != nil || !strings.Contains(tpl, "  github.com/arg-sh/argsh:\n    asset: argsh\n") || !strings.Contains(tpl, "  yq:\n    groups: [bash]\n") {
		t.Errorf("template without the active bash entries: %v\n%s", err, tpl)
	}
	plain, _ := toolchainTemplate("p", nil)
	if strings.Contains(plain, "\n  yq:") {
		t.Error("the default template activates the bash group")
	}
}

func TestToolchainInstallPathFlagWins(t *testing.T) {
	ambient := synthProject(t)
	t.Setenv("PATH_BASE", ambient.Base)
	standing := t.TempDir()
	testutil.WriteFile(t, filepath.Join(standing, "lok8s.yaml"), "kind: Project\n")
	t.Chdir(standing)
	target := t.TempDir()

	stdout, stderr, err := runLo(t, NewRoot(ambient), "toolchain", "install", "--dry-run", "--path", target)
	if err != nil {
		t.Fatalf("toolchain install --path: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "lo toolchain install — "+target+" (") {
		t.Errorf("--path did not win:\n%s", stdout)
	}
}

// Outside any project the default is the working directory itself.
func TestToolchainInstallFallsBackToCwd(t *testing.T) {
	ambient := synthProject(t)
	t.Setenv("PATH_BASE", ambient.Base)
	bare := t.TempDir()
	t.Chdir(bare)
	stdout, _, err := runLo(t, NewRoot(ambient), "toolchain", "install", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "lo toolchain install — "+bare+" (") {
		t.Errorf("no-marker fallback is not cwd:\n%s", stdout)
	}
}

// `lo init toolchain` runs `lo toolchain install` under its old name for
// one release: hidden from `lo init --help`, the same flags and output,
// plus the one-line hint on stderr.
func TestInitToolchainIsAHiddenAliasWithAHint(t *testing.T) {
	project := synthProject(t)
	t.Chdir(project.Base)
	stdout, stderr, err := runLo(t, NewRoot(project), "init", "toolchain", "--dry-run", "--groups", "core,cloud")
	if err != nil {
		t.Fatalf("init toolchain: %v\n%s", err, stderr)
	}
	if stderr != deprecatedInitToolchain+"\n" {
		t.Errorf("stderr = %q, want the deprecation hint alone", stderr)
	}
	want, _, err := runLo(t, NewRoot(project), "toolchain", "install", "--dry-run", "--groups", "core,cloud")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != want {
		t.Errorf("alias output differs from lo toolchain install:\n--- alias\n%s--- install\n%s", stdout, want)
	}
	help, _, err := runLo(t, NewRoot(project), "init", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(help, "\n  toolchain ") {
		t.Errorf("lo init --help still lists toolchain:\n%s", help)
	}
	root := NewRoot(project)
	tc, _, err := root.Find([]string{"init", "toolchain"})
	if err != nil || tc == nil || !tc.Hidden {
		t.Errorf("init toolchain: found=%v hidden=%v err=%v", tc != nil, tc != nil && tc.Hidden, err)
	}
	if inst, _, err := root.Find([]string{"toolchain", "install"}); err != nil || inst.Hidden {
		t.Errorf("toolchain install must be visible: err=%v", err)
	}
}

// The MCP projection reads the markers: install is mutating (no
// readonly, no destructive) and idempotent; doctor is readonly. The
// hidden alias is not a tool.
func TestToolchainCommandMarkers(t *testing.T) {
	root := NewRoot(synthProject(t))
	inst, _, err := root.Find([]string{"toolchain", "install"})
	if err != nil {
		t.Fatal(err)
	}
	if mcpTier(inst) != mcpTierMutating || inst.Annotations[AnnotationIdempotent] != "true" {
		t.Errorf("toolchain install: tier=%d annotations=%v", mcpTier(inst), inst.Annotations)
	}
	doc, _, err := root.Find([]string{"toolchain", "doctor"})
	if err != nil {
		t.Fatal(err)
	}
	if mcpTier(doc) != mcpTierReadonly {
		t.Errorf("toolchain doctor: tier=%d annotations=%v", mcpTier(doc), doc.Annotations)
	}
	alias, _, _ := root.Find([]string{"init", "toolchain"})
	if !mcpHiddenSubtree(alias) {
		t.Error("the init toolchain alias is exposed to MCP")
	}
}

// `lo toolchain doctor` prints the toolchain section alone, gated by
// nothing (no marker, no flag), and exits 1 when a pinned tool is
// missing: a bare project has no .bin/b. PATH is emptied so no tool on
// the developer's machine is probed.
func TestToolchainDoctorSectionAlone(t *testing.T) {
	p := synthProject(t)
	t.Setenv("PATH", t.TempDir())
	stdout, stderr, err := runLo(t, NewRoot(p), "toolchain", "doctor")
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want the handled sentinel (b is missing)", err)
	}
	if !strings.HasPrefix(stdout, "\n--- toolchain (lo ") {
		t.Errorf("no toolchain header:\n%s", stdout)
	}
	if strings.Contains(stdout, "=== lok8s doctor ===") || strings.Contains(stdout, "--- environment ---") {
		t.Errorf("the full doctor ran:\n%s", stdout)
	}
	if !strings.Contains(stdout, "b missing at .bin/b — "+toolchain.Fix) {
		t.Errorf("b line missing:\n%s", stdout)
	}
	if !strings.Contains(stderr, "toolchain doctor: a pinned tool is missing") {
		t.Errorf("stderr = %q", stderr)
	}
	// With b present (a stub that answers --version) the b line is ✓ and
	// the exit code follows the render tools only.
	testutil.WriteFile(t, filepath.Join(p.Bin, "b"), "#!/bin/sh\necho 4.18.7\n")
	if err := os.Chmod(filepath.Join(p.Bin, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, _, _ = runLo(t, NewRoot(p), "toolchain", "doctor")
	if !strings.Contains(stdout, "b 4.18.7 (.bin/b)") {
		t.Errorf("stubbed b not reported:\n%s", stdout)
	}
}
