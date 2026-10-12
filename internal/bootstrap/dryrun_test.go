package bootstrap

// dryrun_test.go — the Go half of the --dry-run / --render-dir / --no-diff
// / --only contract (bats: the "bootstrap::apply dry-run" and "--only"
// cases of tests/unit/bootstrap_test.bats). Every kubectl goes through the
// fake runner; the recorded argv is the assertion that nothing is applied.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// cannedDiff is a two-object `kubectl diff` (GNU diff -u -N over the LIVE
// and MERGED trees): one object modified, two the live side lacks
// (asymmetric on purpose: a swapped changed/new count must show).
const cannedDiff = `diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.testcni /tmp/MERGED-1/v1.ConfigMap.default.testcni
--- /tmp/LIVE-1/v1.ConfigMap.default.testcni	2026-10-12 10:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.testcni	2026-10-12 10:00:00.000000000 +0000
@@ -3,4 +3,5 @@
 kind: ConfigMap
 metadata:
   name: testcni
+  foo: bar
diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.extra /tmp/MERGED-1/v1.ConfigMap.default.extra
--- /tmp/LIVE-1/v1.ConfigMap.default.extra	1970-01-01 00:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.extra	2026-10-12 10:00:00.000000000 +0000
@@ -0,0 +1,4 @@
+apiVersion: v1
+kind: ConfigMap
+metadata:
+  name: extra
diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.other /tmp/MERGED-1/v1.ConfigMap.default.other
--- /tmp/LIVE-1/v1.ConfigMap.default.other	1970-01-01 00:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.other	2026-10-12 10:00:00.000000000 +0000
@@ -0,0 +1,4 @@
+apiVersion: v1
+kind: ConfigMap
+metadata:
+  name: other
`

// diffAnswers scripts the kubectl diff calls in order: each element is the
// exit code of one call (1 → cannedDiff on stdout). Any other kubectl verb
// fails the test: a dry run may only ever diff.
func diffAnswers(t *testing.T, f *fakeRunner, codes ...int) {
	t.Helper()
	call := 0
	f.handler = func(c execx.Cmd) error {
		// Every call is `--kubeconfig <path> <verb> …`.
		verb := ""
		if len(c.Args) > 2 && c.Args[0] == "--kubeconfig" {
			verb = c.Args[2]
		}
		if verb != "diff" {
			t.Errorf("a dry run reached kubectl %q: %v", verb, c.Args)
			return &rcError{9}
		}
		if c.Stdin == nil {
			t.Error("kubectl diff got no manifest on stdin")
		}
		rc := 0
		if call < len(codes) {
			rc = codes[call]
		}
		call++
		switch rc {
		case 0:
			return nil
		case 1:
			fmt.Fprint(c.Stdout, cannedDiff)
		default:
			fmt.Fprintln(c.Stderr, "error: the server could not be reached")
		}
		return &rcError{rc}
	}
}

func dryEngine(t *testing.T, entries ...string) (*Engine, *fakeRunner, string, string) {
	t.Helper()
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	mkChartAddon(t, p, "second")
	spec := writeClusterSpec(t, p, entries...)
	kc := writeKubeconfig(t, p)
	e.Options.DryRun = true
	return e, f, spec, kc
}

func stdoutOf(e *Engine) string { return e.Stdout.(interface{ String() string }).String() }
func stderrOf(e *Engine) string { return e.Stderr.(interface{ String() string }).String() }

func TestDryRunDiffsEveryEntryAndAppliesNothing(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second: {dependsOn: [testcni]}")
	diffAnswers(t, f, 0, 1)
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !errors.Is(err, ErrDryRunDiffers) || driver.ExitCode(err) != 1 {
		t.Fatalf("err = %v (rc %d), want ErrDryRunDiffers rc 1", err, driver.ExitCode(err))
	}
	want := "bootstrap dry-run: 01-testcni: unchanged\n" +
		"bootstrap dry-run: 02-second: differs changed=1 new=2\n" +
		"bootstrap dry-run: entries=2 unchanged=1 differs=1 rendered=0 skipped=0 errors=0\n"
	if got := stdoutOf(e); got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
	log := f.log()
	if strings.Contains(log, " apply ") || strings.Contains(log, " replace ") || strings.Contains(log, " patch ") {
		t.Errorf("a dry run wrote to the cluster:\n%s", log)
	}
	if n := strings.Count(log, "kubectl --kubeconfig "+kc+" diff --server-side --force-conflicts -f -"); n != 2 {
		t.Errorf("want 2 server-side diffs, got %d:\n%s", n, log)
	}
}

