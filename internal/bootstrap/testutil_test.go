package bootstrap

// testutil_test.go — hermetic test infrastructure. ALL external tools run
// through the fake runner; no real kubectl/kustomize/docker is ever invoked
// (live kind clusters exist on dev machines — hermeticity is a safety
// property, not just speed).

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/render"
	"gopkg.in/yaml.v3"
)

// rcError fakes a subprocess exit code.
type rcError struct{ code int }

func (e *rcError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e *rcError) ExitCode() int { return e.code }

// fakeRunner mirrors the bats stubs: kustomize captures the build dir +
// staged merged values and emits a canned manifest; kubectl logs calls and
// succeeds (overridable via handler).
type fakeRunner struct {
	mu            sync.Mutex
	calls         []string
	mergedOut     string     // last staged values.merged.yaml
	buildDirs     []string   // kustomize build dirs, in call order
	kustomizeEnvs [][]string // each kustomize call's Env
	manifest      string     // kustomize output (default: one ConfigMap)
	// handler, when set, answers kubectl calls (return the error; write to
	// c.Stdout/c.Stderr as needed). nil → success, no output.
	handler func(c execx.Cmd) error
}

func (f *fakeRunner) Run(ctx context.Context, c execx.Cmd) error {
	f.mu.Lock()
	f.calls = append(f.calls, c.Name+" "+strings.Join(c.Args, " "))
	f.mu.Unlock()
	switch c.Name {
	case "kustomize":
		buildDir := c.Args[len(c.Args)-1]
		f.mu.Lock()
		f.kustomizeEnvs = append(f.kustomizeEnvs, c.Env)
		f.buildDirs = append(f.buildDirs, buildDir)
		if raw, err := os.ReadFile(filepath.Join(buildDir, "values.merged.yaml")); err == nil {
			f.mergedOut = string(raw)
		}
		f.mu.Unlock()
		manifest := f.manifest
		if manifest == "" {
			manifest = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: testcni\n"
		}
		if c.Stdout != nil {
			fmt.Fprint(c.Stdout, manifest)
		}
		return nil
	case "kubectl":
		if f.handler != nil {
			return f.handler(c)
		}
		return nil
	default:
		return nil
	}
}

func (f *fakeRunner) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

// testEngine builds an Engine over a temp project tree + fake runner, with
// buffered stdout/stderr and instant sleeps. The addon render is pinned to
// the exec pipeline (LO_RENDER=exec) so the fake runner is the kustomize
// seam; the in-process pipeline itself is covered by internal/render and
// internal/addons.
func testEngine(t *testing.T) (*Engine, *fakeRunner, *bytes.Buffer, *bytes.Buffer, *config.Paths) {
	t.Helper()
	t.Setenv(render.ModeEnv, string(render.ModeExec))
	p := testPaths(t)
	f := &fakeRunner{}
	var out, errOut bytes.Buffer
	e := &Engine{
		Paths: p, Runner: f, Stdout: &out, Stderr: &errOut,
		Sleep: func(time.Duration) {},
		SopsDecrypt: func(path string) ([]byte, error) {
			return nil, fmt.Errorf("no sops in tests")
		},
	}
	return e, f, &out, &errOut, p
}

// writeClusterSpec writes the bats fixture spec (kind Lo, provider hetzner)
// with the given bootstrap entry lines, returning its path.
func writeClusterSpec(t *testing.T, p *config.Paths, entries ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: e2e-test\nspec:\n  provider:\n    name: hetzner\n  bootstrap:\n")
	for _, e := range entries {
		b.WriteString("  - " + e + "\n")
	}
	f := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, f, b.String())
	return f
}

// writeKubeconfig lays down the fake kubeconfig the [[ -f ]] guard needs.
func writeKubeconfig(t *testing.T, p *config.Paths) string {
	t.Helper()
	f := filepath.Join(p.Base, ".kubeconfig", "e2e-test.yaml")
	writeFile(t, f, "")
	return f
}

// mkAddonDirs creates bare addon dirs (the -d guard) for scheduler tests
// that stub ApplyOne.
func mkAddonDirs(t *testing.T, p *config.Paths, names ...string) {
	t.Helper()
	for _, n := range names {
		if err := os.MkdirAll(filepath.Join(p.Lok8s, "addons", n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// eventLog is a mutex-guarded ordered event recorder for the concurrency
// tests (the bats timestamped START/END log).
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(ev string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ev)
}

// pos returns the 1-based position of the first exact-match event, or 0.
func (l *eventLog) pos(ev string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, e := range l.events {
		if e == ev {
			return i + 1
		}
	}
	return 0
}

func (l *eventLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

func (l *eventLog) has(ev string) bool { return l.pos(ev) > 0 }

// yqLike resolves a dotted path in a YAML doc, stringifying the scalar
// ("missing" when absent) — the test-side stand-in for `yq -r`.
func yqLike(t *testing.T, doc, path string) string {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("parse: %v\n%s", err, doc)
	}
	cur := any(m)
	for _, key := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return "missing"
		}
		cur, ok = mm[key]
		if !ok {
			return "missing"
		}
	}
	return fmt.Sprintf("%v", cur)
}
