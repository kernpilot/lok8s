package cli

// cmd_kubehz_agent_test.go — `lo kubehz space …` and `lo kubehz cluster …`
// through the real root: the -o formats as an agent reads them, the argsh
// parse errors, and the kubeconfig that reaches a file and never a stream.
// The api is an httptest TLS server (kubehzHTTP); the library tests in
// internal/kubehz/agent_test.go cover every refusal.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// agentAPI serves fixed api answers and counts the requests. KUBEHZ_TOKEN
// is the credential (no agent key in the environment).
func agentAPI(t *testing.T, routes map[string]string) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer khzt_cli" {
			w.WriteHeader(401)
			return
		}
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	old := kubehzHTTP
	kubehzHTTP = srv.Client()
	t.Cleanup(func() { kubehzHTTP = old })
	t.Setenv("KUBEHZ_API_URL", srv.URL)
	t.Setenv("KUBEHZ_TOKEN", "khzt_cli")
	for _, k := range []string{"KUBEHZ_AGENT_CLIENT_ID", "KUBEHZ_AGENT_CLIENT_SECRET", "KUBEHZ_AGENT_TOKEN_URL", "KUBEHZ_AGENT_SCOPE"} {
		t.Setenv(k, "")
	}
	return &calls
}

const (
	cliSpaces = `{"ok":true,"data":[{"id":"sp-1a2b3c4d","name":"Acme","slug":"acme","status":"Active","maxNodes":2,` +
		`"maxNamespaces":1,"maxObjectKiB":256,"nodeCount":1,"namespaces":["acme"],"leaseExpiresAt":"2026-10-04T14:00:00.000Z",` +
		`"createdAt":"2026-10-04T12:00:00.000Z"}],"meta":{"pagination":{"total":1}}}`
	cliCluster = `{"ok":true,"data":{"id":"cl-1a2b3c4d","domain":"agent.example.org","hosting":"hosted","status":"Running",` +
		`"region":"fsn1","kubernetesVersion":"v1.34.1","controlPlaneReplicas":1,"apiEndpoint":null,"health":"healthy",` +
		`"leaseExpiresAt":"2026-10-04T14:00:00.000Z","createdAt":"2026-10-04T12:00:00.000Z"}}`
)

func TestKubehzAgentOutputFormats(t *testing.T) {
	agentAPI(t, map[string]string{"GET /api/spaces": cliSpaces, "GET /api/clusters/cl-1a2b3c4d": cliCluster})
	base := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"kubehz", "space", "list"},
			"ID           SLUG  NAME  STATUS  NODES  LEASE ENDS\n" +
				"sp-1a2b3c4d  acme  Acme  Active  1/2    2026-10-04T14:00:00.000Z\n"},
		{[]string{"kubehz", "space", "list", "-o", "json"}, `{
  "spaces": [
    {
      "id": "sp-1a2b3c4d",
      "name": "Acme",
      "slug": "acme",
      "status": "Active",
      "maxNodes": 2,
      "maxNamespaces": 1,
      "maxObjectKiB": 256,
      "nodeCount": 1,
      "namespaces": [
        "acme"
      ],
      "leaseExpiresAt": "2026-10-04T14:00:00.000Z",
      "createdAt": "2026-10-04T12:00:00.000Z"
    }
  ]
}
`},
		{[]string{"kubehz", "cluster", "get", "cl-1a2b3c4d", "--output", "yaml"}, `id: cl-1a2b3c4d
domain: agent.example.org
hosting: hosted
status: Running
region: fsn1
kubernetesVersion: v1.34.1
controlPlaneReplicas: 1
apiEndpoint: null
health: healthy
leaseExpiresAt: "2026-10-04T14:00:00.000Z"
createdAt: "2026-10-04T12:00:00.000Z"
`},
	} {
		stdout, stderr, err := runKubehzLo(t, base, tc.args...)
		if err != nil || stdout != tc.want || stderr != "" {
			t.Errorf("lo %s: err=%v stderr=%q\nstdout:\n%s\nwant:\n%s", strings.Join(tc.args, " "), err, stderr, stdout, tc.want)
		}
	}
}

