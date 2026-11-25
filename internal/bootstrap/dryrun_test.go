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
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// cannedDiff is a two-object `kubectl diff` (GNU diff -u -N over the LIVE
// and MERGED trees): one object modified, two the live side lacks
// (asymmetric on purpose: a swapped changed/new count must show).
const cannedDiff = `diff -u -N /tmp/LIVE-1/v1.ConfigMap.default.testcni /tmp/MERGED-1/v1.ConfigMap.default.testcni
--- /tmp/LIVE-1/v1.ConfigMap.default.testcni	2026-10-12 10:00:00.000000000 +0000
+++ /tmp/MERGED-1/v1.ConfigMap.default.testcni	2026-10-12 10:00:00.000000000 +0000
@@ -3,3 +3,4 @@
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
// exit code of one call (1 → cannedDiff on stdout). The read of the field
// managers finds no live object. Any other kubectl verb fails the test: a
// dry run may only diff and read.
func diffAnswers(t *testing.T, f *fakeRunner, codes ...int) {
	t.Helper()
	call := 0
	f.handler = func(c execx.Cmd) error {
		if isFieldManagerRead(c) {
			return nil
		}
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
		refuseTerminalStreams(t, c)
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
	want := "bootstrap dry-run: testcni: unchanged\n" +
		"bootstrap dry-run: second: differs changed=1 new=2\n" +
		"bootstrap dry-run: entries=2 unchanged=1 differs=1 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0\n"
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
	if got := strings.Join(names, " "); got != ".lo-dry-run 01-testcni.diff 01-testcni.yaml 02-second.diff 02-second.yaml" {
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
			if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: "+tc.status+"\n") {
				t.Errorf("stdout:\n%s", stdoutOf(e))
			}
			// diffAnswers prints its stderr line only at rc > 1: the rc 0
			// and rc 1 cases with a kubectl message are the tests below.
			if tc.rc == 2 {
				if want := "bootstrap: kubectl diff failed for testcni (rc=2). "; !strings.Contains(stderrOf(e), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderrOf(e))
				}
				refuteOutput(t, stderrOf(e), "the server could not be reached")
			}
		})
	}
}

// The kubectl output of the next tests is synthetic. Its stderr has the
// shapes of a field error and of a typed-patch error of a server-side
// diff, which quote a value of the manifest; its stdout is diff text. The
// two streams carry different canaries, obvious fakes, so a test can tell
// which stream leaked.
const (
	diffValueCanary = "canary-value-0000"     // on stderr
	diffTextCanary  = "diff-text-canary-1111" // on stdout
	diffErrText     = `The Secret "tls" is invalid: data[tls.crt]: Invalid value: "` + diffValueCanary + `": a value in the manifest` + "\n" +
		`Error from server: failed to create typed patch object (/v1, Kind=Secret) default/tls: .stringData.tls.crt: expected string, got &value.valueUnstructured{Value:` + diffValueCanary + `}`
	diffPartialText = "partial diff text: " + diffTextCanary + "\n"
	// diffOutWithCanary is cannedDiff with the stdout canary in its last
	// hunk (the counts stay changed=1 new=2).
	diffOutWithCanary = cannedDiff + "+  note: " + diffTextCanary + "\n"
)

// refuseTerminalStreams fails the test when the kubectl diff of c writes
// its stdout or its stderr to nil (the terminal of lo) or to an *os.File
// (os.Stderr, for example): its output would reach the terminal.
func refuseTerminalStreams(t *testing.T, c execx.Cmd) {
	t.Helper()
	for _, s := range []struct {
		name string
		w    io.Writer
	}{{"stdout", c.Stdout}, {"stderr", c.Stderr}} {
		if s.w == nil {
			t.Errorf("kubectl diff ran with a nil %s: its output would reach the terminal", s.name)
		} else if _, ok := s.w.(*os.File); ok {
			t.Errorf("kubectl diff writes its %s to a file (%T): its output can reach the terminal", s.name, s.w)
		}
	}
}

// diffScript scripts the kubectl diff calls: call i exits codes[i] (0 past
// the end) and prints outText on stdout and errText on stderr, at every
// exit code. The read of the field managers finds no live object. Any
// other kubectl verb fails the test.
func diffScript(t *testing.T, f *fakeRunner, outText, errText string, codes ...int) {
	t.Helper()
	call := 0
	f.handler = func(c execx.Cmd) error {
		if isFieldManagerRead(c) {
			return nil
		}
		if len(c.Args) < 3 || c.Args[2] != "diff" {
			t.Errorf("a dry run reached kubectl %v", c.Args)
			return &rcError{9}
		}
		refuseTerminalStreams(t, c)
		if c.Stdout == nil || c.Stderr == nil {
			return &rcError{9}
		}
		rc := 0
		if call < len(codes) {
			rc = codes[call]
		}
		call++
		fmt.Fprint(c.Stdout, outText)
		if errText != "" {
			fmt.Fprintln(c.Stderr, errText)
		}
		if rc == 0 {
			return nil
		}
		return &rcError{rc}
	}
}

// plantKubectl puts an executable kubectl into the project's .bin, which
// the runner resolves first (execx.Look), and returns its path: the hint
// of a failed diff must name it. The fake runner never runs it.
func plantKubectl(t *testing.T, e *Engine) string {
	t.Helper()
	path := filepath.Join(e.Paths.Bin, "kubectl")
	testutil.WriteFile(t, path, "#!/bin/sh\nexit 9\n")
	if err := os.Chmod(path, 0o755); err != nil { // #nosec G302 -- a test stand-in that must be executable
		t.Fatal(err)
	}
	return path
}

// setDebug sets DEBUG for the test: "1" or empty (off).
func setDebug(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv("DEBUG", "1")
	} else {
		t.Setenv("DEBUG", "")
	}
}

// wantFailedLine is the fixed line of a failed diff (rc 2) as the tests
// expect it on stderr. It has its own copy of the quoting rule.
func wantFailedLine(label, kubectl, kc, renderDir, stem string, filtered bool) string {
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
	dir, again, more := renderDir, "", ""
	if dir == "" {
		dir, again = "<dir>", "do the dry run again with --render-dir <dir> (<dir> is a directory that you choose), then "
	}
	if filtered {
		more = " (this diff can show more: the dry run diffed the render without its non-deterministic objects)"
	}
	return "[error] bootstrap: kubectl diff failed for " + label + " (rc=2). lo does not show the kubectl message: it can quote a value of the manifest. To see it, " +
		again + "run" + more + ": " + q(kubectl) + " --kubeconfig " + q(kc) + " diff --server-side --force-conflicts -f " + q(dir+"/"+stem+".yaml") + "\n"
}

// readDiffFile returns the content of a .diff file under --render-dir.
func readDiffFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the dry run did not write %s: %v", path, err)
	}
	return string(raw)
}

// The line of a failed diff, byte for byte, for paths that hold a space,
// `$(...)`, a `;` and a single quote: each argument that lo puts into the
// command is in single quotes (shQuote has the rule). The bats
// twin "bootstrap::_diff_failed_text prints the bytes of the Go twin"
// holds the same three lines.
func TestDiffFailedTextGolden(t *testing.T) {
	const odd = "/p/a b $(touch x); c'd"
	for _, tc := range []struct {
		name, renderDir string
		without         string
		want            string
	}{
		{"render dir", odd + "/out", "",
			`bootstrap: kubectl diff failed for testcni (rc=2). lo does not show the kubectl message: it can quote a value of the manifest. To see it, run: '/p/a b $(touch x); c'\''d/.bin/kubectl' --kubeconfig '/p/a b $(touch x); c'\''d/kc.yaml' diff --server-side --force-conflicts -f '/p/a b $(touch x); c'\''d/out/01-testcni.yaml'`},
		{"no render dir", "", "",
			`bootstrap: kubectl diff failed for testcni (rc=2). lo does not show the kubectl message: it can quote a value of the manifest. To see it, do the dry run again with --render-dir <dir> (<dir> is a directory that you choose), then run: '/p/a b $(touch x); c'\''d/.bin/kubectl' --kubeconfig '/p/a b $(touch x); c'\''d/kc.yaml' diff --server-side --force-conflicts -f '<dir>/01-testcni.yaml'`},
		{"filtered", odd + "/out", "its non-deterministic objects",
			`bootstrap: kubectl diff failed for testcni (rc=2). lo does not show the kubectl message: it can quote a value of the manifest. To see it, run (this diff can show more: the dry run diffed the render without its non-deterministic objects): '/p/a b $(touch x); c'\''d/.bin/kubectl' --kubeconfig '/p/a b $(touch x); c'\''d/kc.yaml' diff --server-side --force-conflicts -f '/p/a b $(touch x); c'\''d/out/01-testcni.yaml'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffFailedText("testcni", 2, odd+"/.bin/kubectl", odd+"/kc.yaml", tc.renderDir, "01-testcni", tc.without); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// A failed server-side diff (rc 2) prints fixed text that names the entry,
// also at debug, and never the kubectl message: it can quote a value of
// the manifest. The line names the project's kubectl and the rendered file
// to diff by hand: under --render-dir, or the file a dry run with
// --render-dir writes. The .diff file holds the stdout of the diff and
// never its stderr. The bats twin is "bootstrap::apply --dry-run: a failed
// diff prints no kubectl text, also at debug".
func TestDryRunFailedDiffPrintsNoKubectlText(t *testing.T) {
	for _, tc := range []struct {
		name      string
		debug     bool
		renderDir bool
	}{
		{"default level", false, false},
		{"debug", true, false},
		{"default level, render dir", false, true},
		{"debug, render dir", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setDebug(t, tc.debug)
			e, f, spec, kc := dryEngine(t, "testcni")
			kubectl := plantKubectl(t, e)
			dir := ""
			if tc.renderDir {
				dir = filepath.Join(t.TempDir(), "out")
				e.Options.RenderDir = dir
			}
			diffScript(t, f, diffPartialText, diffErrText, 2)
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if driver.ExitCode(err) != 2 {
				t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
			}
			if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: error\n") {
				t.Errorf("stdout:\n%s", stdoutOf(e))
			}
			if want := wantFailedLine("testcni", kubectl, kc, dir, "01-testcni", false); !strings.Contains(stderrOf(e), want) {
				t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
			}
			if tc.renderDir {
				if !fsutil.FileExists(filepath.Join(dir, "01-testcni.yaml")) {
					t.Errorf("the line names %s/01-testcni.yaml, which the dry run did not write", dir)
				}
				if got := readDiffFile(t, filepath.Join(dir, "01-testcni.diff")); got != diffPartialText {
					t.Errorf("01-testcni.diff = %q, want the stdout of the diff only (%q)", got, diffPartialText)
				}
			}
			if got := strings.Contains(stderrOf(e), "[debug] "); got != tc.debug {
				t.Errorf("debug lines on stderr = %v, want %v:\n%s", got, tc.debug, stderrOf(e))
			}
			// A failed diff prints no diff text, also at debug.
			refuteOutput(t, stderrOf(e)+stdoutOf(e), diffValueCanary, "Invalid value", "typed patch", diffTextCanary)
		})
	}
}

// The command in the line quotes each path: a render dir and a kubeconfig
// path with a space, `$(...)`, a `;` and a single quote paste as one word
// each. The bats twin is "bootstrap::apply --dry-run: the line of a failed
// diff quotes the paths".
func TestDryRunFailedDiffQuotesThePaths(t *testing.T) {
	const odd = "a b $(touch pwned); c'd"
	e, f, spec, _ := dryEngine(t, "testcni")
	kubectl := plantKubectl(t, e)
	kc := filepath.Join(e.Paths.Base, odd, "kc.yaml")
	testutil.WriteFile(t, kc, "")
	dir := filepath.Join(t.TempDir(), odd)
	e.Options.RenderDir = dir
	diffScript(t, f, "", diffErrText, 2)
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
	}
	want := wantFailedLine("testcni", kubectl, kc, dir, "01-testcni", false)
	if !strings.Contains(stderrOf(e), want) {
		t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
	}
	for _, quoted := range []string{
		" --kubeconfig '" + e.Paths.Base + `/a b $(touch pwned); c'\''d/kc.yaml' diff `,
		" -f '" + filepath.Dir(dir) + `/a b $(touch pwned); c'\''d/01-testcni.yaml'` + "\n",
	} {
		if !strings.Contains(stderrOf(e), quoted) {
			t.Errorf("stderr misses %q:\n%s", quoted, stderrOf(e))
		}
	}
	refuteOutput(t, stderrOf(e), diffValueCanary)
}

