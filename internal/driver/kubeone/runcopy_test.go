package kubeone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/interrupt"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// runcopy_test.go pins the run copy of Apply (stageRun) beyond the main
// snapshot test in kubeone_test.go: the forms of addons.path, the merged
// settings, the reads that it refuses, the order of the copy, the path of
// the manifest copy, and the cleanup on a SIGHUP, before lo ends with it.

const (
	rcClean  = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"
	rcMarked = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: hcloud\n  annotations:\n    secrets.lok8s.dev/keys-only: \"true\"\ndata:\n  token: \"\"\n"
)

// rcRefusal is the guard's message for a file of the addons directory.
func rcRefusal(what string) string {
	return "[error] kubeone: refusing to apply " + what + ": it holds a keys-only Secret (secrets.lok8s.dev/keys-only), which would replace each value with an empty one\n"
}

// rcUnreadable is the guard's message for a file that cannot be read.
func rcUnreadable(what string) string {
	return "[error] kubeone: refusing to apply " + what + ": it cannot be read, so it cannot be checked for a keys-only Secret (secrets.lok8s.dev/keys-only)\n"
}

// rcDriver is a driver with the manifest and the addons in its work dir.
func rcDriver(t *testing.T, manifest string, addons map[string]string) (*Driver, *fakeRunner, *bytes.Buffer, string) {
	t.Helper()
	clearVarEnv(t)
	d, runner, errBuf, p := testDriver(t, nil)
	wd := filepath.Join(p.Clusters, "test.lok8s.dev", ".kubeone")
	testutil.WriteFile(t, filepath.Join(wd, "kubeone.yaml"), manifest)
	for name, body := range addons {
		testutil.WriteFile(t, filepath.Join(wd, "addons", name), body)
	}
	return d, runner, errBuf, wd
}

// runSpec is what kubeone reads in its manifest copy.
type runSpec struct {
	Name   string `yaml:"name"`
	Addons struct {
		Enable bool   `yaml:"enable"`
		Path   string `yaml:"path"`
	} `yaml:"addons"`
}

func readRunSpec(t *testing.T, c execx.Cmd) runSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.Dir, c.Args[2]))
	if err != nil {
		t.Fatal(err)
	}
	var spec runSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	return spec
}

// assertExploded fails when the manifest copy raw holds an alias, an
// anchor or a merge key (<<), which lo resolves (explode). It finds a
// merge key with its own check, and not with isMergeKey: a fault in
// isMergeKey would also hide the key from the test. A << key is a merge
// key unless it is a string (quoted, or tagged !!str). yaml.v3 writes a
// merge key that it parsed as "!!merge <<", with the tag.
func assertExploded(t *testing.T, raw []byte) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the manifest copy does not parse: %v", err)
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			t.Errorf("the manifest copy holds an alias or an anchor:\n%s", raw)
			return
		}
		for i, c := range n.Content {
			mergeKey := c.Kind == yaml.ScalarNode && c.Value == "<<" && c.Tag != "!!str" &&
				c.Style&(yaml.SingleQuotedStyle|yaml.DoubleQuotedStyle) == 0
			if n.Kind == yaml.MappingNode && i%2 == 0 && mergeKey {
				t.Errorf("the manifest copy holds a merge key:\n%s", raw)
				return
			}
			walk(c)
		}
	}
	walk(&doc)
}

func assertNoRunCopy(t *testing.T, wd string) {
	t.Helper()
	if left, _ := filepath.Glob(filepath.Join(wd, runPrefix+"*")); len(left) != 0 {
		t.Errorf("the run copy is left: %v", left)
	}
}

