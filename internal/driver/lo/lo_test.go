package lo

// lo_test.go — the driver contract surface (port of the lo slices of
// tests/unit/kind_contract_test.bats): status words, kubeconfig extraction,
// destroy ordering (cleanup skips shared mirrors, proxy removed), the
// export env, and the registry wiring. The Provision tests port
// tests/unit/lo_provision_guards_test.bats and
// tests/unit/lo_validate_ips_honored_test.bats: `lo up` on the default
// driver must not report success when the cluster was never created, a
// rejected IP config stops the provision BEFORE anything is built, the
// remote-CI rc-100 sentinel is never returned for a failure, and the
// remote-provider refusal never falls back to a local kind cluster. The
// bash suite reproduced the caller's suppressed-errexit `|| rc=$?`; the Go
// equivalent asserts the returned error, and the BEFORE-anything-built
// ordering assertions carry over unchanged.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bytes"
	"errors"
	"fmt"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestDriverRegisteredAsLo(t *testing.T) {
	t.Parallel()
	if _, ok := driver.Get("lo"); !ok {
		t.Fatal("driver 'lo' not registered — the dispatch cannot construct it")
	}
	found := false
	for _, n := range driver.Names() {
		if n == "lo" {
			found = true
		}
	}
	if !found {
		t.Fatal("'lo' missing from driver.Names()")
	}
}

func TestStatusRunningAndNotFound(t *testing.T) {
	d, runner, _, p := testDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"),
		"metadata:\n  name: test-local\n")

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "clusters" {
			writeOut(c, "test-local\n")
		}
		return nil
	}
	if got, _ := d.Status(t.Context(), "test.lok8s.dev"); got != "Running" {
		t.Fatalf("Status = %q, want Running", got)
	}

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "clusters" {
			writeOut(c, "other-cluster\n")
		}
		return nil
	}
	if got, _ := d.Status(t.Context(), "test.lok8s.dev"); got != "NotFound" {
		t.Fatalf("Status = %q, want NotFound", got)
	}
}

func TestKubeconfigExtractsAndReturnsPath(t *testing.T) {
	d, runner, _, p := testDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"),
		"metadata:\n  name: test-local\n")
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "kubeconfig" {
			writeOut(c, "apiVersion: v1\nkind: Config\n")
		}
		return nil
	}

	path, err := d.Kubeconfig(t.Context(), "test.lok8s.dev")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".kubeconfig/test-local.yaml") {
		t.Fatalf("path = %q", path)
	}
	if got := readFileT(t, path); got != "apiVersion: v1\nkind: Config\n" {
		t.Fatalf("kubeconfig content = %q", got)
	}
}

func TestDestroyDeletesClusterCleansRegistriesAndProxy(t *testing.T) {
	d, runner, fd, _, p, cy := lifecycleDriver(t)
	_ = cy
	// Bring the registries up first so cleanup has something to observe.
	var sink strings.Builder
	if err := d.registries(t.Context(), &sink, &sink, "test.lok8s.dev",
		filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")); err != nil {
		t.Fatal(err)
	}

	kindDeleted := false
	base := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) > 0 && c.Args[0] == "delete" {
			kindDeleted = true
			return nil
		}
		return base(c)
	}

	if err := d.Destroy(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatal(err)
	}
	if !kindDeleted {
		t.Fatal("kind delete never ran")
	}
	// Project registries removed, shared mirrors kept.
	if _, _, ok := fd.containerStatus("lok8s-registry-build"); ok {
		t.Fatal("project registry survived destroy")
	}
	if _, _, ok := fd.containerStatus("lok8s-registry-io-docker"); !ok {
		t.Fatal("shared mirror was removed by destroy")
	}
	// Proxy container removal was attempted.
	found := false
	for _, call := range runner.calls {
		if call == "docker rm -f test-lifecycle-proxy" {
			found = true
		}
	}
	if !found {
		t.Fatal("proxy container not removed")
	}
}

