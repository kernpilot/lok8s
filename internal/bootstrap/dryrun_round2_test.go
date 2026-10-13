package bootstrap

// dryrun_round2_test.go — review round 2 on #268: the kubectl-diff
// reclassification (item F: inconclusive vs. differs vs. error, dropping
// the "--- " heuristic), the render-dir marker/ownership and symlink guard
// (item G), the restore.d/--only/--with-restore policy (item H) and the
// unmarked repeatable entry (item I). Round 3 (the in-place redesign) is in
// dryrun_round3_test.go.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// launchFailError does NOT implement ExitCode() — execx.LaunchFailed
// reports true for it, the shape a missing kubectl binary or a context
// that ended before the process started would take.
type launchFailError struct{}

func (launchFailError) Error() string { return "exec: \"kubectl\": executable file not found in $PATH" }

// ── Item F: classify the process result directly ──────────────────────

func TestDryRunInconclusiveDoesNotCountAsDiffers(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error {
		_, _ = c.Stderr.Write([]byte("error: ensure CRDs are installed first\n"))
		return &rcError{2} // >1, race pattern on stderr
	}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
	}
	if errors.Is(err, ErrDryRunDiffers) {
		t.Error("inconclusive must not report as ErrDryRunDiffers")
	}
	want := "bootstrap dry-run: 01-testcni: inconclusive (needs-crd)\n" +
		"bootstrap dry-run: entries=1 unchanged=0 differs=0 rendered=0 skipped=0 inconclusive=1 errors=0 nondeterministic=0\n"
	if got := stdoutOf(e); got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
}

// The race pattern at rc=1 (the shape the original implementation and the
// bats fixtures actually produce) classifies as inconclusive too, not
// differs.
func TestDryRunInconclusiveAtRCOne(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error {
		_, _ = c.Stderr.Write([]byte("error: no matches for kind \"Foo\" in version \"v1\"\n"))
		return &rcError{1}
	}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2", driver.ExitCode(err))
	}
	if !strings.Contains(stdoutOf(e), "01-testcni: inconclusive (needs-crd)") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
}

// KUBECTL_EXTERNAL_DIFF (e.g. `diff -q`) exits 1 on a real difference
// with NO "--- " header at all — the old heuristic misclassified this as
// an error; it must still be "differs".
func TestDryRunExternalDiffWithoutHeaderStillDiffers(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error {
		_, _ = c.Stdout.Write([]byte("Files LIVE and MERGED differ\n"))
		return &rcError{1}
	}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v, want ErrDryRunDiffers", err)
	}
	if !strings.Contains(stdoutOf(e), "01-testcni: differs\n") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
}

// A launch failure (the kubectl binary was never found) is always error,
// never misread as a diff result.
func TestDryRunLaunchFailureIsError(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error { return launchFailError{} }
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2", driver.ExitCode(err))
	}
	if !strings.Contains(stdoutOf(e), "01-testcni: error\n") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	if !strings.Contains(stderrOf(e), "failed to start") {
		t.Errorf("stderr should say the process never started:\n%s", stderrOf(e))
	}
}

// ── Item G: render-dir ownership (marker) + never follow a symlink ────

func TestRenderDirRefusesForeignNonEmptyDir(t *testing.T) {
	e, _, spec, kc := dryEngine(t, "testcni")
	dir := t.TempDir()
	testutil.WriteFile(t, filepath.Join(dir, "README.md"), "not mine\n")
	e.Options.RenderDir = dir
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), ".lo-dry-run") {
		t.Errorf("err = %v, want it to name the marker", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "01-testcni.yaml")); statErr == nil {
		t.Error("wrote into a foreign directory despite the refusal")
	}
}

func TestRenderDirMarkerAllowsReuseAndClearsOwnedFiles(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	dir := t.TempDir()
	e.Options.RenderDir = dir
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// A stale file from an earlier, larger selection — must be cleared on
	// the next run even though it is no longer in the new set.
	testutil.WriteFile(t, filepath.Join(dir, "01-stale.yaml"), "stale\n")
	testutil.WriteFile(t, filepath.Join(dir, "01-stale.diff"), "stale\n")

	e2, f2, spec2, kc2 := dryEngine(t, "testcni")
	e2.Options.RenderDir = dir
	diffAnswers(t, f2, 0)
	if err := e2.Apply(t.Context(), "test.lok8s.dev", spec2, kc2); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "01-stale.yaml")); !os.IsNotExist(err) {
		t.Errorf("stale owned file survived a second run (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "01-testcni.yaml")); err != nil {
		t.Errorf("the current run's own file is missing: %v", err)
	}
}

