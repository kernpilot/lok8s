package bridge

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

type fakeRunner struct {
	calls   []execx.Cmd
	handler func(c execx.Cmd) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	r.calls = append(r.calls, c)
	if r.handler != nil {
		return r.handler(c)
	}
	return nil
}

// testLoader is a Loader over a project that holds the bash tree
// (.lok8s/lo), so the children's PATH_LOK8S is the project's own tree
// (precedence: a checkout wins over the cache).
func testLoader(t *testing.T) (*Loader, *fakeRunner, *strings.Builder) {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	testutil.WriteFile(t, filepath.Join(p.Lok8s, "lo"), "#!/usr/bin/env bash\n")
	r := &fakeRunner{}
	var errBuf strings.Builder
	return &Loader{Paths: p, Runner: r, Stdout: io.Discard, Stderr: &errBuf}, r, &errBuf
}

func hasEnv(c execx.Cmd, kv string) bool {
	return slices.Contains(c.Env, kv)
}

func TestLoadProbesThenCallsThroughBash(t *testing.T) {
	l, r, _ := testLoader(t)
	prov, err := l.Load(t.Context(), "hetzner")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || r.calls[0].Name != "bash" || r.calls[0].Args[2] != "lo-provider" || r.calls[0].Args[3] != "hetzner" {
		t.Fatalf("probe call = %+v", r.calls)
	}
	if !strings.Contains(r.calls[0].Args[1], "provider::load") {
		t.Errorf("probe script does not load the provider: %q", r.calls[0].Args[1])
	}
	for _, kv := range []string{"PATH_BASE=" + l.Paths.Base, "PATH_LOK8S=" + l.Paths.Lok8s, "PATH_SCRIPTS=" + l.Paths.Lok8s, "PROVIDER_NAME=hetzner"} {
		if !hasEnv(r.calls[0], kv) {
			t.Errorf("probe env missing %s: %v", kv, r.calls[0].Env)
		}
	}

	if err := prov.Provision(t.Context(), "/cfg.yaml", "/work"); err != nil {
		t.Fatal(err)
	}
	c := r.calls[1]
	want := []string{"lo-provider", "hetzner", "provider::provision", "/cfg.yaml", "/work"}
	if got := c.Args[2:]; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("provision argv = %v, want %v", got, want)
	}
	if c.Dir != l.Paths.Base {
		t.Errorf("Dir = %q", c.Dir)
	}
}

func TestLoadFailsWhenProbeFails(t *testing.T) {
	l, r, _ := testLoader(t)
	r.handler = func(c execx.Cmd) error { return errors.New("exit status 1") }
	if _, err := l.Load(t.Context(), "nosuch"); err == nil {
		t.Fatal("expected the probe failure to surface")
	}
	if _, err := l.Load(t.Context(), "../evil"); err == nil {
		t.Fatal("expected the name allowlist to refuse a traversal")
	}
}

func TestCredentialDataAndOutputCaptureStdout(t *testing.T) {
	l, r, _ := testLoader(t)
	r.handler = func(c execx.Cmd) error {
		switch c.Args[4] {
		case "provider::credential_data":
			io.WriteString(c.Stdout, "hcloud-token=tok\nrobot-user=u\n\nbogus\n")
		case "provider::output":
			io.WriteString(c.Stdout, `{"nodes":[]}`)
			io.WriteString(c.Stderr, "noise")
		case "provider::status":
			io.WriteString(c.Stdout, "Running\n")
		}
		return nil
	}
	p := &Provider{l: l, name: "hetzner"}
	creds, err := p.CredentialData(t.Context(), "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if creds["hcloud-token"] != "tok" || creds["robot-user"] != "u" || len(creds) != 2 {
		t.Errorf("creds = %v", creds)
	}
	out, err := p.Output(t.Context(), "/cfg")
	if err != nil || string(out) != `{"nodes":[]}` {
		t.Errorf("output = %q err=%v", out, err)
	}
	st, err := p.ProviderStatus(t.Context(), "/cfg")
	if err != nil || st != "Running" {
		t.Errorf("status = %q err=%v", st, err)
	}
	var _ driver.ProviderStatuser = p
}

