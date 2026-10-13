package addons

// dryrun_exec_test.go — the dry-run boundary proven against the REAL pinned
// kustomize (exec pipeline) and the REAL secrets.lok8s.dev plugin: this test
// binary serves the plugin (secretPluginMain, hooked into TestMain). Every
// refusal case is checked twice: the dry run never executes the fixture's
// code (its marker file stays absent), and --allow-generators does (the
// marker appears), so a fixture that never could execute cannot pass.
//
// Needs .bin/kustomize (b install): skips locally without it, fails on CI.

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
	"github.com/kernpilot/lok8s/internal/testutil"
	"github.com/kernpilot/lok8s/internal/toolchain"
	kplugin "github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// secretPluginEnv makes the test binary answer as the Secret plugin.
const secretPluginEnv = "LOK8S_TEST_SECRET_PLUGIN"

// secretPluginMain serves secrets.lok8s.dev/v1/Secret (and its
// --capabilities answer) when a test linked this binary as the plugin.
func secretPluginMain(args []string, stdout, stderr io.Writer) (bool, int) {
	if os.Getenv(secretPluginEnv) != "1" || len(args) == 0 || !strings.HasSuffix(filepath.ToSlash(args[0]), "/secret/Secret") {
		return false, 0
	}
	if len(args) == 2 && args[1] == "--capabilities" {
		fmt.Fprintln(stdout, secret.Capabilities)
		return true, 0
	}
	if err := secret.Run(args, os.Stdin, stdout, kplugin.DefaultEnv); err != nil {
		fmt.Fprintln(stderr, "secret plugin:", err)
		return true, 1
	}
	return true, 0
}

// execProject is a project whose renders run the pinned kustomize binary
// (LO_RENDER=exec) with a plugin home that holds this test binary as the
// Secret plugin, plus the dry run's boundary.
func execProject(t *testing.T) (*config.Paths, execx.Runner, *DryRunBoundary) {
	t.Helper()
	repo := testutil.RepoRoot(t)
	bin := filepath.Join(repo, ".bin")
	if info, err := os.Stat(filepath.Join(bin, "kustomize")); err != nil || info.Mode()&0o111 == 0 {
		what := "pinned kustomize not installed under .bin (b install): the dry-run boundary tests skipped"
		if os.Getenv("CI") != "" {
			t.Fatal(what + "; on CI the toolchain must be installed before the Go tests")
		}
		t.Skip(what)
	}
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	t.Setenv(secretPluginEnv, "1")
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: bin, Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	home := filepath.Join(base, ".kustomize")
	t.Setenv(KustomizePluginHomeEnv, home)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pluginPath := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel))
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, pluginPath); err != nil {
		t.Fatal(err)
	}
	runner := execx.NewRunner(p)
	b, err := NewDryRunBoundary(t.Context(), p, runner)
	if err != nil {
		t.Fatalf("NewDryRunBoundary: %v", err)
	}
	t.Cleanup(b.Close)
	return p, runner, b
}

// fixtureAddon writes an addon directory from files.
func fixtureAddon(t *testing.T, base, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(base, "addons", name)
	for rel, content := range files {
		testutil.WriteFile(t, filepath.Join(dir, rel), content)
	}
	return dir
}

// spyScript writes an executable that touches marker, prints out and exits
// with rc.
func spyScript(t *testing.T, path, marker, out string, rc int) {
	t.Helper()
	script := fmt.Sprintf("#!/bin/sh\ntouch '%s'\ncat <<'EOF'\n%sEOF\nexit %d\n", marker, out, rc)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
}

const plainCM = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: plain\n"

func renderBoth(t *testing.T, p *config.Paths, runner execx.Runner, b *DryRunBoundary, dir string, allow bool) (string, string, error) {
	t.Helper()
	var errBuf strings.Builder
	out, err := RenderDryRun(t.Context(), p, runner, &errBuf, dir, "", "", "", nil, b, allow)
	return out, errBuf.String(), err
}