// The hint names the kubectl that the runner runs: the project's .bin
// first, else the one on PATH, else the bare name.
func TestDryRunFailedDiffNamesTheKubectlThatRuns(t *testing.T) {
	t.Run("on PATH", func(t *testing.T) {
		e, f, spec, kc := dryEngine(t, "testcni")
		bin := t.TempDir()
		onPath := filepath.Join(bin, "kubectl")
		testutil.WriteFile(t, onPath, "#!/bin/sh\nexit 9\n")
		if err := os.Chmod(onPath, 0o755); err != nil { // #nosec G302 -- a test stand-in that must be executable
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		diffScript(t, f, "", diffErrText, 2)
		_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		if want := wantFailedLine("testcni", onPath, kc, "", "01-testcni", false); !strings.Contains(stderrOf(e), want) {
			t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
		}
	})
	t.Run("not found", func(t *testing.T) {
		e, f, spec, kc := dryEngine(t, "testcni")
		t.Setenv("PATH", t.TempDir())
		diffScript(t, f, "", diffErrText, 2)
		_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		if want := wantFailedLine("testcni", "kubectl", kc, "", "01-testcni", false); !strings.Contains(stderrOf(e), want) {
			t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
		}
	})
}

// With --ignore-nondeterministic, the dry run diffed the render without
// its non-deterministic objects, but the file under --render-dir holds
// the full render: the line says that the diff by hand can show more.
// Without the option, the line does not say it.
func TestDryRunFailedDiffUnderIgnoreNondeterministic(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore=%v", ignore), func(t *testing.T) {
			e, _, spec, kc := dryEngine(t, "testcni")
			kubectl := plantKubectl(t, e)
			e.Runner = &alternatingRunner{diffRC: func(string) int { return 2 }}
			e.Options.IgnoreNondeterministic = ignore
			e.Options.RenderDir = filepath.Join(t.TempDir(), "out")
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if driver.ExitCode(err) != 2 {
				t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
			}
			if want := wantFailedLine("testcni", kubectl, kc, e.Options.RenderDir, "01-testcni", ignore); !strings.Contains(stderrOf(e), want) {
				t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
			}
		})
	}
}