func TestDryRunRenderDirHoldsManifestsAndDiffs(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second")
	dir := filepath.Join(t.TempDir(), "out", "nested")
	e.Options.RenderDir = dir
	diffAnswers(t, f, 0, 1)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("render dir: %v", err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if got := strings.Join(names, " "); got != "01-testcni.diff 01-testcni.yaml 02-second.diff 02-second.yaml" {
		t.Errorf("files = %q", got)
	}
	manifest, _ := os.ReadFile(filepath.Join(dir, "01-testcni.yaml"))
	if m := string(manifest); !strings.Contains(m, "kind: ConfigMap") || !strings.HasSuffix(m, "testcni\n") || strings.HasSuffix(m, "\n\n") {
		t.Errorf("01-testcni.yaml = %q", m)
	}
	if d, _ := os.ReadFile(filepath.Join(dir, "01-testcni.diff")); len(d) != 0 {
		t.Errorf("an unchanged entry's diff file must be empty, got %q", d)
	}
	if d, _ := os.ReadFile(filepath.Join(dir, "02-second.diff")); string(d) != cannedDiff {
		t.Errorf("02-second.diff = %q", d)
	}
	if !strings.Contains(stderrOf(e), "bootstrap dry-run: files under "+dir) {
		t.Errorf("stderr does not name the render dir:\n%s", stderrOf(e))
	}
}

func TestDryRunExitCodes(t *testing.T) {
	for _, tc := range []struct {
		rc     int
		status string
		exit   int
	}{{0, "unchanged", 0}, {1, "differs changed=1 new=2", 1}, {2, "error", 2}} {
		t.Run(tc.status, func(t *testing.T) {
			e, f, spec, kc := dryEngine(t, "testcni")
			diffAnswers(t, f, tc.rc)
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if got := driver.ExitCode(err); got != tc.exit {
				t.Errorf("exit = %d (%v), want %d", got, err, tc.exit)
			}
			if !strings.Contains(stdoutOf(e), "bootstrap dry-run: 01-testcni: "+tc.status+"\n") {
				t.Errorf("stdout:\n%s", stdoutOf(e))
			}
			if tc.rc == 2 {
				for _, want := range []string{"error: the server could not be reached", "bootstrap: kubectl diff failed for testcni (rc=2)"} {
					if !strings.Contains(stderrOf(e), want) {
						t.Errorf("stderr missing %q:\n%s", want, stderrOf(e))
					}
				}
			}
		})
	}
}

func TestDryRunNoDiffNeverReachesTheCluster(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	e.Options.NoDiff = true
	e.Options.RenderDir = t.TempDir()
	f.handler = func(c execx.Cmd) error {
		t.Errorf("--no-diff reached kubectl: %v", c.Args)
		return &rcError{9}
	}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "bootstrap dry-run: 01-testcni: rendered\n" +
		"bootstrap dry-run: entries=1 unchanged=0 differs=0 rendered=1 skipped=0 errors=0\n"
	if got := stdoutOf(e); got != want {
		t.Errorf("stdout:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(e.Options.RenderDir, "01-testcni.yaml")); err != nil {
		t.Errorf("manifest not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Options.RenderDir, "01-testcni.diff")); err == nil {
		t.Error("--no-diff must not write a .diff file")
	}
}

func TestDryRunNoDiffSkipsTheHostedProbe(t *testing.T) {
	e, f, _, spec, kc := hostedHarness(t)
	e.Options.DryRun, e.Options.NoDiff = true, true
	f.handler = func(c execx.Cmd) error {
		t.Errorf("a render-only dry run probed the cluster: %v", c.Args)
		return &rcError{9}
	}
	if err := e.Apply(t.Context(), "h.test", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(stdoutOf(e), "01-cert-manager: rendered") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
}

func TestDryRunIgnoresForce(t *testing.T) {
	// --force on a dry run heals nothing: an immutable conflict in the diff
	// is reported, never recreated.
	t.Setenv("LOK8S_FORCE_RECREATE", "1")
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error {
		if !strings.Contains(strings.Join(c.Args, " "), " diff ") {
			t.Errorf("force reached kubectl %v", c.Args)
			return &rcError{9}
		}
		fmt.Fprint(c.Stdout, cannedDiff)
		fmt.Fprintln(c.Stderr, `The Deployment "testcni" is invalid: spec.selector: field is immutable`)
		return &rcError{1}
	}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v", err)
	}
	if log := f.log(); strings.Contains(log, "replace") || strings.Contains(log, "apply") {
		t.Errorf("force healed under a dry run:\n%s", log)
	}
	if !strings.Contains(stderrOf(e), "field is immutable") {
		t.Errorf("the diff's stderr must be forwarded:\n%s", stderrOf(e))
	}
}

func TestDryRunReportsTheRealRunsSkips(t *testing.T) {
	// A KubeOne full provision defers cilium to the driver; the dry run
	// says so with the real run's line and counts it as skipped.
	t.Setenv("LOK8S_BOOTSTRAP_ONLY", "0")
	e, f, _, _, p := testEngine(t)
	spec, kc := kubeoneAddonSpec(t, p, "cilium")
	e.Options.DryRun = true
	diffAnswers(t, f)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: 01-cilium: skipped\n") || !strings.Contains(stdoutOf(e), "skipped=1") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	if !strings.Contains(stderrOf(e), "cilium is applied by the KubeOne driver on a full provision") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}
	if f.log() != "" {
		t.Errorf("a skipped entry reached a tool:\n%s", f.log())
	}
}

// A dry run NEVER applies restore.d — but it DOES decrypt it in memory
// (read-only, same as any other sops decrypt in this codebase) so it can
// report what a real run would restore, as its own entry ahead of the
// addons.
func TestDryRunDiffsRestoreDAsItsOwnEntry(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "restore.d", "tls.sops.yaml"), "sops: {}\n")
	decrypted := false
	e.SopsDecrypt = func(string) ([]byte, error) {
		decrypted = true
		return []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls\n"), nil
	}
	diffAnswers(t, f, 1, 0) // restore.d differs, testcni unchanged
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v", err)
	}
	if !decrypted {
		t.Error("a dry run must decrypt restore.d to diff it (never to apply it)")
	}
	want := "bootstrap dry-run: restore.d: differs changed=1 new=2\n" +
		"bootstrap dry-run: 01-testcni: unchanged\n" +
		"bootstrap dry-run: entries=2 unchanged=1 differs=1 rendered=0 skipped=0 errors=0\n"
	if got := stdoutOf(e); got != want {
		t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
	}
	if log := f.log(); strings.Contains(log, " apply ") {
		t.Errorf("a dry run applied restore.d:\n%s", log)
	}
}