// bats: "kubeone::apply: the run copy keeps the merged settings and each
// form of the addons path". KubeOne resolves the aliases and merge keys
// of its manifest: the manifest copy keeps the addon settings that a merge
// key (<<) or an alias gives (enable), with addons.path set to the run's
// addons directory. A trailing slash and a leading ./ name the same
// directory: the copy holds its files, and a refusal names them as
// addons/<file>. The manifest copy has no alias, anchor or merge key left
// (assertExploded): yaml.Unmarshal into runSpec resolves a merge key
// itself, so the settings alone do not show that lo resolved it.
func TestApplyRunCopyKeepsMergedSettingsAndPathForms(t *testing.T) {
	for name, manifest := range map[string]string{
		"plain":            "name: test-prod\naddons:\n  enable: true\n  path: ./addons\n",
		"trailing slash":   "name: test-prod\naddons:\n  enable: true\n  path: ./addons/\n",
		"no leading ./":    "name: test-prod\naddons:\n  enable: true\n  path: addons\n",
		"root merge key":   "name: test-prod\n<<: {addons: {enable: true, path: ./addons}}\n",
		"merge in addons":  "defaults: &d {enable: true, path: ./elsewhere}\nname: test-prod\naddons:\n  <<: *d\n  path: ./addons/\n",
		"alias of addons":  "base: &a {enable: true, path: ./addons}\nname: test-prod\naddons: *a\n",
		"merge sequence":   "a: &a {enable: true}\nb: &b {enable: false, path: ./addons}\nname: test-prod\naddons:\n  <<: [*a, *b]\n",
		"first merge wins": "a: &a {enable: true, path: ./addons}\nb: &b {enable: false, path: ./elsewhere}\nname: test-prod\naddons:\n  <<: [*a, *b]\n",
		"root merge alias": "x: &x {addons: {enable: true, path: ./addons/}}\nname: test-prod\n<<: *x\n",
	} {
		t.Run(name, func(t *testing.T) {
			d, runner, _, wd := rcDriver(t, manifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})
			var spec runSpec
			var ccm, raw []byte
			runner.handler = func(c execx.Cmd) error {
				spec = readRunSpec(t, c)
				ccm, _ = os.ReadFile(filepath.Join(spec.Addons.Path, "ccm-hetzner", "ccm.yaml"))
				raw, _ = os.ReadFile(filepath.Join(c.Dir, c.Args[2]))
				return nil
			}
			if err := d.Apply(t.Context(), wd, ""); err != nil {
				t.Fatal(err)
			}
			assertExploded(t, raw)
			if spec.Name != "test-prod" || !spec.Addons.Enable {
				t.Errorf("the manifest copy lost a setting: %+v", spec)
			}
			if !strings.HasPrefix(filepath.Base(filepath.Dir(spec.Addons.Path)), runPrefix) || filepath.Base(spec.Addons.Path) != "addons" {
				t.Errorf("addons.path = %s, want <run>/addons", spec.Addons.Path)
			}
			if string(ccm) != rcClean {
				t.Errorf("the run copy holds %q, want %q", ccm, rcClean)
			}
			assertNoRunCopy(t, wd)
		})
	}
	for _, path := range []string{"./addons/", "addons", ".//addons//"} {
		t.Run("refusal with "+path, func(t *testing.T) {
			d, runner, errBuf, wd := rcDriver(t, "name: test-prod\naddons:\n  enable: true\n  path: "+path+"\n",
				map[string]string{"ccm-hetzner/ccm.yaml": rcMarked})
			if err := d.Apply(t.Context(), wd, ""); err == nil || len(runner.calls) != 0 {
				t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
			}
			if want := rcRefusal("addons/ccm-hetzner/ccm.yaml"); errBuf.String() != want {
				t.Errorf("stderr = %q, want %q", errBuf.String(), want)
			}
			assertNoRunCopy(t, wd)
		})
	}
}

// bats: "kubeone::apply: both implementations refuse the same first file".
// The copy walks the addons directory as filepath.WalkDir does: directory
// by directory, the names sorted by bytes. a/ comes before a-c.yaml ("a"
// is a prefix of "a-c.yaml"), so a/b.yaml is the first file that the guard
// refuses, although "a-c.yaml" < "a/b.yaml" as whole paths.
func TestApplyRunCopyOrder(t *testing.T) {
	d, runner, errBuf, wd := rcDriver(t, kubeoneAddonsManifest,
		map[string]string{"a/b.yaml": rcMarked, "a-c.yaml": rcMarked})
	if err := d.Apply(t.Context(), wd, ""); err == nil || len(runner.calls) != 0 {
		t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
	}
	if want := rcRefusal("addons/a/b.yaml"); errBuf.String() != want {
		t.Errorf("stderr = %q, want %q", errBuf.String(), want)
	}
	assertNoRunCopy(t, wd)
}