func TestExportSetsSpecEnvs(t *testing.T) {
	d, _, errBuf, p := testDriver(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specShared)

	if err := d.Export(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Export: %v\n%s", err, errBuf.String())
	}
	checks := map[string]string{
		"LOK8S_SPEC_CLUSTER_NAME":       "test-lifecycle",
		"LOK8S_SPEC_CLUSTER_DOMAIN":     "test.lok8s.dev",
		"LOK8S_SPEC_CLUSTER_NAMESPACE":  "default",
		"LOK8S_SPEC_NETWORK_NAME":       "lok8s",
		"LOK8S_SPEC_NETWORK_SUBNET":     "10.125.50.0/24",
		"LOK8S_SPEC_NETWORK_BASE_IP":    "10.125.50.0",
		"LOK8S_SPEC_REGISTRY_PREFIX":    "lok8s.local",
		"LOK8S_SPEC_REGISTRY_BUILD_IP":  "10.125.50.101",
		"LOK8S_SPEC_REGISTRY_CACHE_IP":  "10.125.50.102",
		"LOK8S_SPEC_KIND_PODSUBNET":     DefaultPodCIDR,
		"LOK8S_SPEC_KIND_SERVICESUBNET": DefaultSvcCIDR,
		// No spec.loadBalancer + non-slot domain → empty pool triplet.
		"LOK8S_SPEC_LOADBALANCER_POOL":       "",
		"LOK8S_SPEC_LOADBALANCER_POOL_START": "",
		"LOK8S_SPEC_LOADBALANCER_POOL_END":   "",
		// Registry hosts come from the generated JSON.
		"LOK8S_SPEC_REGISTRY_BUILD_HOST": "lok8s.local",
		"LOK8S_SPEC_REGISTRY_CACHE_HOST": "lok8s.cache",
	}
	for k, want := range checks {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestReadRemoteConfigRejectsUnsafeSyncDest(t *testing.T) {
	// Boundary validation: dest is interpolated into single-quoted REMOTE
	// shell commands — a quote or metacharacter in it would break out of
	// the quoting on the remote host.
	_, _, _, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, "spec:\n  remote:\n    sync:\n      dest: \"/tmp/x'; rm -rf /\"\n")

	var errBuf strings.Builder
	if err := readRemoteConfig(cy, remoteDeps{}, &errBuf); err == nil {
		t.Fatal("shell-metacharacter dest accepted")
	}
	if !strings.Contains(errBuf.String(), "spec.remote.sync.dest must be a plain absolute/relative path") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}

	// Tilde is refused too (single quotes suppress remote tilde expansion).
	testutil.WriteFile(t, cy, "spec:\n  remote:\n    sync:\n      dest: \"~/work\"\n")
	if err := readRemoteConfig(cy, remoteDeps{}, &errBuf); err == nil {
		t.Fatal("tilde dest accepted")
	}
}

// provisionFixture writes a full slot-free spec + the driver's static
// cluster files, and wires the file-backed docker fake with kind/kubectl
// answered success. Plain-HTTP (tls: false) keeps the Secret plugin out of
// the path.
func provisionFixture(t *testing.T) (*Driver, *fakeRunner, *fakeDocker, *bytes.Buffer, *config.Paths) {
	t.Helper()
	d, runner, errBuf, p := testDriver(t)
	fd := newFakeDocker(t)
	// Pre-existing, correctly-configured networks (the network phase is not
	// what these tests probe).
	fd.setNetworkMeta("lok8s", "10.125.50.0/24", "10.125.50.192/26")
	fd.setNetworkMeta("lok8s-registries", "10.125.200.0/24", "10.125.200.128/25")
	writeLifecycleFixture(t, p)

	// coredns static files (content irrelevant — kubectl is faked).
	corednsDir := filepath.Join(p.Lok8s, "drivers", "lo", "cluster", "coredns")
	testutil.WriteFile(t, filepath.Join(corednsDir, "corefile.yaml"), "{}\n")
	testutil.WriteFile(t, filepath.Join(corednsDir, "expose.yaml"), "{}\n")
	testutil.WriteFile(t, filepath.Join(corednsDir, "patch.json"), "[]\n")

	runner.handler = func(c execx.Cmd) error {
		switch c.Name {
		case "docker":
			return fd.handle(c)
		case "kind":
			if len(c.Args) >= 2 && c.Args[0] == "get" {
				switch c.Args[1] {
				case "clusters":
					return nil // empty — no existing cluster
				case "kubeconfig":
					writeOut(c, "apiVersion: v1\nkind: Config\nclusters: []\n")
					return nil
				case "nodes":
					return nil
				}
			}
			return nil
		case "kubectl":
			writeOut(c, "ok\n")
			return nil
		}
		return nil
	}
	return d, runner, fd, errBuf, p
}

func TestProvisionFailedKindCreateDoesNotReportSuccess(t *testing.T) {
	d, runner, fd, errBuf, _ := provisionFixture(t)
	base := runner.handler
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) > 0 && c.Args[0] == "create" {
			return fmt.Errorf("kind create failed")
		}
		return base(c)
	}

	err := d.Provision(t.Context(), "test.lok8s.dev")
	if err == nil {
		t.Fatalf("Provision returned success although 'kind create cluster' FAILED — lo up reports a provisioned cluster that does not exist.\nstderr:\n%s", errBuf.String())
	}
	_ = fd
}

