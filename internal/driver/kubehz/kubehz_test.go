package kubehz

// kubehz_test.go — the space driver's contract: ensure_shared_config's
// guards, the rc-100 full-lifecycle sentinel, status rendering, and the
// no-kubeconfig refusal. The platform api is an httptest TLS server.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	platform "github.com/kernpilot/lok8s/internal/kubehz"
)

type nopRunner struct{}

func (nopRunner) Run(context.Context, execx.Cmd) error { return nil }

type fixture struct {
	d      *Driver
	out    bytes.Buffer
	stderr bytes.Buffer
	srv    *httptest.Server
	mux    *http.ServeMux
	base   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{base: t.TempDir(), mux: http.NewServeMux()}
	f.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	f.srv = httptest.NewTLSServer(f.mux)
	t.Cleanup(f.srv.Close)
	paths := &config.Paths{Base: f.base, Clusters: filepath.Join(f.base, "clusters")}
	f.d = New(&driver.Deps{Paths: paths, Runner: nopRunner{}, Stderr: &f.stderr})
	f.d.Out = &f.out
	f.d.Lib = &platform.Context{
		Paths: paths, Runner: nopRunner{}, ErrOut: &f.stderr, HTTP: f.srv.Client(),
		Env: map[string]string{"KUBEHZ_TOKEN": "khz_test"}, Sleep: func(time.Duration) {},
	}
	return f
}

func (f *fixture) spec(domain, yaml string) {
	dir := filepath.Join(f.base, "clusters", domain)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "cluster.lok8s.yaml"), []byte(yaml), 0o644)
}

func (f *fixture) sharedSpec(domain string) {
	f.spec(domain, "kind: Kubehz\nspec:\n  kubehz:\n    hosting: shared\n    apiUrl: "+f.srv.URL+"\n")
}

func (f *fixture) handle(route string, status int, body string) {
	f.mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

func TestDriverIsRegistered(t *testing.T) {
	if _, ok := driver.Get(Name); !ok {
		t.Fatal("kubehz driver not registered")
	}
}

func TestEnsureSharedConfigGuards(t *testing.T) {
	f := newFixture(t)
	if err := f.d.Destroy(context.Background(), "nowhere.dev"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(f.stderr.String(), "No cluster.lok8s.yaml for domain: nowhere.dev") {
		t.Fatalf("stderr: %s", f.stderr.String())
	}

	f = newFixture(t)
	f.spec("broken.dev", "{{ not yaml")
	if err := f.d.Destroy(context.Background(), "broken.dev"); err == nil {
		t.Fatal("expected error")
	}
	// The guard's whole point: the READ error, not the empty-var misdiagnosis.
	if strings.Contains(f.stderr.String(), "invalid spec.kubehz.hosting:") {
		t.Fatalf("misdiagnosed: %s", f.stderr.String())
	}

	f = newFixture(t)
	f.spec("self.dev", "kind: Kubehz\nspec:\n  kubehz:\n    hosting: self\n")
	if err := f.d.Destroy(context.Background(), "self.dev"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(f.stderr.String(), "kind: Kubehz requires spec.kubehz.hosting: shared (got: self)") {
		t.Fatalf("stderr: %s", f.stderr.String())
	}
}

func TestProvisionReturnsFullLifecycle(t *testing.T) {
	f := newFixture(t)
	f.sharedSpec("acme.example.org")
	f.handle("GET /api/spaces", 200, `{"data":[{"id":"sp-1","slug":"acme","status":"Active"}]}`)
	f.handle("GET /api/spaces/sp-1", 200, `{"data":{"status":"Active"}}`)
	err := f.d.Provision(context.Background(), "acme.example.org")
	if !errors.Is(err, driver.ErrFullLifecycle) {
		t.Fatalf("err = %v (stderr %s)", err, f.stderr.String())
	}
	if driver.ExitCode(err) != 100 {
		t.Fatal("rc 100")
	}
	if !strings.Contains(f.out.String(), "Space 'acme' is Active (id: sp-1)") {
		t.Fatalf("stdout: %s", f.out.String())
	}
}

func TestDestroyAndStatus(t *testing.T) {
	f := newFixture(t)
	f.sharedSpec("acme.example.org")
	f.handle("GET /api/spaces", 200, `{"data":[{"id":"sp-9","slug":"acme","status":"Active","planId":"shared-free"}]}`)
	f.handle("DELETE /api/spaces/sp-9", 200, `{}`)
	f.handle("GET /api/spaces/sp-9/nodes", 200, `{"data":{"nodes":[{"name":"worker-1","status":"Ready","lane":"hcloud"}],"usage":{"nodes":1,"maxNodes":2}}}`)
	if err := f.d.Destroy(context.Background(), "acme.example.org"); err != nil {
		t.Fatalf("%v: %s", err, f.stderr.String())
	}
	if !strings.Contains(f.out.String(), "Space 'acme' removed (id: sp-9)") {
		t.Fatalf("stdout: %s", f.out.String())
	}
	status, err := f.d.Status(context.Background(), "acme.example.org")
	if err != nil {
		t.Fatal(err)
	}
	want := "Space:   acme (id: sp-9)\nPhase:   Active\nPlan:    shared-free\nNodes:   1/2\n  worker-1  Ready  hcloud"
	if status != want {
		t.Fatalf("status:\n%s\nwant:\n%s", status, want)
	}
}

func TestKubeconfigRefuses(t *testing.T) {
	f := newFixture(t)
	if _, err := f.d.Kubeconfig(context.Background(), "acme.example.org"); err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(f.stderr.String(), "A space has no downloadable kubeconfig") ||
		!strings.Contains(f.stderr.String(), "kubectl oidc-login") {
		t.Fatalf("stderr: %s", f.stderr.String())
	}
}
