package render

// render_test.go — the variant-independent surface: LO_RENDER parsing on
// both builds, the exec pipeline through the Runner seam, and the
// SecretInProcess switch. The in-process assertions (lo-full only) are in
// render_inprocess_test.go behind the `inprocess` tag; `go test ./...`
// runs this file on core, `go test -tags inprocess ./...` runs both.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

func TestMain(m *testing.M) {
	// lo-full: the self-exec plugin home points at THIS binary — when
	// kustomize execs …/secret/Secret or …/chartrenderer/ChartRenderer,
	// the test binary must behave as the plugin, exactly like `lo` does.
	// lo core: DispatchPlugin is a no-op and the call falls through.
	if handled, rc := DispatchPlugin(os.Args, os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(rc)
	}
	rc := m.Run()
	Cleanup()
	os.Exit(rc)
}

func TestCurrentModePerVariant(t *testing.T) {
	t.Setenv(ModeEnv, "sometimes")
	if _, err := CurrentMode(); err == nil || !strings.Contains(err.Error(), "LO_RENDER") {
		t.Fatalf("unknown LO_RENDER accepted: %v", err)
	}
	t.Setenv(ModeEnv, "EXEC")
	if m, err := CurrentMode(); err != nil || m != ModeExec {
		t.Fatalf("LO_RENDER=EXEC → %q, %v", m, err)
	}
	t.Setenv(ModeEnv, "")
	m, err := CurrentMode()
	if err != nil {
		t.Fatalf("unset: %v", err)
	}
	t.Setenv(ModeEnv, "inprocess")
	mi, errI := CurrentMode()
	if InProcessAvailable() {
		if Variant() != "full" || m != ModeInProcess {
			t.Fatalf("lo-full: variant=%q unset→%q", Variant(), m)
		}
		if errI != nil || mi != ModeInProcess {
			t.Fatalf("lo-full: LO_RENDER=inprocess → %q, %v", mi, errI)
		}
		return
	}
	// lo core: exec is the only pipeline; an explicit inprocess names the
	// build that has it.
	if Variant() != "core" || m != ModeExec {
		t.Fatalf("lo core: variant=%q unset→%q", Variant(), m)
	}
	if errI == nil || !strings.Contains(errI.Error(), "lo core") || !strings.Contains(errI.Error(), "lo-full") {
		t.Fatalf("lo core: LO_RENDER=inprocess must error naming lo-full, got %q / %v", mi, errI)
	}
	// Build spells the rejection out on the render's stderr stream: the
	// callers only print "kustomize build failed for <domain>".
	var stderr strings.Builder
	if _, err := Build(context.Background(), t.TempDir(), Options{Runner: &recordingRunner{}, Stderr: &stderr}); err == nil {
		t.Fatal("lo core: Build under LO_RENDER=inprocess rendered")
	}
	if !strings.Contains(stderr.String(), "Error: LO_RENDER=inprocess: this is lo core") || !strings.Contains(stderr.String(), "lo-full") {
		t.Fatalf("core rejection not printed to stderr: %q", stderr.String())
	}
}

func TestSecretInProcessOnlyExecOptsOut(t *testing.T) {
	t.Setenv(ModeEnv, "")
	if !SecretInProcess() {
		t.Fatal("unset LO_RENDER: the imported generator must mint in-process on both variants")
	}
	t.Setenv(ModeEnv, "exec")
	if SecretInProcess() {
		t.Fatal("LO_RENDER=exec must exec the plugin binary")
	}
}

// recordingRunner is the exec-mode seam: it records the kustomize argv/env
// and answers with a canned stream.
type recordingRunner struct {
	cmd execx.Cmd
}

func (r *recordingRunner) Run(_ context.Context, c execx.Cmd) error {
	r.cmd = c
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte("kind: Canned\n"))
	}
	return nil
}

func TestBuildExecModeUsesRunnerAndDefaultsPluginHome(t *testing.T) {
	t.Setenv(ModeEnv, "exec")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	r := &recordingRunner{}
	out, err := Build(context.Background(), "/some/dir", Options{
		Paths: p, Runner: r, EnableExec: true, Env: []string{"KHELM_TRUST_ANY_REPO=true"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "kind: Canned\n" {
		t.Fatalf("out = %q", out)
	}
	if r.cmd.Name != "kustomize" || strings.Join(r.cmd.Args, " ") != "build --enable-alpha-plugins --enable-exec /some/dir" {
		t.Fatalf("argv = %s %v", r.cmd.Name, r.cmd.Args)
	}
	wantEnv := []string{"KHELM_TRUST_ANY_REPO=true", "KUSTOMIZE_PLUGIN_HOME=" + filepath.Join(base, ".kustomize")}
	if strings.Join(r.cmd.Env, "\n") != strings.Join(wantEnv, "\n") {
		t.Fatalf("env = %v, want %v", r.cmd.Env, wantEnv)
	}

	// No Paths (the addon call shape): the plugin home is left to the
	// environment, exactly as before.
	r = &recordingRunner{}
	if _, err := Build(context.Background(), "/d", Options{Runner: r}); err != nil {
		t.Fatal(err)
	}
	if len(r.cmd.Env) != 0 || strings.Join(r.cmd.Args, " ") != "build --enable-alpha-plugins /d" {
		t.Fatalf("addon-shape exec: args=%v env=%v", r.cmd.Args, r.cmd.Env)
	}

	// --load-restrictor is passed through as the CLI flag.
	r = &recordingRunner{}
	if _, err := Build(context.Background(), "/d", Options{Runner: r, LoadRestrictions: LoadRestrictionsNone}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.cmd.Args, " ") != "build --enable-alpha-plugins --load-restrictor LoadRestrictionsNone /d" {
		t.Fatalf("load-restrictor argv: %v", r.cmd.Args)
	}
}

// TestBuildCoreDefaultIsExec: on core an UNSET LO_RENDER takes the exec
// pipeline (the only one), so the Runner seam is what renders.
func TestBuildCoreDefaultIsExec(t *testing.T) {
	if InProcessAvailable() {
		t.Skip("lo-full: the default is in-process")
	}
	t.Setenv(ModeEnv, "")
	r := &recordingRunner{}
	out, err := Build(context.Background(), "/d", Options{Runner: r})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "kind: Canned\n" || r.cmd.Name != "kustomize" {
		t.Fatalf("core default did not exec kustomize: out=%q cmd=%q", out, r.cmd.Name)
	}
}
