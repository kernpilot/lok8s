package capi

// testutil_test.go — the hermetic harness: a fake execx.Runner (records
// every Cmd, scripted per-call behavior), a project layout under t.TempDir,
// and the spec fixtures. NOTHING in this package's tests executes a real
// clusterctl/kubectl/kind/curl — the Runner seam is the only exec path.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

type fakeRunner struct {
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
	r.calls = append(r.calls, c)
	r.stdins = append(r.stdins, stdin)
	if r.handler != nil {
		return r.handler(c, stdin)
	}
	return nil
}

// argvLine renders a recorded call as "name arg1 arg2 …".
func argvLine(c execx.Cmd) string {
	return c.Name + " " + strings.Join(c.Args, " ")
}

func (r *fakeRunner) anyCall(substr string) bool {
	for _, c := range r.calls {
		if strings.Contains(argvLine(c), substr) {
			return true
		}
	}
	return false
}

// testDriver builds a driver over a temp project. Lok8s points at the REAL
// framework tree (read-only — the templates), Base/Clusters at the temp
// dir. The sleep seam counts instead of sleeping.
func testDriver(t *testing.T) (*Driver, *fakeRunner, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	runner := &fakeRunner{}
	var stderr bytes.Buffer
	paths := &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    repoLok8s(t),
		Clusters: filepath.Join(base, "clusters"),
	}
	d := New(&driver.Deps{Paths: paths, Runner: runner, Stderr: &stderr})
	d.sleep = func(time.Duration) {}
	return d, runner, &stderr
}

// repoLok8s locates the repo's .lok8s tree relative to this package
// (internal/driver/capi → ../../../.lok8s).
func repoLok8s(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", ".lok8s"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p, "drivers", "capi", "generate")); err != nil {
		t.Fatalf("repo .lok8s not found at %s: %v", p, err)
	}
	return p
}

func fixturePath(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "..", "tests", "fixtures", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// writeSpec installs a cluster.lok8s.yaml for a domain in the temp project.
func writeSpec(t *testing.T, d *Driver, domain, yaml string) string {
	t.Helper()
	dir := filepath.Join(d.deps.Paths.Clusters, domain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cluster.lok8s.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeKubeconfig(t *testing.T, d *Driver, name string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(d.deps.Paths.Base, ".kubeconfig"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := d.kubeconfigPath(name)
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// destroySpecYAML is the kkp_capi_destroy_guards fixture (kind: Lo there is
// irrelevant — the driver is invoked directly).
const destroySpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: destroytest
spec:
  managementCluster:
    domain: mgmt.dev
    local: %s
  cluster:
    namespace: default
`

// provisionGuardSpecYAML is the capi_provision_guards fixture.
const provisionGuardSpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Capi
metadata:
  name: provtest
spec:
  managementCluster:
    domain: mgmt.dev
    local: false
  cluster:
    namespace: default
  provider:
    name: hetzner
    config:
      region: fsn1
      sshKeyName: test-key
`

// happyHandler scripts the full provision happy path: every exec succeeds,
// the wait poll reports Provisioned, clusterctl emits a kubeconfig.
func happyHandler(c execx.Cmd, stdin string) error {
	line := argvLine(c)
	switch {
	case c.Name == "kubectl" && strings.Contains(line, "jsonpath={.status.phase}"):
		fmt.Fprint(c.Stdout, "Provisioned")
	case c.Name == "clusterctl" && strings.Contains(line, "get kubeconfig"):
		fmt.Fprint(c.Stdout, "apiVersion: v1\nkind: Config\n")
	case c.Name == "kind" && strings.Contains(line, "get kubeconfig"):
		fmt.Fprint(c.Stdout, "apiVersion: v1\nkind: Config\n")
	}
	return nil
}