// A diff with differences (rc 1) still reports as before, also when kubectl
// prints a message on stderr: "differs" with the counts, exit 1. The diff
// text is not printed, also at debug (it can quote a value of the
// manifest): debug prints one line for each object that differs and the
// path of the .diff file. The kubectl stderr is not printed, and the .diff
// file holds the diff text only. The bats twin is "bootstrap::apply
// --dry-run: a diff with differences and a kubectl message still differs
// (rc 1)".
func TestDryRunDiffersWithKubectlTextStillDiffers(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v", debug), func(t *testing.T) {
			setDebug(t, debug)
			t.Setenv(kapply.ShowSensitiveEnv, "")
			e, f, spec, kc := dryEngine(t, "testcni")
			dir := filepath.Join(t.TempDir(), "out")
			e.Options.RenderDir = dir
			diffScript(t, f, diffOutWithCanary, diffErrText, 1)
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if !errors.Is(err, ErrDryRunDiffers) || driver.ExitCode(err) != 1 {
				t.Fatalf("err = %v (rc %d), want ErrDryRunDiffers rc 1", err, driver.ExitCode(err))
			}
			want := "bootstrap dry-run: testcni: differs changed=1 new=2\n" +
				"bootstrap dry-run: entries=1 unchanged=0 differs=1 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0\n"
			if got := stdoutOf(e); got != want {
				t.Errorf("stdout:\n%s\nwant:\n%s", got, want)
			}
			debugLines := wantDebugDiffLines("testcni", dir+"/01-testcni.diff")
			for _, line := range debugLines {
				if got := strings.Contains(stderrOf(e), line); got != debug {
					t.Errorf("stderr has %q = %v, want %v (debug only):\n%s", line, got, debug, stderrOf(e))
				}
			}
			if got := readDiffFile(t, filepath.Join(dir, "01-testcni.diff")); got != diffOutWithCanary {
				t.Errorf("01-testcni.diff = %q, want the stdout of the diff only", got)
			}
			refuteOutput(t, stdoutOf(e)+stderrOf(e), diffTextCanary, "+  foo: bar", diffValueCanary, "Invalid value", "typed patch", "kubectl diff failed")
		})
	}
}

