package cli

// shim_test.go covers the env the bash shim inherits.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
)

func TestShimEnvPreparesPathAndPluginHome(t *testing.T) {
	p := &config.Paths{
		Base:  "/proj",
		Bin:   "/proj/.bin",
		Lok8s: "/proj/.lok8s",
	}
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("KUSTOMIZE_PLUGIN_HOME", "")
	os.Unsetenv("KUSTOMIZE_PLUGIN_HOME")

	env := shimEnv(p)
	var path, pluginHome string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
		if v, ok := strings.CutPrefix(kv, "KUSTOMIZE_PLUGIN_HOME="); ok {
			pluginHome = v
		}
	}
	if want := "/proj/.bin:/proj/.lok8s:/usr/bin"; path != want {
		t.Errorf("PATH = %q, want %q", path, want)
	}
	if want := filepath.Join("/proj", ".kustomize"); pluginHome != want {
		t.Errorf("KUSTOMIZE_PLUGIN_HOME = %q, want %q", pluginHome, want)
	}

	// Already-present entries are not duplicated.
	t.Setenv("PATH", "/proj/.bin:/usr/bin")
	env = shimEnv(p)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if strings.Count(v, "/proj/.bin") != 1 {
				t.Errorf("PATH duplicated .bin entry: %q", v)
			}
		}
	}
}
