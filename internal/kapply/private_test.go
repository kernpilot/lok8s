package kapply

// private_test.go — the kubectl output of an apply stays private (bats: the
// "private:" cases of tests/unit/kapply_test.bats). canaryValue stands for
// a value of the manifest that kubectl quotes in its output (a field error
// prints "Invalid value: <value>"); it is a made-up string. lo prints fixed
// lines; the kubectl text prints only under LO_SHOW_SENSITIVE_OUTPUT=1.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

const (
	canaryValue = "zq7Canary9xLm2"
	cfgManifest = "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg, namespace: default}"
	// fixedFailLine is the line of a failed pass of the label "resources",
	// byte for byte (the bats twin: _fixed_fail_line).
	fixedFailLine = "[error] kapply: kubectl apply failed for resources (rc=1). lo does not show the kubectl output: it can quote a value of the manifest. To see it, run again with LO_SHOW_SENSITIVE_OUTPUT=1\n"
	invalidValue  = `The ConfigMap "cfg" is invalid: data.v: Invalid value: "` + canaryValue + `"`
)

// setUI sets the display for the test: off a terminal (no KAPPLY_TTY, a
// non-interactive run), or the terminal UI drawn into a file (KAPPLY_TTY),
// and the debug level.
func setUI(t *testing.T, tty, debug bool) {
	t.Helper()
	t.Setenv("CI", "")
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	t.Setenv("KAPPLY_TTY", "")
	if tty {
		t.Setenv("KAPPLY_TTY", filepath.Join(t.TempDir(), "ui.txt"))
	}
	t.Setenv("DEBUG", "")
	if debug {
		t.Setenv("DEBUG", "1")
	}
}

// eachUI runs fn for each display (off a terminal, on a terminal) at each
// level (normal, debug: -v and --debug both set DEBUG=1).
func eachUI(t *testing.T, fn func(t *testing.T)) {
	t.Helper()
	for _, tty := range []bool{false, true} {
		for _, debug := range []bool{false, true} {
			t.Run(fmt.Sprintf("tty=%v/debug=%v", tty, debug), func(t *testing.T) {
				setUI(t, tty, debug)
				fn(t)
			})
		}
	}
}

// Every apply pass (the first and the one after a heal) runs kubectl with
// the user defaults off. bats: "private: each apply pass runs kubectl with
// the user defaults off (KUBERC=off)".
func TestApplyPassRunsWithKubeRCOff(t *testing.T) {
	setUI(t, false, false)
	f := &fakeKubectl{applyRC: 1,
		applyOut: `Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: field is immutable`}
	a, _, _ := testApplier(f)
	a.ForceRecreate = true
	a.Apply(t.Context(), "resources", deployManifest)
	applies := 0
	for i, call := range f.calls {
		if !strings.Contains(call, " apply ") {
			continue
		}
		applies++
		if !slices.Contains(f.envs[i], execx.KubeRCOff) {
			t.Errorf("%s ran with Env %q, want %s", call, f.envs[i], execx.KubeRCOff)
		}
	}
	if applies != 2 {
		t.Errorf("apply calls = %d, want 2:\n%s", applies, f.log())
	}
}

// A failed pass prints the fixed line, never the kubectl output, off and
// on a terminal, at each level. bats: "private: a failed pass prints the
// fixed line, never the kubectl output (off a tty, each level)" and
// "private: on a tty a failed pass prints the fixed line, never the kubectl
// output".
func TestApplyFailurePrintsTheFixedLineOnly(t *testing.T) {
	eachUI(t, func(t *testing.T) {
		f := &fakeKubectl{applyRC: 1, applyOut: "configmap/a serverside-applied\n" + invalidValue}
		a, out, errOut := testApplier(f)
		got, rc := a.Apply(t.Context(), "resources", cfgManifest)
		if rc != 1 || !strings.Contains(got, canaryValue) {
			t.Fatalf("rc = %d, output %q: want rc 1 and the kubectl output for the caller", rc, got)
		}
		if strings.Count(errOut.String(), fixedFailLine) != 1 {
			t.Errorf("stderr misses the fixed line once:\n%s", errOut.String())
		}
		refuteText(t, out.String()+errOut.String(), canaryValue, "Invalid value")
	})
}

// The pass after a heal prints the fixed line too. bats: "private: the pass
// after a heal prints the fixed line too, never the kubectl output".
func TestApplyPassAfterHealPrintsTheFixedLineToo(t *testing.T) {
	eachUI(t, func(t *testing.T) {
		f := &fakeKubectl{applyRC: 1,
			applyOut: `Error from server (Invalid): Deployment.apps "web" is invalid: spec.selector: Invalid value: "` + canaryValue + `": field is immutable`}
		a, out, errOut := testApplier(f)
		a.ForceRecreate = true
		if _, rc := a.Apply(t.Context(), "resources", deployManifest); rc != 1 {
			t.Fatalf("rc = %d, want 1", rc)
		}
		if !strings.Contains(errOut.String(), "healing blocked objects, then re-applying once") {
			t.Fatalf("no heal:\n%s", errOut.String())
		}
		if n := strings.Count(errOut.String(), fixedFailLine); n != 2 {
			t.Errorf("the fixed line %d times, want 2 (each pass):\n%s", n, errOut.String())
		}
		refuteText(t, out.String()+errOut.String(), canaryValue)
	})
}

