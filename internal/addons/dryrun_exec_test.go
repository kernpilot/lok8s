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
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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
	if got := b.ReadReport().Generated; len(got) != 1 || got[0] != "secret Secret.app.ns.PASSWORD" {
		t.Errorf("report = %q, want the one generated entry", got)
	}

	// A later render of the same run reads the value back from the run
	// store: the same bytes, reported as a reuse (the value is this run's,
	// not the store's: a render that reads it is not repeatable either).
	first := out
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	if again, _, err := renderBoth(t, p, runner, b, dir, false); err != nil || again != first {
		t.Errorf("a second render of the run differs (err %v):\n%s\n%s", err, first, again)
	}
	if got := b.ReadReport().Generated; len(got) != 1 || got[0] != "reuse Secret.app.ns.PASSWORD" {
		t.Errorf("the second render reported %q, want the reuse", got)
	}

	// A new run against an EXISTING store that holds the value: read, not
	// reported, the store untouched (a store with other entries too).
	testutil.WriteFile(t, filepath.Join(store, "Secret.app.ns.PASSWORD"), "from-the-store")
	testutil.WriteFile(t, filepath.Join(store, "Secret.other.ns.KEY"), "other")
	before := hashDir(t, store)
	b2, err := NewDryRunBoundary(t.Context(), p, runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.Close)
	out, stderr, err = renderBoth(t, p, runner, b2, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, base64.StdEncoding.EncodeToString([]byte("from-the-store"))) {
		t.Errorf("the stored value was not read:\n%s", out)
	}
	if got := b2.ReadReport().Generated; len(got) != 0 {
		t.Errorf("a stored value was reported as generated: %q", got)
	}
	if hashDir(t, store) != before {
		t.Error("a dry run changed the secrets store")
	}
}

