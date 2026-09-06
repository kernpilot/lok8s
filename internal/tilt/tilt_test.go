package tilt

// tilt_test.go — hermetic tests for the tilt lifecycle port. ALL external
// processes (tilt, kill, pkill, kubectl) run through the fake runner; no
// real tilt/pkill is EVER invoked (a live Tilt session for another project
// runs on this machine — hermeticity is a safety property, not just speed).
// Mirrors tests/unit/tilt_ci_test.bats + tilt_preflight_test.bats.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

type fakeExit struct{ code int }

func (e *fakeExit) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *fakeExit) ExitCode() int { return e.code }

type fakeRunner struct {
	calls   []string
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(_ context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

func (r *fakeRunner) matching(sub string) []string {
	var out []string
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

func writeOut(c execx.Cmd, s string) {
	if c.Stdout != nil {
		fmt.Fprint(c.Stdout, s)
	}
}

// doctorKindHandler answers `tilt doctor` with the kind marker and
// `tilt get session` per running; everything else succeeds silently.
func doctorKindHandler(running bool) func(c execx.Cmd) error {
	return func(c execx.Cmd) error {
		if c.Name == "tilt" && len(c.Args) > 0 && c.Args[0] == "doctor" {
			writeOut(c, "Env: kind\n")
			return nil
		}
		if c.Name == "tilt" && len(c.Args) > 1 && c.Args[0] == "get" && c.Args[1] == "session" {
			if running {
				return nil
			}
			return &fakeExit{1}
		}
		return nil
	}
}

func testCtx(t *testing.T) (*Context, *fakeRunner, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "clusters"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
	runner := &fakeRunner{}
	var out, errOut bytes.Buffer
	c := &Context{Paths: paths, Runner: runner, Out: &out, ErrOut: &errOut,
		Stdin: strings.NewReader("")}
	// Deterministic, isolated env per test.
	for _, v := range []string{"TILT_PORT", "DOMAIN_NAME", "LOK8S_PREFLIGHT",
		"LOK8S_FORCE_CLEAR_TERMINATING", "DEBUG"} {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	return c, runner, &out, &errOut
}

// ── tilt::port — the cksum derivation ────────────────────

func TestPortCksumPinnedAgainstCoreutils(t *testing.T) {
	// Pinned against `printf '%s' <domain> | cksum` on the real coreutils
	// binary. kubehz.dev MUST stay 10466 — that is the live project's port.
	for domain, want := range map[string]string{
		"lok8s.dev":  "10465",
		"kubehz.dev": "10466",
		"alpha.dev":  "10394",
	} {
		c, _, _, _ := testCtx(t)
		t.Setenv("DOMAIN_NAME", domain)
		if got := c.Port(); got != want {
			t.Errorf("Port(%s) = %s, want %s", domain, got, want)
		}
	}
}

func TestPortDefaultsToLok8sDevWithoutDomain(t *testing.T) {
	c, _, _, _ := testCtx(t)
	if got := c.Port(); got != "10465" {
		t.Errorf("Port() = %s, want 10465 (lok8s.dev default)", got)
	}
}

func TestPortEnvOverrideWins(t *testing.T) {
	c, _, _, _ := testCtx(t)
	t.Setenv("TILT_PORT", "14242")
	if got := c.Port(); got != "14242" {
		t.Errorf("Port() = %s, want 14242", got)
	}
}

func TestPortSpecOverride(t *testing.T) {
	c, _, _, _ := testCtx(t)
	t.Setenv("DOMAIN_NAME", "spec.test")
	dir := filepath.Join(c.Paths.Clusters, "spec.test")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "cluster.lok8s.yaml"),
		[]byte("kind: Lo\nspec:\n  tilt:\n    port: 12345\n"), 0o644)
	if got := c.Port(); got != "12345" {
		t.Errorf("Port() = %s, want 12345 (spec.tilt.port)", got)
	}
}

func TestPortSpecNullFallsThroughToHash(t *testing.T) {
	c, _, _, _ := testCtx(t)
	t.Setenv("DOMAIN_NAME", "alpha.dev")
	dir := filepath.Join(c.Paths.Clusters, "alpha.dev")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "cluster.lok8s.yaml"),
		[]byte("kind: Lo\nspec:\n  tilt:\n    port: null\n"), 0o644)
	if got := c.Port(); got != "10394" {
		t.Errorf("Port() = %s, want 10394 (hash)", got)
	}
}

// ── tilt::running / tilt::reload ─────────────────────────

func TestRunningReflectsApiserver(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	runner.handler = doctorKindHandler(false)
	if c.Running(context.Background(), "14242") {
		t.Error("Running = true with the apiserver down")
	}
	runner.handler = doctorKindHandler(true)
	if !c.Running(context.Background(), "14242") {
		t.Error("Running = false with the apiserver up")
	}
}