// bats: "kubeone::apply: an addons path that cannot be read, or a file of
// it that is not a regular file, stops the apply". A stat error other
// than "does not exist" (a parent directory without search permission) is
// not "no addons directory": lo refuses the path. A FIFO in the addons
// directory is refused as a file that cannot be read: lo does not open it
// (an open would wait for a writer).
func TestApplyRunCopyRefusesWhatItCannotRead(t *testing.T) {
	t.Run("a parent without search permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root searches a directory with mode 0")
		}
		d, runner, errBuf, wd := rcDriver(t, "name: test-prod\naddons:\n  enable: true\n  path: ./locked/addons\n", nil)
		testutil.WriteFile(t, filepath.Join(wd, "locked", "addons", "ccm.yaml"), rcClean)
		if err := os.Chmod(filepath.Join(wd, "locked"), 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(wd, "locked"), 0o755) })
		if err := d.Apply(t.Context(), wd, ""); err == nil || len(runner.calls) != 0 {
			t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
		}
		if want := rcUnreadable("locked/addons"); errBuf.String() != want {
			t.Errorf("stderr = %q, want %q", errBuf.String(), want)
		}
		assertNoRunCopy(t, wd)
	})
	t.Run("a FIFO", func(t *testing.T) {
		d, runner, errBuf, wd := rcDriver(t, kubeoneAddonsManifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})
		if err := syscall.Mkfifo(filepath.Join(wd, "addons", "ccm-hetzner", "fifo.yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- d.Apply(t.Context(), wd, "") }()
		select {
		case err := <-done:
			if err == nil || len(runner.calls) != 0 {
				t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Apply waits on the FIFO")
		}
		if want := rcUnreadable("addons/ccm-hetzner/fifo.yaml"); errBuf.String() != want {
			t.Errorf("stderr = %q, want %q", errBuf.String(), want)
		}
		assertNoRunCopy(t, wd)
	})
}

// bats: "kubeone::apply: a file at the path of the manifest copy stops the
// apply". lo creates the manifest copy as a new file: a FIFO that is at
// its path already (made after lo named the run directory) gets no byte,
// the apply stops, the run directory goes, and the FIFO stays.
func TestApplyRunCopyRefusesAnExistingManifestCopy(t *testing.T) {
	for _, manifest := range []string{"name: test-prod\n", kubeoneAddonsManifest} {
		d, runner, errBuf, wd := rcDriver(t, manifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})
		run := filepath.Join(wd, runPrefix+"fixed")
		orig := mkdirTemp
		mkdirTemp = func(string, string) (string, error) {
			if err := os.Mkdir(run, 0o700); err != nil {
				return "", err
			}
			return run, syscall.Mkfifo(run+".yaml", 0o600)
		}
		err := d.Apply(t.Context(), wd, "")
		mkdirTemp = orig
		if err == nil || len(runner.calls) != 0 {
			t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
		}
		if want := "[error] kubeone: cannot write the manifest copy " + run + ".yaml\n"; errBuf.String() != want {
			t.Errorf("stderr = %q, want %q", errBuf.String(), want)
		}
		if _, err := os.Stat(run); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the run directory is left: %v", err)
		}
		if fi, err := os.Lstat(run + ".yaml"); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("the FIFO that was there is gone or changed: %v %v", fi, err)
		}
	}
}

// A write that fails after writeExclusive made the file (a full disk)
// stops the apply, and no part of the copy stays: writeExclusive removes
// the file that it made, the cleanup removes the run directory. The
// manifest copy is beside the run directory, so only writeExclusive
// removes a part of it. bash writes each file with set -C and removes the
// run copy in the EXIT trap of the apply (_kubeone_copy is set before the
// write).
func TestApplyRunCopyRemovesAPartOfACopyThatItCouldNotWrite(t *testing.T) {
	for name, c := range map[string]struct{ manifest, want string }{
		"the manifest copy": {"name: test-prod\n", "[error] kubeone: cannot write the manifest copy "},
		"an addon file":     {kubeoneAddonsManifest, "[error] kubeone: cannot write the run copy of addons/ccm-hetzner/ccm.yaml\n"},
	} {
		t.Run(name, func(t *testing.T) {
			d, runner, errBuf, wd := rcDriver(t, c.manifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})
			orig := writeData
			writeData = func(f *os.File, data []byte) (int, error) {
				n, _ := f.Write(data[:len(data)/2])
				return n, syscall.ENOSPC
			}
			err := d.Apply(t.Context(), wd, "")
			writeData = orig
			if err == nil || len(runner.calls) != 0 {
				t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
			}
			if !errors.Is(err, syscall.ENOSPC) {
				t.Errorf("err = %v, want the write error", err)
			}
			if !strings.HasPrefix(errBuf.String(), c.want) {
				t.Errorf("stderr = %q, want %q...", errBuf.String(), c.want)
			}
			assertNoRunCopy(t, wd)
		})
	}
}