// A miss in an EXISTING store directory (the production shape: the store is
// there, one value is new) generates the value in the run store and writes
// nothing into the store.
func TestDryRunMissInAnExistingStoreWritesNothing(t *testing.T) {
	p, runner, b := execProject(t)
	store := filepath.Join(p.Base, "clusters", "x.dev", "secrets")
	testutil.WriteFile(t, filepath.Join(store, "Secret.app.ns.OLD"), "old")
	t.Setenv("PATH_SECRETS", store)
	before := hashDir(t, store)
	dir := fixtureAddon(t, p.Base, "app", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: ns\npasswd:\n  NEW: 24\n",
	})
	if _, stderr, err := renderBoth(t, p, runner, b, dir, false); err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if hashDir(t, store) != before {
		t.Errorf("a dry run wrote into the existing store:\n%s", hashDir(t, store))
	}
	if got := b.ReadReport().Generated; len(got) != 1 || got[0] != "secret Secret.app.ns.NEW" {
		t.Errorf("report = %q", got)
	}
	if ents, _ := os.ReadDir(b.Store); len(ents) == 0 {
		t.Error("the run store holds nothing: the value was not kept for the run")
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
	minted := strings.Join(b.ReadReport().Generated, "\n")
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

// The check of the Secret plugin runs the plugin that the render runs (in
// the boundary's plugin home), with the environment of a dry-run render, and
// so that no plugin version can do anything with it: in an empty directory
// of the boundary that is removed after, with an empty stdin, PATH_SECRETS at
// a path that does not exist, the store-free switch on and the keys-only
// setting off. The caller's directory, stdin, store and settings do not reach
// it.
func TestDryRunBoundaryChecksThePluginAsTheRenderRunsIt(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	t.Setenv(KustomizePluginHomeEnv, "")
	os.Unsetenv(KustomizePluginHomeEnv)
	// The caller: its own directory, a store, the settings and stdin data.
	caller := filepath.Join(base, "caller")
	testutil.WriteFile(t, filepath.Join(caller, "spec.yaml"), "kind: x\n")
	t.Chdir(caller)
	t.Setenv("PATH_SECRETS", filepath.Join(base, "caller-store"))
	t.Setenv(secret.DisableEnv, "")
	t.Setenv(secret.KeysOnlyEnv, "1")
	testutil.WriteFile(t, filepath.Join(base, "stdin"), "caller input\n")
	in, err := os.Open(filepath.Join(base, "stdin"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	stdin := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = stdin }()

	log := filepath.Join(base, "probe.log")
	plugin := filepath.Join(base, ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel))
	script := "#!/bin/sh\n{\n" +
		"  echo \"self=$0\"\n" +
		"  echo \"pwd=$(pwd -P)\"\n" +
		"  echo \"entries=$(ls -A | wc -l | tr -d ' ')\"\n" +
		"  echo \"stdin=$(wc -c | tr -d ' ')\"\n" +
		"  echo \"PATH_SECRETS=${PATH_SECRETS-<unset>}\"\n" +
		"  if [ -e \"${PATH_SECRETS:-/nonexistent-store}\" ]; then echo store=exists; else echo store=absent; fi\n" +
		"  echo \"DISABLE=${" + secret.DisableEnv + "-<unset>}\"\n" +
		"  echo \"KEYS_ONLY=${" + secret.KeysOnlyEnv + "-<unset>}\"\n" +
		"  echo \"DRYRUN=${" + DryRunEnv + "-<unset>}\"\n" +
		"  echo \"HOME=${" + KustomizePluginHomeEnv + "-<unset>}\"\n" +
		"  echo \"PATH0=${PATH%%:*}\"\n" +
		"  echo \"TMPDIR=${TMPDIR-<unset>}\"\n" +
		"  echo \"STORE=${" + DryRunStoreEnv + "-<unset>}\"\n" +
		"  echo \"HELMP=${" + HelmPluginsEnv + "-<unset>}\"\n" +
		"  echo \"REPORT=${" + DryRunReportEnv + "-<unset>}\"\n" +
		"} > '" + log + "'\necho dry-run\n"
	testutil.WriteFile(t, plugin, script)
	if err := os.Chmod(plugin, 0o700); err != nil {
		t.Fatal(err)
	}

	b, err := NewDryRunBoundary(t.Context(), p, execx.NewRunner(p))
	if err != nil {
		t.Fatalf("NewDryRunBoundary: %v", err)
	}
	defer b.Close()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the plugin was not run: %v", err)
	}
	got := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	boundary, err := filepath.EvalSymlinks(b.Dir)
	if err != nil {
		t.Fatal(err)
	}
	probe := got["pwd"]
	if filepath.Dir(probe) != boundary || !strings.HasPrefix(filepath.Base(probe), "probe-") {
		t.Errorf("the check ran in %s, want a probe- directory of the boundary %s", probe, boundary)
	}
	want := map[string]string{
		"self":         filepath.Join(b.PluginHome, filepath.FromSlash(toolchain.SecretPluginRel)),
		"entries":      "0",
		"stdin":        "0",
		"PATH_SECRETS": filepath.Join(b.Dir, filepath.Base(probe), "store"),
		"store":        "absent",
		"DISABLE":      "1",
		"KEYS_ONLY":    "0",
		"DRYRUN":       "1",
		"HOME":         b.PluginHome,
		"PATH0":        b.StubBin,
		"TMPDIR":       b.Tmp,
		"STORE":        b.Store,
		"HELMP":        b.HelmPlugins,
		"REPORT":       b.Report,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the check saw %s=%q, want %q", k, got[k], v)
		}
	}
	if fsutil.DirExists(probe) {
		t.Errorf("the check left its directory %s", probe)
	}
}

