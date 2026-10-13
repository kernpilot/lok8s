package bootstrap

// dryrun_round3_test.go — review round 3 on #268, the in-place redesign:
// the entry env that may not set lo's own variables (dry and real run), the
// non-repeatable report of the secrets plugin, the hardened --render-dir,
// the restore.d decrypt failure (exit 2, never a warning), and a dry run
// that leaves no temp files behind (finished or cancelled).

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/kernpilot/lok8s/internal/addons"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/secrets"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// withBoundary gives a direct dryRunOne caller the boundary Apply builds.
func withBoundary(t *testing.T, e *Engine) {
	t.Helper()
	b, err := addons.NewDryRunBoundary(t.Context(), e.Paths, e.Runner)
	if err != nil {
		t.Fatalf("NewDryRunBoundary: %v", err)
	}
	e.boundary = b
	t.Cleanup(b.Close)
}

// reservedEnv are names an entry's env: may not set: lo's orchestration
// and safety variables. Every one is an entry error, dry run or real run,
// and the render (kustomize) never starts.
var reservedEnv = []string{
	"LOK8S_BOOTSTRAP_DRYRUN", "LOK8S_BOOTSTRAP_ALLOW_GENERATORS", "LOK8S_BOOTSTRAP_DRYRUN_REPORT",
	"BOOTSTRAP_PROTECTED_ENV_NAMES", "bootstrap_render_dir", "PATH_SECRETS", "KUBECONFIG", "KUSTOMIZE_PLUGIN_HOME",
}

func TestEntryEnvReservedNamesAreAnEntryError(t *testing.T) {
	for _, name := range reservedEnv {
		for _, dry := range []bool{true, false} {
			t.Run(name, func(t *testing.T) {
				e, f, _, _, p := testEngine(t)
				writeStackAddon(t, p)
				spec := writeClusterSpec(t, p, "testcni:\n      env:\n        LOK8S_USER_OK: fine\n        "+name+": \"0\"")
				kc := writeKubeconfig(t, p)
				e.Options.DryRun = dry
				err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
				want := 1
				if dry {
					want = 2
				}
				if driver.ExitCode(err) != want {
					t.Fatalf("dry=%v: exit = %d, want %d (err %v)", dry, driver.ExitCode(err), want, err)
				}
				// A real run renders an entry's output as one captured block
				// on stdout; a dry run prints the [error] on stderr.
				if !strings.Contains(stdoutOf(e)+stderrOf(e), "bootstrap: testcni: env: may not set "+name+" (reserved for lo)") {
					t.Errorf("dry=%v: the output does not name %s:\n%s\n%s", dry, name, stdoutOf(e), stderrOf(e))
				}
				if strings.Contains(f.log(), "kustomize") {
					t.Errorf("dry=%v: the render ran despite the refusal:\n%s", dry, f.log())
				}
				if dry && !strings.Contains(stdoutOf(e), "01-testcni: error") {
					t.Errorf("stdout:\n%s", stdoutOf(e))
				}
			})
		}
	}
}

// reportingRunner is the fake runner with the secrets plugin's half of the
// report: on the render it appends lines to the file the render's env names.
type reportingRunner struct {
	inner *fakeRunner
	lines string
}

func (r reportingRunner) Run(ctx context.Context, c execx.Cmd) error {
	if c.Name == "kustomize" && r.lines != "" {
		for _, kv := range c.Env {
			if path, ok := strings.CutPrefix(kv, "LOK8S_BOOTSTRAP_DRYRUN_REPORT="); ok {
				fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					return err
				}
				_, _ = io.WriteString(fh, r.lines)
				_ = fh.Close()
			}
		}
	}
	return r.inner.Run(ctx, c)
}

// An entry whose render generated material in memory (reported by the
// secrets plugin) is marked non-repeatable; the next entry starts from an
// empty report.
func TestDryRunReportMarksTheEntryNonRepeatable(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni", "second")
	e.Runner = &onceReporting{inner: f, lines: "secret Secret.app.ns.PASSWORD\ncaroot /x\n"}
	diffAnswers(t, f, 0, 0)
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	out := stdoutOf(e)
	if !strings.Contains(out, "01-testcni: unchanged (non-repeatable)\n") {
		t.Errorf("the reporting entry is not marked:\n%s", out)
	}
	if !strings.Contains(out, "02-second: unchanged\n") {
		t.Errorf("the report leaked into the next entry:\n%s", out)
	}
}