// bats: "kubeone::apply: a manifest with more than one document, or an
// addons.path that is not a string, stops the apply". lo reads the first
// document of kubeone.yaml, and yq in bash reads each one: so both refuse
// a manifest with more than one document (a last --- starts an empty
// one). An addons.path that is not a string is refused too, also one that
// a merge key gives. A first ---, an addons that is not a mapping, and a
// null or empty addons.path are no addons path: kubeone runs.
func TestApplyRunCopyRefusesAManifestItCannotStage(t *testing.T) {
	const (
		docs   = "holds more than one YAML document"
		notStr = "is not a string"
	)
	for name, c := range map[string]struct{ manifest, want string }{
		"two documents":  {"name: test-prod\n---\nname: other\naddons:\n  enable: true\n  path: ./addons\n", docs},
		"a last ---":     {kubeoneAddonsManifest + "---\n", docs},
		"path false":     {"name: test-prod\naddons:\n  enable: true\n  path: false\n", notStr},
		"path a number":  {"name: test-prod\naddons:\n  enable: true\n  path: 5\n", notStr},
		"path a mapping": {"name: test-prod\naddons:\n  enable: true\n  path: {dir: ./addons}\n", notStr},
		"path a list":    {"name: test-prod\naddons:\n  enable: true\n  path: [./addons]\n", notStr},
		"a merged false": {"d: &d {path: false}\nname: test-prod\naddons:\n  <<: *d\n  enable: true\n", notStr},
		"a first ---":    {"---\n" + kubeoneAddonsManifest, ""},
		"addons false":   {"name: test-prod\naddons: false\n", ""},
		"addons a list":  {"name: test-prod\naddons: [./addons]\n", ""},
		"a null path":    {"name: test-prod\naddons:\n  enable: true\n  path: ~\n", ""},
		"an empty path":  {"name: test-prod\naddons:\n  enable: true\n  path: \"\"\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			d, runner, errBuf, wd := rcDriver(t, c.manifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})
			err := d.Apply(t.Context(), wd, "")
			if c.want == "" {
				if err != nil || len(runner.calls) != 1 || errBuf.Len() != 0 {
					t.Fatalf("err %v, calls %d, stderr %q: want one kubeone run", err, len(runner.calls), errBuf.String())
				}
				assertNoRunCopy(t, wd)
				return
			}
			if err == nil || len(runner.calls) != 0 {
				t.Fatalf("err %v, calls %v: kubeone must not run", err, runner.calls)
			}
			want := "[error] kubeone: " + filepath.Join(wd, "kubeone.yaml") + " " + c.want + "\n"
			if c.want == notStr {
				want = "[error] kubeone: addons.path in " + filepath.Join(wd, "kubeone.yaml") + " " + c.want + "\n"
			}
			if errBuf.String() != want {
				t.Errorf("stderr = %q, want %q", errBuf.String(), want)
			}
			assertNoRunCopy(t, wd)
		})
	}
}

