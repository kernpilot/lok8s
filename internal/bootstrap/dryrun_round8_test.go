package bootstrap

// dryrun_round8_test.go — review round 8 on #268: --render-dir together
// with restore.d. The render dir is prepared (created, or a reused dry-run
// dir cleared of its owned files) BEFORE the restore.d preview, which writes
// restore.d.{yaml,diff} into it. The bats twin is "bootstrap::apply
// --dry-run --render-dir with restore.d" in tests/unit/bootstrap_test.bats.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

const restoreDSecret = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls\n"

func restoreDEngine(t *testing.T) (*Engine, *fakeRunner, string, string) {
	t.Helper()
	e, f, spec, kc := dryEngine(t, "testcni")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "restore.d", "tls.sops.yaml"), "sops: {}\n")
	e.SopsDecrypt = func(string) ([]byte, error) { return []byte(restoreDSecret), nil }
	return e, f, spec, kc
}

// (a) A render dir that does not exist yet: the run creates it before the
// restore.d preview, so restore.d.{yaml,diff} land in it and restore.d's
// status is the real diff status, not an error.
func TestDryRunRenderDirNewWithRestoreD(t *testing.T) {
	e, f, spec, kc := restoreDEngine(t)
	dir := filepath.Join(t.TempDir(), "fresh", "out")
	e.Options.RenderDir = dir
	diffAnswers(t, f, 1, 0) // restore.d differs, testcni unchanged
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v\n%s", err, stderrOf(e))
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: restore.d: differs changed=1 new=2\n") {
		t.Errorf("restore.d's status is not the diff's:\n%s", stdoutOf(e))
	}
	if m, err := os.ReadFile(filepath.Join(dir, "restore.d.yaml")); err != nil || string(m) != restoreDSecret {
		t.Errorf("restore.d.yaml = %q (%v)", m, err)
	}
	if d, err := os.ReadFile(filepath.Join(dir, "restore.d.diff")); err != nil || string(d) != cannedDiff {
		t.Errorf("restore.d.diff = %q (%v)", d, err)
	}
}

// (b) A reused dry-run dir that holds the files of an earlier run (an old
// restore.d.yaml/.diff and an entry that this run does not have): the
// owned files are cleared before the restore.d preview, so restore.d's
// files are this run's and the stale entry file is gone.
func TestDryRunRenderDirReusedWithRestoreD(t *testing.T) {
	e, f, spec, kc := restoreDEngine(t)
	dir := filepath.Join(t.TempDir(), "out")
	testutil.WriteFile(t, filepath.Join(dir, renderDirMarker), renderDirMarkerText)
	testutil.WriteFile(t, filepath.Join(dir, "restore.d.yaml"), "old restore.d manifest\n")
	testutil.WriteFile(t, filepath.Join(dir, "restore.d.diff"), "old restore.d diff\n")
	testutil.WriteFile(t, filepath.Join(dir, "05-stale.yaml"), "old entry\n")
	testutil.WriteFile(t, filepath.Join(dir, "05-stale.diff"), "old entry diff\n")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Options.RenderDir = dir
	diffAnswers(t, f, 0, 0) // both unchanged
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("err = %v\n%s", err, stderrOf(e))
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: restore.d: unchanged\n") {
		t.Errorf("restore.d's status is not the diff's:\n%s", stdoutOf(e))
	}
	if m, err := os.ReadFile(filepath.Join(dir, "restore.d.yaml")); err != nil || string(m) != restoreDSecret {
		t.Errorf("restore.d.yaml = %q (%v), want this run's", m, err)
	}
	if d, err := os.ReadFile(filepath.Join(dir, "restore.d.diff")); err != nil || len(d) != 0 {
		t.Errorf("restore.d.diff = %q (%v), want this run's (empty: unchanged)", d, err)
	}
	for _, name := range []string{"05-stale.yaml", "05-stale.diff"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the stale %s is still there (%v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "01-testcni.yaml")); err != nil {
		t.Errorf("01-testcni.yaml: %v", err)
	}
}
