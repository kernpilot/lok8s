package build

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

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

func TestAmbientClusterName(t *testing.T) {
	p := testPaths(t)
	t.Setenv("LOK8S_CLUSTER_NAME", "")
	os.Unsetenv("LOK8S_CLUSTER_NAME")

	if got := AmbientClusterName(p, "d1", ""); got != "local" {
		t.Errorf("default = %q, want local", got)
	}
	t.Setenv("LOK8S_CLUSTER_NAME", "envname")
	if got := AmbientClusterName(p, "d1", ""); got != "envname" {
		t.Errorf("env = %q, want envname", got)
	}
	if got := AmbientClusterName(p, "d1", "flagname"); got != "flagname" {
		t.Errorf("flag = %q, want flagname", got)
	}
	writeFileT(t, filepath.Join(p.Clusters, "d1", "cluster.lok8s.yaml"), "metadata:\n  name: specname\n")
	if got := AmbientClusterName(p, "d1", "flagname"); got != "specname" {
		t.Errorf("spec metadata.name must win, got %q", got)
	}
	if got := AmbientKubeconfig(p, "d1", ""); got != filepath.Join(p.Base, ".kubeconfig", "specname.yaml") {
		t.Errorf("AmbientKubeconfig = %q", got)
	}
}

func TestResolveKubeconfigForDomainDeployRef(t *testing.T) {
	p := testPaths(t)
	writeFileT(t, filepath.Join(p.Clusters, "dep", "deploy.lok8s.yaml"), "spec:\n  clusterRef:\n    domain: target\n")
	writeFileT(t, filepath.Join(p.Clusters, "target", "cluster.lok8s.yaml"), "metadata:\n  name: tgt\n")

	// No secret.<ref>.yaml, no <name>.yaml → canonical secret path anyway.
	t.Setenv("KUBECONFIG", "/ambient")
	var errBuf bytes.Buffer
	if err := ResolveKubeconfigForDomain(p, "dep", "", &errBuf); err != nil {
		t.Fatalf("unexpected error: %v (%s)", err, errBuf.String())
	}
	want := filepath.Join(p.Base, ".kubeconfig", "secret.target.yaml")
	if got := os.Getenv("KUBECONFIG"); got != want {
		t.Errorf("KUBECONFIG = %q, want %q", got, want)
	}

	// metadata.name fallback fires when secret.<ref>.yaml is absent and
	// <name>.yaml exists.
	writeFileT(t, filepath.Join(p.Base, ".kubeconfig", "tgt.yaml"), "clusters: []\n")
	if err := ResolveKubeconfigForDomain(p, "dep", "", &errBuf); err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(p.Base, ".kubeconfig", "tgt.yaml")
	if got := os.Getenv("KUBECONFIG"); got != want {
		t.Errorf("fallback KUBECONFIG = %q, want %q", got, want)
	}

	// The canonical secret.<ref>.yaml wins when present.
	writeFileT(t, filepath.Join(p.Base, ".kubeconfig", "secret.target.yaml"), "clusters: []\n")
	if err := ResolveKubeconfigForDomain(p, "dep", "", &errBuf); err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(p.Base, ".kubeconfig", "secret.target.yaml")
	if got := os.Getenv("KUBECONFIG"); got != want {
		t.Errorf("canonical KUBECONFIG = %q, want %q", got, want)
	}
}

func TestResolveKubeconfigForDomainOverrideAndAmbient(t *testing.T) {
	p := testPaths(t)
	writeFileT(t, filepath.Join(p.Clusters, "c1", "cluster.lok8s.yaml"), "metadata:\n  name: c1\n")

	// Cluster domain without override: ambient KUBECONFIG untouched.
	t.Setenv("KUBECONFIG", "/ambient")
	var errBuf bytes.Buffer
	if err := ResolveKubeconfigForDomain(p, "c1", "", &errBuf); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("KUBECONFIG"); got != "/ambient" {
		t.Errorf("no ref must keep ambient KUBECONFIG, got %q", got)
	}

	// Cluster domain WITH --cluster-override follows the override.
	if err := ResolveKubeconfigForDomain(p, "c1", "other", &errBuf); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(p.Base, ".kubeconfig", "secret.other.yaml")
	if got := os.Getenv("KUBECONFIG"); got != want {
		t.Errorf("override KUBECONFIG = %q, want %q", got, want)
	}
}