// bats: "kubeone::apply: a signal after the first copied file removes the
// run copy". A SIGHUP while kubeone runs stops kubeone (it cancels the
// command context of interrupt.Watch, so the context of kubeone ends),
// removes the run copy, and Apply returns: the SIGHUP maps to 129 for
// main, which ends lo with it after the cleanup of each caller
// (TestApplyHangupEndsLoAfterTheOuterCleanup). A SIGHUP during the staging
// (once the run directory exists, before the first copied file) does the
// same: kubeone gets a cancelled context, so exec does not start it. The
// test keeps a SIGHUP handler of its own, so the signal never ends the
// test binary.
func TestApplyRemovesTheRunCopyOnASIGHUP(t *testing.T) {
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored")
	}
	keep := make(chan os.Signal, 1)
	signal.Notify(keep, syscall.SIGHUP)
	defer signal.Stop(keep)
	d, runner, _, wd := rcDriver(t, kubeoneAddonsManifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})

	ctx, exitCode := interrupt.Watch(t.Context())
	stopped := false
	runner.handler = func(c execx.Cmd) error {
		if left, _ := filepath.Glob(filepath.Join(wd, runPrefix+"*")); len(left) != 2 {
			t.Errorf("run copy while kubeone runs: %v", left)
		}
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatal(err)
		}
		select {
		case <-runner.ctx.Done():
			stopped = true
			return runner.ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("kubeone was not stopped")
		}
	}
	if err := d.Apply(ctx, wd, ""); err == nil {
		t.Error("Apply = nil after the SIGHUP")
	}
	if !stopped || ctx.Err() == nil {
		t.Errorf("kubeone stopped %v, command context %v: want true, cancelled", stopped, ctx.Err())
	}
	assertNoRunCopy(t, wd)
	if got := exitCode(); got != 129 {
		t.Errorf("exit code = %d, want 129", got)
	}

	ctx, exitCode = interrupt.Watch(t.Context())
	origMkdirTemp := mkdirTemp
	defer func() { mkdirTemp = origMkdirTemp }()
	mkdirTemp = func(dir, pattern string) (string, error) {
		made, err := origMkdirTemp(dir, pattern)
		if err == nil {
			err = syscall.Kill(os.Getpid(), syscall.SIGHUP)
		}
		return made, err
	}
	cancelled := false
	runner.handler = func(c execx.Cmd) error {
		select {
		case <-runner.ctx.Done():
			cancelled = true
			return runner.ctx.Err()
		case <-time.After(10 * time.Second):
			return errors.New("the context of kubeone was not cancelled")
		}
	}
	if err := d.Apply(ctx, wd, ""); err == nil {
		t.Error("Apply = nil after a SIGHUP during the staging")
	}
	if !cancelled || ctx.Err() == nil {
		t.Errorf("staging: kubeone context cancelled %v, command context %v: want true, cancelled", cancelled, ctx.Err())
	}
	assertNoRunCopy(t, wd)
	if got := exitCode(); got != 129 {
		t.Errorf("staging: exit code = %d, want 129", got)
	}
	mkdirTemp = origMkdirTemp

	// Without a SIGHUP, the run ends as it did.
	ctx, exitCode = interrupt.Watch(t.Context())
	runner.handler = nil
	if err := d.Apply(ctx, wd, ""); err != nil || ctx.Err() != nil {
		t.Errorf("Apply = %v, command context %v: want nil, nil", err, ctx.Err())
	}
	assertNoRunCopy(t, wd)
	if got := exitCode(); got != 0 {
		t.Errorf("exit code without a signal = %d, want 0", got)
	}
}

// A SIGHUP while kubeone runs ends lo with the SIGHUP after the cleanup of
// each caller of Apply, not in Apply: here the temporary provider config
// of lo provision (provision.WriteProviderConfig), which a deferred
// cleanup of the provision dispatch removes. The test runs a child of the
// test binary as lo runs (interrupt.Watch, the command, interrupt.Exit),
// sends it a real SIGHUP while kubeone runs, and checks that the child
// ended with the SIGHUP and that neither the provider config nor the run
// copy is left.
func TestApplyHangupEndsLoAfterTheOuterCleanup(t *testing.T) {
	if dir := os.Getenv("LOK8S_TEST_HANGUP_DIR"); dir != "" {
		hangupChild(t, dir)
		return
	}
	if signal.Ignored(syscall.SIGHUP) {
		t.Skip("SIGHUP is ignored")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(exe, "-test.run=^TestApplyHangupEndsLoAfterTheOuterCleanup$", "-test.count=1")
	cmd.Env = append(os.Environ(), "LOK8S_TEST_HANGUP_DIR="+dir, "TMPDIR="+dir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "kubeone-runs")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("kubeone did not start in the child:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGHUP {
		t.Errorf("the child ended with %v, want the signal SIGHUP:\n%s", cmd.ProcessState, out.String())
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "lok8s-provider-config.*.yaml")); len(left) != 0 {
		t.Errorf("the cleanup of the caller did not run, the provider config is left: %v", left)
	}
	var left []string
	_ = filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(filepath.Base(path), runPrefix) {
			left = append(left, path)
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("the run copy is left: %v", left)
	}
	if strings.Contains(out.String(), "--- FAIL") {
		t.Errorf("the child failed:\n%s", out.String())
	}
}

