package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/spf13/cobra"
)

// kustomizePaths is a project root with the framework plugin sources
// present (an empty <root>/kustomize is enough: the build is a fake).
func kustomizePaths(t *testing.T) *config.Paths {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "kustomize"), 0o755); err != nil {
		t.Fatal(err)
	}
	return projectPaths(root)
}

func TestKustomizeBuildInstallsIntoThePluginHome(t *testing.T) {
	// BIN_ROOT follows config.KustomizePluginHome: <Base>/.kustomize in a
	// shell without the variable, the exported home otherwise, so doctor's
	// fix hint (`run: lo kustomize build`) puts the plugins where the render
	// looks. The bash libs/kustomize always used ${PATH_BASE}/.kustomize
	// (D36).
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go on PATH")
	}
	p := kustomizePaths(t)
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	r := &fakeRunner{}
	if err := kustomizeBuild(t.Context(), r, p); err != nil {
		t.Fatalf("kustomizeBuild: %v", err)
	}
	if len(r.cmds) != 1 || r.cmds[0].Name != "make" {
		t.Fatalf("cmds = %+v", r.cmds)
	}
	if want := "BIN_ROOT=" + filepath.Join(p.Base, ".kustomize"); strings.Join(r.cmds[0].Env, "\n") != want {
		t.Errorf("unset: env = %v, want [%s]", r.cmds[0].Env, want)
	}

	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "/opt/plugins")
	r = &fakeRunner{}
	if err := kustomizeBuild(t.Context(), r, p); err != nil {
		t.Fatalf("kustomizeBuild: %v", err)
	}
	if strings.Join(r.cmds[0].Env, "\n") != "BIN_ROOT=/opt/plugins" {
		t.Errorf("exported: env = %v", r.cmds[0].Env)
	}
	r = &fakeRunner{}
	if err := kustomizeClean(t.Context(), r, p); err != nil {
		t.Fatalf("kustomizeClean: %v", err)
	}
	if strings.Join(r.cmds[0].Env, "\n") != "BIN_ROOT=/opt/plugins" {
		t.Errorf("clean, exported: env = %v", r.cmds[0].Env)
	}
}

func TestKustomizeListWalksThePluginHome(t *testing.T) {
	p := kustomizePaths(t)
	home := filepath.Join(t.TempDir(), "plugins")
	plugin := filepath.Join(home, "secrets.lok8s.dev", "v1", "secret", "Secret")
	if err := os.MkdirAll(filepath.Dir(plugin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plugin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", home)
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := kustomizeList(p, cmd); err != nil {
		t.Fatalf("kustomizeList: %v", err)
	}
	want := "Discoverable kustomize plugins under " + home + ":\n  secrets.lok8s.dev/v1/secret/Secret\n"
	if out.String() != want {
		t.Errorf("list = %q, want %q", out.String(), want)
	}
}

func TestPluginFileDirFollowsThePluginHome(t *testing.T) {
	p := projectPaths(t.TempDir())
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	if got := pluginFileDir(p); got != "../.kustomize" {
		t.Errorf("default: %q, want ../.kustomize", got)
	}
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", filepath.Join(p.Base, "tools", "plugins"))
	if got := pluginFileDir(p); got != filepath.Join("..", "tools", "plugins") {
		t.Errorf("inside the project: %q", got)
	}
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "/opt/plugins")
	if got := pluginFileDir(p); got != "/opt/plugins" {
		t.Errorf("outside the project: %q", got)
	}
	// The generated b.yaml carries it.
	tpl, err := toolchainTemplate(p, "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tpl, "file: /opt/plugins/") {
		t.Errorf("template does not name the exported home:\n%s", tpl)
	}
}