// --only takes restore.d out of scope entirely — never applied, never
// diffed — and says so.
func TestDryRunOnlySkipsRestoreD(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "restore.d", "tls.sops.yaml"), "sops: {}\n")
	decrypted := false
	e.SopsDecrypt = func(string) ([]byte, error) { decrypted = true; return nil, nil }
	e.Options.Only = []string{"testcni"}
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if decrypted {
		t.Error("--only must not decrypt restore.d either")
	}
	if strings.Contains(stdoutOf(e), "restore.d") {
		t.Errorf("--only must not report a restore.d entry:\n%s", stdoutOf(e))
	}
	if !strings.Contains(stderrOf(e), "[bootstrap] --only: restore.d skipped (not applied) — 1 file(s) under clusters/test.lok8s.dev/restore.d") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}
}

func TestDryRunRenderFailureIsAnError(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second")
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error {
		// "second" is a chart.yaml addon: RenderIsolated always copies it
		// to a temp dir under dry-run (see addons.RenderIsolated), so the
		// build dir path itself no longer names the addon — identify it by
		// the copied chart.yaml's own content instead.
		if c.Name == "kustomize" {
			buildDir := c.Args[len(c.Args)-1]
			if raw, err := os.ReadFile(filepath.Join(buildDir, "chart.yaml")); err == nil && strings.Contains(string(raw), "name: second") {
				return &rcError{1}
			}
		}
		return f.Run(ctx, c)
	})
	diffAnswers(t, f, 0)
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("err = %v, want rc 2", err)
	}
	if !strings.Contains(stdoutOf(e), "02-second: error\n") || !strings.Contains(stdoutOf(e), "errors=1") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	if !strings.Contains(stderrOf(e), "bootstrap: render failed for second") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}
}

