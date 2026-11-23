package kapply

// run_private_test.go — RunPrivate keeps the kubectl output of one write
// private (bats: the "run_private:" cases of tests/unit/kapply_test.bats).
// The canaries are those of private_test.go.

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
)

// runPrivate runs RunPrivate for an apply of "-f -" with the label "the
// objects", and returns the stdout, the stderr and the error. An empty c
// is an apply of a ConfigMap on stdin.
func runPrivate(t *testing.T, f execx.Runner, c execx.Cmd) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	if c.Name == "" {
		c = execx.Cmd{Name: "kubectl", Args: []string{"apply", "-f", "-"}, Stdin: strings.NewReader(cfgManifest)}
	}
	err := RunPrivate(t.Context(), f, c, "the objects", &out, &errOut)
	return out.String(), errOut.String(), err
}

// A failed write prints the fixed line and the summary, never the kubectl
// stdout or stderr, off and on a terminal, at each level. bats:
// "run_private: a failed write prints the fixed line, never the kubectl
// output".
func TestRunPrivateFailurePrintsTheFixedLineOnly(t *testing.T) {
	eachUI(t, func(t *testing.T) {
		f := &fakeKubectl{applyRC: 1, applyOut: "configmap/cfg configured\n" + invalidValue,
			applyErr: `Warning: the value "` + stderrCanary + `" of ConfigMap cfg is deprecated`}
		out, errOut, err := runPrivate(t, f, execx.Cmd{})
		if execx.ExitCode(err) != 1 {
			t.Fatalf("err = %v, want the exit 1 of kubectl", err)
		}
		if want := "[error] " + ApplyFailedText("the objects", 1) + "\n"; strings.Count(errOut, want) != 1 {
			t.Errorf("stderr misses the fixed line once:\n%s", errOut)
		}
		if out != "✓ the objects · 1 resource\n" {
			t.Errorf("stdout = %q, want the summary line only", out)
		}
		refuteText(t, out+errOut, canaryValue, stderrCanary, "Invalid value")
	})
}

// A clean write prints the summary on stdout and, at the debug level, the
// exit code and the count on stderr: never the kubectl output. bats:
// "run_private: a clean write prints the summary line, never the kubectl
// output, and kubectl reads the stdin of the caller".
func TestRunPrivateSuccessPrintsTheSummaryOnly(t *testing.T) {
	for _, debug := range []bool{false, true} {
		setUI(t, false, debug)
		f := &fakeKubectl{applyOut: "configmap/cfg serverside-applied\nsecret/s configured\nWarning: unknown field \"data." + canaryValue + "\""}
		out, errOut, err := runPrivate(t, f, execx.Cmd{})
		if err != nil {
			t.Fatal(err)
		}
		if out != "✓ the objects · 2 resources\n" {
			t.Errorf("stdout = %q, want the summary line only", out)
		}
		wantDebug := ""
		if debug {
			wantDebug = "[debug] kapply: kubectl apply for the objects exited 0 (2 resources)\n"
		}
		if errOut != wantDebug {
			t.Errorf("debug=%v: stderr = %q, want %q", debug, errOut, wantDebug)
		}
		refuteText(t, out+errOut, canaryValue)
	}
}

// The child runs with the kubectl user defaults off, keeps the Env of the
// caller and gets the stdin of the caller. bats: "run_private: the KUBERC of
// the caller does not reach kubectl".
func TestRunPrivateRunsWithKubeRCOff(t *testing.T) {
	setUI(t, false, false)
	t.Setenv("KUBERC", "/user/kuberc")
	for _, env := range [][]string{nil, {"A=1"}, {execx.KubeRCOff}} {
		f := &stdinKubectl{}
		c := execx.Cmd{Name: "kubectl", Args: []string{"apply", "-f", "-"}, Env: slices.Clone(env), Stdin: strings.NewReader(cfgManifest)}
		if _, _, err := runPrivate(t, f, c); err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(strings.Join(f.env, " "), execx.KubeRCOff); n != 1 {
			t.Errorf("Env %q: the child ran with Env %q, want %s once", env, f.env, execx.KubeRCOff)
		}
		for _, e := range env {
			if !slices.Contains(f.env, e) {
				t.Errorf("Env %q: the child lost %s (Env %q)", env, e, f.env)
			}
		}
		if f.stdin != cfgManifest {
			t.Errorf("the child read %q on stdin, want the manifest", f.stdin)
		}
	}
}

// Under the opt-in the write prints a [warn] line and then the kubectl
// output on stderr, and no summary and no fixed line. bats: "run_private:
// LO_SHOW_SENSITIVE_OUTPUT=1 prints the kubectl output after a [warn] line,
// and no summary".
func TestRunPrivateShowSensitivePrintsTheOutput(t *testing.T) {
	setUI(t, false, false)
	t.Setenv(ShowSensitiveEnv, "1")
	f := &fakeKubectl{applyRC: 1, applyOut: "configmap/cfg configured\n" + invalidValue, applyErr: "Warning: " + stderrCanary}
	out, errOut, err := runPrivate(t, f, execx.Cmd{})
	if execx.ExitCode(err) != 1 {
		t.Fatalf("err = %v, want the exit 1 of kubectl", err)
	}
	if want := "[warn] kapply: " + ShowSensitiveText("the kubectl output for the objects") + "\n"; !strings.HasPrefix(errOut, want) {
		t.Errorf("stderr does not start with the [warn] line:\n%s", errOut)
	}
	for _, s := range []string{canaryValue, stderrCanary} {
		if !strings.Contains(errOut, s) {
			t.Errorf("the opt-in did not print %q:\n%s", s, errOut)
		}
	}
	if out != "" || strings.Contains(errOut, "kapply: kubectl apply failed for") {
		t.Errorf("the opt-in printed a summary or the fixed line:\nstdout:\n%s\nstderr:\n%s", out, errOut)
	}
}

// stdinKubectl records the Env and the stdin of its one call.
type stdinKubectl struct {
	env   []string
	stdin string
}

func (f *stdinKubectl) Run(_ context.Context, c execx.Cmd) error {
	f.env = slices.Clone(c.Env)
	if c.Stdin != nil {
		var b bytes.Buffer
		_, _ = b.ReadFrom(c.Stdin)
		f.stdin = b.String()
	}
	return nil
}