// wantDebugDiffLines is what a dry run prints at debug for the canned diff
// (diffOutWithCanary) of label: one line for each object of the manifest
// that differs (the render holds testcni only: extra and other are in the
// diff, not in the manifest), then the line that names the .diff file at
// diffPath ("" without --render-dir). The testcni ConfigMap of the render
// sets no namespace, so its line has none: the namespace of the diff header
// (default) comes from the diff text.
func wantDebugDiffLines(label, diffPath string) []string {
	lines := []string{
		"[debug] bootstrap dry-run: " + label + ": differs: v1 ConfigMap testcni\n",
	}
	if diffPath == "" {
		return append(lines, "[debug] bootstrap dry-run: "+label+": lo does not print the diff text: it can quote a value of the manifest. To keep it in a file, do the dry run again with --render-dir <dir> (LO_SHOW_SENSITIVE_OUTPUT=1 prints it)\n")
	}
	return append(lines, "[debug] bootstrap dry-run: "+label+": the diff text is in "+diffPath+". lo does not print it: it can quote a value of the manifest (LO_SHOW_SENSITIVE_OUTPUT=1 prints it)\n")
}

// At debug without --render-dir the dry run prints the object lines and
// says how to keep the diff text in a file; it prints no diff text and
// writes no .diff file. The bats twin is "bootstrap::apply --dry-run at
// debug without --render-dir names the objects, not the diff text".
func TestDryRunDebugDiffWithoutRenderDir(t *testing.T) {
	setDebug(t, true)
	t.Setenv(kapply.ShowSensitiveEnv, "")
	e, f, spec, kc := dryEngine(t, "testcni")
	diffScript(t, f, diffOutWithCanary, "", 1)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v, want ErrDryRunDiffers", err)
	}
	for _, line := range wantDebugDiffLines("testcni", "") {
		if !strings.Contains(stderrOf(e), line) {
			t.Errorf("stderr misses %q:\n%s", line, stderrOf(e))
		}
	}
	refuteOutput(t, stdoutOf(e)+stderrOf(e), diffTextCanary, "+  foo: bar", "@@ ",
		"differs: v1 ConfigMap default/extra", "differs: v1 ConfigMap default/other", "ConfigMap default/testcni")
}

// LO_SHOW_SENSITIVE_OUTPUT=1 at debug prints the diff text after a [warn]
// line, as lo printed it before; without debug it prints nothing more. The
// bats twin is "bootstrap::apply --dry-run: LO_SHOW_SENSITIVE_OUTPUT=1
// prints the diff text at debug".
func TestDryRunDiffTextUnderShowSensitive(t *testing.T) {
	const warn = "[warn] bootstrap dry-run: testcni: LO_SHOW_SENSITIVE_OUTPUT=1: lo shows the diff text, which can quote a value of the manifest\n"
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v", debug), func(t *testing.T) {
			setDebug(t, debug)
			t.Setenv(kapply.ShowSensitiveEnv, "1")
			e, f, spec, kc := dryEngine(t, "testcni")
			diffScript(t, f, diffOutWithCanary, diffErrText, 1)
			if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); !errors.Is(err, ErrDryRunDiffers) {
				t.Fatalf("err = %v, want ErrDryRunDiffers", err)
			}
			if got := strings.Contains(stderrOf(e), warn+diffOutWithCanary); got != debug {
				t.Errorf("stderr has the [warn] line and the diff text = %v, want %v:\n%s", got, debug, stderrOf(e))
			}
			if debug && strings.Contains(stderrOf(e), "the diff text is in") {
				t.Errorf("under the opt-in, stderr still has the path line:\n%s", stderrOf(e))
			}
			// The kubectl stderr stays private also under the opt-in.
			refuteOutput(t, stderrOf(e), diffValueCanary, "Invalid value")
		})
	}
}

// diffManifest is the manifest (the diff input) of TestDiffObjects: each
// object that its cases name, and "anyns", a ConfigMap with no namespace.
const diffManifest = `apiVersion: v1
kind: ConfigMap
metadata: {name: cfg, namespace: default}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: front, namespace: web}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: r, namespace: ns}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: admin}
---
apiVersion: v1beta1
kind: Ingress
metadata: {name: a.example.com, namespace: default}
---
apiVersion: v1
kind: Secret
metadata: {name: s, namespace: default}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: anyns}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: "we\x1bb", namespace: default}
`

// diffFile is the `diff`, `--- ` and `+++ ` lines of the file of one
// object (its kubectl file name), as kubectl diff prints them.
func diffFile(name string) string {
	const ts = "\t2026-10-12 10:00:00.000000000 +0000"
	return "diff -u -N /tmp/LIVE-1/" + name + " /tmp/MERGED-1/" + name + "\n" +
		"--- /tmp/LIVE-1/" + name + ts + "\n" +
		"+++ /tmp/MERGED-1/" + name + ts + "\n"
}

