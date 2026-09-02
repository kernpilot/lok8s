package build

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

// stubKustomize installs a fake kustomize into p.Bin that cats the file
// named by KUSTOMIZE_STUB_OUTPUT (empty output when unset) and exits with
// KUSTOMIZE_STUB_RC.
func stubKustomize(t *testing.T, p *config.Paths) {
	t.Helper()
	if err := os.MkdirAll(p.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ -n \"${KUSTOMIZE_STUB_OUTPUT:-}\" ] && cat \"${KUSTOMIZE_STUB_OUTPUT}\"\n" +
		"exit \"${KUSTOMIZE_STUB_RC:-0}\"\n"
	if err := os.WriteFile(filepath.Join(p.Bin, "kustomize"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func stubRender(t *testing.T, p *config.Paths, content string) {
	t.Helper()
	out := filepath.Join(p.Base, "stub-render.yaml")
	writeFileT(t, out, content)
	t.Setenv("KUSTOMIZE_STUB_OUTPUT", out)
	t.Setenv("KUSTOMIZE_STUB_RC", "0")
}

func artifactsProject(t *testing.T) (*config.Paths, string) {
	t.Helper()
	p := testPaths(t)
	stubKustomize(t, p)
	domainDir := filepath.Join(p.Clusters, "d.dev")
	writeFileT(t, filepath.Join(domainDir, "kustomization.yaml"), "resources: []\n")
	// Keep resolveAPI inert: an ambient developer KUBECONFIG would export
	// LOK8S_USER_API_* into the process env mid-test.
	t.Setenv("KUBECONFIG", filepath.Join(p.Base, "no-such-kubeconfig.yaml"))
	return p, domainDir
}

const twoDocRender = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"

func runArtifacts(t *testing.T, p *config.Paths) (string, error) {
	t.Helper()
	var errBuf bytes.Buffer
	err := Artifacts(Options{Paths: p, Domain: "d.dev", SplitOverride: "0", Stderr: &errBuf})
	return errBuf.String(), err
}

func TestArtifactsMissingKustomization(t *testing.T) {
	p := testPaths(t)
	stubKustomize(t, p)
	writeFileT(t, filepath.Join(p.Clusters, "d.dev", "cluster.lok8s.yaml"), "kind: Lo\n")
	stderr, err := runArtifacts(t, p)
	if err == nil {
		t.Fatal("missing kustomization must fail")
	}
	want := "domain d.dev has no kustomization.yaml — compose its targets there, e.g. resources: [./targets/foo, ../../.targets/bar]"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestArtifactsRenderAndPromoteOnChange(t *testing.T) {
	p, domainDir := artifactsProject(t)
	stubRender(t, p, twoDocRender)

	stderr, err := runArtifacts(t, p)
	if err != nil {
		t.Fatalf("build failed: %v (%s)", err, stderr)
	}
	artifact := filepath.Join(domainDir, "artifacts.yaml")
	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != twoDocRender {
		t.Errorf("artifacts.yaml = %q", raw)
	}
	if want := "lo build: d.dev rendered 2 document(s) -> " + artifact + "\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}

	// Second identical render: no promote, no "rendered" line, mtime kept.
	before, _ := os.Stat(artifact)
	stderr, err = runArtifacts(t, p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "rendered 2 document(s)") {
		t.Errorf("unchanged render must not re-announce a promote: %q", stderr)
	}
	after, _ := os.Stat(artifact)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged render must leave artifacts.yaml untouched (mtime churn feeds file watchers)")
	}

	// No temp files may remain either way.
	if leftovers, _ := filepath.Glob(filepath.Join(domainDir, "tmp.*")); len(leftovers) > 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

func TestArtifactsEmptyRenderGuard(t *testing.T) {
	p, domainDir := artifactsProject(t)
	stubRender(t, p, twoDocRender)
	if _, err := runArtifacts(t, p); err != nil {
		t.Fatal(err)
	}

	// Now an empty render (rc 0, zero docs) must refuse and keep the prior.
	stubRender(t, p, "")
	stderr, err := runArtifacts(t, p)
	if err == nil {
		t.Fatal("empty render over a prior artifact must refuse")
	}
	for _, want := range []string{
		"refusing to overwrite d.dev's rendered output (2 existing document(s)/file(s)) with an EMPTY render",
		"kustomize succeeded but produced nothing — check " + filepath.Join(domainDir, "kustomization.yaml") + " resources:",
		"applying an empty artifact would prune everything it manages (Flux prune: true)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing %q in %q", want, stderr)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml"))
	if string(raw) != twoDocRender {
		t.Error("prior artifact must survive the refused empty render")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(domainDir, "tmp.*")); len(leftovers) > 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

func TestArtifactsEmptyRenderCountsSplitLayout(t *testing.T) {
	p, domainDir := artifactsProject(t)
	// No artifacts.yaml, but a committed split layout — the guard must count
	// the [A-Z]*.yaml files and still refuse.
	writeFileT(t, filepath.Join(domainDir, "artifacts", "ConfigMap.a.yaml"), "kind: ConfigMap\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "kustomization.yaml"), "resources: []\n")
	stubRender(t, p, "")
	stderr, err := runArtifacts(t, p)
	if err == nil {
		t.Fatal("empty render over a split layout must refuse")
	}
	if want := "(1 existing document(s)/file(s))"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestArtifactsEmptyRenderNoPriorWarns(t *testing.T) {
	p, domainDir := artifactsProject(t)
	stubRender(t, p, "")
	stderr, err := runArtifacts(t, p)
	if err != nil {
		t.Fatalf("empty render with nothing to lose must pass: %v (%s)", err, stderr)
	}
	want := "d.dev rendered 0 documents (no prior artifact, so nothing was lost) — is its kustomization.yaml composing any targets?"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if raw, err := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); err != nil || len(raw) != 0 {
		t.Errorf("empty artifact should be written: %q %v", raw, err)
	}
}

func TestArtifactsKustomizeFailureKeepsPrior(t *testing.T) {
	p, domainDir := artifactsProject(t)
	stubRender(t, p, twoDocRender)
	if _, err := runArtifacts(t, p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUSTOMIZE_STUB_RC", "1")
	stderr, err := runArtifacts(t, p)
	if err == nil {
		t.Fatal("kustomize failure must fail the build")
	}
	if want := "kustomize build failed for d.dev"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml"))
	if string(raw) != twoDocRender {
		t.Error("prior artifact must survive a kustomize failure")
	}
	if leftovers, _ := filepath.Glob(filepath.Join(domainDir, "tmp.*")); len(leftovers) > 0 {
		t.Errorf("leftover temp files: %v", leftovers)
	}
}

func TestArtifactsEnvsubstPass(t *testing.T) {
	p, domainDir := artifactsProject(t)
	t.Setenv("LOK8S_SPEC_ART_TEST", "sub-ok")
	stubRender(t, p, "kind: ConfigMap\ndata:\n  a: ${LOK8S_SPEC_ART_TEST}\n  b: $LOK8S_SPEC_ART_TEST\n  c: ${NOT_LISTED}\n")
	if _, err := runArtifacts(t, p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml"))
	want := "kind: ConfigMap\ndata:\n  a: sub-ok\n  b: sub-ok\n  c: ${NOT_LISTED}\n"
	if string(raw) != want {
		t.Errorf("artifacts.yaml = %q, want %q", raw, want)
	}
}

func TestArtifactsPrunesStalePerTargetDirs(t *testing.T) {
	p, domainDir := artifactsProject(t)
	// Stale pre-domain-build dir (holds an artifacts.yaml) is pruned; a dir
	// without one, and plain files, survive.
	writeFileT(t, filepath.Join(domainDir, "artifacts", "old-target", "artifacts.yaml"), "kind: X\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "keep-dir", "note.txt"), "hi\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "capi.yaml"), "kind: Cluster\n")
	stubRender(t, p, twoDocRender)
	if _, err := runArtifacts(t, p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts", "old-target")); !os.IsNotExist(err) {
		t.Error("stale per-target dir must be pruned")
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts", "keep-dir")); err != nil {
		t.Error("non-generated dir must survive")
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts", "capi.yaml")); err != nil {
		t.Error("env-owned capi.yaml must survive")
	}
}

func TestArtifactsExportsSecretsPath(t *testing.T) {
	p, domainDir := artifactsProject(t)
	t.Setenv("PATH_SECRETS", "/ambient/secrets")
	stubRender(t, p, twoDocRender)
	if _, err := runArtifacts(t, p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH_SECRETS"); got != "/ambient/secrets" {
		t.Errorf("domain without own store must keep ambient PATH_SECRETS, got %q", got)
	}
	if err := os.MkdirAll(filepath.Join(domainDir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runArtifacts(t, p); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PATH_SECRETS"); got != filepath.Join(domainDir, "secrets") {
		t.Errorf("per-domain store must override PATH_SECRETS, got %q", got)
	}
}

func TestCountKindLines(t *testing.T) {
	in := []byte("kind: A\nfoo: 1\n  kind: nested-not-counted\n---\nkind: B\nunkind: x\n")
	if got := countKindLines(in); got != 2 {
		t.Errorf("countKindLines = %d, want 2", got)
	}
}
