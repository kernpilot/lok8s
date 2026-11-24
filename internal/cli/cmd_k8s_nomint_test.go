package cli

// cmd_k8s_nomint_test.go: with LOK8S_SECRETS_NO_MINT set, `lo k8s
// infrastructure/platform` refuses a Secret plugin that does not have the
// no-mint run before the render (build.CheckNoMintPlugin), and kustomize
// gets the plugin home that the check probed. bats: the k8s::infrastructure
// and k8s::platform no-mint tests in tests/unit/secrets_store_resolution_test.bats.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// k8sNoMintRunner runs the Secret plugin for real (the probe) and records
// the env of every other command (the render), which it answers with an
// empty stream. After a probe it sets KUSTOMIZE_PLUGIN_HOME to switchTo
// when that is set (the test registers the variable first).
type k8sNoMintRunner struct {
	renders  [][]string
	switchTo string
}

func (r *k8sNoMintRunner) Run(ctx context.Context, c execx.Cmd) error {
	if filepath.Base(c.Name) != "Secret" {
		r.renders = append(r.renders, c.Env)
		return nil
	}
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	err := cmd.Run()
	if r.switchTo != "" {
		_ = os.Setenv(config.KustomizePluginHomeEnv, r.switchTo)
	}
	return err
}

// k8sNoMintProject is an exec-pipeline project with the domain alpha.dev
// and its store, and a Secret plugin in <Base>/.kustomize that prints caps
// for --capabilities and appends one line to the returned log on each run.
func k8sNoMintProject(t *testing.T, caps string) (p *config.Paths, r *k8sNoMintRunner, log string) {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	t.Setenv(config.KustomizePluginHomeEnv, "")
	if err := os.Unsetenv(config.KustomizePluginHomeEnv); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	p = synthProject(t)
	for _, d := range []string{"alpha.dev/targets", "alpha.dev/secrets"} {
		if err := os.MkdirAll(filepath.Join(p.Clusters, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log = filepath.Join(p.Base, "plugin-runs.log")
	plugin := filepath.Join(p.Base, ".kustomize", "secrets.lok8s.dev", "v1", "secret", "Secret")
	if err := os.MkdirAll(filepath.Dir(plugin), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n[ \"$1\" = --capabilities ] && { printf '" + caps + "'; exit 0; }\nexit 1\n"
	if err := os.WriteFile(plugin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r = &k8sNoMintRunner{}
	prev := newRunner
	newRunner = func(*config.Paths) execx.Runner { return r }
	t.Cleanup(func() { newRunner = prev })
	return p, r, log
}

func k8sPluginRuns(t *testing.T, log string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(raw), "\n")
}

// With the switch on, a plugin that does not print "no-mint" is refused
// before the render, and no artifact is written. A plugin that prints it
// renders, and kustomize gets the home that the check probed, also when
// the environment names another home after the check.
func TestK8sArtifactChecksTheNoMintPlugin(t *testing.T) {
	src := func(p *config.Paths) string { return filepath.Join(p.Clusters, "alpha.dev", "targets") }

	p, r, log := k8sNoMintProject(t, "dry-run\\nkeys-only\\n")
	t.Setenv(secret.NoMintEnv, "1")
	out := t.TempDir()
	var stderr bytes.Buffer
	err := k8sKustomizeArtifact(t.Context(), p, "alpha.dev", src(p), out, "infrastructure.yaml", &stderr)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled\n%s", err, stderr.String())
	}
	want := "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the no-mint run (LOK8S_SECRETS_NO_MINT is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q\nwant %q", stderr.String(), want)
	}
	if n := k8sPluginRuns(t, log); n != 1 {
		t.Errorf("the plugin ran %d times, want one probe", n)
	}
	if len(r.renders) != 0 {
		t.Error("the render ran")
	}
	if _, err := os.Stat(filepath.Join(out, "infrastructure.yaml")); err == nil {
		t.Error("an artifact was written")
	}

	p, r, log = k8sNoMintProject(t, "dry-run\\nkeys-only\\nno-mint\\n")
	checked := filepath.Join(p.Base, ".kustomize")
	r.switchTo = filepath.Join(p.Base, "other-home")
	stderr.Reset()
	if err := k8sKustomizeArtifact(t.Context(), p, "alpha.dev", src(p), out, "infrastructure.yaml", &stderr); err != nil {
		t.Fatalf("k8sKustomizeArtifact: %v\n%s", err, stderr.String())
	}
	if home := os.Getenv(config.KustomizePluginHomeEnv); home != r.switchTo {
		t.Fatalf("the probe did not run through the runner: KUSTOMIZE_PLUGIN_HOME = %q, want %q", home, r.switchTo)
	}
	if n := k8sPluginRuns(t, log); n != 1 {
		t.Errorf("the plugin ran %d times, want one probe", n)
	}
	if len(r.renders) != 1 {
		t.Fatalf("renders = %d, want 1", len(r.renders))
	}
	var homes []string
	for _, kv := range r.renders[0] {
		if v, ok := strings.CutPrefix(kv, config.KustomizePluginHomeEnv+"="); ok {
			homes = append(homes, v)
		}
	}
	if !slices.Equal(homes, []string{checked}) {
		t.Errorf("kustomize got the plugin homes %q, want only the checked home %q", homes, checked)
	}
}

// A missing plugin is refused with the name of the command, and with the
// switch off the plugin is not probed.
func TestK8sArtifactNoMintNamesTheCommandAndIsOffByDefault(t *testing.T) {
	p, r, log := k8sNoMintProject(t, "dry-run\\nkeys-only\\n")
	if err := os.Remove(filepath.Join(p.Base, ".kustomize", "secrets.lok8s.dev", "v1", "secret", "Secret")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(secret.NoMintEnv, "1")
	var stderr bytes.Buffer
	err := k8sKustomizeArtifact(t.Context(), p, "alpha.dev", filepath.Join(p.Clusters, "alpha.dev", "targets"), t.TempDir(), "platform.yaml", &stderr)
	if !errors.Is(err, ErrHandled) {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	want := "[error] lo k8s: LOK8S_SECRETS_NO_MINT is set, but the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not exist, so kustomize could load another form of the plugin (a Go plugin, Secret.so) that lo does not check: run 'lo toolchain install' to install the plugin of this lo\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q\nwant %q", stderr.String(), want)
	}
	if len(r.renders) != 0 {
		t.Error("the render ran")
	}

	p, r, log = k8sNoMintProject(t, "dry-run\\nkeys-only\\n")
	t.Setenv(secret.NoMintEnv, "0")
	if err := k8sKustomizeArtifact(t.Context(), p, "alpha.dev", filepath.Join(p.Clusters, "alpha.dev", "targets"), t.TempDir(), "platform.yaml", &stderr); err != nil {
		t.Fatalf("k8sKustomizeArtifact: %v", err)
	}
	if n := k8sPluginRuns(t, log); n != 0 {
		t.Errorf("the plugin ran %d times, want none", n)
	}
	if len(r.renders) != 1 {
		t.Errorf("renders = %d, want 1", len(r.renders))
	}
}
