package addons

// mailpit_image_test.go: the mailpit addon pins its image by digest. The
// test is offline: it reads the chart, it does not render it, and it does
// not ask the registry. So it cannot see a digest that is not the digest of
// the tag, or a digest of one platform only: check both on a bump.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

const mailpitAddon = "internal/assets/lok8s/addons/mailpit"

// readMailpitYAML decodes one YAML file of the addon into out.
func readMailpitYAML(t *testing.T, path string, out any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), mailpitAddon, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

// TestMailpitImagePin: the tag in the chart values holds a digest, the
// appVersion names the version of the tag, the one image line of the
// templates renders repository and tag unchanged, and no values file and no
// transformer of the addon replaces the pin.
func TestMailpitImagePin(t *testing.T) {
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
	}
	readMailpitYAML(t, "chart/values.yaml", &values)
	m := imageDigestTag.FindStringSubmatch(values.Image.Tag)
	if values.Image.Repository == "" || m == nil {
		t.Fatalf("chart/values.yaml: image %q tag %q is not `<version>@sha256:<64 hex>`", values.Image.Repository, values.Image.Tag)
	}

	var chart struct {
		AppVersion string `yaml:"appVersion"`
	}
	readMailpitYAML(t, "chart/Chart.yaml", &chart)
	if chart.AppVersion != m[1] {
		t.Errorf("chart/Chart.yaml: appVersion %q, the image tag is %q", chart.AppVersion, m[1])
	}

	addonDir := filepath.Join(testutil.RepoRoot(t), mailpitAddon)
	templates, err := filepath.Glob(filepath.Join(addonDir, "chart/templates/*"))
	if err != nil {
		t.Fatal(err)
	}
	const ref = "image: {{ .Values.image.repository }}:{{ .Values.image.tag }}\n"
	refs, images := 0, 0
	for _, f := range templates {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		refs += strings.Count(string(raw), ref)
		images += strings.Count(string(raw), "image:")
	}
	if refs != 1 || images != 1 {
		t.Errorf("chart/templates: %d image lines, %d of them %q, want 1 and 1 (each image needs its own pin)", images, refs, strings.TrimSpace(ref))
	}

	// values.yaml, and values.<driver>.yaml and values.<provider>.yaml.
	overrides, err := filepath.Glob(filepath.Join(addonDir, "values*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(overrides) == 0 {
		t.Error("no values*.yaml in the addon: the override check reads nothing")
	}
	for _, f := range overrides {
		var addon map[string]any
		readMailpitYAML(t, filepath.Base(f), &addon)
		if _, ok := addon["image"]; ok {
			t.Errorf("%s sets image: it replaces the pin of the chart values", filepath.Base(f))
		}
	}
	var kustomization map[string]any
	readMailpitYAML(t, "kustomization.yaml", &kustomization)
	if _, ok := kustomization["images"]; ok {
		t.Error("kustomization.yaml sets images: the transformer can replace the pin")
	}
}