// The check passes only on a plugin that exits 0 and prints the line
// "dry-run" exactly, as the keys-only probe reads its line (bash: the same
// cases in tests/unit/bootstrap_test.bats).
func TestDryRunBoundaryReadsTheCapabilityLineExactly(t *testing.T) {
	cases := []struct {
		name string
		out  string
		rc   int
		ok   bool
	}{
		{"the line alone", "dry-run\n", 0, true},
		{"the line among others", "keys-only\ndry-run\nother\n", 0, true},
		{"the line and a non-zero exit", "dry-run\n", 1, false},
		{"a blank after the word", "dry-run \n", 0, false},
		{"a blank before the word", " dry-run\n", 0, false},
		{"a carriage return after the word", "dry-run\r\n", 0, false},
		{"a longer word", "dry-runs\n", 0, false},
		{"the word inside a line", "no dry-run\n", 0, false},
		{"no output", "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(render.ModeEnv, string(render.ModeExec))
			base := t.TempDir()
			p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
			t.Setenv(KustomizePluginHomeEnv, "")
			os.Unsetenv(KustomizePluginHomeEnv)
			plugin := filepath.Join(base, ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel))
			spyScript(t, plugin, filepath.Join(base, "ran"), c.out, c.rc)
			b, err := NewDryRunBoundary(t.Context(), p, execx.NewRunner(p))
			if b != nil {
				b.Close()
			}
			if !fsutil.FileExists(filepath.Join(base, "ran")) {
				t.Fatal("the plugin was not run")
			}
			switch {
			case c.ok && err != nil:
				t.Errorf("the plugin (%q, exit %d) was refused: %v", c.out, c.rc, err)
			case !c.ok && (err == nil || !strings.Contains(err.Error(), "does not support a dry run")):
				t.Errorf("the plugin (%q, exit %d): err = %v, want the refusal", c.out, c.rc, err)
			}
		})
	}
}

// A relative plugin home would make the boundary's links not resolve, and
// the check would not run: the dry run refuses the home first, before the
// plugin runs (bash: the same test).
func TestDryRunBoundaryRefusesARelativePluginHome(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	plugin := filepath.Join(base, "rel", ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel))
	spyScript(t, plugin, filepath.Join(base, "ran"), "dry-run\n", 0)
	t.Chdir(base)
	t.Setenv(KustomizePluginHomeEnv, "rel/.kustomize")
	b, err := NewDryRunBoundary(t.Context(), p, execx.NewRunner(p))
	if b != nil {
		b.Close()
	}
	want := "the kustomize plugin home rel/.kustomize is a relative path, so the dry run's links to its plugins would not resolve — set KUSTOMIZE_PLUGIN_HOME to an absolute path"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if fsutil.FileExists(filepath.Join(base, "ran")) {
		t.Error("the plugin ran")
	}
}

// boundaryWithPlugin builds the boundary of a project whose Secret plugin
// prints "dry-run", and returns the plugin home and the plugin's project
// path.
func boundaryWithPlugin(t *testing.T) (*config.Paths, *DryRunBoundary, string, string) {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	t.Setenv(KustomizePluginHomeEnv, "")
	os.Unsetenv(KustomizePluginHomeEnv)
	home := filepath.Join(base, ".kustomize")
	plugin := filepath.Join(home, filepath.FromSlash(toolchain.SecretPluginRel))
	spyScript(t, plugin, filepath.Join(base, "ran"), "dry-run\n", 0)
	b, err := NewDryRunBoundary(t.Context(), p, execx.NewRunner(p))
	if err != nil {
		t.Fatalf("NewDryRunBoundary: %v", err)
	}
	t.Cleanup(b.Close)
	return p, b, home, config.RelTo(base, plugin)
}

// With a Secret plugin in the project, a link of the boundary that does not
// resolve is refused, not skipped (bash: the same test).
func TestDryRunBoundaryRefusesAPluginLinkThatDoesNotResolve(t *testing.T) {
	p, b, home, rel := boundaryWithPlugin(t)
	link := filepath.Join(b.PluginHome, filepath.FromSlash(toolchain.SecretPluginRel))
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.Base, "gone"), link); err != nil {
		t.Fatal(err)
	}
	err := b.checkSecretPlugin(t.Context(), p, nil, home)
	want := "the dry run's link to the Secret plugin " + rel + " does not resolve, so the dry run cannot check the plugin"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// A probe directory that cannot be made has its own message, the same as