// diffObjects names the objects of the --- file headers of the LIVE tree
// that the unified-diff framing allows and that are objects of the
// manifest, in the objectLabel shape, escaped; any other line names
// nothing. The bats twin is "bootstrap::_diff_objects names the objects of
// the LIVE headers only" (the same cases).
func TestDiffObjects(t *testing.T) {
	const ts = "\t2026-10-12 10:00:00.000000000 +0000"
	hunk := "@@ -1 +1 @@\n-a: x\n+a: y\n"
	for _, tc := range []struct {
		name string
		diff string
		want []string
	}{
		{"core", diffFile("v1.ConfigMap.default.cfg") + hunk, []string{"v1 ConfigMap default/cfg"}},
		{"group", diffFile("apps.v1.Deployment.web.front") + hunk, []string{"apps/v1 Deployment web/front"}},
		{"dotted group", diffFile("rbac.authorization.k8s.io.v1.Role.ns.r") + hunk, []string{"rbac.authorization.k8s.io/v1 Role ns/r"}},
		{"cluster-scoped", diffFile("rbac.authorization.k8s.io.v1.ClusterRole..admin") + hunk, []string{"rbac.authorization.k8s.io/v1 ClusterRole admin"}},
		{"dotted name", diffFile("v1beta1.Ingress.default.a.example.com") + hunk, []string{"v1beta1 Ingress default/a.example.com"}},
		{"a header at the start, no diff line", "--- /tmp/LIVE-1/v1.ConfigMap.default.cfg" + ts + "\n+++ /tmp/MERGED-1/v1.ConfigMap.default.cfg" + ts, []string{"v1 ConfigMap default/cfg"}},
		{"external diff tool, no header", "the files differ\n", nil},
		{"not the LIVE tree", "--- /tmp/MERGED-1/v1.ConfigMap.default.cfg" + ts + "\n+++ a/v1.ConfigMap.default.cfg", nil},
		{"no directory", "--- v1.ConfigMap.default.cfg\n+++ v1.ConfigMap.default.cfg", nil},
		{"not the kubectl shape", diffFile("configmap-cfg.yaml") + hunk + diffFile("V1.ConfigMap.default.cfg") + hunk, nil},
		{"no +++ line after the header", "--- /tmp/LIVE-1/v1.ConfigMap.default.cfg" + ts + "\n@@ -1 +1 @@", nil},
		{"a header in no manifest object", diffFile("v1.ConfigMap.default.unknown") + hunk, nil},
		{"another namespace", diffFile("v1.ConfigMap.other.cfg") + hunk, nil},
		{"another version", diffFile("apps.v2.Deployment.web.front") + hunk, nil},
		{"no manifest namespace: any namespace, printed with no namespace", diffFile("v1.ConfigMap.kube-system.anyns") + hunk, []string{"v1 ConfigMap anyns"}},
		{"no manifest namespace: the namespace of the diff text never prints", diffFile("v1.ConfigMap.canary9.anyns") + hunk, []string{"v1 ConfigMap anyns"}},
		{"cluster-scoped: a namespace in the diff text never prints", diffFile("rbac.authorization.k8s.io.v1.ClusterRole.canary9.admin") + hunk, []string{"rbac.authorization.k8s.io/v1 ClusterRole admin"}},
		{"a name with an ESC byte, escaped", diffFile("v1.ConfigMap.default.we\x1bb") + hunk, []string{`v1 ConfigMap default/we\x1bb`}},
		{"a forged header inside a hunk", diffFile("v1.Secret.default.s") +
			"@@ -1,2 +1,2 @@\n--- /tmp/LIVE-1/v1.ConfigMap.default.cfg" + ts + "\n+++ /tmp/MERGED-1/v1.ConfigMap.default.cfg" + ts + "\n a: x\n",
			[]string{"v1 Secret default/s"}},
		{"two files", diffFile("v1.Secret.default.s") + hunk + diffFile("v1.ConfigMap.default.cfg") + hunk,
			[]string{"v1 Secret default/s", "v1 ConfigMap default/cfg"}},
		{"two hunks and a no-newline line", diffFile("v1.Secret.default.s") + hunk + "@@ -5,2 +5 @@\n c\n-d\n\\ No newline at end of file\n" + diffFile("v1.ConfigMap.default.cfg") + hunk,
			[]string{"v1 Secret default/s", "v1 ConfigMap default/cfg"}},
		{"a line that breaks the framing stops the parse", diffFile("v1.Secret.default.s") + "@@ -1 +1 @@\n-a: x\n+a: y\ngarbage\n" + diffFile("v1.ConfigMap.default.cfg") + hunk,
			[]string{"v1 Secret default/s"}},
		{"a removed line past the count stops the parse", diffFile("v1.Secret.default.s") + "@@ -1 +1 @@\n-a: x\n-b: z\n+a: y\n" + diffFile("v1.ConfigMap.default.cfg") + hunk,
			[]string{"v1 Secret default/s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := diffObjects(tc.diff, diffManifest); !slices.Equal(got, tc.want) {
				t.Errorf("diffObjects = %q, want %q", got, tc.want)
			}
		})
	}
}

