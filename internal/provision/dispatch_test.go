package provision

// dispatch_test.go ports tests/unit/provision_test.bats — spec resolution,
// the dispatch flows, clusterRef resolution, and provider-credential
// loading. The bats' "kind script missing driver::provision" contract-
// violation case has no Go analogue: the Driver interface enforces the
// contract at compile time.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
)

// loSpecYAML mirrors the lo-cluster fixture the bats copy in.
const loSpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Lo
metadata:
  name: test-cluster
spec:
  kubernetes:
    version: "v1.31.10"
`

const deploySpecYAML = `apiVersion: cluster.lok8s.dev/v1beta1
kind: Deploy
metadata:
  name: staging-apps
spec:
  clusterRef:
    domain: test.lok8s.dev
`

// fakeDriver records lifecycle calls; optional-interface variants wrap it.
type fakeDriver struct {
	log          *[]string
	provisionErr error
	destroyErr   error
	statusWord   string
	statusErr    error
}

func (f *fakeDriver) Provision(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "provision:"+domain)
	return f.provisionErr
}

func (f *fakeDriver) Destroy(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "destroy:"+domain)
	return f.destroyErr
}

func (f *fakeDriver) Status(ctx context.Context, domain string) (string, error) {
	*f.log = append(*f.log, "status:"+domain)
	if f.statusWord == "" {
		return "Running", f.statusErr
	}
	return f.statusWord, f.statusErr
}

func (f *fakeDriver) Kubeconfig(ctx context.Context, domain string) (string, error) {
	return "", nil
}

type fakeExportingDriver struct{ *fakeDriver }

func (f fakeExportingDriver) Export(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "export:"+domain)
	return nil
}

type fakePostProvisionDriver struct{ *fakeDriver }

func (f fakePostProvisionDriver) PostProvision(ctx context.Context, domain string) error {
	*f.log = append(*f.log, "post_provision:"+domain)
	return nil
}

// loDispatcher builds a Dispatcher around a test.lok8s.dev lo-driver
// domain, mirroring the bats setup (kubehz/bootstrap hooks stubbed off =
// nil hooks skipped).
func loDispatcher(t *testing.T, drv driver.Driver) (*Dispatcher, *bytes.Buffer) {
	t.Helper()
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{
		Paths:  p,
		Stderr: &errBuf,
		Stdout: &bytes.Buffer{},
		Drivers: func(name string) (driver.Factory, bool) {
			if name != "lo" || drv == nil {
				return nil, false
			}
			return func(deps *driver.Deps) (driver.Driver, error) { return drv, nil }, true
		},
	}
	return d, &errBuf
}

// ── ResolveSpec ───────────────────────────────────────────

// bats: "provision::resolve_spec resolves cluster.lok8s.yaml"
func TestResolveSpecCluster(t *testing.T) {
	d, _ := loDispatcher(t, nil)
	spec, err := ResolveSpec(d.Paths, "test.lok8s.dev", d.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != SpecKindCluster || !strings.HasSuffix(spec.File, "cluster.lok8s.yaml") {
		t.Fatalf("got %+v", spec)
	}
}

// bats: "provision::resolve_spec resolves deploy.lok8s.yaml"
func TestResolveSpecDeploy(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	spec, err := ResolveSpec(p, "test.lok8s.dev", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != SpecKindDeploy || !strings.HasSuffix(spec.File, "deploy.lok8s.yaml") {
		t.Fatalf("got %+v", spec)
	}
}

// bats: "provision::resolve_spec fails for missing domain"
func TestResolveSpecMissingDomain(t *testing.T) {
	p := testPaths(t)
	var errBuf bytes.Buffer
	if _, err := ResolveSpec(p, "nonexistent.domain", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	// The historical ".lok8s/<domain>/" spelling is part of the contract.
	assertContains(t, errBuf.String(), "No cluster.lok8s.yaml or deploy.lok8s.yaml found in .lok8s/nonexistent.domain/")
}

// bats: "provision::resolve_spec prefers cluster.lok8s.yaml over deploy.lok8s.yaml"
func TestResolveSpecPrefersCluster(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	spec, err := ResolveSpec(p, "test.lok8s.dev", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Kind != SpecKindCluster || !strings.HasSuffix(spec.File, "cluster.lok8s.yaml") {
		t.Fatalf("got %+v", spec)
	}
}

// Empty and path-shaped domains fail fast (the bash guards; note the
// invalid-domain message is the RAW `error: …` family, not [error]).
func TestResolveSpecGuards(t *testing.T) {
	p := testPaths(t)

	var errBuf bytes.Buffer
	if _, err := ResolveSpec(p, "", &errBuf); err == nil {
		t.Fatal("expected failure for empty domain")
	}
	assertContains(t, errBuf.String(), "no active domain — set one with 'lo use <domain>' or pass --domain <domain>")

	errBuf.Reset()
	if _, err := ResolveSpec(p, "../evil", &errBuf); err == nil {
		t.Fatal("expected failure for traversal domain")
	}
	if got := errBuf.String(); got != "error: invalid domain name: ../evil\n" {
		t.Fatalf("raw error family mismatch: %q", got)
	}
}

// ── Dispatch ──────────────────────────────────────────────

// bats: "provision::dispatch reads .kind from cluster spec and sources kind script"
func TestDispatchReadsKindAndProvisions(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", false); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0] != "provision:test.lok8s.dev" {
		t.Fatalf("got %v", log)
	}
}

// bats: "provision::dispatch fails for deploy domains"
func TestDispatchRefusesDeployDomain(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", false); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "Cannot provision a deployment domain. Use 'lo deploy test.lok8s.dev' instead.")
	assertContains(t, errBuf.String(), "Deployment domains reference a cluster via spec.clusterRef.domain.")
}

// bats: "provision::dispatch fails for unknown kind"
func TestDispatchUnknownKind(t *testing.T) {
	d, errBuf := loDispatcher(t, nil) // Drivers lookup answers false
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", false); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "Unknown cluster kind")
}

// A malformed `.kind` is NEVER defaulted (bash: read_kind rc 2 branch).
func TestDispatchMalformedKind(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "bad.dev", "cluster.lok8s.yaml"),
		"kind: \"lo; rm -rf /\"\nmetadata:\n  name: x\n")
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.Dispatch(context.Background(), "bad.dev", false); err == nil {
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
	writeFile(t, filepath.Join(d.Paths.Base, ".kubeconfig", "test-cluster.yaml"), "")
	d.Hooks.BootstrapApply = func(ctx context.Context, domain, yaml, kubeconfig string) error {
		log = append(log, "bootstrap_applied:"+domain)
		if !strings.HasSuffix(kubeconfig, filepath.Join(".kubeconfig", "test-cluster.yaml")) {
			t.Errorf("bootstrap kubeconfig = %s", kubeconfig)
		}
		return nil
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", true); err != nil {
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
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", true); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "existing cluster")
	refuteContains(t, strings.Join(log, ","), "provision:")
}

// bats: "provision::dispatch invokes driver::post_provision when defined"
func TestDispatchInvokesPostProvision(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, fakePostProvisionDriver{&fakeDriver{log: &log}})
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", false); err != nil {
		t.Fatal(err)
	}
	assertContains(t, strings.Join(log, ","), "post_provision:test.lok8s.dev")
}

// bats: "provision::dispatch triggers gitops bootstrap when configured"
func TestDispatchTriggersGitops(t *testing.T) {
	log := []string{}
	d, _ := loDispatcher(t, &fakeDriver{log: &log})
	writeFile(t, filepath.Join(d.Paths.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"),
		loSpecYAML+"  gitops:\n    provider: flux\n")
	gitopsCalled := ""
	d.Hooks.GitopsBootstrap = func(ctx context.Context, domain, provider string) error {
		gitopsCalled = domain + ":" + provider
		return nil
	}
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", false); err != nil {
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
	if err := d.Dispatch(context.Background(), "test.lok8s.dev", false); err != nil {
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
	if err := d.DispatchDestroy(context.Background(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || log[0] != "destroy:test.lok8s.dev" {
		t.Fatalf("got %v", log)
	}
}

// bats: "provision::dispatch_destroy fails for deploy domains"
func TestDispatchDestroyRefusesDeployDomain(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.DispatchDestroy(context.Background(), "test.lok8s.dev"); err == nil {
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
	if err := d.DispatchStatus(context.Background(), "test.lok8s.dev"); err != nil {
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
	writeFile(t, filepath.Join(d.Paths.Clusters, "staging.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	var out bytes.Buffer
	d.Stdout = &out
	if err := d.DispatchStatus(context.Background(), "staging.lok8s.dev"); err != nil {
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
	writeFile(t, filepath.Join(p.Clusters, "orphan.lok8s.dev", "deploy.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: orphan-apps\nspec: {}\n")
	var errBuf bytes.Buffer
	d := &Dispatcher{Paths: p, Stderr: &errBuf}
	if err := d.DispatchStatus(context.Background(), "orphan.lok8s.dev"); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "missing spec.clusterRef.domain")
}

// ── ResolveClusterRef ─────────────────────────────────────

// bats: "provision::resolve_clusterref resolves valid clusterRef"
func TestResolveClusterRefValid(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	writeFile(t, filepath.Join(p.Clusters, "staging.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
	ref, err := ResolveClusterRef(p, "staging.lok8s.dev", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "test.lok8s.dev" {
		t.Fatalf("ref = %q", ref)
	}
}

// bats: "provision::resolve_clusterref fails for non-deploy domain"
func TestResolveClusterRefNonDeploy(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	var errBuf bytes.Buffer
	if _, err := ResolveClusterRef(p, "test.lok8s.dev", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "No deploy.lok8s.yaml")
}

// bats: "provision::resolve_clusterref fails for missing clusterRef"
func TestResolveClusterRefMissingRef(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "orphan.lok8s.dev", "deploy.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: orphan\nspec: {}\n")
	var errBuf bytes.Buffer
	if _, err := ResolveClusterRef(p, "orphan.lok8s.dev", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "missing spec.clusterRef.domain")
}

// bats: "provision::resolve_clusterref fails when referenced domain missing"
func TestResolveClusterRefDanglingRef(t *testing.T) {
	p := testPaths(t)
	writeFile(t, filepath.Join(p.Clusters, "bad-ref.lok8s.dev", "deploy.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: bad-ref\nspec:\n  clusterRef:\n    domain: nonexistent.lok8s.dev\n")
	var errBuf bytes.Buffer
	if _, err := ResolveClusterRef(p, "bad-ref.lok8s.dev", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "clusterRef domain not found")
}

// ── LoadProviderCreds ─────────────────────────────────────

func clearCredsEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"HCLOUD_TOKEN", "HROBOT_USER", "HROBOT_PASSWORD",
		"ROBOT_USER", "ROBOT_PASSWORD", "HETZNER_ROBOT_USER", "HETZNER_ROBOT_PASSWORD",
	} {
		t.Setenv(k, "") // registers restore of the original value
		os.Unsetenv(k)
	}
}

// bats: "provision::load_provider_creds returns 0 when robot creds absent
// (set -e regression)" — pre-fix, the trailing `[[ -n ]] && export` made
// the bash function return 1 with no creds.
func TestLoadProviderCredsNilWhenAbsent(t *testing.T) {
	clearCredsEnv(t)
	p := testPaths(t)
	if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
		t.Fatalf("must be nil-error when robot creds absent, got %v", err)
	}
}

// bats: "provision::load_provider_creds loads creds from the per-domain store"
func TestLoadProviderCredsLoadsStore(t *testing.T) {
	clearCredsEnv(t)
	p := testPaths(t)
	secd := filepath.Join(p.Clusters, "test.lok8s.dev", "secrets")
	writeFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HCLOUD_TOKEN"), "tok-123")
	writeFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HROBOT_USER"), "rob-usr")
	writeFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HROBOT_PASSWORD"), "rob-pwd")

	if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HCLOUD_TOKEN":           "tok-123",
		"ROBOT_USER":             "rob-usr",
		"HETZNER_ROBOT_USER":     "rob-usr",
		"ROBOT_PASSWORD":         "rob-pwd",
		"HETZNER_ROBOT_PASSWORD": "rob-pwd",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// bats: "provision::load_provider_creds does not clobber preset env"
func TestLoadProviderCredsKeepsPresetEnv(t *testing.T) {
	clearCredsEnv(t)
	p := testPaths(t)
	secd := filepath.Join(p.Clusters, "test.lok8s.dev", "secrets")
	writeFile(t, filepath.Join(secd, "Secret.hetzner.provisioning.HCLOUD_TOKEN"), "store-token")
	t.Setenv("HCLOUD_TOKEN", "env-token")

	if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("HCLOUD_TOKEN"); got != "env-token" {
		t.Fatalf("HCLOUD_TOKEN = %q, want env-token", got)
	}
}