// onceReporting reports on the first render only.
type onceReporting struct {
	inner *fakeRunner
	lines string
	done  bool
}

func (r *onceReporting) Run(ctx context.Context, c execx.Cmd) error {
	if c.Name == "kustomize" && !r.done {
		r.done = true
		return reportingRunner{inner: r.inner, lines: r.lines}.Run(ctx, c)
	}
	return r.inner.Run(ctx, c)
}

// --render-dir refuses a path with a symlinked component and a symlinked
// leaf; a fresh dir is created 0700; the marker must be a regular file; the
// run says once that the directory holds plaintext Secrets.
func TestRenderDirHardening(t *testing.T) {
	t.Run("symlinked leaf", func(t *testing.T) {
		e, _, spec, kc := dryEngine(t, "testcni")
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "out")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		e.Options.RenderDir = link
		err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		if driver.ExitCode(err) != 2 || !strings.Contains(stderrOf(e), "is a symlink") {
			t.Fatalf("err=%v stderr=%s", err, stderrOf(e))
		}
		if entries, _ := os.ReadDir(target); len(entries) != 0 {
			t.Errorf("wrote through the symlink: %v", entries)
		}
	})
	t.Run("symlinked component", func(t *testing.T) {
		e, _, spec, kc := dryEngine(t, "testcni")
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "parent")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		e.Options.RenderDir = filepath.Join(link, "out")
		err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
		if driver.ExitCode(err) != 2 || !strings.Contains(stderrOf(e), link+" is a symlink") {
			t.Fatalf("err=%v stderr=%s", err, stderrOf(e))
		}
		if _, err := os.Stat(filepath.Join(target, "out")); err == nil {
			t.Error("created the render dir behind the symlink")
		}
	})
	t.Run("fresh dir is 0700 and named plaintext", func(t *testing.T) {
		e, f, spec, kc := dryEngine(t, "testcni")
		dir := filepath.Join(t.TempDir(), "out")
		e.Options.RenderDir = dir
		diffAnswers(t, f, 0)
		if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		fi, err := os.Stat(dir)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("render dir mode = %v (%v), want 0700", fi.Mode().Perm(), err)
		}
		note := "bootstrap dry-run: " + dir + " holds the rendered manifests in plaintext, Secrets included\n"
		if strings.Count(stderrOf(e), note) != 1 {
			t.Errorf("want the plaintext note once:\n%s", stderrOf(e))
		}
		if strings.Contains(stderrOf(e), "\033[") {
			t.Errorf("ANSI on piped stderr:\n%q", stderrOf(e))
		}
	})
	t.Run("marker must be a regular file", func(t *testing.T) {
		for name, mk := range map[string]func(string) error{
			"dir":     func(p string) error { return os.Mkdir(p, 0o700) },
			"symlink": func(p string) error { return os.Symlink(filepath.Join(t.TempDir(), "x"), p) },
		} {
			e, _, spec, kc := dryEngine(t, "testcni")
			dir := t.TempDir()
			if err := mk(filepath.Join(dir, renderDirMarker)); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(dir, "01-old.yaml"), "x\n")
			e.Options.RenderDir = dir
			err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
			if driver.ExitCode(err) != 2 || !strings.Contains(stderrOf(e), "as a regular file") {
				t.Errorf("%s marker: err=%v stderr=%s", name, err, stderrOf(e))
			}
			if _, err := os.Stat(filepath.Join(dir, "01-old.yaml")); err != nil {
				t.Errorf("%s marker: cleared a directory it does not own: %v", name, err)
			}
		}
	})
}

// A restore.d file that does not decrypt is an error: the run exits 2,
// never a warning and exit 0 (round 2 warned and dropped the file from the
// preview).
func TestDryRunRestoreDDecryptFailureIsExitTwo(t *testing.T) {
	e, f, spec, kc := dryEngine(t, "testcni")
	testutil.WriteFile(t, filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "restore.d", "wildcard-tls.sops.yaml"), "sops: {}\n")
	e.SopsDecrypt = func(string) ([]byte, error) { return nil, errors.New("no identity matched any of the recipients") }
	diffAnswers(t, f, 0)
	err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc)
	if driver.ExitCode(err) != 2 {
		t.Fatalf("exit = %d, want 2 (err %v)", driver.ExitCode(err), err)
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: restore.d: error\n") {
		t.Errorf("stdout:\n%s", stdoutOf(e))
	}
	if !strings.Contains(stderrOf(e), "[error] bootstrap: dry-run: restore.d: cannot decrypt wildcard-tls.sops.yaml: no identity matched") {
		t.Errorf("stderr:\n%s", stderrOf(e))
	}
}