func TestProvisionRejectedIPConfigStopsBeforeAnythingIsBuilt(t *testing.T) {
	// Non-nil at the end is not enough: by then the docker networks,
	// registry TLS cert and containerd config would already exist for a
	// config that was rejected. Nothing after the validator may run.
	d, runner, _, errBuf, p := provisionFixture(t)
	// Break the layout: build IP outside the subnet. The validator reads
	// the freshly generated JSON, so mutate the SPEC's network to disagree
	// with the registries: easiest honest trigger — a MetalLB pool outside
	// the subnet in the spec.
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, strings.Replace(specShared,
		"  runtime: kind",
		"  loadBalancer:\n    pool: \"10.99.0.10-10.99.0.20\"\n  runtime: kind", 1))

	err := d.Provision(t.Context(), "test.lok8s.dev")
	if err == nil {
		t.Fatal("Provision returned success after validateIPs REJECTED the config — it printed 'Aborting.' and then did not abort")
	}
	if !strings.Contains(errBuf.String(), "IP validation error(s). Aborting.") {
		t.Fatalf("abort message missing:\n%s", errBuf.String())
	}
	// The trace: no docker network/run and no kind create happened after
	// the rejection.
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "docker network create") ||
			strings.HasPrefix(call, "docker run") ||
			strings.HasPrefix(call, "kind create") {
			t.Fatalf("step ran AFTER the IP config was rejected: %s", call)
		}
	}
}

func TestProvisionFailedRemoteDoesNotFallBackToLocalKind(t *testing.T) {
	// provisionRemote fails when the VM's SSH/Docker never came up.
	// Unguarded, that failure fell through into the LOCAL kind path and
	// `lo up` reported success — for a local cluster the user never asked
	// for.
	d, runner, _, errBuf, _ := provisionFixture(t)
	d.deps.ProviderName = "hetzner"
	d.deps.Provider = &fakeLoProvider{provisionErr: fmt.Errorf("quota exceeded")}

	err := d.Provision(t.Context(), "test.lok8s.dev")
	if err == nil {
		t.Fatal("Provision returned success although the remote provision FAILED")
	}
	if !strings.Contains(errBuf.String(), "refusing to fall back to a local kind cluster") {
		t.Fatalf("refusal message missing:\n%s", errBuf.String())
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "kind create") {
			t.Fatalf("a LOCAL 'kind create cluster' ran after the remote provision failed: %s", call)
		}
	}
}

func TestProvisionFailedProviderDoesNotMasqueradeAsNoNodes(t *testing.T) {
	// One level below the provisionRemote guard: an unguarded
	// provider Provision/Output failure fell through with empty output and
	// took the legitimate no-nodes local-kind fallback with rc 0.
	d, runner, _, _, _ := provisionFixture(t)
	d.deps.ProviderName = "hetzner"
	d.deps.Provider = &fakeLoProvider{provisionErr: fmt.Errorf("api error")}

	if err := d.Provision(t.Context(), "test.lok8s.dev"); err == nil {
		t.Fatal("Provision returned success although provider provision FAILED — the failure masqueraded as the legitimate no-nodes fallback")
	}
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "kind create") {
			t.Fatal("a LOCAL 'kind create cluster' ran after the provider FAILED")
		}
	}
}

func TestProvisionProviderWithNoNodesStillRunsLocalKind(t *testing.T) {
	// Anti-vacuity companion for the guard above: the intentional fallback
	// (provider ok, zero nodes in output) must keep working — success and a
	// real local kind create.
	d, runner, _, errBuf, _ := provisionFixture(t)
	d.deps.ProviderName = "hetzner"
	d.deps.Provider = &fakeLoProvider{output: []byte(`{"nodes":[]}`)}

	if err := d.Provision(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatalf("the legitimate no-nodes fallback now FAILS (%v) — the guard conflated provider failure with an empty node list.\nstderr:\n%s", err, errBuf.String())
	}
	created := false
	for _, call := range runner.calls {
		if strings.HasPrefix(call, "kind create") {
			created = true
		}
	}
	if !created {
		t.Fatal("local 'kind create' never ran on the legitimate no-nodes fallback")
	}
}

func remoteCIFixture(t *testing.T) (*Driver, *fakeRunner) {
	t.Helper()
	d, runner, _, p := provisionFixtureLight(t)
	_ = p
	t.Setenv("LOK8S_REMOTE", "1")
	t.Setenv("LOK8S_REMOTE_IP", "203.0.113.7")
	t.Setenv("LOK8S_REMOTE_USER", "root")
	return d, runner
}

// provisionFixtureLight: spec only (remote-CI path never reaches the local
// build steps).
func provisionFixtureLight(t *testing.T) (*Driver, *fakeRunner, *bytes.Buffer, *config.Paths) {
	t.Helper()
	d, runner, errBuf, p := testDriver(t)
	writeLifecycleFixture(t, p)
	// Route the remote.mode read: append spec.remote with mode ci.
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, specShared+"  remote:\n    mode: ci\n")
	return d, runner, errBuf, p
}