// The argsh parse errors come first, and no parse error reaches the api.
func TestKubehzAgentParseErrors(t *testing.T) {
	calls := agentAPI(t, nil)
	base := t.TempDir()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"kubehz", "space", "get"}, "Error: missing required argument: id"},
		{[]string{"kubehz", "space", "get", "sp-1", "sp-2"}, "Error: too many arguments: sp-2"},
		{[]string{"kubehz", "cluster", "list", "x"}, "Error: too many arguments: x"},
		{[]string{"kubehz", "space", "create", "--slug", "acme"}, "Error: missing required flag: name"},
		{[]string{"kubehz", "space", "create", "--name", "Acme"}, "Error: missing required flag: slug"},
		{[]string{"kubehz", "space", "lease", "sp-1a2b3c4d"}, "Error: missing required flag: hours"},
		{[]string{"kubehz", "cluster", "kubeconfig", "cl-1a2b3c4d"}, "Error: missing required flag: file"},
		{[]string{"kubehz", "space", "list", "-o", "xml"}, `Error: invalid --output "xml": text, json or yaml`},
		// The required flag is checked before the output format.
		{[]string{"kubehz", "space", "lease", "sp-1a2b3c4d", "-o", "xml"}, "Error: missing required flag: hours"},
		{[]string{"kubehz", "space", "bogus"}, "Error: Invalid command: bogus"},
	} {
		_, stderr, err := runKubehzLo(t, base, tc.args...)
		if !errors.Is(err, ErrHandled) || !strings.HasPrefix(stderr, tc.want+"\n") {
			t.Errorf("lo %s: err=%v stderr=%q, want %q", strings.Join(tc.args, " "), err, stderr, tc.want)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d requests reached the api", n)
	}
}

// The agent kubeconfig reaches the file, and stdout carries the path only:
// a tool result never holds the kubeconfig.
func TestKubehzAgentKubeconfigGoesToTheFileOnly(t *testing.T) {
	const kc = "apiVersion: v1\nkind: Config\ncurrent-context: kubehz-acme\n"
	agentAPI(t, map[string]string{"GET /api/spaces/sp-1a2b3c4d/kubeconfig/agent": kc})
	base := t.TempDir()
	file := filepath.Join(base, "agent.yaml")
	stdout, stderr, err := runKubehzLo(t, base, "kubehz", "space", "kubeconfig", "sp-1a2b3c4d", "--file", file)
	if err != nil || stdout != file+"\n" || stderr != "" {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
	raw, err := os.ReadFile(file)
	if err != nil || string(raw) != kc {
		t.Fatalf("file = %q (%v)", raw, err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

// A file that exists needs the global --force (or -f): --file
// ~/.kube/config must not lose its contexts by accident.
func TestKubehzAgentKubeconfigNeedsForceForAnExistingFile(t *testing.T) {
	const kc = "apiVersion: v1\nkind: Config\n"
	calls := agentAPI(t, map[string]string{"GET /api/clusters/cl-1a2b3c4d/kubeconfig/agent": kc})
	base := t.TempDir()
	file := filepath.Join(base, "config")
	if err := os.WriteFile(file, []byte("contexts"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runKubehzLo(t, base, "kubehz", "cluster", "kubeconfig", "cl-1a2b3c4d", "--file", file)
	if !errors.Is(err, ErrHandled) || stderr != "[error] kubehz cluster kubeconfig cl-1a2b3c4d: "+file+" exists\n  Pass --force to replace it, or name a new file.\n" {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
	if raw, _ := os.ReadFile(file); string(raw) != "contexts" || calls.Load() != 0 {
		t.Fatalf("file = %q, requests = %d: a refusal changes nothing and calls nothing", raw, calls.Load())
	}
	for _, force := range []string{"--force", "-f"} {
		_ = os.WriteFile(file, []byte("contexts"), 0o600)
		if _, stderr, err := runKubehzLo(t, base, "kubehz", "cluster", "kubeconfig", "cl-1a2b3c4d", "--file", file, force); err != nil {
			t.Fatalf("%s: %v %s", force, err, stderr)
		}
		if raw, _ := os.ReadFile(file); string(raw) != kc {
			t.Errorf("%s: file = %q", force, raw)
		}
	}
}

// The commands read no project and print no run header: an agent runs
// `lo mcp` from any directory.
func TestKubehzAgentCommandsNeedNoProject(t *testing.T) {
	agentAPI(t, map[string]string{"GET /api/spaces": cliSpaces})
	t.Setenv("DOMAIN_NAME", "")
	stdout, stderr, err := runKubehzLo(t, filepath.Join(t.TempDir(), "nowhere"), "kubehz", "space", "list", "-o", "json")
	if err != nil || stderr != "" || !strings.HasPrefix(stdout, "{\n  \"spaces\": [") {
		t.Fatalf("err=%v stderr=%q stdout=%q", err, stderr, stdout)
	}
}
