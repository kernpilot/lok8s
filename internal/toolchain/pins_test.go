package toolchain

// pins_test.go — the drift gate. The pins in pins.go must equal what
// go.mod links (lo-full renders through those modules) and must be what
// the generated .bin/b.yaml installs (lo core execs those binaries).
// Bumping any one side alone fails here.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goModRequire reads the version go.mod requires for module (direct or
// indirect).
func goModRequire(t *testing.T, module string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(wd, "..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(module) + `\s+(v\S+)`)
	m := re.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("go.mod does not require %s", module)
	}
	return m[1]
}

func TestPinsMatchGoMod(t *testing.T) {
	if got := goModRequire(t, "sigs.k8s.io/kustomize/api"); got != KustomizeAPI {
		t.Errorf("go.mod sigs.k8s.io/kustomize/api = %s, pins.go KustomizeAPI = %s — bump both (and kustomizeAPIToCLI + the b.yaml template)", got, KustomizeAPI)
	}
	if got := goModRequire(t, "github.com/mgoltzsche/khelm/v2"); got != "v"+KhelmVersion {
		t.Errorf("go.mod github.com/mgoltzsche/khelm/v2 = %s, pins.go KhelmVersion = v%s — bump both (the ChartRenderer binary the template pins must be the library release)", got, KhelmVersion)
	}
	if got := goModRequire(t, "helm.sh/helm/v3"); got != "v"+HelmVersion {
		t.Errorf("go.mod helm.sh/helm/v3 = %s, pins.go HelmVersion = v%s (khelm v%s's requirement)", got, HelmVersion, KhelmVersion)
	}
}

func TestKustomizeCLIMatchesAPIMapping(t *testing.T) {
	cli, ok := kustomizeAPIToCLI[KustomizeAPI]
	if !ok {
		t.Fatalf("kustomizeAPIToCLI has no entry for api %s — add the CLI release built from it", KustomizeAPI)
	}
	if cli != KustomizeCLI {
		t.Fatalf("KustomizeCLI = %s but api %s maps to %s", KustomizeCLI, KustomizeAPI, cli)
	}
}

// TestTemplateCarriesThePins: the generated b.yaml installs exactly the
// pinned releases, at the plugin paths the exec render resolves.
func TestTemplateCarriesThePins(t *testing.T) {
	tpl := Template(TemplateOptions{Name: "t", LoVersion: "0.3.0", Variant: "core"})
	pins := PinnedEntries("0.3.0")
	want := map[string]string{
		"kustomize":                   KustomizeCLI,
		"github.com/mgoltzsche/khelm": "v" + KhelmVersion,
		"github.com/kernpilot/lok8s":  "v0.3.0",
	}
	for k, v := range want {
		if pins[k] != v {
			t.Errorf("template pins %s = %q, want %q", k, pins[k], v)
		}
	}
	for _, s := range []string{
		"  kustomize:\n    version: " + KustomizeCLI + "\n",
		"  github.com/mgoltzsche/khelm:\n    version: v" + KhelmVersion + "\n    file: ../.kustomize/" + ChartRendererPluginRel + "\n",
		"  github.com/kernpilot/lok8s:\n    version: v0.3.0\n    asset: kustomize-secret-*\n    file: ../.kustomize/" + SecretPluginRel + "\n",
	} {
		if !strings.Contains(tpl, s) {
			t.Errorf("template lacks:\n%s\n--- template:\n%s", s, tpl)
		}
	}
}