func TestReloadTriggersTiltfileResource(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	if err := c.Reload(context.Background(), "14242"); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls; len(got) != 1 || got[0] != "tilt trigger (Tiltfile) --port 14242" {
		t.Errorf("calls = %v", got)
	}
}

// ── tilt::up ─────────────────────────────────────────────

func TestUpSpawnsDetachedAndWritesPidFile(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	t.Setenv("TILT_PORT", "14242")
	runner.handler = doctorKindHandler(false)
	started := ""
	c.StartDetached = func(port string) (int, error) { started = port; return 4242, nil }

	if err := c.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if started != "14242" {
		t.Errorf("StartDetached port = %q", started)
	}
	if !strings.Contains(out.String(), "Tilt UI: http://localhost:14242") {
		t.Errorf("stdout = %q", out.String())
	}
	pid, err := os.ReadFile(c.Paths.Base + "/.tilt.pid")
	if err != nil || string(pid) != "4242\n" {
		t.Errorf(".tilt.pid = %q, err %v", pid, err)
	}
}

func TestUpGateFailsWithoutKindEnv(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "tilt" && c.Args[0] == "doctor" {
			writeOut(c, "Env: docker-desktop\n")
			return nil
		}
		if c.Name == "tilt" && c.Args[0] == "get" {
			return &fakeExit{1}
		}
		return nil
	}
	c.StartDetached = func(string) (int, error) {
		t.Fatal("StartDetached must not run when the gate fails")
		return 0, nil
	}
	if err := c.Up(context.Background()); err != ErrHandled {
		t.Fatalf("err = %v, want ErrHandled", err)
	}
	if !strings.Contains(errOut.String(), "Did not recognize local kind environment.") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestUpReloadsWhenAlreadyRunning(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	t.Setenv("TILT_PORT", "14242")
	runner.handler = doctorKindHandler(true)
	c.StartDetached = func(string) (int, error) {
		t.Fatal("must not background a second instance")
		return 0, nil
	}
	if err := c.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Tilt already running on http://localhost:14242 — reloading Tiltfile") {
		t.Errorf("stdout = %q", out.String())
	}
	if got := runner.matching("trigger (Tiltfile)"); len(got) != 1 || !strings.Contains(got[0], "--port 14242") {
		t.Errorf("trigger calls = %v", got)
	}
}

func TestUpReloadFailureWarnsButSucceeds(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	t.Setenv("TILT_PORT", "14242")
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Args[0] == "get" {
			return nil // session up
		}
		if cmd.Args[0] == "trigger" {
			return &fakeExit{1}
		}
		return nil
	}
	if err := c.Up(context.Background()); err != nil {
		t.Fatalf("err = %v, want nil (reload failure is a warn)", err)
	}
	if !strings.Contains(errOut.String(), "Tiltfile reload trigger failed (Tilt still running on :14242)") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// ── tilt::ci ─────────────────────────────────────────────