func TestSummarizeDiff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		diff           string
		changed, added int
		counted        bool
	}{
		{"canned", cannedDiff, 1, 2, true},
		{"empty", "", 0, 0, false},
		{"external tool", "Deployment/x differs\n", 0, 0, false},
		{"only new", "--- a\n+++ b\n@@ -0,0 +1,2 @@\n+x\n+y\n", 0, 1, true},
	} {
		changed, added, counted := summarizeDiff(tc.diff)
		if changed != tc.changed || added != tc.added || counted != tc.counted {
			t.Errorf("%s: got (%d, %d, %v), want (%d, %d, %v)", tc.name, changed, added, counted, tc.changed, tc.added, tc.counted)
		}
	}
}

// --- --only ---------------------------------------------------------------

func TestOnlyAppliesTheNamedEntriesWithoutExpandingDeps(t *testing.T) {
	e, log, spec, kc, _ := schedEngine(t, "a", "b: {dependsOn: [a]}", "c")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(fmt.Sprintf("%s wait=%v", job.Name, job.Wait))
		return 0
	}
	e.Options.Only = []string{"b"}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if log.count() != 1 || !log.has("b wait=false") {
		t.Errorf("applied %v, want only b (its dependency a is NOT pulled in)", log.events)
	}
}

func TestOnlyKeepsTheEdgesAmongTheSelected(t *testing.T) {
	e, log, spec, kc, _ := schedEngine(t, "a", "b: {dependsOn: [a]}", "c")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(fmt.Sprintf("%s wait=%v", job.Name, job.Wait))
		return 0
	}
	e.Options.Only = []string{"b", "a"}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// a is still b's dependency: it gets the readiness wait and runs first.
	if log.count() != 2 || log.pos("a wait=true") != 1 || log.pos("b wait=false") != 2 {
		t.Errorf("events = %v", log.events)
	}
}

func TestOnlyMatchesTheAddonNameOfARenamedEntry(t *testing.T) {
	e, log, spec, kc, _ := schedEngine(t, "cilium: {name: my-cni}", "c")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name)
		return 0
	}
	e.Options.Only = []string{"cilium"}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if log.count() != 1 || !log.has("my-cni") {
		t.Errorf("events = %v", log.events)
	}
}

func TestOnlyUnknownNameListsTheValidOnes(t *testing.T) {
	e, log, spec, kc, _ := schedEngine(t, "a", "b: {name: bee}", "c")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name)
		return 0
	}
	e.Options.Only = []string{"a", "nope"}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if err == nil || driver.ExitCode(err) != 1 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderrOf(e), "bootstrap: --only: unknown entry 'nope' (valid: a, bee, c)") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}
	if log.count() != 0 {
		t.Errorf("an unknown --only name must apply nothing, got %v", log.events)
	}
}

func TestOnlyDryRunNumbersTheSelection(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second")
	e.Options.Only = []string{"second"}
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "bootstrap dry-run: 01-second: unchanged\n" +
		"bootstrap dry-run: entries=1 unchanged=1 differs=0 rendered=0 skipped=0 errors=0\n"
	if got := stdoutOf(e); got != want {
		t.Errorf("stdout:\n%s", got)
	}
}

