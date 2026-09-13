package cli

// cmd_init_test.go covers the `lo init` routing.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestInitCommandRouting(t *testing.T) {
	p := synthProject(t)
	root := NewRoot(p)

	_, stderr, err := runLo(t, root, "init", "bogus")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "Error: Invalid command: bogus") {
		t.Errorf("init bogus: err=%v stderr=%q", err, stderr)
	}

	_, stderr, err = runLo(t, NewRoot(p), "init", "service")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "service name is required") {
		t.Errorf("init service (no name): err=%v stderr=%q", err, stderr)
	}

	svc := filepath.Join(p.Base, "svc")
	stdout, _, err := runLo(t, NewRoot(p), "init", "service", "foo", "--path", svc)
	if err != nil {
		t.Fatalf("init service: %v", err)
	}
	if !strings.Contains(stdout, "Scaffolded "+svc+"/lok8s.yaml\n") || !strings.Contains(stdout, "Wrote canonical Tiltfile") {
		t.Errorf("stdout:\n%s", stdout)
	}
	// The inherited --force|-f reaches the scaffold.
	testutil.WriteFile(t, svc+"/lok8s.yaml", "build: { context: ., dockerfile: Keep }\n")
	if _, _, err := runLo(t, NewRoot(p), "init", "service", "foo", "-p", svc, "-f"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(svc + "/lok8s.yaml")
	if strings.Contains(string(raw), "Keep") {
		t.Error("-f did not force the overwrite")
	}

	stdout, _, err = runLo(t, NewRoot(p), "init", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Base, "tests", "playwright.config.ts")); err != nil {
		t.Error("init test did not scaffold the suite")
	}
	if !strings.Contains(stdout, "Scaffolded Playwright test suite into "+p.Base+"/tests\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
}

// `lo init project --cluster <domain> --driver <driver>` writes the first
// cluster spec beside the project files (the flag twin of the wizard's
// cluster step); the driver defaults to lo and an unknown one is refused.
func TestInitProjectClusterFlags(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := synthProject(t)
	stdout, stderr, err := runLo(t, NewRoot(p), "init", "project", "acme", "--env", "none", "--cluster", "demo.dev", "--driver", "kubeone")
	if err != nil {
		t.Fatalf("init project --cluster: %v\n%s", err, stderr)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "clusters", "demo.dev", "cluster.lok8s.yaml"))
	if err != nil || !strings.Contains(string(raw), "kind: KubeOne\n") || !strings.Contains(string(raw), "domain: demo.dev\n") {
		t.Errorf("spec: %v\n%s", err, raw)
	}
	if !strings.Contains(stdout, "  lo use demo.dev   # then lo up\n") {
		t.Errorf("stdout:\n%s", stdout)
	}

	_, stderr, err = runLo(t, NewRoot(p), "init", "project", "--env", "none", "--cluster", "x.dev", "--driver", "nope")
	if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, "--driver must be one of lo|kubeone|capi|kkp|kubehz-hosted") {
		t.Errorf("bad driver: err=%v stderr=%s", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "clusters", "x.dev")); err == nil {
		t.Error("a refused driver wrote the domain dir")
	}
}

// `lo init project --implementation` through the cli: the wizard's
// implementation switch and its twin share this path.
func TestInitProjectImplementationFlag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	p := synthProject(t)
	stdout, stderr, err := runLo(t, NewRoot(p), "init", "project", "acme", "--env", "none", "--implementation", "bash")
	if err != nil {
		t.Fatalf("--implementation: %v\n%s", err, stderr)
	}
	if !strings.Contains(stdout, "Set spec.implementation.default: bash in "+filepath.Join(dir, "lok8s.yaml")+"\n") {
		t.Errorf("stdout:\n%s", stdout)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "lok8s.yaml"))
	if !strings.Contains(string(raw), "    default: bash\n") {
		t.Errorf("lok8s.yaml:\n%s", raw)
	}
	if _, _, err := runLo(t, NewRoot(p), "init", "project", "--env", "none", "--implementation", "python"); !errors.Is(err, ErrHandled) {
		t.Errorf("python accepted: %v", err)
	}
}
