package bridge

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
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

func testLoader(t *testing.T) (*Loader, *fakeRunner, *strings.Builder) {
	t.Helper()
	base := t.TempDir()
	p := &config.Paths{Base: base, Bin: filepath.Join(base, ".bin"), Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters")}
	r := &fakeRunner{}
	var errBuf strings.Builder
	return &Loader{Paths: p, Runner: r, Stdout: io.Discard, Stderr: &errBuf}, r, &errBuf
}

func hasEnv(c execx.Cmd, kv string) bool {
	for _, e := range c.Env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestLoadProbesThenCallsThroughBash(t *testing.T) {
	l, r, _ := testLoader(t)
	prov, err := l.Load("hetzner")
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

	if err := prov.Provision(context.Background(), "/cfg.yaml", "/work"); err != nil {
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
	if _, err := l.Load("nosuch"); err == nil {
		t.Fatal("expected the probe failure to surface")
	}
	if _, err := l.Load("../evil"); err == nil {
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
	creds, err := p.CredentialData(context.Background(), "/cfg")
	if err != nil {
		t.Fatal(err)
	}
	if creds["hcloud-token"] != "tok" || creds["robot-user"] != "u" || len(creds) != 2 {
		t.Errorf("creds = %v", creds)
	}
	out, err := p.Output(context.Background(), "/cfg")
	if err != nil || string(out) != `{"nodes":[]}` {
		t.Errorf("output = %q err=%v", out, err)
	}
	st, err := p.ProviderStatus(context.Background(), "/cfg")
	if err != nil || st != "Running" {
		t.Errorf("status = %q err=%v", st, err)
	}
	var _ driver.ProviderStatuser = p
}

func TestKubeoneSeamsReadTheProviderAtCallTime(t *testing.T) {
	l, r, _ := testLoader(t)
	deps := &driver.Deps{Paths: l.Paths}
	appendInv := l.KubeoneAppendInventory(deps)
	if err := appendInv(context.Background(), "/cfg", "/work/kubeone.yaml"); !errors.Is(err, ErrNoProvider) {
		t.Fatalf("expected ErrNoProvider before the dispatch loads a provider, got %v", err)
	}
	// The dispatch fills the provider AFTER the factory ran.
	deps.Provider, deps.ProviderName = &Provider{l: l, name: "hetzner"}, "hetzner"
	if err := appendInv(context.Background(), "/cfg", "/work/kubeone.yaml"); err != nil {
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
	if err := prep(context.Background(), "/work", "/spec.yaml"); err != nil {
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

func TestPATHPrependsProjectDirsOnce(t *testing.T) {
	p := &config.Paths{Base: "/p", Bin: "/p/.bin", Lok8s: "/p/.lok8s"}
	t.Setenv("PATH", "/usr/bin:/p/.bin")
	got := PATH(p)
	if !strings.HasPrefix(got, "/p/.lok8s"+string(os.PathListSeparator)) || strings.Count(got, "/p/.bin") != 1 {
		t.Errorf("PATH = %q", got)
	}
}