// An unknown --only name is validated BEFORE any writer runs — restore.d
// included. A typo must never apply (or dry-run-apply) restore.d and only
// THEN fail; it must not even decrypt it.
func TestOnlyTypoNeverTouchesRestoreD(t *testing.T) {
	e, log, spec, kc, p := schedEngine(t, "a", "b: {name: bee}", "c")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name)
		return 0
	}
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls.sops.yaml"), "sops: {}\n")
	decrypted := false
	e.SopsDecrypt = func(string) ([]byte, error) { decrypted = true; return nil, nil }
	var applyCalls []string
	runner := runnerFunc(func(ctx context.Context, c execx.Cmd) error {
		applyCalls = append(applyCalls, c.Name+" "+strings.Join(c.Args, " "))
		return nil
	})
	e.Runner = runner
	e.Options.Only = []string{"a", "nope"}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if err == nil || driver.ExitCode(err) != 1 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderrOf(e), "bootstrap: --only: unknown entry 'nope'") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}
	if log.count() != 0 {
		t.Errorf("applied: %v", log.events)
	}
	if decrypted {
		t.Error("--only typo decrypted restore.d before failing")
	}
	for _, c := range applyCalls {
		if strings.Contains(c, "apply") {
			t.Errorf("--only typo reached a kubectl apply: %s", c)
		}
	}
}

// A hosted cluster a dry run cannot reach (or that has no Ready workers
// yet) is an INCONCLUSIVE assessment, never "nothing would change" — that
// must exit >1, not the clean 0 a real run gets for the identical miss.
func TestHostedGateDryRunUnreachableIsExitTwo(t *testing.T) {
	e, f, errBuf, spec, kc := hostedHarness(t)
	f.handler = func(c execx.Cmd) error {
		fmt.Fprintln(c.Stderr, "error: You must be logged in")
		return &rcError{1}
	}
	e.Options.DryRun = true
	err := e.Apply(t.Context(), "h.test", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("err = %v, want rc 2", err)
	}
	if !strings.Contains(errBuf.String(), "assessment not done") {
		t.Errorf("stderr:\n%s", errBuf.String())
	}
}

func TestHostedGateDryRunNoWorkersIsExitTwo(t *testing.T) {
	e, f, _, spec, kc := hostedHarness(t)
	f.handler = func(c execx.Cmd) error {
		fmt.Fprintln(c.Stdout, "cp1 NotReady control-plane 1d v1.33")
		return nil
	}
	e.Options.DryRun = true
	err := e.Apply(t.Context(), "h.test", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("err = %v, want rc 2", err)
	}
}

// A hermetic filesystem snapshot: the WHOLE project tree is byte-for-byte
// identical before and after a --dry-run — a builtin addon (served from
// the embed via Peek, never ejected) and the per-domain secrets store (read
// through an isolated copy, never the original) both included.
func TestDryRunChangesNothingOnDisk(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "cilium", "testcni")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "secrets", "Secret.app.default.TOKEN"), "ENC[token]\n")
	diffAnswers(t, f, 0, 0)

	before := hashTree(t, e.Paths.Base)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after := hashTree(t, e.Paths.Base)
	if before != after {
		t.Error("a dry run changed the project tree")
	}
	if fsutil.DirExists(filepath.Join(e.Paths.Lok8s, "addons", "cilium")) {
		t.Error("a dry run ejected the builtin cilium addon into the project")
	}
}

