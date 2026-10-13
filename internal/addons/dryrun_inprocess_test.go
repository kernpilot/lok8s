//go:build inprocess

package addons

// dryrun_inprocess_test.go — the dry-run boundary on lo-full's default,
// in-process render: kustomize runs inside this binary, its plugin home is
// the self-exec home (lok8s' two built-ins only, served by this test binary
// through render.DispatchPlugin in TestMain), and a KRM container function
// or a remote base resolves docker/git through this process's PATH.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/render"
)

func inProcessProject(t *testing.T) (*config.Paths, *DryRunBoundary) {
	t.Helper()
	t.Setenv(render.ModeEnv, "")
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	b, err := NewDryRunBoundary(t.Context(), p, nil)
	if err != nil {
		t.Fatalf("NewDryRunBoundary: %v", err)
	}
	t.Cleanup(b.Close)
	return p, b
}

func renderInProcess(t *testing.T, p *config.Paths, b *DryRunBoundary, dir string, allow bool) (string, string, error) {
	t.Helper()
	var errBuf strings.Builder
	out, err := RenderDryRun(t.Context(), p, nil, &errBuf, dir, "", "", "", nil, b, allow)
	return out, errBuf.String(), err
}

func TestDryRunInProcessNeverRunsAKRMExecFunction(t *testing.T) {
	p, b := inProcessProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	fn := filepath.Join(t.TempDir(), "fn.sh")
	spyScript(t, fn, marker, "apiVersion: config.kubernetes.io/v1\nkind: ResourceList\nitems:\n- apiVersion: v1\n  kind: ConfigMap\n  metadata:\n    name: from-fn\n", 0)
	dir := fixtureAddon(t, p.Base, "krm", map[string]string{
		"kustomization.yaml": "resources:\n- cm.yaml\ngenerators:\n- fn.yaml\n",
		"cm.yaml":            plainCM,
		"fn.yaml":            "apiVersion: example.com/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      exec:\n        path: " + fn + "\n",
	})
	out, stderr, err := renderInProcess(t, p, b, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if fsutil.FileExists(marker) || strings.Contains(out, "from-fn") {
		t.Fatalf("a dry run executed a KRM exec function:\n%s", out)
	}
	if out, _, err = renderInProcess(t, p, b, dir, true); err != nil || !fsutil.FileExists(marker) || !strings.Contains(out, "from-fn") {
		t.Fatalf("--allow-generators did not run the function (fixture dead?): %v\n%s", err, out)
	}
}

func TestDryRunInProcessRefusesAContainerFunction(t *testing.T) {
	p, b := inProcessProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	spyBin := t.TempDir()
	spyScript(t, filepath.Join(spyBin, "docker"), marker, "", 1)
	t.Setenv("PATH", spyBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := fixtureAddon(t, p.Base, "container", map[string]string{
		"kustomization.yaml": "generators:\n- fn.yaml\n",
		"fn.yaml":            "apiVersion: example.com/v1\nkind: Fn\nmetadata:\n  name: fn\n  annotations:\n    config.kubernetes.io/function: |\n      container:\n        image: example.com/fn:1\n",
	})
	if _, _, err := renderInProcess(t, p, b, dir, false); err == nil {
		t.Fatal("the dry render of a container function succeeded")
	}
	if fsutil.FileExists(marker) {
		t.Fatal("a dry run ran docker")
	}
	if got := os.Getenv("PATH"); strings.HasPrefix(got, b.StubBin) {
		t.Errorf("the stub PATH outlived the render: %s", got)
	}
	if _, _, _ = renderInProcess(t, p, b, dir, true); !fsutil.FileExists(marker) {
		t.Fatal("--allow-generators did not reach docker (fixture dead?)")
	}
}

// lo-full serves only the two built-ins from its self-exec plugin home, in
// a real run as in a dry run: a foreign legacy plugin never resolves.
func TestDryRunInProcessRefusesAForeignExecPlugin(t *testing.T) {
	p, b := inProcessProject(t)
	marker := filepath.Join(t.TempDir(), "EXECUTED")
	home := filepath.Join(p.Base, ".kustomize")
	t.Setenv(KustomizePluginHomeEnv, home)
	spyScript(t, filepath.Join(home, "example.com", "v1", "evil", "Evil"), marker, plainCM, 0)
	dir := fixtureAddon(t, p.Base, "foreign", map[string]string{
		"kustomization.yaml": "generators:\n- evil.yaml\n",
		"evil.yaml":          "apiVersion: example.com/v1\nkind: Evil\nmetadata:\n  name: evil\n",
	})
	if _, _, err := renderInProcess(t, p, b, dir, false); err == nil {
		t.Fatal("the dry render of a foreign exec plugin succeeded")
	}
	if fsutil.FileExists(marker) {
		t.Fatal("a dry run executed a foreign exec plugin")
	}
}

func TestDryRunInProcessSecretMintIsInMemoryAndReported(t *testing.T) {
	p, b := inProcessProject(t)
	store := filepath.Join(p.Base, "clusters", "x.dev", "secrets")
	t.Setenv("PATH_SECRETS", store)
	dir := fixtureAddon(t, p.Base, "app", map[string]string{
		"kustomization.yaml": "generators:\n- secret.yaml\n",
		"secret.yaml":        "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n  namespace: ns\npasswd:\n  PASSWORD: 24\n",
	})
	if err := b.ResetReport(); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := renderInProcess(t, p, b, dir, false)
	if err != nil {
		t.Fatalf("dry render: %v\n%s", err, stderr)
	}
	if !strings.Contains(out, "PASSWORD:") || fsutil.DirExists(store) {
		t.Fatalf("store created=%v, out:\n%s", fsutil.DirExists(store), out)
	}
	if got := b.Minted(); len(got) != 1 || got[0] != "secret Secret.app.ns.PASSWORD" {
		t.Errorf("report = %q", got)
	}
}
