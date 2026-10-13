package bootstrap

// dryrun_round4_test.go — review round 4 on #268: objects a chart
// regenerates on every apply (two renders, `--ignore-nondeterministic`), the
// ChartRenderer gate's refusal as `inconclusive`, the KRM `partial` hint, the
// --render-dir permissions and order, and the reserved-env check after the
// real run's skips.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// alternatingRunner renders a CA Secret whose data changes on every
// render (a chart that mints TLS material per render) next to a stable
// ConfigMap; kubectl diff answers via diffRC from what it is given.
type alternatingRunner struct {
	renders int
	diffIn  []string
	diffRC  func(stdin string) int
}

func (r *alternatingRunner) Run(_ context.Context, c execx.Cmd) error {
	switch c.Name {
	case "kustomize":
		r.renders++
		fmt.Fprintf(c.Stdout, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: stable\n  namespace: kube-system\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: cilium-ca\n  namespace: kube-system\ndata:\n  ca.crt: Q0Et%d\n", r.renders)
		return nil
	case "kubectl":
		raw, _ := io.ReadAll(c.Stdin)
		r.diffIn = append(r.diffIn, string(raw))
		if rc := r.diffRC(string(raw)); rc != 0 {
			fmt.Fprint(c.Stdout, "--- a\n+++ b\n")
			return &rcError{rc}
		}
		return nil
	}
	return nil
}

// An object whose content differs between the entry's two renders is listed
// as non-deterministic; the verdict stays honest (it differs on the cluster:
// every apply rotates it, exit 1); --ignore-nondeterministic judges the entry
// without it (exit 0 when only it differs). The render dir keeps the full
// render either way.
func TestDryRunNonDeterministicObjects(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		e, _, spec, kc := dryEngine(t, "testcni")
		r := &alternatingRunner{diffRC: func(in string) int {
			if strings.Contains(in, "cilium-ca") {
				return 1
			}
			return 0
		}}
		e.Runner = r
		e.Options.IgnoreNondeterministic = ignore
		e.Options.RenderDir = filepath.Join(t.TempDir(), "out")
		err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		out := stdoutOf(e)
		if r.renders != 2 {
			t.Errorf("ignore=%v: %d renders, want 2", ignore, r.renders)
		}
		if !strings.Contains(out, "bootstrap dry-run: 01-testcni: non-deterministic (regenerated on every apply): v1 Secret kube-system/cilium-ca\n") {
			t.Errorf("ignore=%v: the object is not listed:\n%s", ignore, out)
		}
		if !strings.Contains(out, " nondeterministic=1\n") {
			t.Errorf("ignore=%v: summary:\n%s", ignore, out)
		}
		full, _ := os.ReadFile(filepath.Join(e.Options.RenderDir, "01-testcni.yaml"))
		if !strings.Contains(string(full), "cilium-ca") {
			t.Errorf("ignore=%v: the render dir lacks the object", ignore)
		}
		if ignore {
			if err != nil || !strings.Contains(out, "01-testcni: unchanged (non-deterministic: 1)\n") {
				t.Errorf("--ignore-nondeterministic: err=%v\n%s", err, out)
			}
			if len(r.diffIn) != 1 || strings.Contains(r.diffIn[0], "cilium-ca") || !strings.Contains(r.diffIn[0], "stable") {
				t.Errorf("--ignore-nondeterministic diffed %q", r.diffIn)
			}
		} else if driver.ExitCode(err) != 1 || !strings.Contains(out, "01-testcni: differs changed=1 new=0 (non-deterministic: 1)\n") {
			t.Errorf("default: err=%v\n%s", err, out)
		}
	}
}

