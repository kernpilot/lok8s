package provision

// dispatch_test.go ports the dispatch flows of tests/unit/provision_test.bats
// (spec resolution is in spec_test.go, the credential loading in
// creds_test.go). The bats' "kind script missing driver::provision" contract-
// violation case has no Go analogue: the Driver interface enforces the
// contract at compile time.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// ── Dispatch ──────────────────────────────────────────────

// bats: "provision::dispatch reads .kind from cluster spec and sources kind script"
func TestDispatchReadsKindAndProvisions(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", false); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0] != "provision:test.lok8s.dev" {
		t.Fatalf("got %v", log)
	}
}

// bats: "provision::dispatch fails for deploy domains"
func TestDispatchRefusesDeployDomain(t *testing.T) {
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", false); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "Cannot provision a deployment domain. Use 'lo deploy test.lok8s.dev' instead.")
	assertContains(t, errBuf.String(), "Deployment domains reference a cluster via spec.clusterRef.domain.")
}

// bats: "provision::dispatch fails for unknown kind"
func TestDispatchUnknownKind(t *testing.T) {
	d, errBuf := loDispatcher(t, nil) // Drivers lookup answers false
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", false); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "Unknown cluster kind")
}

// A malformed `.kind` is NEVER defaulted (bash: read_kind rc 2 branch).
func TestDispatchMalformedKind(t *testing.T) {
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "bad.dev", "cluster.lok8s.yaml"),
		"kind: \"lo; rm -rf /\"\nmetadata:\n  name: x\n")
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.Dispatch(t.Context(), "bad.dev", false); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "invalid cluster kind in")
	assertContains(t, errBuf.String(), "(not a bare driver name)")
}

// bats: "provision::dispatch --bootstrap skips driver::provision but runs
// driver::export + bootstrap::apply"
func TestDispatchBootstrapOnlyRunsExportAndBootstrap(t *testing.T) {
	t.Setenv("LOK8S_BOOTSTRAP_ONLY", "") // register restore; Dispatch mutates it
	log := []string{}
	d, _ := loDispatcher(t, fakeExportingDriver{&fakeDriver{log: &log}})
	// An existing kubeconfig so the --bootstrap guard passes.
	testutil.WriteFile(t, filepath.Join(d.Paths.Base, ".kubeconfig", "test-cluster.yaml"), "")
	d.Hooks.BootstrapApply = func(ctx context.Context, domain, yaml, kubeconfig string) error {
		log = append(log, "bootstrap_applied:"+domain)
		if !strings.HasSuffix(kubeconfig, filepath.Join(".kubeconfig", "test-cluster.yaml")) {
			t.Errorf("bootstrap kubeconfig = %s", kubeconfig)
		}
		return nil
	}
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", true); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(log, ",")
	assertContains(t, joined, "export:test.lok8s.dev")
	assertContains(t, joined, "bootstrap_applied:test.lok8s.dev")
	refuteContains(t, joined, "provision:")
	if os.Getenv("LOK8S_BOOTSTRAP_ONLY") != "1" {
		t.Error("LOK8S_BOOTSTRAP_ONLY not exported as 1")
	}
}

// bats: "provision::dispatch --bootstrap fails when the cluster is not provisioned"
func TestDispatchBootstrapOnlyNeedsExistingCluster(t *testing.T) {
	log := []string{}
	d, errBuf := loDispatcher(t, &fakeDriver{log: &log})
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", true); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "existing cluster")
	refuteContains(t, strings.Join(log, ","), "provision:")
}

// bats: "provision::dispatch invokes driver::post_provision when defined"
func TestDispatchInvokesPostProvision(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, fakePostProvisionDriver{&fakeDriver{log: &log}})
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", false); err != nil {
		t.Fatal(err)
	}
	assertContains(t, strings.Join(log, ","), "post_provision:test.lok8s.dev")
}

// bats: "provision::dispatch triggers gitops bootstrap when configured"
func TestDispatchTriggersGitops(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	testutil.WriteFile(t, filepath.Join(d.Paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"),
		loSpecYAML+"  gitops:\n    provider: flux\n")
	gitopsCalled := ""
	d.Hooks.GitopsBootstrap = func(ctx context.Context, domain, provider string) error {
		gitopsCalled = domain + ":" + provider
		return nil
	}
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", false); err != nil {
		t.Fatal(err)
	}
	if gitopsCalled != "test.lok8s.dev:flux" {
		t.Fatalf("gitops hook got %q", gitopsCalled)
	}
}

