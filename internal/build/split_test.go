package build

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

const testAgeKey = "age1zvkyg2lqzraa2lnjvqej32nkuu0ues2s82hzrye869xeexvn73equnujwj"

// splitProject wires a Paths whose .bin holds the REAL pinned yq (the split
// transforms exec it) and a STUB sops: decrypt always fails (→ re-encrypt
// path), encrypt writes stdin + a "sops:" marker to --output. Set
// SOPS_STUB_PLAINTEXT=1 to make the stub emit output WITHOUT the marker
// (exercises the trust-nothing verify).
func splitProject(t *testing.T) (*config.Paths, string) {
	t.Helper()
	p := testPaths(t)
	if err := os.MkdirAll(p.Bin, 0o755); err != nil {
		t.Fatal(err)
	}
	repoYq, err := filepath.Abs(filepath.Join("..", "..", ".bin", "yq"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(repoYq); err != nil {
		t.Skipf("pinned yq not installed at %s (run b install)", repoYq)
	}
	if err := os.Symlink(repoYq, filepath.Join(p.Bin, "yq")); err != nil {
		t.Fatal(err)
	}
	sopsStub := `#!/bin/sh
if [ "$1" = "decrypt" ]; then exit 1; fi
out=""
prev=""
for a in "$@"; do
  [ "$prev" = "--output" ] && out="$a"
  prev="$a"
done
[ -n "$out" ] || exit 1
if [ -n "${SOPS_STUB_PLAINTEXT:-}" ]; then
  cat > "$out"
else
  { cat; printf '\nsops:\n    stub: true\n'; } > "$out"
fi
`
	if err := os.WriteFile(filepath.Join(p.Bin, "sops"), []byte(sopsStub), 0o755); err != nil {
		t.Fatal(err)
	}
	domainDir := filepath.Join(p.Clusters, "s.dev")
	writeFileT(t, filepath.Join(domainDir, "cluster.lok8s.yaml"),
		"kind: Lo\nmetadata:\n  name: s\nspec:\n  build:\n    artifacts: split\n  gitops:\n    provider: flux\n    age:\n      - "+testAgeKey+"\n")
	return p, domainDir
}

const splitArtifact = `apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: cr
rules: []
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
  namespace: ns1
data:
  k: v
---
apiVersion: batch/v1
kind: Job
metadata:
  name: jb
  namespace: ns1
spec:
  ttlSecondsAfterFinished: 60
  template:
    spec:
      restartPolicy: Never
---
apiVersion: v1
kind: Secret
metadata:
  name: sec
  namespace: ns1
stringData:
  password: hunter2
`

func runSplit(t *testing.T, p *config.Paths, noSecrets bool) (string, error) {
	t.Helper()
	var errBuf bytes.Buffer
	err := Split(Options{Paths: p, Domain: "s.dev", NoSecrets: noSecrets, Stderr: &errBuf})
	return errBuf.String(), err
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSplitNamingShapingAndGitignore(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), splitArtifact)

	stderr, err := runSplit(t, p, false)
	if err != nil {
		t.Fatalf("split failed: %v (%s)", err, stderr)
	}
	outDir := filepath.Join(domainDir, "artifacts")
	want := []string{".gitignore", "ClusterRole.cr.yaml", "ConfigMap.ns1.cm.yaml", "Job.ns1.jb.yaml", "Secret.ns1.sec.sops.yaml"}
	got := dirNames(t, outDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("split dir = %v, want %v", got, want)
	}

	job, _ := os.ReadFile(filepath.Join(outDir, "Job.ns1.jb.yaml"))
	if bytes.Contains(job, []byte("ttlSecondsAfterFinished")) {
		t.Error("split Job must lose ttlSecondsAfterFinished (TTL-reaped Jobs loop a reconciler)")
	}
	if !bytes.Contains(job, []byte(`kustomize.toolkit.fluxcd.io/force: enabled`)) {
		t.Error("split Job must gain the flux force annotation (fixed-name Jobs are immutable)")
	}

	gi, _ := os.ReadFile(filepath.Join(outDir, ".gitignore"))
	if string(gi) != gitignoreContent {
		t.Errorf(".gitignore = %q", gi)
	}

	sec, _ := os.ReadFile(filepath.Join(outDir, "Secret.ns1.sec.sops.yaml"))
	if !bytes.Contains(sec, []byte("\nsops:")) && !bytes.HasPrefix(sec, []byte("sops:")) {
		t.Errorf("secret twin must carry sops metadata: %q", sec)
	}

	// No stage dir may survive.
	if stale, _ := filepath.Glob(filepath.Join(domainDir, ".artifacts-stage.*")); len(stale) > 0 {
		t.Errorf("stage dirs left behind: %v", stale)
	}
}

func TestSplitNoArtifact(t *testing.T) {
	p, domainDir := splitProject(t)
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("split without artifacts.yaml must fail")
	}
	want := "no " + filepath.Join(domainDir, "artifacts.yaml") + " to split — build first"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestSplitCollision(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"),
		"apiVersion: acme.io/v1\nkind: Widget\nmetadata:\n  name: w\n---\napiVersion: other.io/v1\nkind: Widget\nmetadata:\n  name: w\n")
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("cross-group name collision must refuse")
	}
	want := "split: 2 non-Secret documents rendered but 1 files emitted — kind/namespace/name collision across API groups; not supported"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts")); !os.IsNotExist(err) {
		t.Error("a refused split must not create/replace the live dir")
	}
}