func TestKubeoneSeamsBindTheProviderAtConstruction(t *testing.T) {
	l, r, _ := testLoader(t)
	// No provider in Deps when the driver is built: the inventory seam
	// reports it, whatever happens to Deps later (the dispatch completes
	// Deps BEFORE it builds the driver, so nothing does).
	bare := &driver.Deps{Paths: l.Paths}
	noProv := l.KubeoneAppendInventory(bare)
	bare.Provider, bare.ProviderName = &Provider{l: l, name: "hetzner"}, "hetzner"
	if err := noProv(t.Context(), "/cfg", "/work/kubeone.yaml"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider for a driver built without a provider, got %v", err)
	}
	deps := &driver.Deps{Paths: l.Paths, Provider: &Provider{l: l, name: "hetzner"}, ProviderName: "hetzner"}
	appendInv := l.KubeoneAppendInventory(deps)
	if err := appendInv(t.Context(), "/cfg", "/work/kubeone.yaml"); err != nil {
		t.Fatal(err)
	}
	c := r.calls[len(r.calls)-1]
	if got := strings.Join(c.Args[2:], " "); got != "lo-kubeone hetzner _append_inventory /cfg /work/kubeone.yaml" {
		t.Errorf("append argv = %q", got)
	}
	if !strings.Contains(c.Args[1], `source "${PATH_LOK8S}/drivers/kubeone/main"`) {
		t.Errorf("append script does not source the bash driver")
	}

	prep := l.KubeonePrepareApply(deps)
	if err := prep(t.Context(), "/work", "/spec.yaml"); err != nil {
		t.Fatal(err)
	}
	c = r.calls[len(r.calls)-1]
	if got := strings.Join(c.Args[2:], " "); got != "lo-kubeone hetzner /work /spec.yaml" {
		t.Errorf("prepare argv = %q", got)
	}
	script := c.Args[1]
	for _, want := range []string{`kubeone::_clean_reinstalled_workers "${2}" || true`, `kubeone::_name_robot_workers "${2}/kubeone.yaml"`, `kubeone::render_addons "${2}" "${3}"`} {
		if !strings.Contains(script, want) {
			t.Errorf("prepare script missing %q", want)
		}
	}
}

// Without a checkout the children source the embedded tree, extracted
// into the versioned cache: PATH_LOK8S/PATH_SCRIPTS point there, the
// project paths stay the project's, and the project gets no .lok8s.
func TestLoaderResolvesTheCacheWithoutACheckout(t *testing.T) {
	cacheRoot := t.TempDir()
	t.Setenv(assets.EnvCacheHome, cacheRoot)
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	r := &fakeRunner{}
	l := &Loader{Paths: p, Runner: r, Stdout: io.Discard, Stderr: io.Discard}
	if _, err := l.Load(t.Context(), "hetzner"); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(cacheRoot, "lok8s", assets.Version(), "lok8s")
	for _, kv := range []string{"PATH_LOK8S=" + tree, "PATH_SCRIPTS=" + tree, "PATH_BASE=" + base, "PATH_BIN=" + p.Bin} {
		if !hasEnv(r.calls[0], kv) {
			t.Errorf("env missing %s: %v", kv, r.calls[0].Env)
		}
	}
	if _, err := os.Stat(filepath.Join(tree, "providers", "hetzner", "main")); err != nil {
		t.Fatalf("cache tree not extracted: %v", err)
	}
	if _, err := os.Stat(p.Lok8s); err == nil {
		t.Fatal("the bridge wrote into the project")
	}
}

func TestPATHPrependsProjectDirsOnce(t *testing.T) {
	p := &config.Paths{Base: "/p", Bin: "/p/.bin", Lok8s: "/p/.lok8s"}
	t.Setenv("PATH", "/usr/bin:/p/.bin")
	got := PathEnv(p, p.Lok8s)
	if !strings.HasPrefix(got, "/p/.lok8s"+string(os.PathListSeparator)) || strings.Count(got, "/p/.bin") != 1 {
		t.Errorf("PATH = %q", got)
	}
}

// The bash tree's kustomize runs need the plugin home the shim gives the
// binary's own kustomize children; a clean shell has no such variable.
func TestEnvCarriesKustomizePluginHome(t *testing.T) {
	p := &config.Paths{Base: "/proj", Bin: "/proj/.bin", Clusters: "/proj/clusters"}
	has := func(want string) bool {
		for _, kv := range Env(p, "/tree") {
			if kv == want {
				return true
			}
		}
		return false
	}
	t.Setenv(config.KustomizePluginHomeEnv, "")
	if !has("KUSTOMIZE_PLUGIN_HOME=/proj/.kustomize") {
		t.Fatalf("Env lacks the project's plugin home: %v", Env(p, "/tree"))
	}
	// An exported value wins, as it does for the shim.
	t.Setenv(config.KustomizePluginHomeEnv, "/opt/plugins")
	if !has("KUSTOMIZE_PLUGIN_HOME=/opt/plugins") {
		t.Fatalf("Env ignores the exported plugin home: %v", Env(p, "/tree"))
	}
}