// rc 100 (ErrFullLifecycle): remote CI handled everything — dispatch
// reports success and skips the whole tail.
func TestDispatchFullLifecycleSentinel(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log, provisionErr: driver.ErrFullLifecycle})
	tailRan := false
	d.Hooks.BootstrapApply = func(ctx context.Context, domain, yaml, kubeconfig string) error {
		tailRan = true
		return nil
	}
	if err := d.Dispatch(t.Context(), "test.lok8s.dev", false); err != nil {
		t.Fatalf("rc 100 must map to success, got %v", err)
	}
	if tailRan {
		t.Fatal("post-provision tail must be skipped on ErrFullLifecycle")
	}
}

// ── DispatchDestroy / DispatchStatus ──────────────────────

// bats: "provision::dispatch_destroy calls driver::destroy"
func TestDispatchDestroyCallsDriver(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	if err := d.DispatchDestroy(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0] != "destroy:test.lok8s.dev" {
		t.Fatalf("got %v", log)
	}
}

// bats: "provision::dispatch_destroy fails for deploy domains"
func TestDispatchDestroyRefusesDeployDomain(t *testing.T) {
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.DispatchDestroy(t.Context(), "test.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "Cannot destroy a deployment domain. Destroy the cluster domain instead.")
}

// bats: "provision::dispatch_status calls driver::status"
func TestDispatchStatusCallsDriver(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	var out bytes.Buffer
	d.Stdout = &out
	if err := d.DispatchStatus(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Running\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

// bats: "provision::dispatch_status follows clusterRef for deploy domains"
func TestDispatchStatusFollowsClusterRef(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	testutil.WriteFile(t, filepath.Join(d.Paths.Clusters, "staging.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	var out bytes.Buffer
	d.Stdout = &out
	if err := d.DispatchStatus(t.Context(), "staging.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Running\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	assertContains(t, strings.Join(log, ","), "status:test.lok8s.dev")
}

// bats: "provision::dispatch_status fails for deploy domain without clusterRef"
func TestDispatchStatusDeployMissingClusterRef(t *testing.T) {
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "orphan.lok8s.dev", "deploy.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: orphan-apps\nspec: {}\n")
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.DispatchStatus(t.Context(), "orphan.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "missing spec.clusterRef.domain")
}

// ── LoadProviderCreds ─────────────────────────────────────

// The provider is loaded and validated BEFORE the driver is built, so the
// driver's Deps carry it from construction on (the seams bound at
// construction, bridge.KubeoneAppendInventory and the kubehz fingerprint
// reader, see it). Same on the destroy path in remote mode.
func TestDispatchLoadsProviderBeforeDriverConstruction(t *testing.T) {
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "cloud.dev", "cluster.lok8s.yaml"), `kind: KubeOne
metadata:
  name: cloud
spec:
  provider:
    name: hetzner
    config:
      location: fsn1
`)
	log := []string{}
	var seen []string
	loader := &fakeLoader{}
	d := &Dispatcher{
		Paths: p, Stderr: &bytes.Buffer{}, Stdout: &bytes.Buffer{},
		Force:     true,
		Remote:    true,
		Providers: loader,
		Drivers: func(name string) (driver.Factory, bool) {
			return func(deps *driver.Deps) (driver.Driver, error) {
				got := "none"
				if deps.Provider != nil {
					got = deps.ProviderName + ":" + filepath.Base(deps.ProviderConfigFile)
				}
				seen = append(seen, got)
				return &fakeDriver{log: &log}, nil
			}, true
		},
	}
	if err := d.Dispatch(t.Context(), "cloud.dev", false); err != nil {
		t.Fatal(err)
	}
	if err := d.DispatchDestroy(t.Context(), "cloud.dev"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[0], "hetzner:lok8s-provider-config.") || !strings.HasPrefix(seen[1], "hetzner:lok8s-provider-config.") {
		t.Errorf("the driver was built without the provider: %v (loaded %v)", seen, loader.loaded)
	}
	if fmt.Sprint(log) != "[provision:cloud.dev destroy:cloud.dev]" {
		t.Errorf("driver calls = %v", log)
	}
}