// hashTree hashes every path's name, mode, size and content under root into
// one digest — a cheap, exact "did anything change" oracle for a dry run.
func hashTree(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fmt.Fprintf(h, "%s|%s|%d\n", rel, info.Mode(), info.Size())
		if info.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		h.Write(data)
		return nil
	})
	if err != nil {
		t.Fatalf("hashTree(%s): %v", root, err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// The real apply and the dry run's diff must see the EXACT SAME per-entry
// env: dropping jobEnv(job) from either path (dryRunOne or applyOne) would
// render a DIFFERENT manifest — an entry's env: override feeds envsubst —
// and kubectl would apply or diff something this run never actually
// rendered.
func TestApplyAndDryRunSendTheSameManifest(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	f.manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: testcni\ndata:\n  foo: ${LOK8S_USER_FOO}\n"
	kc := writeKubeconfig(t, p)

	job := Job{Name: "testcni", Dir: filepath.Join(p.Lok8s, "addons", "testcni"), Kubeconfig: kc, EnvLines: "LOK8S_USER_FOO=bar123"}

	var applyStdin string
	f.handler = func(c execx.Cmd) error {
		if len(c.Args) > 2 && c.Args[2] == "apply" {
			raw, _ := io.ReadAll(c.Stdin)
			applyStdin = string(raw)
		}
		return nil
	}
	if rc := e.applyOne(t.Context(), job, io.Discard, io.Discard); rc != 0 {
		t.Fatalf("applyOne rc=%d", rc)
	}

	var diffStdin string
	f.handler = func(c execx.Cmd) error {
		if len(c.Args) > 2 && c.Args[2] == "diff" {
			raw, _ := io.ReadAll(c.Stdin)
			diffStdin = string(raw)
		}
		return nil
	}
	e.Options.DryRun = true
	res := e.dryRunOne(t.Context(), job, "01-testcni")
	if res.status != dryUnchanged {
		t.Fatalf("dryRunOne status = %v (%s)", res.status, stderrOf(e))
	}

	if applyStdin == "" || diffStdin == "" {
		t.Fatalf("missing capture: apply=%q diff=%q", applyStdin, diffStdin)
	}
	if !strings.Contains(applyStdin, "bar123") {
		t.Errorf("apply stdin missing the env: override:\n%s", applyStdin)
	}
	if applyStdin != diffStdin {
		t.Errorf("apply and dry-run rendered DIFFERENT manifests:\napply:\n%s\ndiff:\n%s", applyStdin, diffStdin)
	}
}

func TestDispatchDryRunSkipsTheInventory(t *testing.T) {
	p := testPaths(t)
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, spec, "kind: Lo\nmetadata:\n  name: x\nspec:\n  bootstrap: []\n")
	testutil.WriteFile(t, filepath.Join(p.Base, ".kubeconfig", "x.yaml"), "")
	published := false
	d := &Dispatcher{
		Engine: &Engine{Paths: p, Runner: runnerFunc(func(context.Context, execx.Cmd) error { return nil }), Stdout: io.Discard, Stderr: io.Discard, Options: Options{DryRun: true}},
		Drivers: func(string) (driver.Factory, bool) {
			return func(*driver.Deps) (driver.Driver, error) { return &fakeDriver{}, nil }, true
		},
		InventoryPublish: func(context.Context, string, string, string) {
			published = true
		},
	}
	if err := d.Dispatch(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if published {
		t.Error("a dry run published the inventory")
	}
}

// PATH_SECRETS, under a dry run, must point at a COPY of the store — never
// the original path — so a generator's cache write (passwd:, or bash: with
// --allow-generators) lands in something disposable. The copy carries the
// same content and is gone again once Apply returns; PATH_SECRETS itself
// is restored to whatever it was before (never leaked across calls).
func TestDryRunIsolatesPathSecrets(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	secretsDir := filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "secrets")
	testutil.WriteFile(t, filepath.Join(secretsDir, "Secret.app.default.TOKEN"), "ENC[token]\n")
	os.Setenv("PATH_SECRETS", "/should-be-restored")
	t.Cleanup(func() { os.Unsetenv("PATH_SECRETS") })

	// Observed and read INSIDE the handler, while Apply is still running:
	// the isolation copy is removed by the time Apply returns (correctly —
	// that's the cleanup assertion below), so the content check has to
	// happen before then.
	var seenDuring, copyContent string
	var copyReadErr error
	diffAnswers(t, f, 0)
	f2 := f.handler
	f.handler = func(c execx.Cmd) error {
		if seenDuring == "" {
			seenDuring = os.Getenv("PATH_SECRETS")
			raw, err := os.ReadFile(filepath.Join(seenDuring, "Secret.app.default.TOKEN"))
			copyContent, copyReadErr = string(raw), err
		}
		return f2(c)
	}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if seenDuring == "" {
		t.Fatal("PATH_SECRETS was never observed during the run")
	}
	if seenDuring == secretsDir {
		t.Errorf("PATH_SECRETS pointed at the real store during a dry run: %s", seenDuring)
	}
	if copyReadErr != nil || copyContent != "ENC[token]\n" {
		t.Errorf("the copy did not carry the real store's content: %v %q", copyReadErr, copyContent)
	}
	if fsutil.DirExists(seenDuring) {
		t.Errorf("the isolated copy %s was not cleaned up", seenDuring)
	}
	if got := os.Getenv("PATH_SECRETS"); got != "/should-be-restored" {
		t.Errorf("PATH_SECRETS = %q after Apply, want the restored prior value", got)
	}
}
