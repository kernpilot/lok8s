package provision

// spec_test.go covers spec resolution and the clusterRef resolution.

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// bats: "provision::resolve_spec resolves cluster.lok8s.yaml"
func TestResolveSpecCluster(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
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
	t.Parallel()
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
	t.Parallel()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
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
	t.Parallel()
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

// bats: "provision::resolve_clusterref resolves valid clusterRef"
func TestResolveClusterRefValid(t *testing.T) {
	t.Parallel()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "staging.lok8s.dev", "deploy.lok8s.yaml"), deploySpecYAML)
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
	t.Parallel()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "test.lok8s.dev", "cluster.lok8s.yaml"), loSpecYAML)
	var errBuf bytes.Buffer
	if _, err := ResolveClusterRef(p, "test.lok8s.dev", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "No deploy.lok8s.yaml")
}

// bats: "provision::resolve_clusterref fails for missing clusterRef"
func TestResolveClusterRefMissingRef(t *testing.T) {
	t.Parallel()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "orphan.lok8s.dev", "deploy.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: orphan\nspec: {}\n")
	var errBuf bytes.Buffer
	if _, err := ResolveClusterRef(p, "orphan.lok8s.dev", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "missing spec.clusterRef.domain")
}

// bats: "provision::resolve_clusterref fails when referenced domain missing"
func TestResolveClusterRefDanglingRef(t *testing.T) {
	t.Parallel()
	p := testPaths(t)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "bad-ref.lok8s.dev", "deploy.lok8s.yaml"),
		"apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: bad-ref\nspec:\n  clusterRef:\n    domain: nonexistent.lok8s.dev\n")
	var errBuf bytes.Buffer
	if _, err := ResolveClusterRef(p, "bad-ref.lok8s.dev", &errBuf); err == nil {
		t.Fatal("expected failure")
	}
	assertContains(t, errBuf.String(), "clusterRef domain not found")
}
