package lo

// lo_test.go — the driver contract surface (port of the lo slices of
// tests/unit/kind_contract_test.bats): status words, kubeconfig extraction,
// destroy ordering (cleanup skips shared mirrors, proxy removed), the
// export env, and the registry wiring.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/driver"
	"github.com/kernpilot/lok8s/internal/execx"
)

func TestDriverRegisteredAsLo(t *testing.T) {
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
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"),
		"metadata:\n  name: test-local\n")

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "clusters" {
			writeOut(c, "test-local\n")
		}
		return nil
	}
	if got, _ := d.Status(context.Background(), "test.lok8s.dev"); got != "Running" {
		t.Fatalf("Status = %q, want Running", got)
	}

	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "clusters" {
			writeOut(c, "other-cluster\n")
		}
		return nil
	}
	if got, _ := d.Status(context.Background(), "test.lok8s.dev"); got != "NotFound" {
		t.Fatalf("Status = %q, want NotFound", got)
	}
}

func TestKubeconfigExtractsAndReturnsPath(t *testing.T) {
	d, runner, _, p := testDriver(t)
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"),
		"metadata:\n  name: test-local\n")
	runner.handler = func(c execx.Cmd) error {
		if c.Name == "kind" && len(c.Args) >= 2 && c.Args[0] == "get" && c.Args[1] == "kubeconfig" {
			writeOut(c, "apiVersion: v1\nkind: Config\n")
		}
		return nil
	}

	path, err := d.Kubeconfig(context.Background(), "test.lok8s.dev")
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
	if err := d.registries(context.Background(), &sink, &sink, "test.lok8s.dev",
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

	if err := d.Destroy(context.Background(), "test.lok8s.dev"); err != nil {
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
	writeFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), specShared)

	if err := d.Export(context.Background(), "test.lok8s.dev"); err != nil {
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
	writeFile(t, cy, "spec:\n  remote:\n    sync:\n      dest: \"/tmp/x'; rm -rf /\"\n")

	var errBuf strings.Builder
	if err := readRemoteConfig(cy, remoteDeps{}, &errBuf); err == nil {
		t.Fatal("shell-metacharacter dest accepted")
	}
	if !strings.Contains(errBuf.String(), "spec.remote.sync.dest must be a plain absolute/relative path") {
		t.Fatalf("wrong error:\n%s", errBuf.String())
	}

	// Tilde is refused too (single quotes suppress remote tilde expansion).
	writeFile(t, cy, "spec:\n  remote:\n    sync:\n      dest: \"~/work\"\n")
	if err := readRemoteConfig(cy, remoteDeps{}, &errBuf); err == nil {
		t.Fatal("tilde dest accepted")
	}
}
