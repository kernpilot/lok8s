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
	assertContains(t, errBuf.String(), "No cluster.lok8s.yaml or deploy.lok8s.yaml found in clusters/nonexistent.domain/")
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

// TestReadKindRefusesASecondDocument: a cluster spec file with more than
// one YAML document stops the dispatch before the driver is chosen, with
// the line that starts the second document (domain.SpecLayout). The Go
// readers decode the first document and the bash readers every document, so
// such a file reads as two different specs. Bash twin: provision::read_kind
// in tests/unit/provision_test.bats.
func TestReadKindRefusesASecondDocument(t *testing.T) {
	for _, tc := range []struct{ name, src, want string }{
		{"trailing separator", "kind: Lo\nspec: {}\n---\n", "(line 3: '---')"},
		{"two empty documents then the spec", "---\n---\nkind: Lo\n", "(line 2: '---')"},
		{"two documents", "kind: Lo\n---\nkind: KubeOne\n", "(line 2: '---')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
			testutil.WriteFile(t, spec, tc.src)
			var stderr bytes.Buffer
			kind, err := ReadKind(spec, &stderr)
			if err == nil {
				t.Fatalf("ReadKind = %q, nil; want the layout error", kind)
			}
			want := "[error] cluster spec " + spec + " holds more than one YAML document " + tc.want + ". Keep one document: remove line "
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr = %q, want %q…", stderr.String(), want)
			}
		})
	}
	// One document, also after a `---`, reads as before.
	spec := filepath.Join(t.TempDir(), "cluster.lok8s.yaml")
	testutil.WriteFile(t, spec, "---\nkind: Lo\n")
	if kind, err := ReadKind(spec, &bytes.Buffer{}); err != nil || kind != "lo" {
		t.Fatalf("ReadKind = %q, %v; want lo", kind, err)
	}
}
