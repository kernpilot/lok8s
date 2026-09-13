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