// The same with REAL sops material: a restore.d file encrypted by the
// pinned sops CLI to a fresh age key decrypts through the normal key
// discovery (SOPS_AGE_KEY here) and is diffed; with another key in the
// environment it fails and the run exits 2.
func TestDryRunRestoreDRealSops(t *testing.T) {
	sopsBin := filepath.Join(testutil.RepoRoot(t), ".bin", "sops")
	if info, err := os.Stat(sopsBin); err != nil || info.Mode()&0o111 == 0 {
		if os.Getenv("CI") != "" {
			t.Fatal("pinned sops CLI missing under .bin on CI")
		}
		t.Skip("pinned sops CLI not installed under .bin (b install)")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	e, f, spec, kc := dryEngine(t, "testcni")
	e.SopsDecrypt = secrets.DecryptYAMLFile
	rdir := filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "restore.d")
	plain := filepath.Join(t.TempDir(), "tls.yaml")
	testutil.WriteFile(t, plain, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: wildcard-tls\n  namespace: cert-manager\nstringData:\n  tls.key: restored\n")
	cmd := exec.CommandContext(t.Context(), sopsBin, "--encrypt", "--age", id.Recipient().String(), "--input-type", "yaml", "--output-type", "yaml", plain)
	// Outside the repo: sops would read the checkout's own .sops.yaml.
	cmd.Dir = filepath.Dir(plain)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	var serr strings.Builder
	cmd.Stderr = &serr
	enc, err := cmd.Output()
	if err != nil {
		t.Fatalf("sops --encrypt: %v\n%s", err, serr.String())
	}
	testutil.WriteFile(t, filepath.Join(rdir, "wildcard-tls.sops.yaml"), string(enc))

	t.Setenv("SOPS_AGE_KEY", id.String())
	var diffed string
	diffAnswers(t, f, 0, 0)
	inner := f.handler
	f.handler = func(c execx.Cmd) error {
		if diffed == "" && c.Stdin != nil {
			raw, _ := io.ReadAll(c.Stdin)
			diffed = string(raw)
			c.Stdin = strings.NewReader(diffed)
		}
		return inner(c)
	}
	if err := e.Apply(t.Context(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("Apply with the right key: %v\n%s", err, stderrOf(e))
	}
	if !strings.Contains(stdoutOf(e), "bootstrap dry-run: restore.d: unchanged\n") || !strings.Contains(diffed, "tls.key: restored") {
		t.Fatalf("restore.d was not decrypted and diffed:\n%s\n%s", stdoutOf(e), diffed)
	}

	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY", other.String())
	e2, f2, spec2, kc2 := dryEngine(t, "testcni")
	e2.SopsDecrypt = secrets.DecryptYAMLFile
	testutil.WriteFile(t, filepath.Join(e2.Paths.Clusters, "test.lok8s.dev", "restore.d", "wildcard-tls.sops.yaml"), string(enc))
	diffAnswers(t, f2, 0)
	err = e2.Apply(t.Context(), "test.lok8s.dev", spec2, kc2)
	if driver.ExitCode(err) != 2 || !strings.Contains(stdoutOf(e2), "restore.d: error") {
		t.Fatalf("the wrong key: exit=%d, want 2\n%s\n%s", driver.ExitCode(err), stdoutOf(e2), stderrOf(e2))
	}
}

// A dry run leaves no temp file behind: not when it finishes, not when its
// context is cancelled in the middle of a render (SIGINT/SIGTERM cancel the
// command context).
func TestDryRunLeavesNoTempFiles(t *testing.T) {
	for _, cancelMidRender := range []bool{false, true} {
		tmp := t.TempDir()
		t.Setenv("TMPDIR", tmp)
		e, f, spec, kc := dryEngine(t, "testcni")
		ctx, cancel := context.WithCancel(t.Context())
		diffAnswers(t, f, 0)
		if cancelMidRender {
			e.Runner = runnerFunc(func(c context.Context, cmd execx.Cmd) error {
				if cmd.Name == "kustomize" {
					cancel()
					return c.Err()
				}
				return f.Run(c, cmd)
			})
		}
		_ = e.Apply(ctx, "test.lok8s.dev", spec, kc)
		cancel()
		if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
			names := []string{}
			for _, en := range entries {
				names = append(names, en.Name())
			}
			t.Errorf("cancel=%v: temp files left behind: %v", cancelMidRender, names)
		}
	}
}