func TestResolveKubeconfigForDomainBadRef(t *testing.T) {
	p := testPaths(t)
	writeFileT(t, filepath.Join(p.Clusters, "dep", "deploy.lok8s.yaml"), "spec: {}\n")
	var errBuf bytes.Buffer
	if err := ResolveKubeconfigForDomain(p, "dep", "", &errBuf); err == nil {
		t.Fatal("missing clusterRef must fail")
	}
	if want := "deploy.lok8s.yaml for dep missing spec.clusterRef.domain"; !bytes.Contains(errBuf.Bytes(), []byte(want)) {
		t.Errorf("stderr = %q, want it to contain %q", errBuf.String(), want)
	}
}

func TestRenderKubeconfigPassB(t *testing.T) {
	p := testPaths(t)
	writeFileT(t, filepath.Join(p.Clusters, "d", "cluster.lok8s.yaml"), "metadata:\n  name: named\n")

	// Neither file exists → canonical secret path (may be nonexistent —
	// tolerated).
	want := filepath.Join(p.Base, ".kubeconfig", "secret.d.yaml")
	if got := renderKubeconfig(p, "d"); got != want {
		t.Errorf("renderKubeconfig = %q, want %q", got, want)
	}
	// metadata.name fallback.
	writeFileT(t, filepath.Join(p.Base, ".kubeconfig", "named.yaml"), "clusters: []\n")
	want = filepath.Join(p.Base, ".kubeconfig", "named.yaml")
	if got := renderKubeconfig(p, "d"); got != want {
		t.Errorf("renderKubeconfig fallback = %q, want %q", got, want)
	}
	// secret.<domain>.yaml wins.
	writeFileT(t, filepath.Join(p.Base, ".kubeconfig", "secret.d.yaml"), "clusters: []\n")
	want = filepath.Join(p.Base, ".kubeconfig", "secret.d.yaml")
	if got := renderKubeconfig(p, "d"); got != want {
		t.Errorf("renderKubeconfig canonical = %q, want %q", got, want)
	}
}

func TestResolveAPI(t *testing.T) {
	p := testPaths(t)
	domainDir := filepath.Join(p.Clusters, "d")
	kc := filepath.Join(p.Base, ".kubeconfig", "api.yaml")
	writeFileT(t, kc, "clusters:\n  - cluster:\n      server: https://10.0.0.9:8443\n")

	t.Setenv("KUBECONFIG", kc)
	t.Setenv("LOK8S_USER_API_HOST", "")
	t.Setenv("LOK8S_USER_API_PORT", "")
	resolveAPI(p, domainDir)
	if got := os.Getenv("LOK8S_USER_API_HOST"); got != "10.0.0.9" {
		t.Errorf("HOST = %q", got)
	}
	if got := os.Getenv("LOK8S_USER_API_PORT"); got != "8443" {
		t.Errorf("PORT = %q", got)
	}

	// No port → 6443 default.
	writeFileT(t, kc, "clusters:\n  - cluster:\n      server: https://api.example.com\n")
	resolveAPI(p, domainDir)
	if got := os.Getenv("LOK8S_USER_API_PORT"); got != "6443" {
		t.Errorf("default PORT = %q", got)
	}

	// Missing kubeconfig, no cluster spec fallback → exports untouched.
	t.Setenv("KUBECONFIG", filepath.Join(p.Base, "nope.yaml"))
	t.Setenv("LOK8S_USER_API_HOST", "prev")
	resolveAPI(p, domainDir)
	if got := os.Getenv("LOK8S_USER_API_HOST"); got != "prev" {
		t.Errorf("best-effort resolveAPI must not export on failure, got %q", got)
	}

	// metadata.name fallback path.
	writeFileT(t, filepath.Join(domainDir, "cluster.lok8s.yaml"), "metadata:\n  name: fb\n")
	writeFileT(t, filepath.Join(p.Base, ".kubeconfig", "fb.yaml"), "clusters:\n  - cluster:\n      server: https://5.6.7.8:6444\n")
	resolveAPI(p, domainDir)
	if got := os.Getenv("LOK8S_USER_API_HOST"); got != "5.6.7.8" {
		t.Errorf("fallback HOST = %q", got)
	}
	if got := os.Getenv("LOK8S_USER_API_PORT"); got != "6444" {
		t.Errorf("fallback PORT = %q", got)
	}
}