// in bash ("bootstrap dry-run: " is the engine's prefix on both sides).
func TestDryRunBoundaryNamesAProbeDirectoryItCannotMake(t *testing.T) {
	p, b, home, rel := boundaryWithPlugin(t)
	gone := *b
	gone.Dir = filepath.Join(p.Base, "gone")
	err := gone.checkSecretPlugin(t.Context(), p, nil, home)
	want := "cannot make a directory for the check of the Secret plugin " + rel + ": "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v, want a message that starts with %q", err, want)
	}
}

// slowPlugin writes a Secret plugin that records its pid in dir/pid, then
// runs for about 3 s and touches dir/ended: a plugin that the check stopped
// never does. onSignal is its INT and TERM trap ("" ignores them).
func slowPlugin(t *testing.T, plugin, dir, onSignal string) {
	t.Helper()
	testutil.WriteFile(t, plugin, "#!/bin/sh\ntrap '"+onSignal+"' INT TERM\necho $$ > \""+dir+"/pid\"\n"+
		"i=0\nwhile [ \"$i\" -lt 30 ]; do sleep 0.1; i=$((i + 1)); done\ntouch \""+dir+"/ended\"\necho dry-run\n")
	if err := os.Chmod(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
}

// pluginStopped fails the test if the plugin of slowPlugin still runs, or
// if it ran to its end. It waits until the plugin would have ended (3.5 s
// after start).
func pluginStopped(t *testing.T, dir string, start time.Time) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatalf("the plugin did not start: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the plugin (pid %d) still runs after the check returned (kill -0: %v)", pid, err)
	}
	time.Sleep(time.Until(start.Add(3500 * time.Millisecond)))
	if fsutil.FileExists(filepath.Join(dir, "ended")) {
		t.Error("the plugin ran to its end: the check did not stop it")
	}
}

// slowProject is a project whose Secret plugin is slowPlugin, with its
// markers in a directory of their own.
func slowProject(t *testing.T, onSignal string) (*config.Paths, string) {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	t.Setenv(KustomizePluginHomeEnv, "")
	os.Unsetenv(KustomizePluginHomeEnv)
	marks := t.TempDir()
	slowPlugin(t, filepath.Join(base, ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel)), marks, onSignal)
	return p, marks
}

// boundaryIn builds the boundary in a goroutine and returns its error, or
// fails the test after 20 s.
func boundaryIn(t *testing.T, ctx context.Context, p *config.Paths) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		b, err := NewDryRunBoundary(ctx, p, execx.NewRunner(p))
		if b != nil {
			b.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the check did not end in 20 s")
	}
	return nil
}