// A clean pass prints the summary line, never the kubectl output; at debug
// the [debug] line with the exit code and the count. bats: "private: a
// clean pass prints the summary line, never the kubectl output".
func TestApplySuccessPrintsTheSummaryOnly(t *testing.T) {
	eachUI(t, func(t *testing.T) {
		f := &fakeKubectl{applyOut: "configmap/cfg serverside-applied\nWarning: unknown field \"data." + canaryValue + "\""}
		a, out, errOut := testApplier(f)
		if _, rc := a.Apply(t.Context(), "resources", cfgManifest); rc != 0 {
			t.Fatalf("rc = %d, want 0", rc)
		}
		want := "✓ resources · 1 resource\n"
		if ttyUI() {
			want = "\033[32m✓\033[0m resources · 1 resource\n"
		}
		if out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		debugLine := "[debug] kapply: kubectl apply for resources exited 0 (1 resources)\n"
		if got, want := strings.Contains(errOut.String(), debugLine), os.Getenv("DEBUG") != ""; got != want {
			t.Errorf("stderr has the [debug] line = %v, want %v:\n%s", got, want, errOut.String())
		}
		refuteText(t, out.String()+errOut.String(), canaryValue)
	})
}

// With Private set (the bootstrap scheduler), the pass copies the kubectl
// output there and prints no summary (the scheduler prints the block of
// the entry). bats: "private: KAPPLY_PRIVATE_LOG gets the kubectl output,
// the pass prints no summary".
func TestApplyPrivateGetsTheOutput(t *testing.T) {
	eachUI(t, func(t *testing.T) {
		f := &fakeKubectl{applyRC: 1, applyOut: "configmap/cfg serverside-applied\n" + invalidValue}
		a, out, errOut := testApplier(f)
		var priv bytes.Buffer
		a.Private = &priv
		a.Apply(t.Context(), "resources", cfgManifest)
		if want := "configmap/cfg serverside-applied\n" + invalidValue + "\n"; priv.String() != want {
			t.Errorf("Private = %q, want %q", priv.String(), want)
		}
		if strings.Contains(out.String(), "✓") {
			t.Errorf("a summary with Private set:\n%s", out.String())
		}
		if !strings.Contains(errOut.String(), fixedFailLine) {
			t.Errorf("stderr misses the fixed line:\n%s", errOut.String())
		}
		refuteText(t, out.String()+errOut.String(), canaryValue)
	})
}

// Under the opt-in the pass prints the kubectl output after a [warn] line:
// off a terminal the full text, on a terminal the error lines. bats:
// "private: LO_SHOW_SENSITIVE_OUTPUT=1 prints the kubectl output after a
// [warn] line".
func TestApplyShowSensitivePrintsTheOutput(t *testing.T) {
	const warn = "[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1: lo shows the kubectl output for resources, which can quote a value of the manifest\n"
	eachUI(t, func(t *testing.T) {
		f := &fakeKubectl{applyRC: 1, applyOut: invalidValue}
		a, out, errOut := testApplier(f)
		a.ShowSensitive = true
		a.Apply(t.Context(), "resources", cfgManifest)
		if !strings.Contains(errOut.String(), warn) {
			t.Errorf("stderr misses the [warn] line:\n%s", errOut.String())
		}
		if !strings.Contains(out.String()+errOut.String(), canaryValue) {
			t.Errorf("the opt-in did not print the kubectl output:\nstdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
		}
		if strings.Contains(errOut.String(), "kapply: kubectl apply failed for") {
			t.Errorf("the opt-in printed the fixed line:\n%s", errOut.String())
		}
	})
}

// NewApplier reads the opt-in: only the value 1 turns it on. bats:
// "private: only LO_SHOW_SENSITIVE_OUTPUT=1 is the opt-in".
func TestNewApplierReadsTheOptIn(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "": false, "0": false, "true": false, "yes": false} {
		t.Setenv(ShowSensitiveEnv, v)
		if got := NewApplier(&fakeKubectl{}, nil, nil).ShowSensitive; got != want {
			t.Errorf("%s=%q: ShowSensitive = %v, want %v", ShowSensitiveEnv, v, got, want)
		}
		if got := ShowSensitive(); got != want {
			t.Errorf("%s=%q: ShowSensitive() = %v, want %v", ShowSensitiveEnv, v, got, want)
		}
	}
}

// CountedLines keeps the <type>/<name> lines that end in an OK verb, and
// nothing else. bats: "counted_lines: keeps the <type>/<name> lines that
// end in an OK verb, nothing else".
func TestCountedLines(t *testing.T) {
	in := "configmap/a serverside-applied\nWarning: " + canaryValue + " configured\n  indented/x created\nInvalid value: \"" + canaryValue + "\"\nsecret/b unchanged"
	if got, want := CountedLines(in), "configmap/a serverside-applied\nsecret/b unchanged\n"; got != want {
		t.Errorf("CountedLines = %q, want %q", got, want)
	}
}

// refuteText fails the test when out holds one of leaks.
func refuteText(t *testing.T, out string, leaks ...string) {
	t.Helper()
	for _, leak := range leaks {
		if strings.Contains(out, leak) {
			t.Errorf("the output holds %q:\n%s", leak, out)
		}
	}
}
