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

	"github.com/kernpilot/lok8s/internal/testutil"
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
	t.Parallel()
	testutil.Drift{
		Want: testutil.Tree{Name: "go.mod", Files: map[string]string{
			"sigs.k8s.io/kustomize/api":      goModRequire(t, "sigs.k8s.io/kustomize/api"),
			"github.com/mgoltzsche/khelm/v2": goModRequire(t, "github.com/mgoltzsche/khelm/v2"),
			"helm.sh/helm/v3":                goModRequire(t, "helm.sh/helm/v3"),
		}},
		Got: testutil.Tree{Name: "pins.go", Files: map[string]string{
			"sigs.k8s.io/kustomize/api":      KustomizeAPI,
			"github.com/mgoltzsche/khelm/v2": "v" + KhelmVersion,
			"helm.sh/helm/v3":                "v" + HelmVersion,
		}},
		Sync: "bump go.mod and internal/toolchain/pins.go together: kustomizeAPIToCLI and the b.yaml template follow KustomizeAPI, the khelm ChartRenderer binary must be the library release, HelmVersion is khelm's requirement",
	}.Check(t)
}

func TestKustomizeCLIMatchesAPIMapping(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	tpl := mustTemplate(t, TemplateOptions{Name: "t", LoVersion: "0.3.0", Variant: "core"})
	testutil.Drift{
		Want: testutil.Tree{Name: "pins.go", Files: map[string]string{
			"kustomize":                   KustomizeCLI,
			"github.com/mgoltzsche/khelm": "v" + KhelmVersion,
			"github.com/kernpilot/lok8s":  "v0.3.0",
		}},
		Got:    testutil.Tree{Name: "the b.yaml template", Files: PinnedEntries("0.3.0")},
		Sync:   "bump internal/toolchain/pins.go and template.go together",
		Subset: true,
	}.Check(t)
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