// The check has a time limit (bash: _ADDONS_DRYRUN_CHECK_TIMEOUT and
// _GRACE). At secretCheckTimeout the plugin gets an interrupt and has
// secretCheckGrace to end: a plugin that cleans up in that time finishes
// its cleanup, and is still refused (it did not end in time). A plugin that
// ignores the interrupt is killed at the end of the grace, not before. In
// both cases the plugin does not run on after the check.
func TestDryRunBoundaryBoundsTheCheck(t *testing.T) {
	timeout, grace := secretCheckTimeout, secretCheckGrace
	secretCheckTimeout, secretCheckGrace = 500*time.Millisecond, time.Second
	t.Cleanup(func() { secretCheckTimeout, secretCheckGrace = timeout, grace })

	t.Run("a plugin that ignores the interrupt", func(t *testing.T) {
		p, marks := slowProject(t, "")
		start := time.Now()
		err := boundaryIn(t, t.Context(), p)
		took := time.Since(start)
		if err == nil || !strings.Contains(err.Error(), "did not end in 0.5 s") {
			t.Errorf("err = %v, want the time limit", err)
		}
		if took < 1300*time.Millisecond || took > 2600*time.Millisecond {
			t.Errorf("the check took %s, want about 1.5 s (the limit, then the whole grace)", took)
		}
		pluginStopped(t, marks, start)
	})
	t.Run("a plugin that cleans up after the interrupt", func(t *testing.T) {
		p, marks := slowProject(t, "")
		cleaned := filepath.Join(marks, "cleaned")
		slowPlugin(t, filepath.Join(p.Base, ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel)), marks,
			`sleep 0.2; touch "`+cleaned+`"; echo dry-run; exit 0`)
		start := time.Now()
		err := boundaryIn(t, t.Context(), p)
		if err == nil || !strings.Contains(err.Error(), "did not end in 0.5 s") {
			t.Errorf("err = %v, want the time limit", err)
		}
		if !fsutil.FileExists(cleaned) {
			t.Error("the plugin did not finish its cleanup: it got no grace")
		}
		pluginStopped(t, marks, start)
	})
}

