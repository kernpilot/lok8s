package addons

import (
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/assets"
	"github.com/kernpilot/lok8s/internal/toolchain"
)

// sucKubectlRanges is the range of each tag of rancher/kuberlr-kubectl that
// the addon pinned: the kubectl releases that the image bundles (its
// kubectl-versions.txt at the tag), each with one minor of skew. Add the
// row of a new tag before the bump.
var sucKubectlRanges = map[string][2]int{
	// kubectl v1.34.8, v1.35.5 and v1.36.1.
	"v8.1.1": {33, 37},
}

// The range follows the pin: a tag without a row, or a range that is not
// the row of the tag, fails.
func TestSUCKubectlRangeMatchesThePin(t *testing.T) {
	raw, err := fs.ReadFile(assets.FS(), "addons/"+SUCAddon+"/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		Kubectl struct {
			Image struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
			} `yaml:"image"`
		} `yaml:"kubectl"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatal(err)
	}
	if got := values.Kubectl.Image.Repository; got != "rancher/kuberlr-kubectl" {
		t.Fatalf("kubectl.image.repository = %q: the range is of rancher/kuberlr-kubectl", got)
	}
	tag, _, _ := strings.Cut(values.Kubectl.Image.Tag, "@")
	want, ok := sucKubectlRanges[tag]
	if !ok {
		t.Fatalf("sucKubectlRanges has no row for the pinned tag %s: add the range that the tag serves", tag)
	}
	if got := [2]int{SUCKubectlMinMinor, SUCKubectlMaxMinor}; got != want {
		t.Errorf("the range is 1.%d to 1.%d, but the tag %s serves 1.%d to 1.%d", got[0], got[1], tag, want[0], want[1])
	}
}

// A Lo cluster without a version runs the default node image of the pinned
// kind. Doctor tells that it is in the range, so it must be.
func TestSUCKubectlRangeServesTheKindDefault(t *testing.T) {
	state, minor := SUCKubectl(toolchain.KindDefaultNodeVersion)
	if state != SUCKubectlInRange {
		t.Errorf("the default node image %s of kind %s (Kubernetes %s) is not in the range %s", toolchain.KindDefaultNodeVersion, toolchain.KindVersion, minor, SUCKubectlRange())
	}
}

func TestSUCKubectl(t *testing.T) {
	for _, c := range []struct {
		version string
		state   SUCKubectlState
		minor   string
	}{
		{"v1.31.12", SUCKubectlOutOfRange, "1.31"},
		{"v1.32.9", SUCKubectlOutOfRange, "1.32"},
		{"v1.33.0", SUCKubectlInRange, "1.33"},
		{"1.35.5", SUCKubectlInRange, "1.35"},
		{"v1.35", SUCKubectlInRange, "1.35"},
		{"v1.36.1@sha256:" + strings.Repeat("a", 64), SUCKubectlInRange, "1.36"},
		{"v1.37.2", SUCKubectlInRange, "1.37"},
		{"v1.38.0", SUCKubectlOutOfRange, "1.38"},
		{"v2.35.0", SUCKubectlOutOfRange, "2.35"},
		{"v1.100.0", SUCKubectlOutOfRange, "1.100"},
		{"latest", SUCKubectlNotAVersion, ""},
		{"v1", SUCKubectlNotAVersion, ""},
		{"v1.035.0", SUCKubectlNotAVersion, ""},
		{"v1.1000.0", SUCKubectlNotAVersion, ""},
		{"v1.35.0-rc.1", SUCKubectlNotAVersion, ""},
		{"V1.35.0", SUCKubectlNotAVersion, ""},
		{"v1.35.0@sha256:abc", SUCKubectlNotAVersion, ""},
		{" v1.35.0", SUCKubectlNotAVersion, ""},
	} {
		state, minor := SUCKubectl(c.version)
		if state != c.state || minor != c.minor {
			t.Errorf("SUCKubectl(%q) = %d %q, want %d %q", c.version, state, minor, c.state, c.minor)
		}
	}
}