// writeGuarded itself: refuses to write through an existing symlink
// (item G, "never follow symlinks inside it") rather than following it to
// whatever it points at.
func TestWriteGuardedRefusesExistingSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escape.yaml")
	testutil.WriteFile(t, outside, "original\n")
	if err := os.Symlink(outside, filepath.Join(dir, "target.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := writeGuarded(filepath.Join(dir, "target.yaml"), []byte("new content\n"), 0o600); err == nil {
		t.Fatal("expected a refusal")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "original\n" {
		t.Errorf("escape target was written through the symlink: %q, %v", got, err)
	}
}

// writeGuarded writes normally when nothing (or a plain file) is already
// at path.
func TestWriteGuardedWritesPlainFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.yaml")
	if err := writeGuarded(path, []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "content\n" {
		t.Errorf("got %q, %v", got, err)
	}
}

// A render dir's OWNED-pattern clearing (prepareRenderDir) removes a
// symlink that happens to sit at an owned name via os.Remove — which
// unlinks the symlink itself and never follows it — so the escape target
// is untouched and the NEXT write lands on a fresh, real file.
func TestRenderDirClearingNeverFollowsAnOwnedSymlink(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	dir := t.TempDir()
	e.Options.RenderDir = dir
	diffAnswers(t, f, 0)
	// First run: marker + a legitimate 01-testcni.yaml.
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("first run: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "escape.yaml")
	testutil.WriteFile(t, outside, "original\n")
	if err := os.Remove(filepath.Join(dir, "01-testcni.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "01-testcni.yaml")); err != nil {
		t.Fatal(err)
	}

	e2, f2, spec2, kc2 := dryEngine(t, "testcni")
	e2.Options.RenderDir = dir
	diffAnswers(t, f2, 0)
	if err := e2.Apply(t.Context(), "test.lok8s.dev", spec2, kc2); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "original\n" {
		t.Errorf("the escape target was modified: %q, %v", got, err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "01-testcni.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("01-testcni.yaml is still a symlink — the clearing step did not remove it")
	}
}

// ── Item H: restore.d stays out of --only unless --with-restore ───────

func TestOnlyRealRunSkipsRestoreDAndWarnsLoudly(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-a.sops.yaml"), "cipher\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "secrets", "restore.d", "tls-a.yaml"), "kind: Secret\n")
	writeStackAddon(t, p)
	spec := writeClusterSpec(t, p, "testcni")
	kc := writeKubeconfig(t, p)
	e.Options.Only = []string{"testcni"}

	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(f.log(), "restore.d/tls-a.yaml") {
		t.Errorf("restore.d was applied despite --only without --with-restore:\n%s", f.log())
	}
	errOut := e.Stderr.(interface{ String() string }).String()
	if !strings.Contains(errOut, "WITHOUT restoring restore.d") || !strings.Contains(errOut, "tls-a.sops.yaml") {
		t.Errorf("missing the loud warning naming the file:\n%s", errOut)
	}
}

func TestOnlyWithRestoreAppliesRestoreDBeforeTheSelectedEntry(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls-a.sops.yaml"), "cipher\n")
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "secrets", "restore.d", "tls-a.yaml"), "kind: Secret\n")
	writeStackAddon(t, p)
	spec := writeClusterSpec(t, p, "testcni")
	kc := writeKubeconfig(t, p)
	e.Options.Only = []string{"testcni"}
	e.Options.WithRestore = true

	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	log := f.log()
	if !strings.Contains(log, "restore.d/tls-a.yaml") {
		t.Fatalf("restore.d was not applied with --with-restore:\n%s", log)
	}
}

// --only --dry-run (no --with-restore) still reports restore.d skipped —
// the plain (non-loud) line, since nothing is actually at risk in a
// preview.
func TestOnlyDryRunSkipsRestoreDPlainMessage(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "restore.d", "tls-a.sops.yaml"), "cipher\n")
	e.Options.Only = []string{"testcni"}
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	errOut := stderrOf(e)
	if !strings.Contains(errOut, "--only: restore.d skipped (not applied)") {
		t.Errorf("stderr:\n%s", errOut)
	}
	if strings.Contains(errOut, "WITHOUT restoring") {
		t.Error("the loud real-run warning must not fire on a dry run")
	}
}

// ── Item I: non-repeatable marking (the round-3 report-file plumbing is
// in dryrun_round3_test.go) ────────────────────────────────────────────

// An entry that mints NOTHING is not marked.
func TestDryRunDoesNotMarkNonRepeatableWhenNothingMinted(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(stdoutOf(e), "non-repeatable") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
}
