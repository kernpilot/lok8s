package bootstrap

// dispatch_test.go — the Go port of the bootstrap::dispatch bats block (the
// standalone `lo bootstrap` core): driver export ordering, the reconcile
// gate, and the guards that keep untrusted spec values away from driver
// resolution. Plus the provision Hooks.BootstrapApply wiring.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/provision"
)

// fakeDriver implements the Driver contract; withExport adds the optional
// Exporter hook (the bats fake driver wrote driver::export into main).
type fakeDriver struct{ exported *string }

func (d *fakeDriver) Provision(ctx context.Context, domain string) error { return nil }
func (d *fakeDriver) Destroy(ctx context.Context, domain string) error   { return nil }
func (d *fakeDriver) Status(ctx context.Context, domain string) (string, error) {
	return "Running", nil
}
func (d *fakeDriver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	return "", nil
}

type fakeExportingDriver struct{ fakeDriver }

func (d *fakeExportingDriver) Export(ctx context.Context, domain string) error {
	// The driver contract: driver::export "<domain>" exports LOK8S_SPEC_*.
	os.Setenv("LOK8S_SPEC_CLUSTER_DOMAIN", domain)
	return nil
}

func dispatcherFixture(t *testing.T, drv driver.Driver, kind string) (*Dispatcher, *Engine, *strings.Builder) {
	t.Helper()
	e, _, _, _, p := testEngine(t)
	var errBuf strings.Builder
	e.Stderr = &errBuf
	mkChartAddon(t, p, "testcni")
	writeClusterSpec(t, p, "testcni")
	writeKubeconfig(t, p)
	d := &Dispatcher{Engine: e}
	if drv != nil {
		d.Drivers = func(name string) (driver.Factory, bool) {
			if name != kind {
				return nil, false
			}
			return func(deps *driver.Deps) (driver.Driver, error) { return drv, nil }, true
		}
	} else {
		d.Drivers = func(name string) (driver.Factory, bool) { return nil, false }
	}
	return d, e, &errBuf
}

func TestDispatchRunsDriverExportBeforeApply(t *testing.T) {
	// Regression pin: the standalone path must run driver::export BEFORE
	// Apply, so spec.bootstrap addons' ${LOK8S_SPEC_*} refs substitute
	// (the friction-log empty-render bug).
	t.Setenv("LOK8S_SPEC_CLUSTER_DOMAIN", "")
	t.Setenv("LOK8S_BOOTSTRAP_ONLY", "")
	d, e, _ := dispatcherFixture(t, &fakeExportingDriver{}, "lo")
	var seenDomain, seenGate string
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		seenDomain = os.Getenv("LOK8S_SPEC_CLUSTER_DOMAIN")
		seenGate = os.Getenv("LOK8S_BOOTSTRAP_ONLY")
		return 0
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if seenDomain != "test.lok8s.dev" {
		t.Errorf("LOK8S_SPEC_CLUSTER_DOMAIN = %q at apply time (the empty-render regression)", seenDomain)
	}
	// Standalone `lo bootstrap` runs in reconcile mode (=1) — else it
	// silently skips cilium on KubeOne.
	if seenGate != "1" {
		t.Errorf("LOK8S_BOOTSTRAP_ONLY = %q at apply time, want 1", seenGate)
	}
}

func TestDispatchToleratesDriverWithoutExport(t *testing.T) {
	d, e, _ := dispatcherFixture(t, &fakeDriver{}, "lo")
	applied := false
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		applied = true
		return 0
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !applied {
		t.Error("apply never reached")
	}
}

func TestDispatchFailsOnUnknownDriverKind(t *testing.T) {
	d, e, errBuf := dispatcherFixture(t, nil, "")
	spec := filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Bogus\nmetadata:\n  name: e2e-test\nspec:\n  bootstrap:\n    - testcni\n")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		t.Error("apply must not run")
		return 0
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "Unknown cluster kind: bogus") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestDispatchRejectsPathTraversalDomain(t *testing.T) {
	d, _, errBuf := dispatcherFixture(t, &fakeDriver{}, "lo")
	if err := d.Dispatch(context.Background(), "../../../etc"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "invalid domain name") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestDispatchRejectsMaliciousClusterKind(t *testing.T) {
	// A crafted spec whose kind traverses out of drivers/ must be rejected
	// BEFORE any driver code resolves.
	d, e, errBuf := dispatcherFixture(t, &fakeDriver{}, "lo")
	spec := filepath.Join(e.Paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, "apiVersion: cluster.lok8s.dev/v1beta1\nkind: ../../../tmp/evil\nmetadata:\n  name: e2e-test\n")
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		t.Error("apply must not run")
		return 0
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "invalid cluster kind") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestDispatchMissingSpecFails(t *testing.T) {
	d, _, errBuf := dispatcherFixture(t, &fakeDriver{}, "lo")
	if err := d.Dispatch(context.Background(), "no-such.domain"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errBuf.String(), "cluster spec not found") {
		t.Errorf("stderr = %q", errBuf.String())
	}
}

func TestDispatchInventoryPublishRunsAfterApply(t *testing.T) {
	d, e, _ := dispatcherFixture(t, &fakeDriver{}, "lo")
	order := &eventLog{}
	e.ApplyOne = func(ctx context.Context, job Job, stdout, stderr io.Writer) int {
		order.add("apply")
		return 0
	}
	d.InventoryPublish = func(ctx context.Context, domain, clusterYAML, kubeconfig string) {
		order.add("inventory " + domain)
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if order.pos("apply") == 0 || order.pos("inventory test.lok8s.dev") < order.pos("apply") {
		t.Errorf("inventory did not follow apply: %v", order.events)
	}
}

// --- the provision Hooks.BootstrapApply wiring -------------------------------

func TestApplyHookSatisfiesProvisionHooksSeam(t *testing.T) {
	p := testPaths(t)
	hook := ApplyHook(p, runnerFunc(func(ctx context.Context, c execx.Cmd) error { return nil }),
		io.Discard, io.Discard)
	// Compile-time: the hook slots into the dispatch seam it was built for.
	hooks := provision.Hooks{BootstrapApply: hook}
	if hooks.BootstrapApply == nil {
		t.Fatal("hook not assignable")
	}
	// Functional smoke: the hook runs the real engine (missing kubeconfig →
	// the bash error path).
	spec := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	writeFile(t, spec, "kind: Lo\nmetadata:\n  name: x\nspec:\n  bootstrap: []\n")
	if err := hooks.BootstrapApply(context.Background(), "test.lok8s.dev", spec, "/nope/kubeconfig.yaml"); err == nil {
		t.Fatal("missing kubeconfig must error through the hook")
	}
	// And the no-op path succeeds (empty bootstrap, kubeconfig present).
	kc := filepath.Join(p.Base, ".kubeconfig", "x.yaml")
	writeFile(t, kc, "")
	if err := hooks.BootstrapApply(context.Background(), "test.lok8s.dev", spec, kc); err != nil {
		t.Fatalf("hook no-op failed: %v", err)
	}
}