// A cancel of the caller (Ctrl-C) stops the check the same way: the
// interrupt, then a kill at the end of the grace (bash: addons::_check_stop
// on INT and TERM).
func TestDryRunBoundaryStopsThePluginOnACancel(t *testing.T) {
	grace := secretCheckGrace
	secretCheckGrace = time.Second
	t.Cleanup(func() { secretCheckGrace = grace })
	p, marks := slowProject(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	start := time.Now()
	go func() {
		for !fsutil.FileExists(filepath.Join(marks, "pid")) {
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()
	if err := boundaryIn(t, ctx, p); err == nil {
		t.Error("the cancelled check passed")
	}
	if took := time.Since(start); took > 2600*time.Millisecond {
		t.Errorf("the check took %s after the start, want about 1 s (the grace)", took)
	}
	pluginStopped(t, marks, start)
}

// The plugin's stdout is a file, as in bash: a plugin that prints the line,
// exits 0 and leaves a child that keeps its stdout open passes at once.
func TestDryRunBoundaryDoesNotWaitForAChildOfThePlugin(t *testing.T) {
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin")}
	t.Setenv(KustomizePluginHomeEnv, "")
	os.Unsetenv(KustomizePluginHomeEnv)
	plugin := filepath.Join(base, ".kustomize", filepath.FromSlash(toolchain.SecretPluginRel))
	testutil.WriteFile(t, plugin, "#!/bin/sh\necho dry-run\nsleep 6 &\nexit 0\n")
	if err := os.Chmod(plugin, 0o700); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := boundaryIn(t, t.Context(), p); err != nil {
		t.Errorf("the plugin was refused: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the check took %s: it waited for the plugin's child", took)
	}
}

// The check makes a new file for the plugin's output. If the name is
// already there (here a link to a file elsewhere), it writes nothing
// through it and stops with its own message, the same as in bash.
func TestDryRunBoundaryNamesAnOutputFileItCannotMake(t *testing.T) {
	p, b, home, rel := boundaryWithPlugin(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	testutil.WriteFile(t, target, "keep\n")
	if err := os.Symlink(target, filepath.Join(b.Dir, "check.out")); err != nil {
		t.Fatal(err)
	}
	err := b.checkSecretPlugin(t.Context(), p, nil, home)
	want := "cannot make a file for the output of the check of the Secret plugin " + rel + ": "
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("err = %v, want a message that starts with %q", err, want)
	}
	if raw, err := os.ReadFile(target); err != nil || string(raw) != "keep\n" {
		t.Errorf("the check wrote through the link: %q (%v)", raw, err)
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

// fakeChartRenderer installs a "real" ChartRenderer in the project's plugin
// home: it records its environment, touches marker and renders one
// ConfigMap. The dry run's gate (this test binary, render.DispatchPlugin in
// TestMain) execs it when the chart may render.
func fakeChartRenderer(t *testing.T, marker string) string {
	t.Helper()
	path := filepath.Join(os.Getenv(KustomizePluginHomeEnv), filepath.FromSlash(toolchain.ChartRendererPluginRel))
	script := fmt.Sprintf("#!/bin/sh\ntouch '%s'\nprintf 'TMPDIR=%%s\\nHELM_PLUGINS=%%s\\n' \"$TMPDIR\" \"${HELM_PLUGINS:-}\" > '%s.env'\nprintf 'apiVersion: v1\\nkind: ConfigMap\\nmetadata:\\n  name: from-chart\\n'\n", marker, marker)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
	return path
}

// The ChartRenderer gate refuses a local chart whose dependencies khelm
// would build into the chart directory, for every shape a ChartRenderer
// config takes (a top-level chart.yaml, another file name, a nested base, an
// absolute chart path): khelm never runs, the chart directory is untouched,
// the entry is reported inconclusive. A vendored chart renders; with
// --allow-generators khelm runs as in a real run.
func TestDryRunChartGateRefusesUnvendoredDependencies(t *testing.T) {
	p, _, _ := execProject(t)
	marker := filepath.Join(t.TempDir(), "KHELM")
	fakeChartRenderer(t, marker)
	runner := execx.NewRunner(p)
	b, err := NewDryRunBoundary(t.Context(), p, runner) // after the ChartRenderer exists
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	chartDir := filepath.Join(p.Base, "charts", "app")
	testutil.WriteFile(t, filepath.Join(chartDir, "Chart.yaml"), "apiVersion: v2\nname: app\nversion: 0.1.0\ndependencies:\n  - name: redis\n    version: 1.0.0\n    repository: https://charts.example.test\n")
	renderer := func(chart string) string {
		return "apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: app\nchart: " + chart + "\n"
	}
	shapes := map[string]map[string]string{
		"chart.yaml": {"kustomization.yaml": "generators:\n- chart.yaml\n", "chart.yaml": renderer("../../charts/app")},
		"helm.yaml":  {"kustomization.yaml": "generators:\n- helm.yaml\n", "helm.yaml": renderer("../../charts/app")},
		"absolute":   {"kustomization.yaml": "generators:\n- chart.yaml\n", "chart.yaml": renderer(chartDir)},
		"nested":     {"kustomization.yaml": "resources:\n- base\n", "base/kustomization.yaml": "generators:\n- chart.yaml\n", "base/chart.yaml": renderer("../../../charts/app")},
	}
	before := hashDir(t, chartDir)
	for name, files := range shapes {
		dir := fixtureAddon(t, p.Base, "gate-"+strings.ReplaceAll(name, ".", "-"), files)
		if err := b.ResetReport(); err != nil {
			t.Fatal(err)
		}
		_, stderr, err := renderBoth(t, p, runner, b, dir, false)
		if err == nil {
			t.Errorf("%s: the dry render of an unvendored chart succeeded", name)
		}
		if fsutil.FileExists(marker) {
			t.Fatalf("%s: khelm ran on an unvendored chart", name)
		}
		if got := b.ReadReport().UnvendoredCharts; len(got) != 1 {
			t.Errorf("%s: report = %q, want the refused chart\n%s", name, got, stderr)
		}
		if !strings.Contains(stderr, ": dependency redis: missing (helm dependency list). khelm may build the dependencies into the chart directory. Vendor them into charts/, or pass --allow-generators.") {
			t.Errorf("%s: stderr:\n%s", name, stderr)
		}
	}
	if hashDir(t, chartDir) != before {
		t.Error("the chart directory changed")
	}

	dir := fixtureAddon(t, p.Base, "gate-allow", shapes["chart.yaml"])
	if _, _, err := renderBoth(t, p, runner, b, dir, true); err != nil || !fsutil.FileExists(marker) {
		t.Fatalf("--allow-generators did not run khelm (fixture dead?): %v", err)
	}
	_ = os.Remove(marker)

	// A real chart archive that .helmignore hides: Helm's loader does not
	// see it, khelm would build — refused.
	testutil.WriteFile(t, filepath.Join(chartDir, ".helmignore"), "*.tgz\n")
	writeChartArchive(t, filepath.Join(chartDir, "charts", "redis-1.0.0.tgz"), "redis", "1.0.0")
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := renderBoth(t, p, runner, b, dir, false); err == nil || fsutil.FileExists(marker) {
		t.Fatalf("a .helmignore'd archive passed the gate (err %v)\n%s", err, stderr)
	}
	if hashDir(t, chartDir) == before {
		t.Fatal("the fixture did not change the chart directory")
	}
	// The same archive, not ignored: vendored, the chart renders.
	if err := os.Remove(filepath.Join(chartDir, ".helmignore")); err != nil {
		t.Fatal(err)
	}
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err != nil || !strings.Contains(out, "from-chart") {
		t.Fatalf("a vendored chart did not render: %v\n%s", err, stderr)
	}
	env, _ := os.ReadFile(marker + ".env")
	if !strings.Contains(string(env), "TMPDIR="+b.Tmp+"\n") || !strings.Contains(string(env), "HELM_PLUGINS="+b.HelmPlugins+"\n") {
		t.Errorf("khelm's environment in a dry run:\n%s", env)
	}
	if ents, _ := os.ReadDir(b.HelmPlugins); len(ents) != 0 {
		t.Errorf("the dry run's helm plugin directory is not empty: %v", ents)
	}
}

// kustomize's own helmCharts pulls a chart that chartHome lacks into the
// project (`helm pull --untar`): the dry run's helm refuses the pull; a
// chart already vendored in chartHome renders through the real helm.
func TestDryRunRefusesAHelmChartsPull(t *testing.T) {
	p, runner, b := execProject(t)
	marker := filepath.Join(t.TempDir(), "PULLED")
	spyBin := t.TempDir()
	spy := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n  version) echo v3.14.0 ;;\n  pull) touch '%s' ; exit 1 ;;\n  template) printf 'apiVersion: v1\\nkind: ConfigMap\\nmetadata:\\n  name: from-helm\\n' ;;\nesac\n", marker)
	if err := os.WriteFile(filepath.Join(spyBin, "helm"), []byte(spy), 0o755); err != nil { // #nosec G306 -- a test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", spyBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := fixtureAddon(t, p.Base, "helmcharts", map[string]string{
		"kustomization.yaml": "helmCharts:\n- name: c\n  repo: https://charts.example.test\n  version: 1.0.0\n",
	})
	_, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err == nil || !strings.Contains(stderr, helmRefusal) {
		t.Fatalf("err=%v, want the helm pull refusal:\n%s", err, stderr)
	}
	if fsutil.FileExists(marker) || fsutil.DirExists(filepath.Join(dir, "charts")) {
		t.Fatal("a dry run pulled the chart")
	}
	if _, _, _ = renderBoth(t, p, runner, b, dir, true); !fsutil.FileExists(marker) {
		t.Fatal("--allow-generators did not pull (fixture dead?)")
	}
	_ = os.Remove(marker)
	// kustomize's chartHome for a versioned chart: charts/<name>-<version>/<name>.
	testutil.WriteFile(t, filepath.Join(dir, "charts", "c-1.0.0", "c", "Chart.yaml"), "apiVersion: v2\nname: c\nversion: 1.0.0\n")
	testutil.WriteFile(t, filepath.Join(dir, "charts", "c-1.0.0", "c", "values.yaml"), "{}\n")
	out, stderr, err := renderBoth(t, p, runner, b, dir, false)
	if err != nil || !strings.Contains(out, "from-helm") || fsutil.FileExists(marker) {
		t.Fatalf("a vendored helmCharts chart did not render through the real helm: %v\n%s\n%s", err, out, stderr)
	}
}

// kustomize writes each legacy plugin's config (literal secret values
// included) to a temp file and removes it only on success: a dry run's
// render writes it to the boundary, never the ambient TMPDIR.
func TestDryRunKeepsPluginConfigsInTheBoundary(t *testing.T) {
	p, runner, b := execProject(t)
	ambient := t.TempDir()
	t.Setenv("TMPDIR", ambient)
	t.Setenv("PATH_SECRETS", filepath.Join(p.Base, "store"))
	dir := fixtureAddon(t, p.Base, "fails", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: s\n  namespace: ns\nliterals:\n  TOKEN: literal-s3cret\nbash:\n  X:\n    exec: echo x\n",
	})
	if _, _, err := renderBoth(t, p, runner, b, dir, false); err == nil {
		t.Fatal("the bash: generator was not refused")
	}
	if ents, _ := os.ReadDir(ambient); len(ents) != 0 {
		names := []string{}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("the render left files in the ambient TMPDIR: %v", names)
	}
}

