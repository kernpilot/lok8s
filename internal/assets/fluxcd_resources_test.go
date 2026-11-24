package assets

// fluxcd_resources_test.go: the memory limit of the Flux controllers in the
// fluxcd addon. With a limit of 256Mi, the source controller got OOMKilled
// at the artifact build of each new revision of a Git repository of about
// 60 MB. bats: "the fluxcd addon gives the source, kustomize and helm
// controllers a 1Gi memory limit" in tests/unit/fluxcd_addon_test.bats.

import (
	"io/fs"
	"testing"

	"gopkg.in/yaml.v3"
)

// fluxControllerMemoryLimit is the memory limit that the fluxcd addon gives
// the source, kustomize and helm controllers.
const fluxControllerMemoryLimit = "1Gi"

type fluxControllerValues struct {
	Resources struct {
		Limits struct {
			Memory string `yaml:"memory"`
		} `yaml:"limits"`
	} `yaml:"resources"`
}

func TestFluxAddonControllersHaveTheMemoryLimit(t *testing.T) {
	t.Parallel()
	raw, err := fs.ReadFile(FS(), fluxAddonValues)
	if err != nil {
		t.Fatal(err)
	}
	var values struct {
		SourceController    fluxControllerValues `yaml:"sourceController"`
		KustomizeController fluxControllerValues `yaml:"kustomizeController"`
		HelmController      fluxControllerValues `yaml:"helmController"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("%s: %v", fluxAddonValues, err)
	}
	for _, c := range []struct {
		key string
		v   fluxControllerValues
	}{
		{"sourceController", values.SourceController},
		{"kustomizeController", values.KustomizeController},
		{"helmController", values.HelmController},
	} {
		if got := c.v.Resources.Limits.Memory; got != fluxControllerMemoryLimit {
			t.Errorf("%s: %s.resources.limits.memory = %q, want %q", fluxAddonValues, c.key, got, fluxControllerMemoryLimit)
		}
	}
}
