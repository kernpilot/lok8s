package kubehz

// testutil_test.go — the hermetic harness: a fake execx.Runner (records
// every Cmd, scripted per-call behavior), an httptest TLS server standing in
// for the platform api AND the Hetzner Cloud api (the https-only gates run
// for real against its https://127.0.0.1 URL), and a project layout under
// t.TempDir. NOTHING here reaches a real api, a cluster, kubectl, kubeadm or
// docker — the Runner and the *http.Client are the only I/O seams.

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
)

type fakeRunner struct {
	mu      sync.Mutex
	calls   []execx.Cmd
	stdins  []string
	handler func(c execx.Cmd, stdin string) error
}

func (r *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	stdin := ""
	if c.Stdin != nil {
		var b bytes.Buffer
		_, _ = b.ReadFrom(c.Stdin)
		stdin = b.String()
	}
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.stdins = append(r.stdins, stdin)
	r.mu.Unlock()
	if r.handler != nil {
		return r.handler(c, stdin)
	}
	return nil
}

func argvLine(c execx.Cmd) string { return c.Name + " " + strings.Join(c.Args, " ") }

func (r *fakeRunner) lines() []string {
	var out []string
	for _, c := range r.calls {
		out = append(out, argvLine(c))
	}
	return out
}

func (r *fakeRunner) anyCall(substr string) bool {
	for _, l := range r.lines() {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (r *fakeRunner) countCalls(substr string) int {
	n := 0
	for _, l := range r.lines() {
		if strings.Contains(l, substr) {
			n++
		}
	}
	return n
}

// recordedReq is one api call the fake server saw.
type recordedReq struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   string
}

type harness struct {
	t      *testing.T
	ctx    *Context
	runner *fakeRunner
	out    bytes.Buffer
	errOut bytes.Buffer
	env    map[string]string
	base   string

	srv      *httptest.Server
	routes   map[string]http.HandlerFunc // "METHOD /path" → handler; re-registrable
	mu       sync.Mutex
	requests []recordedReq
}

// newHarness builds a Context over a temp project and a TLS api server.
// Every route is unmocked until h.handle registers it (an unmocked route
// answers 500 {"ok":false,"data":{"code":"UNMOCKED"}}).
func newHarness(t *testing.T) *harness {
	t.Helper()
	base := t.TempDir()
	h := &harness{t: t, base: base, runner: &fakeRunner{}, routes: map[string]http.HandlerFunc{}}
	h.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.requests = append(h.requests, recordedReq{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), Body: string(body),
		})
		h.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.mu.Lock()
		fn, ok := h.routes[r.Method+" "+r.URL.Path]
		h.mu.Unlock()
		if !ok {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"ok":false,"data":{"code":"UNMOCKED"}}`)
			return
		}
		fn(w, r)
	}))
	t.Cleanup(h.srv.Close)

	h.env = map[string]string{"KUBEHZ_TOKEN": "khz_test_token"}
	h.ctx = &Context{
		Paths: &config.Paths{
			Base: base, Bin: filepath.Join(base, ".bin"),
			Lok8s: filepath.Join(base, ".lok8s"), Clusters: filepath.Join(base, "clusters"),
		},
		Runner:   h.runner,
		Out:      &h.out,
		ErrOut:   &h.errOut,
		HTTP:     h.srv.Client(),
		Env:      h.env,
		Sleep:    func(time.Duration) {},
		Now:      func() time.Time { return time.Unix(1700000000, 0) },
		Hostname: func() (string, error) { return "BOX-1.lan", nil },
		IsRoot:   func() bool { return false },
		LookPath: func(string) bool { return false },
	}
	return h
}

// apiURL is the fake server's https origin.
func (h *harness) apiURL() string { return h.srv.URL }

// handle registers one route: "METHOD /path" → a status + body.
func (h *harness) handle(route string, status int, body string) {
	h.handleFunc(route, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
}

// handleFunc registers (or REPLACES) one "METHOD /path" route.
func (h *harness) handleFunc(route string, fn http.HandlerFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.routes[route] = fn
}

// output is the bats `${output}`: stdout + stderr interleaved by stream
// (stdout first, then stderr — assertions are substring-based).
func (h *harness) output() string { return h.out.String() + h.errOut.String() }

func (h *harness) reset() {
	h.out.Reset()
	h.errOut.Reset()
	h.runner.calls = nil
	h.runner.stdins = nil
	h.mu.Lock()
	h.requests = nil
	h.mu.Unlock()
}

func (h *harness) reqs() []recordedReq {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedReq{}, h.requests...)
}

func (h *harness) anyReq(method, pathSubstr string) bool {
	for _, r := range h.reqs() {
		if r.Method == method && strings.Contains(r.Path, pathSubstr) {
			return true
		}
	}
	return false
}

func (h *harness) lastReq(method, pathSubstr string) *recordedReq {
	rs := h.reqs()
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].Method == method && (rs[i].Path == pathSubstr || strings.HasSuffix(rs[i].Path, pathSubstr)) {
			return &rs[i]
		}
	}
	return nil
}

// writeSpec installs a cluster.lok8s.yaml for a domain.
func (h *harness) writeSpec(domain, yaml string) string {
	h.t.Helper()
	dir := filepath.Join(h.ctx.Paths.Clusters, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	p := filepath.Join(dir, "cluster.lok8s.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// specYAML renders a self/hosted/shared spec with the given kubehz block.
func specYAML(kind, kubehzBlock string) string {
	return "kind: " + kind + "\nmetadata:\n  name: test\nspec:\n  kubehz:\n" + kubehzBlock
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("output lacks %q:\n%s", want, got)
	}
}

func mustNotContain(t *testing.T, got, want string) {
	t.Helper()
	if strings.Contains(got, want) {
		t.Fatalf("output must not contain %q:\n%s", want, got)
	}
}

func mustErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
}

func mustOK(t *testing.T, err error, out string) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error %v:\n%s", err, out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// repoRoot locates the repository root (internal/kubehz → ../../).
func repoRoot(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// exitErr is a fake subprocess failure (bash: `return N` from a stubbed
// binary).
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