// hangupChild is the child of TestApplyHangupEndsLoAfterTheOuterCleanup: a
// caller with a deferred cleanup runs Apply, in the order of main (Watch,
// the command, the exit code, Exit). Its temporary files go in dir
// (TMPDIR).
func hangupChild(t *testing.T, dir string) {
	ctx, exitCode := interrupt.Watch(context.Background())
	rc := func() int {
		spec := filepath.Join(dir, "cluster.lok8s.yaml")
		testutil.WriteFile(t, spec, "spec:\n  provider:\n    name: hetzner\n    config:\n      location: fsn1\n")
		_, cleanup, err := provision.WriteProviderConfig(spec, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		d, runner, _, wd := rcDriver(t, kubeoneAddonsManifest, map[string]string{"ccm-hetzner/ccm.yaml": rcClean})
		runner.handler = func(c execx.Cmd) error {
			testutil.WriteFile(t, filepath.Join(dir, "kubeone-runs"), "")
			select {
			case <-runner.ctx.Done():
				return runner.ctx.Err()
			case <-time.After(20 * time.Second):
				return errors.New("kubeone was not stopped")
			}
		}
		if err := d.Apply(ctx, wd, ""); err == nil {
			return 3
		}
		return 1
	}()
	if c := exitCode(); c != 0 {
		rc = c
	}
	interrupt.Exit(rc)
}

// explode resolves aliases and merge keys as yq's explode(.) does with
// --yaml-fix-merge-anchor-to-spec, and stops an alias bomb.
func TestExplode(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"own key wins":       {"a: &a {x: 1, y: 1}\nb:\n  <<: *a\n  y: 2\n", "a: {x: 1, y: 1}\nb: {x: 1, y: 2}\n"},
		"first merge wins":   {"a: &a {x: 1}\nb: &b {x: 2, z: 2}\nc:\n  <<: [*a, *b]\n", "a: {x: 1}\nb: {x: 2, z: 2}\nc: {x: 1, z: 2}\n"},
		"alias":              {"a: &a [1, 2]\nb: *a\n", "a: [1, 2]\nb: [1, 2]\n"},
		"quoted << is a key": {"'<<': {x: 1}\n", "'<<': {x: 1}\n"},
		"scalars stay as is": {"v: 1.30\nq: \"007\"\n", "v: 1.30\nq: \"007\"\n"},
		"nested merge":       {"a: &a {x: {<<: {y: 1}}}\nb: *a\n", "a: {x: {y: 1}}\nb: {x: {y: 1}}\n"},
	} {
		t.Run(name, func(t *testing.T) {
			doc, err := parseYAMLDoc([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			budget := explodeBudget
			got, err := explode(doc, &budget)
			if err != nil {
				t.Fatal(err)
			}
			var gotV, wantV any
			raw, _ := encodeYAML(got)
			if err := yaml.Unmarshal(raw, &gotV); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(tc.want), &wantV); err != nil {
				t.Fatal(err)
			}
			if a, b := mustJSON(t, gotV), mustJSON(t, wantV); a != b {
				t.Errorf("explode = %s, want %s", a, b)
			}
			if strings.Contains(string(raw), "&") || strings.Contains(string(raw), "*") {
				t.Errorf("anchors or aliases are left:\n%s", raw)
			}
			if name == "scalars stay as is" && !strings.Contains(string(raw), "1.30") {
				t.Errorf("a scalar changed:\n%s", raw)
			}
		})
	}
	bomb := "a: &a [x, x, x, x, x, x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a, *a, *a, *a, *a, *a]\nc: &c [*b, *b, *b, *b, *b, *b, *b, *b, *b, *b]\nd: &d [*c, *c, *c, *c, *c, *c, *c, *c, *c, *c]\ne: &e [*d, *d, *d, *d, *d, *d, *d, *d, *d, *d]\nf: &f [*e, *e, *e, *e, *e, *e, *e, *e, *e, *e]\ng: [*f, *f, *f, *f, *f, *f, *f, *f, *f, *f]\n"
	doc, err := parseYAMLDoc([]byte(bomb))
	if err != nil {
		t.Fatal(err)
	}
	budget := explodeBudget
	if _, err := explode(doc, &budget); err == nil {
		t.Error("an alias bomb exploded without an error")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
