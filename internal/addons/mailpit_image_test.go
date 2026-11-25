package addons

// mailpit_image_test.go: the mailpit addon pins its image by digest. The
// test is offline: it reads the chart, it does not render it.

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
// template renders repository and tag unchanged, the appVersion names the
// version of the tag, and the addon values do not replace the pin.
func TestMailpitImagePin(t *testing.T) {
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
	}
	readMailpitYAML(t, "chart/values.yaml", &values)
	m := sucDigestTag.FindStringSubmatch(values.Image.Tag)
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

	raw, err := os.ReadFile(filepath.Join(testutil.RepoRoot(t), mailpitAddon, "chart/templates/deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	const ref = "image: {{ .Values.image.repository }}:{{ .Values.image.tag }}\n"
	if got := strings.Count(string(raw), ref); got != 1 {
		t.Errorf("chart/templates/deployment.yaml: %d lines %q, want 1", got, strings.TrimSpace(ref))
	}
	if got := strings.Count(string(raw), "image:"); got != 1 {
		t.Errorf("chart/templates/deployment.yaml: %d image lines, want 1 (each image needs its own pin)", got)
	}

	var addon map[string]any
	readMailpitYAML(t, "values.yaml", &addon)
	if _, ok := addon["image"]; ok {
		t.Error("values.yaml sets image: it replaces the pin of the chart values")
	}
}