// A render the ChartRenderer gate refused (it reported the chart) is
// inconclusive, not an error, and keeps the run from exiting clean.
func TestDryRunChartGateRefusalIsInconclusive(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	e.Runner = runnerFunc(func(ctx context.Context, c execx.Cmd) error {
		if c.Name == "kustomize" {
			for _, kv := range c.Env {
				if path, ok := strings.CutPrefix(kv, "LOK8S_BOOTSTRAP_DRYRUN_REPORT="); ok {
					fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
					if err != nil {
						return err
					}
					_, _ = io.WriteString(fh, "chart-deps ../../charts/app\n")
					_ = fh.Close()
				}
			}
			return &rcError{1}
		}
		return f.Run(ctx, c)
	})
	diffAnswers(t, f, 0)
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err %v)", driver.ExitCode(err), err)
	}
	if !strings.Contains(stdoutOf(e), "01-testcni: inconclusive (chart dependencies not vendored)\n") || !strings.Contains(stdoutOf(e), " inconclusive=1 errors=0 ") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
}

// An entry whose YAML declares a KRM function is marked partial (kustomize
// skips an exec function without --enable-exec); --allow-generators runs it,
// so there is no mark then.
func TestDryRunMarksADeclaredKRMFunctionPartial(t *testing.T) {
	for _, allow := range []bool{false, true} {
		e, f, spec, kc := dryEngine(t, "testcni")
		testutil.WriteFile(t, filepath.Join(e.Paths.Lok8s, "addons", "testcni", "fn.yaml"), "metadata:\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: ./fn\n")
		e.Options.AllowGenerators = allow
		diffAnswers(t, f, 0)
		if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		marked := strings.Contains(stdoutOf(e), "01-testcni: unchanged (partial: KRM function skipped)\n")
		if marked == allow {
			t.Errorf("allow=%v: marked=%v\n%s", allow, marked, stdoutOf(e))
		}
	}
}

// An existing --render-dir that group or others may read is tightened to
// 0700 (and stderr says so); the directory is created only after the
// boundary is built, so a refusal there leaves nothing behind.
func TestRenderDirPermissionsAndOrder(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.Options.RenderDir = dir
	diffAnswers(t, f, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode = %v, want 0700", fi.Mode().Perm())
	}
	if !strings.Contains(stderrOf(e), "bootstrap dry-run: "+dir+" was 0755: tightened to 0700\n") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}

	// A boundary refusal (a Secret plugin older than the dry run): no
	// render dir is created.
	e2, _, spec2, kc2 := dryEngine(t, "testcni")
	t.Setenv("LO_RENDER", "exec")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	plugin := filepath.Join(e2.Paths.Base, ".kustomize", "secrets.lok8s.dev", "v1", "secret", "Secret")
	testutil.WriteFile(t, plugin, "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	e2.Runner = runnerFunc(func(context.Context, execx.Cmd) error { return &rcError{1} })
	fresh := filepath.Join(t.TempDir(), "fresh")
	e2.Options.RenderDir = fresh
	if err := e2.Apply(t.Context(), "test.lok8s.dev", spec2, kc2); driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err %v)\n%s", driver.ExitCode(err), err, stderrOf(e2))
	}
	if _, err := os.Stat(fresh); err == nil {
		t.Error("a boundary refusal left the render dir behind")
	}
}

// The real run's skips come first: an entry the run skips (a platform-owned
// addon on a hosted cluster) is skipped, not an env error, dry or real.
func TestReservedEnvCheckedAfterTheSkips(t *testing.T) {
	for _, dry := range []bool{true, false} {
		e, f, _, _, p := testEngine(t)
		mkChartAddon(t, p, "cilium")
		spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
		testutil.WriteFile(t, spec, "kind: Lo\nmetadata:\n  name: e2e-test\nspec:\n  kubehz:\n    hosting: hosted\n  bootstrap:\n    - cilium:\n        env:\n          KUBECONFIG: /tmp/evil\n")
		kc := writeKubeconfig(t, p)
		e.Options.DryRun = dry
		f.handler = func(c execx.Cmd) error {
			if strings.Contains(strings.Join(c.Args, " "), " get nodes") {
				fmt.Fprintln(c.Stdout, "w1 Ready <none> 1d v1.31")
			}
			return nil
		}
		err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		if err != nil {
			t.Errorf("dry=%v: err=%v\n%s", dry, err, stderrOf(e))
		}
		if strings.Contains(stdoutOf(e)+stderrOf(e), "may not set") {
			t.Errorf("dry=%v: a skipped entry's env was refused:\n%s", dry, stderrOf(e))
		}
	}
}