func TestProvisionFailedRemoteCIDoesNotReturnTheSentinel(t *testing.T) {
	// ErrFullLifecycle means "remote CI handled the full lifecycle" and the
	// dispatch maps it to SUCCESS. A failed remoteCI (ssh mkdir, rsync, the
	// remote provision itself) must never be relabeled as that sentinel.
	d, runner := remoteCIFixture(t)
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "ssh" {
			return fmt.Errorf("ssh failed")
		}
		return nil
	}

	err := d.Provision(t.Context(), "test.lok8s.dev")
	if err == nil {
		t.Fatal("Provision returned success although remoteCI FAILED")
	}
	if errors.Is(err, driver.ErrFullLifecycle) {
		t.Fatal("Provision returned the remote-CI SUCCESS sentinel although remoteCI FAILED — the dispatch maps it to rc 0")
	}
}

func TestProvisionSuccessfulRemoteCIStillReturnsTheSentinel(t *testing.T) {
	// ANTI-VACUITY for the guard above: the failure guard must not swallow
	// the sentinel on the success path.
	d, runner := remoteCIFixture(t)
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "kubeconfig" {
			writeOut(c, "apiVersion: v1\nclusters: []\n")
		}
		return nil
	}

	err := d.Provision(t.Context(), "test.lok8s.dev")
	if !errors.Is(err, driver.ErrFullLifecycle) {
		t.Fatalf("a successful remote CI run returned %v, not the full-lifecycle sentinel — the dispatch would run the local post-provision steps anyway", err)
	}
}

func TestProvisionHappyPathReturnsNil(t *testing.T) {
	// ANTI-VACUITY for all the guards above.
	d, _, _, errBuf, _ := provisionFixture(t)
	if err := d.Provision(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatalf("happy path regressed: %v\nstderr:\n%s", err, errBuf.String())
	}
}

func TestProvisionUnhappyTLSNudgeCannotFailAGoodProvision(t *testing.T) {
	// The advisory nudge reports whether the HOST trusts the dev CA — it
	// says nothing about whether a cluster was created, and must never
	// decide the verdict in either direction. In Go the nudge has no error
	// return AT ALL (the type system is the `return 0`); this pins that a
	// failing openssl verify inside it still yields a nil provision.
	d, runner, fd, errBuf, p := provisionFixture(t)
	// TLS spec variant so the nudge actually probes.
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, strings.Replace(specShared, "    tls: false\n", "", 1))
	// Fake plugin (mint path) + failing openssl.
	stubbed, _ := stubSecretPlugin(t, runner, p.Base)
	testutil.WriteFile(t, filepath.Join(p.Bin, "openssl"), "#!/bin/sh\nexit 1\n")
	os.Chmod(filepath.Join(p.Bin, "openssl"), 0o755)
	t.Setenv("CAROOT", t.TempDir())

	base := runner.handler // the plugin stub replaced the composite handler — rebuild it
	runner.handler = func(c execx.Cmd) error {
		switch c.Name {
		case stubbed:
			return base(c)
		case "openssl":
			return fmt.Errorf("verify failed")
		case "docker":
			return fd.handle(c)
		case "kind":
			if len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "kubeconfig" {
				writeOut(c, "apiVersion: v1\nclusters: []\n")
			}
			return nil
		default:
			return nil
		}
	}

	if err := d.Provision(t.Context(), "test.lok8s.dev"); err != nil {
		t.Fatalf("Provision failed because the advisory TLS nudge did: %v\nstderr:\n%s", err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "lo trust") {
		t.Fatalf("the nudge itself went missing:\n%s", errBuf.String())
	}
}

func TestProvisionUnsupportedRuntimeFails(t *testing.T) {
	d, _, errBuf, p := testDriver(t)
	cy := filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml")
	testutil.WriteFile(t, cy, "spec:\n  runtime: k3d\n")
	if err := d.Provision(t.Context(), "test.lok8s.dev"); err == nil {
		t.Fatal("unsupported runtime accepted")
	}
	if !strings.Contains(errBuf.String(), "error: unsupported Lo runtime: k3d") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}
}

// fakeLoProvider implements driver.Provider for the remote-path guards.
type fakeLoProvider struct {
	provisionErr error
	output       []byte
}

func (p *fakeLoProvider) Validate(ctx context.Context, configFile string) error { return nil }

func (p *fakeLoProvider) CredentialData(ctx context.Context, configFile string) (map[string]string, error) {
	return nil, nil
}

func (p *fakeLoProvider) Provision(ctx context.Context, configFile, workDir string) error {
	return p.provisionErr
}

func (p *fakeLoProvider) Destroy(ctx context.Context, configFile, workDir string) error { return nil }

func (p *fakeLoProvider) Output(ctx context.Context, configFile string) ([]byte, error) {
	if p.output == nil {
		return nil, fmt.Errorf("no output")
	}
	return p.output, nil
}