// A clean diff (rc 0) with a kubectl warning on stderr is unchanged, exit
// 0, and the warning is not printed, also at debug, nor written to the
// .diff file. The bats twin is "bootstrap::apply --dry-run: a clean diff
// with a kubectl warning is unchanged (rc 0)".
func TestDryRunUnchangedWithKubectlWarning(t *testing.T) {
	setDebug(t, true)
	e, f, spec, kc := dryEngine(t, "testcni")
	dir := filepath.Join(t.TempDir(), "out")
	e.Options.RenderDir = dir
	diffScript(t, f, "", `Warning: the value "`+diffValueCanary+`" is deprecated`, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("err = %v (rc %d), want nil", err, driver.ExitCode(err))
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: unchanged\n") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	if got := readDiffFile(t, filepath.Join(dir, "01-testcni.diff")); got != "" {
		t.Errorf("01-testcni.diff = %q, want empty", got)
	}
	refuteOutput(t, stderrOf(e)+stdoutOf(e), diffValueCanary, "is deprecated")
}

// raceTexts are kubectl messages with one alternative each of the
// race-class pattern (raceRe): a test per alternative fails when the
// pattern loses it.
var raceTexts = []struct{ name, text string }{
	{"ensure CRDs are installed first", `error: unable to recognize "STDIN": ensure CRDs are installed first`},
	{"failed calling webhook", `Error from server (InternalError): Internal error occurred: failed calling webhook "x.example.com": connection refused`},
	{"no matches for kind", `error: unable to recognize "STDIN": no matches for kind "X" in version "example.com/v1"`},
	{"resource mapping not found", `error: resource mapping not found for name: "x" namespace: ""`},
}

// The race-class pattern stays inconclusive (needs-crd), and the rest of
// the kubectl stderr is not printed either. The bats twins are
// "bootstrap::apply --dry-run: an inconclusive diff (<alternative>)
// prints no kubectl text".
func TestDryRunInconclusiveDiffPrintsNoKubectlText(t *testing.T) {
	for _, rt := range raceTexts {
		t.Run(rt.name, func(t *testing.T) {
			setDebug(t, true)
			e, f, spec, kc := dryEngine(t, "testcni")
			diffScript(t, f, "", diffErrText+"\n"+rt.text, 2)
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if driver.ExitCode(err) != 2 {
				t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
			}
			if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: inconclusive (needs-crd)\n") {
				t.Errorf("stdout:\n%s", stdoutOf(e))
			}
			refuteOutput(t, stderrOf(e)+stdoutOf(e), diffValueCanary, "Invalid value", "typed patch", rt.name, "kubectl diff failed")
		})
	}
}

// restore.d goes through the same diff: its failed diff prints the fixed
// line with restore.d.yaml, also at debug, and never the kubectl message;
// restore.d.diff holds the stdout of the diff only. The bats twin is
// "bootstrap::apply --dry-run: a failed restore.d diff prints no kubectl
// text, also at debug".
func TestDryRunRestoreDFailedDiffPrintsNoKubectlText(t *testing.T) {
	for _, renderDir := range []bool{false, true} {
		t.Run(fmt.Sprintf("render dir=%v", renderDir), func(t *testing.T) {
			setDebug(t, true)
			e, f, spec, kc := dryEngine(t, "testcni")
			kubectl := plantKubectl(t, e)
			dir := ""
			if renderDir {
				dir = filepath.Join(t.TempDir(), "out")
				e.Options.RenderDir = dir
			}
			d := filepath.Join(e.Paths.Clusters, "test.lok8s.dev")
			testutil.WriteFile(t, filepath.Join(d, "restore.d", "tls-e.sops.yaml"), "cipher\n")
			testutil.WriteFile(t, filepath.Join(d, "secrets", "restore.d", "tls-e.yaml"), "apiVersion: v1\nkind: Secret\nmetadata:\n  name: tls-e\n")
			diffScript(t, f, diffPartialText, diffErrText, 2, 0)
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if driver.ExitCode(err) != 2 {
				t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
			}
			if !strings.Contains(stdoutOf(e), "bootstrap dry-run: restore.d: error\n") {
				t.Errorf("stdout:\n%s", stdoutOf(e))
			}
			if want := wantFailedLine("restore.d", kubectl, kc, dir, "restore.d", false); !strings.Contains(stderrOf(e), want) {
				t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
			}
			if renderDir {
				if got := readDiffFile(t, filepath.Join(dir, "restore.d.diff")); got != diffPartialText {
					t.Errorf("restore.d.diff = %q, want the stdout of the diff only (%q)", got, diffPartialText)
				}
			}
			refuteOutput(t, stderrOf(e)+stdoutOf(e), diffValueCanary, "Invalid value", "typed patch")
		})
	}
}

// An interrupt or a SIGTERM that ends kubectl (rc 130, 143) prints a fixed
// "interrupted" line, not the hint to diff by hand, and never the kubectl
// message. The bats twin is "bootstrap::apply --dry-run: an interrupted
// diff prints the interrupted line".
func TestDryRunInterruptedDiffPrintsNoHint(t *testing.T) {
	for _, rc := range []int{130, 143} {
		t.Run(fmt.Sprintf("rc=%d", rc), func(t *testing.T) {
			e, f, spec, kc := dryEngine(t, "testcni")
			plantKubectl(t, e)
			diffScript(t, f, "", diffErrText, rc)
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if driver.ExitCode(err) != 2 {
				t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
			}
			if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: error\n") {
				t.Errorf("stdout:\n%s", stdoutOf(e))
			}
			if want := fmt.Sprintf("[error] bootstrap: kubectl diff was interrupted for testcni (rc=%d)\n", rc); !strings.Contains(stderrOf(e), want) {
				t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
			}
			refuteOutput(t, stderrOf(e), "To see it", "kubectl diff failed for", diffValueCanary)
		})
	}
}

