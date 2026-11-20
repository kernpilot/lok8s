package bootstrap

// private_test.go — the kubectl output of an entry stays private (bats: the
// "the kubectl output of an entry stays private" section of
// tests/unit/bootstrap_test.bats). canaryValue stands for a value of the
// manifest that kubectl quotes in its output (a field error prints "Invalid
// value: <value>"); it is a made-up string. A background entry's kapply
// copies the kubectl output to Job.Private; the reap parks and counts from
// it and prints fixed lines. Every kubectl diff and apply of the bootstrap
// runs with the kubectl user defaults off (execx.KubeRCOff).

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/kapply"
	"github.com/kernpilot/lok8s/internal/testutil"
)

const (
	canaryValue      = "zq7Canary9xLm2"
	canaryInvalid    = `The ConfigMap "testcni" is invalid: data.v: Invalid value: "` + canaryValue + `"`
	canaryApplyFixed = "[error] kapply: kubectl apply failed for testcni (rc=1). lo does not show the kubectl output: it can quote a value of the manifest. To see it, run again with LO_SHOW_SENSITIVE_OUTPUT=1\n"
)

// privateKubectl answers kubectl apply with out and rc (every other verb:
// exit 0, no output) and records the Env of each apply.
type privateKubectl struct {
	mu        sync.Mutex
	out       string
	rc        int
	applyEnvs [][]string
}

func (k *privateKubectl) handler(c execx.Cmd) error {
	args := c.Args
	if len(args) >= 2 && args[0] == "--kubeconfig" {
		args = args[2:]
	}
	if len(args) == 0 || args[0] != "apply" {
		return nil
	}
	if c.Stdin != nil {
		_, _ = io.Copy(io.Discard, c.Stdin)
	}
	k.mu.Lock()
	k.applyEnvs = append(k.applyEnvs, c.Env)
	k.mu.Unlock()
	if c.Stdout != nil {
		fmt.Fprintln(c.Stdout, k.out)
	}
	if k.rc != 0 {
		return &rcError{k.rc}
	}
	return nil
}

// privateEngine is an Engine with one entry (testcni) whose kubectl apply
// prints out and exits rc, off a terminal and at the given level.
func privateEngine(t *testing.T, debug bool, out string, rc int) (*Engine, *privateKubectl, string, string) {
	t.Helper()
	setDebug(t, debug)
	t.Setenv("KAPPLY_TTY", "")
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	t.Setenv("LOK8S_FORCE_RECREATE", "")
	t.Setenv(kapply.ShowSensitiveEnv, "")
	e, f, _, _, p := testEngine(t)
	writeStackAddon(t, p)
	spec := writeClusterSpec(t, p, "testcni")
	kc := writeKubeconfig(t, p)
	k := &privateKubectl{out: out, rc: rc}
	f.handler = k.handler
	return e, k, spec, kc
}

func outputOf(e *Engine) string { return stdoutOf(e) + stderrOf(e) }

// assertKubeRCOff fails the test unless every recorded call ran with
// KUBERC=off, and there was at least one.
func assertKubeRCOff(t *testing.T, envs [][]string) {
	t.Helper()
	if len(envs) == 0 {
		t.Fatal("no kubectl call was recorded")
	}
	for i, env := range envs {
		if !slices.Contains(env, execx.KubeRCOff) {
			t.Errorf("call %d ran with Env %q, want %s", i, env, execx.KubeRCOff)
		}
	}
}

// bats: "bootstrap::apply: the kubectl output of an entry stays private
// (each level, success and failure)".
func TestEntryKubectlOutputStaysPrivate(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v/success", debug), func(t *testing.T) {
			e, k, spec, kc := privateEngine(t, debug, "configmap/testcni serverside-applied\nWarning: unknown field \"data."+canaryValue+"\"", 0)
			if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
				t.Fatalf("Apply: %v\n%s", err, outputOf(e))
			}
			want := "✓ testcni · 1 resource\n"
			if debug {
				want = "[debug] kapply: kubectl apply for testcni exited 0 (1 resources)\n"
			}
			if !strings.Contains(outputOf(e), want) {
				t.Errorf("output misses %q:\n%s", want, outputOf(e))
			}
			refuteOutput(t, outputOf(e), canaryValue, "Warning: unknown field")
			assertKubeRCOff(t, k.applyEnvs)
		})
		t.Run(fmt.Sprintf("debug=%v/failure", debug), func(t *testing.T) {
			e, k, spec, kc := privateEngine(t, debug, canaryInvalid, 1)
			if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err == nil {
				t.Fatal("expected a failure")
			}
			for _, want := range []string{canaryApplyFixed, "[error] bootstrap: testcni apply failed (rc=1)"} {
				if !strings.Contains(outputOf(e), want) {
					t.Errorf("output misses %q:\n%s", want, outputOf(e))
				}
			}
			refuteOutput(t, outputOf(e), canaryValue, "Invalid value")
			assertKubeRCOff(t, k.applyEnvs)
		})
	}
}

// bats: "bootstrap::apply: LO_SHOW_SENSITIVE_OUTPUT=1 shows the kubectl
// output of a failed entry".
func TestEntryShowSensitiveShowsTheOutput(t *testing.T) {
	e, _, spec, kc := privateEngine(t, false, canaryInvalid, 1)
	t.Setenv(kapply.ShowSensitiveEnv, "1")
	_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	for _, want := range []string{canaryValue, "[warn] kapply: LO_SHOW_SENSITIVE_OUTPUT=1: lo shows the kubectl output for testcni, which can quote a value of the manifest"} {
		if !strings.Contains(outputOf(e), want) {
			t.Errorf("output misses %q:\n%s", want, outputOf(e))
		}
	}
}