// A KRM exec function (the `config.kubernetes.io/function` exec annotation,
// on any GVK) never runs in a dry run: kustomize runs without --enable-exec
// and skips it, so its objects are missing from the preview.
func TestDryRunNeverRunsAKRMExecFunction(t *testing.T) {
	p, runner, b := execProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	fn := filepath.Join(t.TempDir(), "fn.sh")
	spyScript(t, fn, marker, "apiVersion: config.kubernetes.io/v1\nkind: ResourceList\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: from-fn\n", 0)
	// The annotation sits on a Secret-shaped object of the built-in GVK:
	// the GVK does not matter, the annotation decides.
	dir := fixtureAddon(t, p.Base, "krm", map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\ngenerators:\n- fn.yaml\n",
		"cm.yaml":            plainCM,
		"fn.yaml":            "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: " + fn + "\n",
	})
	out, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if fsutil.FileExists(marker) {
		t.Fatal("a dry run executed a KRM exec function")
	}
	if strings.Contains(out, "from-fn") {
		t.Errorf("the dry render carries the function's object:\n%s", out)
	}
	out, stderr, err = renderBoth(t, p, runner, b, dir, true)
	if err != nil || !fsutil.FileExists(marker) || !strings.Contains(out, "from-fn") {
		t.Fatalf("--allow-generators did not run the function (fixture dead?): err=%v marker=%v\n%s\n%s", err, fsutil.FileExists(marker), out, stderr)
	}
}

// A legacy exec plugin that is not one of lok8s' two built-ins does not
// resolve in the dry run's plugin home: the render fails, the plugin never
// runs.
func TestDryRunRefusesAForeignExecPlugin(t *testing.T) {
	p, runner, b := execProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	spyScript(t, filepath.Join(os.Getenv(KustomizePluginHomeEnv), "example.com", "v1", "evil", "Evil"), marker, plainCM, 0)
	dir := fixtureAddon(t, p.Base, "foreign", map[string]string{
		"kustomization.yaml": "generators:\n- evil.yaml\n",
		"evil.yaml":          "apiVersion: example.com/v1\nkind: Evil\nmetadata:\n  name: evil\n",
	})
	_, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err == nil {
		t.Fatalf("the dry render of a foreign exec plugin succeeded\n%s", stderr)
	}
	if fsutil.FileExists(marker) {
		t.Fatal("a dry run executed a foreign exec plugin")
	}
	if !strings.Contains(stderr, "Evil") {
		t.Errorf("stderr does not name the plugin:\n%s", stderr)
	}
	if _, stderr, err = renderBoth(t, p, runner, b, dir, true); err != nil || !fsutil.FileExists(marker) {
		t.Fatalf("--allow-generators did not run the plugin (fixture dead?): %v\n%s", err, stderr)
	}
}