// kubectl that does not start (not found) prints the fixed "failed to
// start" line, not the hint. The bats twin (bash: rc 127) is
// "bootstrap::apply --dry-run: kubectl that is not found prints the failed
// to start line".
func TestDryRunDiffLaunchFailure(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error {
		refuseTerminalStreams(t, c)
		return fmt.Errorf("kubectl: %w", execx.ErrNotFound)
	}
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d (%v), want 2", driver.ExitCode(err), err)
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: error\n") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	if want := "[error] bootstrap: kubectl diff failed to start for testcni: kubectl: executable not found\n"; !strings.Contains(stderrOf(e), want) {
		t.Errorf("stderr misses %q:\n%s", want, stderrOf(e))
	}
	refuteOutput(t, stderrOf(e), "To see it", "kubectl diff failed for")
}

// cancelAtThePlugin cancels the run when the dry run's check of the Secret
// plugin starts: an interrupt of lo (Ctrl-C) during the check. Each other
// command goes to the fake runner.
type cancelAtThePlugin struct {
	*fakeRunner
	cancel  context.CancelFunc
	checked bool
}

func (r *cancelAtThePlugin) Run(ctx context.Context, c execx.Cmd) error {
	if strings.HasSuffix(c.Name, "/"+toolchain.SecretPluginRel) && strings.Join(c.Args, " ") == "--capabilities" {
		r.checked = true
		r.cancel()
		<-ctx.Done()
		return ctx.Err()
	}
	return r.fakeRunner.Run(ctx, c)
}

// An interrupt of the dry run's check of the Secret plugin is not one of
// the dry run's refusals (rc 2): Apply prints the check's line and reports
// the cancelled context, so lo ends with 128+n (main). The render does not
// start. bash: bootstrap::_dry_run_exit keeps the code of the interrupt.
func TestDryRunInterruptOfThePluginCheckReportsTheCancel(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	t.Setenv(addons.KustomizePluginHomeEnv, "")
	os.Unsetenv(addons.KustomizePluginHomeEnv)
	rel := ".kustomize/" + toolchain.SecretPluginRel
	src := filepath.Join(e.Paths.Base, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &cancelAtThePlugin{fakeRunner: f, cancel: cancel}
	e.Runner = r
	err := e.Apply(ctx, "test.lok8s.dev", spec, kc)
	if !r.checked {
		t.Fatalf("the dry run did not check the Secret plugin (err %v)", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v (rc %d), want the cancelled context", err, driver.ExitCode(err))
	}
	want := "bootstrap dry-run: the check of the Secret plugin " + rel + " was interrupted\n"
	if got := stderrOf(e); !strings.Contains(got, want) || strings.Contains(got, "does not support a dry run") {
		t.Errorf("stderr:\n%s\nwant the line %q", got, want)
	}
	if calls := f.log(); strings.Contains(calls, "kustomize ") || strings.Contains(calls, "kubectl ") {
		t.Errorf("the run went on after the interrupt:\n%s", calls)
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
	want := "bootstrap dry-run: testcni: rendered\n" +
		"bootstrap dry-run: entries=1 unchanged=0 differs=0 rendered=1 skipped=0 inconclusive=0 errors=0 nondeterministic=0\n"
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
	if !strings.Contains(stdoutOf(e), "cert-manager: rendered") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
}

func TestDryRunIgnoresForce(t *testing.T) {
	// --force on a dry run heals nothing: an immutable conflict in the diff
	// is reported, never recreated.
	t.Setenv("LOK8S_FORCE_RECREATE", "1")
	e, f, spec, kc := dryEngine(t, "testcni")
	f.handler = func(c execx.Cmd) error {
		if isFieldManagerRead(c) {
			return nil
		}
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
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: testcni: differs changed=1 new=2\n") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	// The diff's stderr is not printed: an immutable-field error can quote
	// the object (a Job's pod template with its env values, for example).
	refuteOutput(t, stderrOf(e), "field is immutable")
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
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: cilium: skipped\n") || !strings.Contains(stdoutOf(e), "skipped=1") {
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
		"bootstrap dry-run: testcni: unchanged\n" +
		"bootstrap dry-run: entries=2 unchanged=1 differs=1 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0\n"
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
	if !strings.Contains(stdoutOf(e), "second: error\n") || !strings.Contains(stdoutOf(e), "errors=1") {
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

func TestOnlyDryRunNumbersTheSelectionsFiles(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second")
	e.Options.Only = []string{"second"}
	e.Options.RenderDir = t.TempDir()
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "bootstrap dry-run: second: unchanged\n" +
		"bootstrap dry-run: entries=1 unchanged=1 differs=0 rendered=0 skipped=0 inconclusive=0 errors=0 nondeterministic=0\n"
	if got := stdoutOf(e); got != want {
		t.Errorf("stdout:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(e.Options.RenderDir, "01-second.yaml")); err != nil {
		t.Errorf("the selection's file is not numbered from 01: %v", err)
	}
}

// A project spec can write `- ./targets/flux: { dependsOn: [fluxcd] }`.
// --only takes the key as written, the same as the entry name `flux`.
func TestOnlyMatchesTheKeyAsWritten(t *testing.T) {
	e, log, spec, kc, p := schedEngine(t, "fluxcd", "./targets/flux: {dependsOn: [fluxcd]}")
	if err := os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "targets", "flux"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name)
		return 0
	}
	e.Options.Only = []string{"./targets/flux"}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v\n%s", err, stderrOf(e))
	}
	if log.count() != 1 || !log.has("flux") {
		t.Errorf("applied %v, want only flux", log.events)
	}
	if !strings.Contains(stderrOf(e), "bootstrap: --only: flux depends on fluxcd, which is not selected") {
		t.Errorf("no warning for the omitted dependency:\n%s", stderrOf(e))
	}
}

// The key tells apart a framework addon and a cluster target with the same
// last path element: the name and the directory name match both.
func TestOnlyKeySelectsTheTargetNotTheAddonOfTheSameName(t *testing.T) {
	e, log, spec, kc, p := schedEngine(t, "rook-ceph", "./targets/rook-ceph")
	if err := os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "targets", "rook-ceph"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name + " " + job.Dir)
		return 0
	}
	e.Options.Only = []string{"./targets/rook-ceph"}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "rook-ceph " + p.Clusters + "/test.lok8s.dev/./targets/rook-ceph"
	if log.count() != 1 || !log.has(want) {
		t.Errorf("applied %v, want only %q", log.events, want)
	}
}

// A bare value also matches the directory name of an addon or a target, so
// --only rook-ceph selects ./targets/rook-ceph named rook-ceph-cluster too
// (the --help text says so).
func TestOnlyBareNameAlsoMatchesATargetDirectory(t *testing.T) {
	e, log, spec, kc, p := schedEngine(t, "rook-ceph", "./targets/rook-ceph: {name: rook-ceph-cluster}")
	if err := os.MkdirAll(filepath.Join(p.Clusters, "test.lok8s.dev", "targets", "rook-ceph"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		log.add(job.Name)
		return 0
	}
	e.Options.Only = []string{"rook-ceph"}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if log.count() != 2 || !log.has("rook-ceph") || !log.has("rook-ceph-cluster") {
		t.Errorf("applied %v, want rook-ceph and rook-ceph-cluster", log.events)
	}
}

// Two entries may share a name (a basename collision nothing depends on).
// Their report lines add the key as written, so the two lines differ and
// each names the --only value that selects that one entry.
func TestDryRunReportAddsTheKeyWhenNamesCollide(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "second", "./targets/second")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "targets", "second", "kustomization.yaml"), "resources: []\n")
	diffAnswers(t, f, 0, 1)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); !errors.Is(err, ErrDryRunDiffers) {
		t.Fatalf("err = %v\n%s", err, stderrOf(e))
	}
	for _, want := range []string{
		"bootstrap dry-run: second (second): unchanged\n",
		"bootstrap dry-run: second (./targets/second): differs changed=1 new=2\n",
	} {
		if !strings.Contains(stdoutOf(e), want) {
			t.Errorf("missing %q in:\n%s", want, stdoutOf(e))
		}
	}
	// The key from the line selects that one entry; alone, it has no clash.
	e.Stdout.(interface{ Reset() }).Reset()
	e.Options.Only = []string{"./targets/second"}
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("--only ./targets/second: %v", err)
	}
	if !strings.HasPrefix(stdoutOf(e), "bootstrap dry-run: second: unchanged\nbootstrap dry-run: entries=1 ") {
		t.Errorf("--only ./targets/second:\n%s", stdoutOf(e))
	}
}