// Review round 5, item 5: an entry whose render reads a value another
// entry of the run generated (a secretRef to it, through the run store) is
// not repeatable either: a real run reads the value that run writes, with
// other bytes. It is reported (`reuse <name>`). A value the store holds is
// read without a report.
func TestDryRunReadingAnotherEntrysValueIsReported(t *testing.T) {
	p, runner, b := execProject(t)
	store := filepath.Join(p.Base, "clusters", "x.dev", "secrets")
	t.Setenv("PATH_SECRETS", store)
	producer := fixtureAddon(t, p.Base, "producer", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: ns\npasswd:\n  PASSWORD: 24\n",
	})
	consumer := fixtureAddon(t, p.Base, "consumer", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: client\n  namespace: ns\nsecretRef:\n  DB_PASSWORD:\n    secret: app\n    key: PASSWORD\n",
	})
	for _, dir := range []string{producer, consumer} {
		if err := b.ResetReport(); err != nil {
			t.Fatal(err)
		}
		if _, stderr, err := renderBoth(t, p, runner, b, dir, false); err != nil {
			t.Fatalf("%s: %v\n%s", filepath.Base(dir), err, stderr)
		}
	}
	if got := b.ReadReport().Generated; len(got) != 1 || got[0] != "reuse Secret.app.ns.PASSWORD" {
		t.Errorf("the consumer's report = %q, want the reuse of the producer's value", got)
	}
	if fsutil.DirExists(store) {
		t.Fatal("a dry run created the secrets store")
	}

	// A new run whose store holds the value: the consumer reads it from
	// the store, nothing is reported.
	testutil.WriteFile(t, filepath.Join(store, "Secret.app.ns.PASSWORD"), "from-the-store")
	b2, err := NewDryRunBoundary(t.Context(), p, runner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b2.Close)
	if _, stderr, err := renderBoth(t, p, runner, b2, consumer, false); err != nil {
		t.Fatalf("consumer, second run: %v\n%s", err, stderr)
	}
	if got := b2.ReadReport().Generated; len(got) != 0 {
		t.Errorf("a read from the store was reported: %q", got)
	}
}

// writeChartArchive writes a real chart archive (a gzipped tar with one
// top-level chart directory, as `helm package` writes it).
func writeChartArchive(t *testing.T, dest, name, version string) {
	t.Helper()
	files := map[string]string{
		name + "/Chart.yaml":        "apiVersion: v2\nname: " + name + "\nversion: " + version + "\n",
		name + "/templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n",
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, n := range []string{name + "/Chart.yaml", name + "/templates/cm.yaml"} {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, dest, buf.String())
}
