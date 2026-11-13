package build

// keysonly_test.go — `lo build --keys-only` (KeysOnly) and the output
// guard of Artifacts. The render is the stub kustomize of
// artifacts_test.go (exec pipeline); it logs the Secret plugin's
// keys-only and store-free settings it was given.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/plugins/secret"
)

// A render that holds one Secret of the plugin's keys-only run.
const keysOnlyRender = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\ndata:\n  USER: \"\"\nkind: Secret\nmetadata:\n  annotations:\n    " +
	secret.KeysOnlyAnnotation + ": \"true\"\n  name: app\n  namespace: demo\ntype: Opaque\n"

func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tmp.") || strings.HasPrefix(e.Name(), ".artifacts-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// --keys-only renders with the keys-only run and the store-free switch on,
// writes the render to stdout and nothing to the domain dir: a prior
// artifacts.yaml, the split dir and a pre-domain per-target dir stay as
// they were.
func TestKeysOnlyRendersToStdoutOnly(t *testing.T) {
	p, domainDir := artifactsProject(t)
	stubRender(t, p, keysOnlyRender)
	log := filepath.Join(p.Base, "kustomize-keys.log")
	t.Setenv("KUSTOMIZE_STUB_KEYSLOG", log)
	writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "ConfigMap.demo.a.yaml"), "kind: ConfigMap\n")
	writeFileT(t, filepath.Join(domainDir, "artifacts", "old", "artifacts.yaml"), "kind: Old\n")

	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err != nil {
		t.Fatalf("KeysOnly: %v\n%s", err, stderr.String())
	}
	if stdout.String() != keysOnlyRender {
		t.Errorf("stdout = %q, want the render", stdout.String())
	}
	if !strings.Contains(stderr.String(), "lo build: d.dev rendered 2 document(s) -> stdout (keys only)\n") {
		t.Errorf("stderr = %q", stderr.String())
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(raw)); got != "keys-only=1 disable=1" {
		t.Errorf("the kustomize child saw %q, want keys-only=1 disable=1", got)
	}
	for rel, want := range map[string]string{
		"artifacts.yaml":                  "kind: Prior\n",
		"artifacts/ConfigMap.demo.a.yaml": "kind: ConfigMap\n",
		"artifacts/old/artifacts.yaml":    "kind: Old\n",
	} {
		got, err := os.ReadFile(filepath.Join(domainDir, rel))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want it untouched", rel, got, err)
		}
	}
	if left := leftovers(t, domainDir); len(left) != 0 {
		t.Errorf("temp files left in the domain dir: %v", left)
	}
}

// --keys-only reads no store, so a domain without its own store in a
// project that keeps one per domain renders (like --no-secrets).
func TestKeysOnlyNeedsNoStore(t *testing.T) {
	p, _ := artifactsProject(t)
	stubRender(t, p, twoDocRender)
	writeFileT(t, filepath.Join(p.Clusters, "other.dev", "secrets", ".gitignore"), "*\n")
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err != nil {
		t.Fatalf("KeysOnly: %v\n%s", err, stderr.String())
	}
	if stdout.String() != twoDocRender {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// A failed render prints nothing on stdout.
func TestKeysOnlyFailedRenderPrintsNothing(t *testing.T) {
	p, _ := artifactsProject(t)
	stubRender(t, p, twoDocRender)
	t.Setenv("KUSTOMIZE_STUB_RC", "1")
	var stdout, stderr bytes.Buffer
	if err := KeysOnly(t.Context(), Options{Paths: p, Domain: "d.dev", Stderr: &stderr}, &stdout); err == nil {
		t.Fatal("a failed render succeeded")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "[error] kustomize build failed for d.dev") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// The output guard: a render that holds an object with the keys-only
// marker is never written into the domain's artifacts. The prior
// artifacts.yaml stays, no split is made, no temp file is left.
func TestArtifactsRefusesTheKeysOnlyMarker(t *testing.T) {
	for _, mode := range []string{"single", "split"} {
		t.Run(mode, func(t *testing.T) {
			p, domainDir := artifactsProject(t)
			stubRender(t, p, keysOnlyRender)
			writeFileT(t, filepath.Join(domainDir, "artifacts.yaml"), "kind: Prior\n")
			split := "0"
			if mode == "split" {
				split = "1"
			}
			var stderr bytes.Buffer
			err := Artifacts(t.Context(), Options{Paths: p, Domain: "d.dev", SplitOverride: split, NoSecrets: true, Stderr: &stderr})
			if err == nil {
				t.Fatal("a render with the keys-only marker was written")
			}
			want := "[error] refusing to write d.dev's render to " + filepath.Join(domainDir, "artifacts.yaml") +
				": it holds a keys-only Secret (secrets.lok8s.dev/keys-only); keys-only output goes to stdout only (lo build --keys-only)\n"
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr = %q\nwant it to hold %q", stderr.String(), want)
			}
			if got, _ := os.ReadFile(filepath.Join(domainDir, "artifacts.yaml")); string(got) != "kind: Prior\n" {
				t.Errorf("artifacts.yaml = %q, want the prior content", got)
			}
			if _, err := os.Stat(filepath.Join(domainDir, "artifacts")); err == nil {
				t.Error("a split dir was written")
			}
			if left := leftovers(t, domainDir); len(left) != 0 {
				t.Errorf("temp files left in the domain dir: %v", left)
			}
		})
	}
}

// The marker is an indented annotation key line; the same text elsewhere
// (a value, a column-0 key) is not the marker.
func TestHasKeysOnlyMarker(t *testing.T) {
	for in, want := range map[string]bool{
		"metadata:\n  annotations:\n    secrets.lok8s.dev/keys-only: \"true\"\n": true,
		"metadata:\n\tsecrets.lok8s.dev/keys-only: x\n":                          true,
		"data:\n  note: secrets.lok8s.dev/keys-only: x\n":                        false,
		"secrets.lok8s.dev/keys-only: x\n":                                       false,
		"metadata:\n  annotations:\n    secrets.lok8s.dev/keys-only-not: x\n":    false,
		twoDocRender: false,
	} {
		if got := hasKeysOnlyMarker([]byte(in)); got != want {
			t.Errorf("hasKeysOnlyMarker(%q) = %v, want %v", in, got, want)
		}
	}
}