// #281: a report line names the entry by a name --only accepts (the entry
// name, here a name: override), never by the NN- file stem.
func TestDryRunReportNamesEachEntryTheWayOnlyTakesIt(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second: {name: zweite}")
	diffAnswers(t, f, 0, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	lines := strings.Split(stdoutOf(e), "\n")
	if len(lines) < 2 || lines[1] != "bootstrap dry-run: zweite: unchanged" {
		t.Fatalf("stdout:\n%s", stdoutOf(e))
	}
	name, _, _ := strings.Cut(strings.TrimPrefix(lines[1], "bootstrap dry-run: "), ":")
	e.Stdout.(interface{ Reset() }).Reset()
	e.Options.Only = []string{name}
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("--only %s: %v\n%s", name, err, stderrOf(e))
	}
	if !strings.HasPrefix(stdoutOf(e), "bootstrap dry-run: zweite: unchanged\nbootstrap dry-run: entries=1 ") {
		t.Errorf("--only %s:\n%s", name, stdoutOf(e))
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
	e, f, spec, kc := dryEngine(t, "cilium", "testcni", "barechart")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "secrets", "Secret.app.default.TOKEN"), "ENC[token]\n")
	// barechart is mkChartAddon's bare chart.yaml with NO values overlay —
	// the "nothing to stack" render.go branch (forceCopy-only, no merge)
	// a review-round-2 mutation found the disk-safety test never exercised.
	mkChartAddon(t, e.Paths, "barechart")
	diffAnswers(t, f, 0, 0, 0)

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
	withBoundary(t, e)
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

// A dry run renders in place: PATH_SECRETS is the REAL per-domain store
// during the render (the secrets plugin reads it and never writes it), the
// same value a real run exports.
func TestDryRunReadsTheRealStore(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	secretsDir := filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "secrets")
	testutil.WriteFile(t, filepath.Join(secretsDir, "Secret.app.default.TOKEN"), "ENC[token]\n")
	t.Setenv("PATH_SECRETS", "")
	var seen string
	diffAnswers(t, f, 0)
	f2 := f.handler
	f.handler = func(c execx.Cmd) error {
		seen = os.Getenv("PATH_SECRETS")
		return f2(c)
	}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if seen != secretsDir {
		t.Errorf("PATH_SECRETS during the dry run = %q, want the real store %s", seen, secretsDir)
	}
}
