package addons

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A builtin chart generator (khelm's ChartRenderer) is waved through.
func TestScanExecPluginsPassesBuiltinChartRenderer(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\ngenerators:\n  - chart.yaml\n")
	writeFile(t, filepath.Join(dir, "chart.yaml"), "apiVersion: khelm.mgoltzsche.github.com/v2\nkind: ChartRenderer\nmetadata:\n  name: x\n")
	refs, err := ScanExecPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none", refs)
	}
}

// A builtin secrets generator (secrets.lok8s.dev/v1 Secret) is waved through.
func TestScanExecPluginsPassesBuiltinSecret(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\ngenerators:\n  - Secret.app.default.yaml\n")
	writeFile(t, filepath.Join(dir, "Secret.app.default.yaml"), "apiVersion: secrets.lok8s.dev/v1\nkind: Secret\nmetadata:\n  name: app\n")
	refs, err := ScanExecPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none", refs)
	}
}

// Review round 2, item C: a generator that is NOT one of lok8s' own two
// builtins is flagged, naming its apiVersion+kind.
func TestScanExecPluginsFlagsUnknownGenerator(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\ngenerators:\n  - evil.yaml\n")
	writeFile(t, filepath.Join(dir, "evil.yaml"), "apiVersion: evil.example.com/v1\nkind: EvilGenerator\nmetadata:\n  name: x\n")
	refs, err := ScanExecPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].APIVersion != "evil.example.com/v1" || refs[0].Kind != "EvilGenerator" {
		t.Fatalf("refs = %+v", refs)
	}
}

// transformers: and validators: are scanned the same way as generators:.
func TestScanExecPluginsFlagsUnknownTransformerAndValidator(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\ntransformers:\n  - t.yaml\nvalidators:\n  - v.yaml\n")
	writeFile(t, filepath.Join(dir, "t.yaml"), "apiVersion: evil.example.com/v1\nkind: EvilTransformer\nmetadata:\n  name: x\n")
	writeFile(t, filepath.Join(dir, "v.yaml"), "apiVersion: evil.example.com/v1\nkind: EvilValidator\nmetadata:\n  name: x\n")
	refs, err := ScanExecPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("refs = %+v, want 2", refs)
	}
}

// A sub-kustomization referenced via resources:/bases:/components: is
// recursed into, so a generator one level down is still caught.
func TestScanExecPluginsRecursesIntoSubKustomization(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - base\n")
	writeFile(t, filepath.Join(dir, "base", "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\ngenerators:\n  - evil.yaml\n")
	writeFile(t, filepath.Join(dir, "base", "evil.yaml"), "apiVersion: evil.example.com/v1\nkind: EvilGenerator\nmetadata:\n  name: x\n")
	refs, err := ScanExecPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want 1 (from the base)", refs)
	}
}

// A resources: entry that is a plain manifest (not a kustomization, not a
// plugin config) is skipped — it never executes anything.
func TestScanExecPluginsSkipsPlainManifestResource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - plain.yaml\n")
	writeFile(t, filepath.Join(dir, "plain.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n")
	refs, err := ScanExecPlugins(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("refs = %v, want none", refs)
	}
}

// A resources: cycle (A -> B -> A) must not hang the scan.
func TestScanExecPluginsCycleSafe(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a", "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../b\n")
	writeFile(t, filepath.Join(dir, "b", "kustomization.yaml"), "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../a\n")
	done := make(chan struct{})
	var err error
	go func() {
		_, err = ScanExecPlugins(filepath.Join(dir, "a"))
		close(done)
	}()
	select {
	case <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ScanExecPlugins hung on a resources: cycle")
	}
}