// A KRM container function runs `docker`; the dry run's refusing docker is
// first on PATH, so the render fails with the refusal and a docker further
// down PATH never runs.
func TestDryRunRefusesAContainerFunction(t *testing.T) {
	p, runner, b := execProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	spyBin := t.TempDir()
	spyScript(t, filepath.Join(spyBin, "docker"), marker, "", 1)
	t.Setenv("PATH", spyBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := fixtureAddon(t, p.Base, "container", map[string]string{
		"kustomization.yaml": "generators:\n- fn.yaml\n",
		"fn.yaml":            "apiVersion: example.com/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      container:\n        image: example.com/fn:1\n",
	})
	_, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err == nil || !strings.Contains(stderr, dockerRefusal) {
		t.Fatalf("err=%v, want the docker refusal in stderr:\n%s", err, stderr)
	}
	if fsutil.FileExists(marker) {
		t.Fatal("a dry run ran docker")
	}
	if _, _, _ = renderBoth(t, p, runner, b, dir, true); !fsutil.FileExists(marker) {
		t.Fatal("--allow-generators did not reach docker (fixture dead?)")
	}
}

// A remote base runs `git`; the dry run's refusing git is first on PATH.
// Nothing reaches the network.
func TestDryRunRefusesARemoteBase(t *testing.T) {
	p, runner, b := execProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	spyBin := t.TempDir()
	spyScript(t, filepath.Join(spyBin, "git"), marker, "", 1)
	t.Setenv("PATH", spyBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := fixtureAddon(t, p.Base, "remote", map[string]string{
		"kustomization.yaml": "resources:\n- https://github.com/kernpilot/no-such-repo//base?ref=v0.0.0\n",
	})
	_, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err == nil || !strings.Contains(stderr, gitRefusal) {
		t.Fatalf("err=%v, want the git refusal in stderr:\n%s", err, stderr)
	}
	if fsutil.FileExists(marker) {
		t.Fatal("a dry run ran git")
	}
	if _, _, _ = renderBoth(t, p, runner, b, dir, true); !fsutil.FileExists(marker) {
		t.Fatal("--allow-generators did not reach git (fixture dead?)")
	}
}

// The secrets plugin reads the store and generates a missing value in
// memory: the store is not created, the value is reported (the entry is
// non-repeatable). A value the store holds is read, not reported.
func TestDryRunSecretMintIsInMemoryAndReported(t *testing.T) {
	p, runner, b := execProject(t)
	store := filepath.Join(p.Base, "clusters", "x.dev", "secrets")
	t.Setenv("PATH_SECRETS", store)
	dir := fixtureAddon(t, p.Base, "app", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: ns\npasswd:\n  PASSWORD: 24\n",
	})
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "PASSWORD:") {
		t.Errorf("the generated Secret is missing:\n%s", out)
	}
	if fsutil.DirExists(store) {
		t.Fatal("a dry run created the secrets store")
	}
	if got := b.Minted(); len(got) != 1 || got[0] != "secret Secret.app.ns.PASSWORD" {
		t.Errorf("report = %q, want the one generated entry", got)
	}

	testutil.WriteFile(t, filepath.Join(store, "Secret.app.ns.PASSWORD"), "from-the-store")
	before := hashDir(t, store)
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	out, stderr, err = renderBoth(t, p, runner, b, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, base64.StdEncoding.EncodeToString([]byte("from-the-store"))) {
		t.Errorf("the stored value was not read:\n%s", out)
	}
	if got := b.Minted(); len(got) != 0 {
		t.Errorf("a stored value was reported as generated: %q", got)
	}
	if hashDir(t, store) != before {
		t.Error("a dry run changed the secrets store")
	}
}

// A leaf signed by the shared CA, with CAROOT empty: the dry run mints the
// CA in memory, never writes CAROOT, and reports it.
func TestDryRunCAMintIsEphemeralAndReported(t *testing.T) {
	p, runner, b := execProject(t)
	caroot := filepath.Join(t.TempDir(), "caroot")
	t.Setenv("CAROOT", caroot)
	store := filepath.Join(p.Base, "clusters", "x.dev", "secrets")
	t.Setenv("PATH_SECRETS", store)
	dir := fixtureAddon(t, p.Base, "tls", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: web\n  namespace: ns\ntype: kubernetes.io/tls\ncert:\n  hosts: [web.example.test]\n",
	})
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "tls.crt:") {
		t.Errorf("the leaf is missing:\n%s", out)
	}
	if fsutil.DirExists(caroot) || fsutil.DirExists(store) {
		t.Fatal("a dry run wrote CAROOT or the store")
	}
	minted := strings.Join(b.Minted(), "\n")
	if !strings.Contains(minted, "caroot "+caroot) {
		t.Errorf("the CA mint was not reported: %q", minted)
	}
}

// A Secret plugin that predates the dry run (no --capabilities) is refused
// before anything renders: it would write the secrets store.
func TestDryRunBoundaryRefusesAnOldSecretPlugin(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	t.Setenv(KustomizePluginHomeEnv, "")
	os.Unsetenv(KustomizePluginHomeEnv)
	old := filepath.Join(base, ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel))
	spyScript(t, old, filepath.Join(base, "ran"), "", 1)
	if _, err := NewDryRunBoundary(t.Context(), p, execx.NewRunner(p)); err == nil || !strings.Contains(err.Error(), "does not support a dry run") {
		t.Fatalf("err = %v", err)
	}
	spyScript(t, old, filepath.Join(base, "ran"), "dry-run\n", 0)
	b, err := NewDryRunBoundary(t.Context(), p, execx.NewRunner(p))
	if err != nil {
		t.Fatalf("a capable plugin was refused: %v", err)
	}
	b.Close()
	if fsutil.DirExists(b.Dir) {
		t.Errorf("Close left %s", b.Dir)
	}
}

// hashDir lists every file of dir with its content.
func hashDir(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %v %d\n", path, info.Mode(), info.Size())
		if info.Mode().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			b.Write(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
