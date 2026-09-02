package provision

// gate_test.go ports tests/unit/provision_gate_test.bats — the
// real-infrastructure gate (ConfirmInfra) and its dispatch wiring. Each
// bats case maps to one Go test; the asserted strings are the bats'
// assert_output patterns, verbatim.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
)

// gateSpecYAML is the bats fixture: a minimal cloud-driver spec the gate
// reads directly.
const gateSpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata:
  name: test-prod
spec:
  kubernetes:
    version: "v1.35.5"
  cluster:
    domain: test.prod
  provider:
    name: hetzner
  bootstrap:
    - name: cilium
    - name: metallb
    - name: cert-manager
`

func testPaths(t *testing.T) *config.Paths {
	t.Helper()
	base := t.TempDir()
	return &config.Paths{
		Base:     base,
		Bin:      filepath.Join(base, ".bin"),
		Lok8s:    filepath.Join(base, ".lok8s"),
		Clusters: filepath.Join(base, "clusters"),
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gateDispatcher builds a Dispatcher with a stubbed interactive check
// (bats: _assume_tty/_assume_no_tty redefine provision::_interactive).
func gateDispatcher(t *testing.T, input string, interactive bool) (*Dispatcher, *bytes.Buffer, string) {
	t.Helper()
	p := testPaths(t)
	specPath := filepath.Join(p.Base, "cluster.lok8s.yaml")
	writeFile(t, specPath, gateSpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{
		Paths:       p,
		Stderr:      &errBuf,
		Stdout:      &bytes.Buffer{},
		In:          strings.NewReader(input),
		Interactive: func() bool { return interactive },
	}
	return d, &errBuf, specPath
}

func assertContains(t *testing.T, out, want string) {
	t.Helper()
	if !strings.Contains(out, want) {
		t.Errorf("output missing %q\n--- output ---\n%s", want, out)
	}
}

func refuteContains(t *testing.T, out, avoid string) {
	t.Helper()
	if strings.Contains(out, avoid) {
		t.Errorf("output unexpectedly contains %q\n--- output ---\n%s", avoid, out)
	}
}

// bats: "gate: local kind driver (lo) passes without prompt or output"
func TestGateLocalLoDriverExempt(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "", false)
	if err := d.ConfirmInfra("test.dev", spec, "lo", ActionReconcile); err != nil {
		t.Fatalf("expected exemption, got %v", err)
	}
	if errBuf.String() != "" {
		t.Fatalf("expected no output, got %q", errBuf.String())
	}
}

// bats: "gate: --force (inherited via dynamic scoping) bypasses silently"
func TestGateForceBypassesSilently(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "", false)
	d.Force = true
	if err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionReconcile); err != nil {
		t.Fatalf("expected force bypass, got %v", err)
	}
	if errBuf.String() != "" {
		t.Fatalf("expected no output, got %q", errBuf.String())
	}
}

// bats: "gate: remote-mode lo driver is NOT exempt (cloud VM)"
func TestGateRemoteLoNotExempt(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "", false)
	d.Remote = true
	err := d.ConfirmInfra("test.dev", spec, "lo", ActionReconcile)
	if !errors.Is(err, driver.ErrDeclined) {
		t.Fatalf("expected ErrDeclined (rc 3), got %v", err)
	}
	assertContains(t, errBuf.String(), "provisions/updates the remote VM")
	assertContains(t, errBuf.String(), "refusing to reconcile")
}

// bats: "gate: non-interactive reconcile refuses with summary + hint"
func TestGateNonInteractiveRefusal(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "", false)
	err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionReconcile)
	if !errors.Is(err, driver.ErrDeclined) {
		t.Fatalf("expected ErrDeclined, got %v", err)
	}
	out := errBuf.String()
	assertContains(t, out, "targets")
	assertContains(t, out, "real infrastructure")
	assertContains(t, out, "kubeone driver · provider hetzner")
	assertContains(t, out, "bootstrap DAG (3 addons)")
	assertContains(t, out, "refusing to reconcile 'test.prod' non-interactively — re-run with --force")
}

// bats: "gate: LOK8S_NONINTERACTIVE forces the refusal branch"
func TestGateLok8sNoninteractiveEnv(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "", false)
	d.Interactive = nil // exercise the DEFAULT check, driven by env
	t.Setenv("LOK8S_NONINTERACTIVE", "1")
	err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionReconcile)
	if !errors.Is(err, driver.ErrDeclined) {
		t.Fatalf("expected ErrDeclined, got %v", err)
	}
	assertContains(t, errBuf.String(), "refusing to reconcile")
}

// bats: "gate: reconcile accepts y and yes"
func TestGateReconcileAcceptsYAndYes(t *testing.T) {
	for _, answer := range []string{"y\n", "yes\n", "Y\n", "YES\n"} {
		d, errBuf, spec := gateDispatcher(t, answer, true)
		if err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionReconcile); err != nil {
			t.Fatalf("answer %q: expected accept, got %v", answer, err)
		}
		assertContains(t, errBuf.String(), "proceed? [y/N]")
	}
}

// bats: "gate: reconcile aborts on n / empty answer with sentinel rc 3"
func TestGateReconcileAbortsOnDecline(t *testing.T) {
	for _, answer := range []string{"n\n", "\n", ""} {
		d, errBuf, spec := gateDispatcher(t, answer, true)
		err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionReconcile)
		if !errors.Is(err, driver.ErrDeclined) {
			t.Fatalf("answer %q: expected ErrDeclined, got %v", answer, err)
		}
		if driver.ExitCode(err) != 3 {
			t.Fatalf("answer %q: expected exit code 3", answer)
		}
		assertContains(t, errBuf.String(), "aborted — 'test.prod' left untouched")
	}
}

// bats: "gate: bootstrap action names the addon re-apply"
func TestGateBootstrapActionNamesReapply(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "y\n", true)
	if err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionBootstrap); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	assertContains(t, errBuf.String(), "re-apply 3 bootstrap addons on the LIVE cluster")
}

// bats: "gate: destroy rejects a mere y"
func TestGateDestroyRejectsMereY(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "y\n", true)
	err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionDestroy)
	if !errors.Is(err, driver.ErrDeclined) {
		t.Fatalf("expected ErrDeclined, got %v", err)
	}
	assertContains(t, errBuf.String(), "type yes to continue")
	assertContains(t, errBuf.String(), "aborted")
}

// bats: "gate: destroy accepts a literal yes"
func TestGateDestroyAcceptsLiteralYes(t *testing.T) {
	d, errBuf, spec := gateDispatcher(t, "yes\n", true)
	if err := d.ConfirmInfra("test.prod", spec, "kubeone", ActionDestroy); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
	assertContains(t, errBuf.String(), "deprovisions the cluster's cloud resources")
}

// ── Dispatch wiring (bats: _setup_dispatch_stubs) ─────────

// dispatchHarness mirrors the bats' minimal dispatch harness: a real spec
// under clusters/test.prod, a fake kubeone driver, recorders proving the
// gate fires FIRST.
func dispatchHarness(t *testing.T, input string) (*Dispatcher, *bytes.Buffer, *[]string) {
	t.Helper()
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.prod", "cluster.lok8s.yaml"), gateSpecYAML)
	log := &[]string{}
	fd := &fakeDriver{log: log}
	var errBuf bytes.Buffer
	d := &Dispatcher{
		Paths:       p,
		Stderr:      &errBuf,
		Stdout:      &bytes.Buffer{},
		In:          strings.NewReader(input),
		Interactive: func() bool { return true },
		Drivers: func(name string) (driver.Factory, bool) {
			if name != "kubeone" {
				return nil, false
			}
			return func(deps *driver.Deps) (driver.Driver, error) { return fd, nil }, true
		},
		Hooks: Hooks{
			KubehzDeregister: func(ctx context.Context, domain, yaml string) error {
				*log = append(*log, "DEREGISTERED")
				return nil
			},
		},
	}
	return d, &errBuf, log
}

// bats: "dispatch_destroy: decline stops before deregistration and the driver"
func TestDispatchDestroyDeclineStopsEverything(t *testing.T) {
	d, _, log := dispatchHarness(t, "no\n")
	err := d.DispatchDestroy(context.Background(), "test.prod")
	if !errors.Is(err, driver.ErrDeclined) || driver.ExitCode(err) != 3 {
		t.Fatalf("expected decline rc 3, got %v", err)
	}
	joined := strings.Join(*log, ",")
	refuteContains(t, joined, "DEREGISTERED")
	refuteContains(t, joined, "destroy:")
}

// bats: "dispatch_destroy: accept runs deregistration then the driver"
func TestDispatchDestroyAcceptRunsDeregThenDriver(t *testing.T) {
	d, _, log := dispatchHarness(t, "yes\n")
	if err := d.DispatchDestroy(context.Background(), "test.prod"); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(*log) != 2 || (*log)[0] != "DEREGISTERED" || (*log)[1] != "destroy:test.prod" {
		t.Fatalf("expected [DEREGISTERED destroy:test.prod], got %v", *log)
	}
}

// bats: "dispatch: bootstrap_only maps to the bootstrap gate action"
func TestDispatchBootstrapOnlyGateAction(t *testing.T) {
	d, errBuf, log := dispatchHarness(t, "n\n")
	err := d.Dispatch(context.Background(), "test.prod", true)
	if !errors.Is(err, driver.ErrDeclined) || driver.ExitCode(err) != 3 {
		t.Fatalf("expected decline rc 3, got %v", err)
	}
	assertContains(t, errBuf.String(), "re-apply 3 bootstrap addons")
	refuteContains(t, strings.Join(*log, ","), "provision:")
}

// A driver-subprocess exit 3 during destroy must NOT read as the gate's
// decline sentinel: dispatch_destroy remaps it to exit code 1 (bash:
// `(( destroy_rc == 3 )) && destroy_rc=1`).
func TestDispatchDestroyRemapsDriverRc3(t *testing.T) {
	d, _, log := dispatchHarness(t, "yes\n")
	_ = log
	d.Drivers = func(name string) (driver.Factory, bool) {
		fd := &fakeDriver{log: &[]string{}, destroyErr: &driver.ExitError{Code: 3, Err: errors.New("curl exited 3")}}
		return func(deps *driver.Deps) (driver.Driver, error) { return fd, nil }, true
	}
	err := d.DispatchDestroy(context.Background(), "test.prod")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := driver.ExitCode(err); got != 1 {
		t.Fatalf("driver rc 3 must remap to 1, got %d", got)
	}
}