func TestCIInvokesTiltCIWithPortAndFile(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	t.Setenv("TILT_PORT", "14242")
	runner.handler = doctorKindHandler(false)
	rc, err := c.CI(context.Background(), "")
	if err != nil || rc != 0 {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	want := "tilt ci --port=14242 --file=" + c.Paths.Base + "/Tiltfile"
	if got := runner.matching("tilt ci"); len(got) != 1 || got[0] != want {
		t.Errorf("ci calls = %v, want %q", got, want)
	}
	if !strings.Contains(out.String(), "Tilt CI (headless) on port 14242 — building, deploying, waiting for readiness...") {
		t.Errorf("stdout = %q", out.String())
	}
	// Headless must NOT shell out to interactive `tilt up`.
	if got := runner.matching("tilt up"); len(got) != 0 {
		t.Errorf("unexpected tilt up: %v", got)
	}
}

func TestCIPassesTimeoutThrough(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	t.Setenv("TILT_PORT", "14242")
	runner.handler = doctorKindHandler(false)
	if _, err := c.CI(context.Background(), "90s"); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("tilt ci"); len(got) != 1 || !strings.HasSuffix(got[0], "--timeout 90s") {
		t.Errorf("ci calls = %v", got)
	}
}

func TestCIOmitsTimeoutWhenNoneGiven(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	runner.handler = doctorKindHandler(false)
	if _, err := c.CI(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.matching("tilt ci") {
		if strings.Contains(call, "--timeout") {
			t.Errorf("unexpected --timeout in %q", call)
		}
	}
}

func TestCIReturnsRealExitStatus(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Args[0] == "doctor" {
			writeOut(cmd, "Env: kind\n")
			return nil
		}
		if cmd.Args[0] == "ci" {
			return &fakeExit{7}
		}
		return nil
	}
	rc, err := c.CI(context.Background(), "")
	if err != nil || rc != 7 {
		t.Fatalf("rc=%d err=%v, want rc=7", rc, err)
	}
}

func TestCIGateFailsWithoutKindEnv(t *testing.T) {
	c, runner, _, errOut := testCtx(t)
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Args[0] == "doctor" {
			return nil // no "Env: kind" line
		}
		t.Fatalf("unexpected call after failed gate: %v", cmd.Args)
		return nil
	}
	rc, err := c.CI(context.Background(), "")
	if rc != 1 || err != ErrHandled {
		t.Fatalf("rc=%d err=%v", rc, err)
	}
	if !strings.Contains(errOut.String(), "Did not recognize local kind environment.") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

// ── tilt::down — pid/pkill logic (fake pid files, fake runner; the pkill
// pattern must stay project-scoped and regex-escaped) ────

func TestDownKillsPidFromFileAndRemovesIt(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	os.WriteFile(c.Paths.Base+"/.tilt.pid", []byte("31337\n"), 0o644)
	if err := c.Down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls; len(got) != 1 || got[0] != "kill 31337" {
		t.Errorf("calls = %v, want [kill 31337]", got)
	}
	if _, err := os.Stat(c.Paths.Base + "/.tilt.pid"); !os.IsNotExist(err) {
		t.Error(".tilt.pid not removed")
	}
}

func TestDownFallsBackToScopedPkillWhenKillFails(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	os.WriteFile(c.Paths.Base+"/.tilt.pid", []byte("31337\n"), 0o644)
	runner.handler = func(cmd execx.Cmd) error {
		if cmd.Name == "kill" {
			return &fakeExit{1}
		}
		return nil
	}
	if err := c.Down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	want := "pkill -f [t]ilt up.*--file=" + pkillEscape(c.Paths.Base+"/Tiltfile")
	if got := runner.matching("pkill"); len(got) != 1 || got[0] != want {
		t.Errorf("pkill calls = %v, want %q", got, want)
	}
}

func TestDownNoPidNoForceIsANoop(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	if err := c.Down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("calls = %v, want none", runner.calls)
	}
}

func TestDownForceWithoutPidUsesScopedPkill(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	if err := c.Down(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("pkill"); len(got) != 1 {
		t.Errorf("pkill calls = %v", got)
	}
	// The first char stays bracketed and the path's metacharacters escaped —
	// the pattern must never broaden onto other projects' sessions.
	if !strings.HasPrefix(runner.calls[0], "pkill -f [t]ilt up.*--file=") {
		t.Errorf("pattern = %q", runner.calls[0])
	}
}

func TestDownEmptyPidFileFallsBackToPkill(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	os.WriteFile(c.Paths.Base+"/.tilt.pid", nil, 0o644)
	if err := c.Down(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || !strings.HasPrefix(runner.calls[0], "pkill -f ") {
		t.Errorf("calls = %v, want exactly one pkill fallback (no bare kill with an empty pid)", runner.calls)
	}
}

func TestPkillEscape(t *testing.T) {
	in := "/tmp/a+b (x)/Tiltfile"
	want := `/tmp/a\+b \(x\)/Tiltfile`
	if got := pkillEscape(in); got != want {
		t.Errorf("pkillEscape = %q, want %q", got, want)
	}
}

// ── tilt::status / tilt::restart ─────────────────────────

func TestStatusRunsTiltDoctorAndPassesRC(t *testing.T) {
	c, runner, out, _ := testCtx(t)
	runner.handler = func(cmd execx.Cmd) error {
		writeOut(cmd, "Tilt: v0.33\nEnv: kind\n")
		return &fakeExit{3}
	}
	if rc := c.Status(context.Background()); rc != 3 {
		t.Errorf("rc = %d, want 3", rc)
	}
	if !strings.Contains(out.String(), "Env: kind") {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestRestartDownsThenUps(t *testing.T) {
	c, runner, _, _ := testCtx(t)
	os.WriteFile(c.Paths.Base+"/.tilt.pid", []byte("31337\n"), 0o644)
	runner.handler = doctorKindHandler(false)
	c.StartDetached = func(string) (int, error) { return 777, nil }
	if err := c.Restart(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if got := runner.matching("kill "); len(got) != 1 {
		t.Errorf("kill calls = %v", runner.calls)
	}
	pid, _ := os.ReadFile(c.Paths.Base + "/.tilt.pid")
	if string(pid) != "777\n" {
		t.Errorf(".tilt.pid = %q", pid)
	}
}