func TestSplitRecipientGate(t *testing.T) {
	p, domainDir := splitProject(t)
	// Drop the age recipients from the spec.
	writeFileT(t, filepath.Join(domainDir, "cluster.lok8s.yaml"),
		"kind: Lo\nspec:\n  build:\n    artifacts: split\n  gitops:\n    provider: flux\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), splitArtifact)
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("Secrets without recipients must refuse before any write")
	}
	want := "split: 1 Secret(s) in the render but no spec.gitops.age recipients — refusing to write plaintext Secrets. Declare the age public keys (reconciler key + break-glass) in the spec."
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts")); !os.IsNotExist(err) {
		t.Error("recipient gate must fire BEFORE any file is written")
	}
}

func TestSplitBadAgeKey(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "cluster.lok8s.yaml"),
		"kind: Lo\nspec:\n  build:\n    artifacts: split\n  gitops:\n    age:\n      - ssh-ed25519 AAAA\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), splitArtifact)
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("non-age recipient must refuse")
	}
	if want := "split: 'ssh-ed25519 AAAA' is not an age public key (spec.gitops.age)"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestSplitTrustNothingVerify(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), splitArtifact)
	t.Setenv("SOPS_STUB_PLAINTEXT", "1")
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("a masked encrypt failure (no sops metadata) must abort")
	}
	if want := "split: Secret.ns1.sec.sops.yaml missing or not sops-encrypted — aborting"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(filepath.Join(domainDir, "artifacts")); !os.IsNotExist(err) {
		t.Error("aborted split must not touch the live dir")
	}
}