// bats: "bootstrap::apply: an immutable conflict in the private output
// PARKS the entry, and no kubectl text prints".
func TestEntryImmutableInPrivateOutputParks(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v", debug), func(t *testing.T) {
			e, _, spec, kc := privateEngine(t, debug,
				`Error from server (Invalid): ConfigMap "testcni" is invalid: data.v: Invalid value: "`+canaryValue+`": field is immutable`, 1)
			if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err == nil {
				t.Fatal("expected a failure (the parked heal fails non-interactively)")
			}
			for _, want := range []string{"will confirm at the end", "needs recreate"} {
				if !strings.Contains(outputOf(e), want) {
					t.Errorf("output misses %q:\n%s", want, outputOf(e))
				}
			}
			refuteOutput(t, outputOf(e), canaryValue, "field is immutable")
		})
	}
}

// bats: "bootstrap::apply: a terminating-namespace 403 in the private
// output reaches the prompt".
func TestEntryTerminating403InPrivateOutputReachesThePrompt(t *testing.T) {
	e, _, spec, kc := privateEngine(t, false,
		`Error from server (Forbidden): error when creating "x": secrets "`+canaryValue+`" is forbidden: unable to create new content in namespace app-system because it is being terminated`, 1)
	var prompt string
	e.Interactive = func() bool { return true }
	e.Ask = func(p string) bool { prompt = p; return false }
	_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if !strings.Contains(prompt, "app-system") {
		t.Errorf("the prompt does not name the namespace:\n%s", prompt)
	}
	if !strings.Contains(outputOf(e), "will confirm at the end") {
		t.Errorf("the entry did not park:\n%s", outputOf(e))
	}
	refuteOutput(t, outputOf(e)+prompt, canaryValue)
}

// The scheduler gives each background job a private writer, counts from
// it and never prints it, at each level. bats: "bootstrap::apply: a
// background job's private file sits in the 0700 scheduler dir and is
// removed at the reap".
func TestSchedulerCountsFromThePrivateWriter(t *testing.T) {
	for _, debug := range []bool{false, true} {
		t.Run(fmt.Sprintf("debug=%v", debug), func(t *testing.T) {
			setDebug(t, debug)
			t.Setenv("LOK8S_BOOTSTRAP_PARALLEL", "8")
			e, _, spec, kc, _ := schedEngine(t, "a", "b")
			var mu sync.Mutex
			seen := 0
			e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
				if job.Private == nil {
					t.Errorf("%s: a background job without a private writer", job.Name)
					return 1
				}
				mu.Lock()
				seen++
				mu.Unlock()
				fmt.Fprintf(job.Private, "configmap/%s-one serverside-applied\nInvalid value: %q\n", job.Name, canaryValue)
				return 0
			}
			if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if seen != 2 {
				t.Errorf("jobs = %d, want 2", seen)
			}
			if !debug {
				for _, want := range []string{"✓ a · 1 resource\n", "✓ b · 1 resource\n"} {
					if !strings.Contains(stdoutOf(e), want) {
						t.Errorf("stdout misses %q:\n%s", want, stdoutOf(e))
					}
				}
			}
			refuteOutput(t, outputOf(e), canaryValue, "serverside-applied")
		})
	}
}

// The .diff file of a dry run is 0600 in the 0700 render dir, and the diff
// ran with the kubectl user defaults off. bats: "bootstrap::apply
// --dry-run: a diff with differences and a kubectl message still differs
// (rc 1)".
func TestDryRunDiffFileIsPrivate(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	dir := filepath.Join(t.TempDir(), "out")
	e.Options.RenderDir = dir
	var envs [][]string
	f.handler = func(c execx.Cmd) error {
		envs = append(envs, c.Env)
		fmt.Fprint(c.Stdout, diffOutWithCanary)
		return &rcError{1}
	}
	_ = e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	for path, want := range map[string]os.FileMode{dir: 0o700, filepath.Join(dir, "01-testcni.diff"): 0o600} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != want {
			t.Errorf("%s: mode %v, want %v", path, fi.Mode().Perm(), want)
		}
	}
	if len(envs) != 1 {
		t.Errorf("kubectl calls = %d, want 1 (the diff)", len(envs))
	}
	assertKubeRCOff(t, envs)
}

// The restore.d apply runs with the kubectl user defaults off.
func TestRestoreDApplyRunsWithKubeRCOff(t *testing.T) {
	e, f, _, _, p := testEngine(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "restore.d", "tls.sops.yaml"), "cipher\n")
	e.SopsDecrypt = func(string) ([]byte, error) {
		return []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: tls\n"), nil
	}
	var envs [][]string
	f.handler = func(c execx.Cmd) error {
		envs = append(envs, c.Env)
		if c.Stdin != nil {
			_, _ = io.Copy(io.Discard, c.Stdin)
		}
		return nil
	}
	var errOut bytes.Buffer
	e.Stderr = &errOut
	e.restoreD(t.Context(), "test.lok8s.dev", "/kc")
	if !strings.Contains(errOut.String(), "restore.d — 1 applied, 0 skipped") {
		t.Errorf("stderr misses the summary:\n%s", errOut.String())
	}
	assertKubeRCOff(t, envs)
}
