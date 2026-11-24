package addons

// nomint_test.go: with LOK8S_SECRETS_NO_MINT set, the addon render (lo
// bootstrap, the KubeOne addon staging and the doctor call Render) refuses
// a Secret plugin that does not have the no-mint run before kustomize runs
// (build.CheckNoMintPlugin), and kustomize gets the plugin home that the
// check probed. A dry run checks too. bats: the addons::render no-mint
// tests in tests/unit/bootstrap_test.bats.

import (
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
	"github.com/kernpilot/lok8s/internal/ui"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

const staleNoMintRefusal = "[error] the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not have the no-mint run (LOK8S_SECRETS_NO_MINT is set, and an older plugin ignores it and generates values): run 'lo toolchain install' to install the plugin of this lo\n"

// noMintPlugin writes the Secret plugin of p's plugin home: a script that
// appends one line to the returned log on each run and prints caps for
// --capabilities. KUSTOMIZE_PLUGIN_HOME is unset, so the home is
// <Base>/.kustomize, and TMPDIR is a new directory.
func noMintPlugin(t *testing.T, p *config.Paths, caps string) (log string) {
	t.Helper()
	t.Setenv(KustomizePluginHomeEnv, "")
	if err := os.Unsetenv(KustomizePluginHomeEnv); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	log = filepath.Join(p.Base, "plugin-runs.log")
	plugin := filepath.Join(p.Base, ".kustomize", "secrets.lok8s.dev", "v1", "secret", "Secret")
	if err := os.MkdirAll(filepath.Dir(plugin), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n[ \"$1\" = --capabilities ] && { printf '" + caps + "'; exit 0; }\nexit 1\n"
	if err := os.WriteFile(plugin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return log
}

func pluginRuns(t *testing.T, log string) int {
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

// noMintRunner runs the Secret plugin for real (the probe) and answers
// every other command with the fake kustomize. After a probe it sets
// KUSTOMIZE_PLUGIN_HOME to switchTo when that is set (the test registers
// the variable with t.Setenv first), so a render that resolves the home
// again after the check gives kustomize the other home.
type noMintRunner struct {
	*fakeKustomize
	switchTo string
}

func (r *noMintRunner) Run(ctx context.Context, c execx.Cmd) error {
	if filepath.Base(c.Name) != "Secret" {
		return r.fakeKustomize.Run(ctx, c)
	}
	err := runPlugin(ctx, c)
	if r.switchTo != "" {
		_ = os.Setenv(KustomizePluginHomeEnv, r.switchTo)
	}
	return err
}

// runPlugin runs c as a child process: its argv, dir, stdin and stdout,
// and its env appended to the environment of the test.
func runPlugin(ctx context.Context, c execx.Cmd) error {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	return cmd.Run()
}

// With the switch on, a plugin that does not print "no-mint" is refused
// before kustomize runs: the plugin of the keys-only run, one that prints
// nothing, one that fails. The refusal is one line that names the plugin
// path, and Render returns the handled error.
func TestRenderNoMintRefusesAPluginWithoutTheNoMintRun(t *testing.T) {
	for _, caps := range []string{"dry-run\\nkeys-only\\n", "", "no-mint-not\\n"} {
		t.Run(caps, func(t *testing.T) {
			dir := writeAddon(t)
			p := testPaths(t)
			log := noMintPlugin(t, p, caps)
			t.Setenv(secret.NoMintEnv, "1")
			f := newFakeKustomize(t, kustomizeManifest)
			var errBuf strings.Builder
			_, err := Render(t.Context(), p, &noMintRunner{fakeKustomize: f}, &errBuf, dir, "lo", "hetzner", inlineValues, nil)
			if !errors.Is(err, ui.ErrHandled) {
				t.Fatalf("Render = %v, want the handled error\n%s", err, errBuf.String())
			}
			if errBuf.String() != staleNoMintRefusal {
				t.Errorf("stderr = %q\nwant %q", errBuf.String(), staleNoMintRefusal)
			}
			if n := pluginRuns(t, log); n != 1 {
				t.Errorf("the plugin ran %d times, want one probe", n)
			}
			if len(f.calls) != 0 {
				t.Errorf("kustomize ran: %v", f.calls)
			}
		})
	}
}

// A plugin that prints "no-mint" renders, and kustomize gets the home that
// the check probed, also when the environment names another home after the
// check. A missing plugin is refused with the caller's name.
func TestRenderNoMintAcceptsAPluginWithTheNoMintRun(t *testing.T) {
	dir := writeAddon(t)
	p := testPaths(t)
	log := noMintPlugin(t, p, "dry-run\\nkeys-only\\nno-mint\\n")
	t.Setenv(secret.NoMintEnv, "1")
	checked := filepath.Join(p.Base, ".kustomize")
	other := filepath.Join(p.Base, "other-home")
	f := newFakeKustomize(t, kustomizeManifest)
	var errBuf strings.Builder
	if _, err := Render(t.Context(), p, &noMintRunner{fakeKustomize: f, switchTo: other}, &errBuf, dir, "lo", "hetzner", "", nil); err != nil {
		t.Fatalf("Render: %v\n%s", err, errBuf.String())
	}
	if home := os.Getenv(KustomizePluginHomeEnv); home != other {
		t.Fatalf("the probe did not run through the runner: KUSTOMIZE_PLUGIN_HOME = %q, want %q", home, other)
	}
	if n := pluginRuns(t, log); n != 1 {
		t.Errorf("the plugin ran %d times, want one probe", n)
	}
	if len(f.calls) != 1 {
		t.Fatalf("kustomize calls = %d, want 1", len(f.calls))
	}
	var homes []string
	for _, kv := range f.envs[0] {
		if v, ok := strings.CutPrefix(kv, KustomizePluginHomeEnv+"="); ok {
			homes = append(homes, v)
		}
	}
	if !slices.Equal(homes, []string{checked}) {
		t.Errorf("kustomize got the plugin homes %q, want only the checked home %q", homes, checked)
	}

	if err := os.Remove(filepath.Join(checked, "secrets.lok8s.dev", "v1", "secret", "Secret")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(KustomizePluginHomeEnv, checked)
	errBuf.Reset()
	f = newFakeKustomize(t, kustomizeManifest)
	if _, err := Render(t.Context(), p, &noMintRunner{fakeKustomize: f}, &errBuf, dir, "lo", "hetzner", "", nil); !errors.Is(err, ui.ErrHandled) {
		t.Fatalf("Render without the plugin = %v, want the handled error", err)
	}
	want := "[error] addons::render: LOK8S_SECRETS_NO_MINT is set, but the Secret plugin .kustomize/secrets.lok8s.dev/v1/secret/Secret does not exist, so kustomize could load another form of the plugin (a Go plugin, Secret.so) that lo does not check: run 'lo toolchain install' to install the plugin of this lo\n"
	if errBuf.String() != want {
		t.Errorf("stderr = %q\nwant %q", errBuf.String(), want)
	}
	if len(f.calls) != 0 {
		t.Errorf("kustomize ran: %v", f.calls)
	}
}

// With the switch off, the plugin is not probed. bats: "addons::render with
// no-mint off does not probe the Secret plugin".
func TestRenderNoMintOffDoesNotProbe(t *testing.T) {
	for _, value := range []string{"", "0", "false"} {
		t.Run("switch "+value, func(t *testing.T) {
			dir := writeAddon(t)
			p := testPaths(t)
			log := noMintPlugin(t, p, "dry-run\\nkeys-only\\n")
			t.Setenv(secret.NoMintEnv, value)
			f := newFakeKustomize(t, kustomizeManifest)
			var errBuf strings.Builder
			if _, err := Render(t.Context(), p, &noMintRunner{fakeKustomize: f}, &errBuf, dir, "lo", "hetzner", "", nil); err != nil {
				t.Fatalf("Render: %v\n%s", err, errBuf.String())
			}
			if n := pluginRuns(t, log); n != 0 {
				t.Errorf("the plugin ran %d times, want none", n)
			}
			if len(f.calls) != 1 {
				t.Errorf("kustomize calls = %d, want 1", len(f.calls))
			}
		})
	}
}

// A dry run checks too, with and without --allow-generators: the switch
// wins over a dry run, and a plugin that has the dry run but not the
// no-mint run would report a value that it would generate instead of
// failing. The check probes the plugin of the real home. A plugin with the
// no-mint run renders: kustomize gets the checked home with
// --allow-generators and the boundary's home without it. bats:
// "addons::render with no-mint checks the Secret plugin in a dry run".
func TestRenderDryRunNoMintChecksThePlugin(t *testing.T) {
	for _, allow := range []bool{false, true} {
		name := "without --allow-generators"
		if allow {
			name = "with --allow-generators"
		}
		t.Run(name, func(t *testing.T) {
			dir := writeAddon(t)
			p := testPaths(t)
			log := noMintPlugin(t, p, "dry-run\\nkeys-only\\n")
			f := newFakeKustomize(t, kustomizeManifest)
			b := dryBoundary(t, p)
			before := pluginRuns(t, log)
			t.Setenv(secret.NoMintEnv, "1")
			var errBuf strings.Builder
			_, err := RenderDryRun(t.Context(), p, &noMintRunner{fakeKustomize: f}, &errBuf, dir, "lo", "hetzner", "", nil, b, allow)
			if !errors.Is(err, ui.ErrHandled) {
				t.Fatalf("RenderDryRun = %v, want the handled error\n%s", err, errBuf.String())
			}
			if errBuf.String() != staleNoMintRefusal {
				t.Errorf("stderr = %q\nwant %q", errBuf.String(), staleNoMintRefusal)
			}
			if n := pluginRuns(t, log) - before; n != 1 {
				t.Errorf("the dry run ran the plugin %d times, want one probe", n)
			}
			if len(f.calls) != 0 {
				t.Errorf("kustomize ran: %v", f.calls)
			}

			noMintPlugin(t, p, "dry-run\\nkeys-only\\nno-mint\\n")
			errBuf.Reset()
			if _, err := RenderDryRun(t.Context(), p, &noMintRunner{fakeKustomize: f}, &errBuf, dir, "lo", "hetzner", "", nil, b, allow); err != nil {
				t.Fatalf("RenderDryRun with the no-mint run: %v\n%s", err, errBuf.String())
			}
			if len(f.calls) != 1 {
				t.Fatalf("kustomize calls = %d, want 1", len(f.calls))
			}
			var homes []string
			for _, kv := range f.envs[0] {
				if v, ok := strings.CutPrefix(kv, KustomizePluginHomeEnv+"="); ok {
					homes = append(homes, v)
				}
			}
			want := b.PluginHome
			if allow {
				want = filepath.Join(p.Base, ".kustomize")
			}
			if !slices.Equal(homes, []string{want}) {
				t.Errorf("kustomize got the plugin homes %q, want only %q", homes, want)
			}
		})
	}
}