func TestSplitNoSecretsPruneGuard(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), splitArtifact)
	outDir := filepath.Join(domainDir, "artifacts")
	// Pre-seed a committed layout: an encrypted Secret twin (must survive
	// --no-secrets), a stale generated file (must be pruned), and env-owned
	// lowercase files (never touched).
	writeFileT(t, filepath.Join(outDir, "Secret.ns1.old.sops.yaml"), "data: x\nsops:\n  age: []\n")
	writeFileT(t, filepath.Join(outDir, "StaleKind.gone.yaml"), "kind: StaleKind\n")
	writeFileT(t, filepath.Join(outDir, "kustomization.yaml"), "resources: []\n")
	writeFileT(t, filepath.Join(outDir, ".cache-queue"), "\n")

	stderr, err := runSplit(t, p, true)
	if err != nil {
		t.Fatalf("no-secrets split failed: %v (%s)", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(outDir, "Secret.ns1.old.sops.yaml")); err != nil {
		t.Error("--no-secrets must leave committed Secret.*.sops.yaml inert (never pruned)")
	}
	if _, err := os.Stat(filepath.Join(outDir, "Secret.ns1.sec.sops.yaml")); !os.IsNotExist(err) {
		t.Error("--no-secrets must not CREATE Secret twins")
	}
	if _, err := os.Stat(filepath.Join(outDir, "StaleKind.gone.yaml")); !os.IsNotExist(err) {
		t.Error("stale generated files must still be pruned under --no-secrets")
	}
	if _, err := os.Stat(filepath.Join(outDir, "kustomization.yaml")); err != nil {
		t.Error("lowercase env-owned files must never be touched")
	}
	if _, err := os.Stat(filepath.Join(outDir, ".cache-queue")); err != nil {
		t.Error("dotfiles must never be touched")
	}
	if _, err := os.Stat(filepath.Join(outDir, "ConfigMap.ns1.cm.yaml")); err != nil {
		t.Error("non-Secret resources must still be emitted under --no-secrets")
	}
}

func TestSplitNormalModePrunesDroppedSecrets(t *testing.T) {
	p, domainDir := splitProject(t)
	outDir := filepath.Join(domainDir, "artifacts")
	writeFileT(t, filepath.Join(outDir, "Secret.ns1.dropped.sops.yaml"), "data: x\nsops:\n  age: []\n")
	// Render carries NO Secrets → the unguarded sweep prunes the dropped twin.
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"),
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: only\n  namespace: ns1\n")
	stderr, err := runSplit(t, p, false)
	if err != nil {
		t.Fatalf("split failed: %v (%s)", err, stderr)
	}
	if _, err := os.Stat(filepath.Join(outDir, "Secret.ns1.dropped.sops.yaml")); !os.IsNotExist(err) {
		t.Error("a Secret dropped from the render must be pruned in a normal build (presence decides pruning)")
	}
}

func TestSplitResidueGuard(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"),
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n  namespace: ns1\ndata:\n  bad: ${LOK8S_SPEC_NEVER_SET}\n")
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("unrendered LOK8S_ residue must abort the split")
	}
	if want := "split: unrendered ${LOK8S_*} residue in the staged output — check the envsubst whitelist/env"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestSplitRejectsNonRFC1123Secret(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"),
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: Bad_Name\n  namespace: ns1\nstringData: {}\n")
	stderr, err := runSplit(t, p, false)
	if err == nil {
		t.Fatal("non-RFC1123 Secret metadata must refuse")
	}
	if want := "split: refusing Secret with non-RFC1123 metadata: ns='ns1' name='Bad_Name'"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

func TestSplitSecretsOnlyRender(t *testing.T) {
	p, domainDir := splitProject(t)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"),
		"apiVersion: v1\nkind: Secret\nmetadata:\n  name: only\n  namespace: ns1\nstringData:\n  k: v\n")
	stderr, err := runSplit(t, p, false)
	if err != nil {
		t.Fatalf("secrets-only split failed: %v (%s)", err, stderr)
	}
	got := dirNames(t, filepath.Join(domainDir, "artifacts"))
	want := []string{".gitignore", "Secret.ns1.only.sops.yaml"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("split dir = %v, want %v (the empty non-Secret stream must not emit a junk '.yml')", got, want)
	}
}

func TestCanonicalYAMLSortsAndNormalizes(t *testing.T) {
	a, err := canonicalYAML([]byte("b: 2\na: {z: 9, y: 8}\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalYAML([]byte("a:\n  y: 8\n  z: 9\nb: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("canonical forms differ:\n%q\n%q", a, b)
	}
	if _, err := canonicalYAML([]byte(": : :\n")); err == nil {
		t.Error("unparsable YAML must error (treated as changed)")
	}
}
