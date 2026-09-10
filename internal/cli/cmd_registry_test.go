package cli

// cmd_registry_test.go covers the `lo registry` driver gate.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

func TestRegistryDriverGate(t *testing.T) {
	p, _, _ := orchestrateProject(t)
	cases := map[string]string{
		"beta.cloud": "error: domain 'beta.cloud' uses the 'kubeone' driver — registry management is a 'lo'-driver (local cluster) feature.\n       Pass --domain <a-lo-domain> or switch with 'lo use <domain>'.\n",
		"gamma.app":  "error: domain 'gamma.app' uses the 'deploy' driver — registry management is a 'lo'-driver (local cluster) feature.\n",
		"nope.dev":   "error: domain 'nope.dev' has no readable cluster/deploy spec under clusters/ — cannot run registry management\n",
	}
	for d, want := range cases {
		for _, verb := range []string{"status", "up", "down", "clean"} {
			_, stderr, err := runLo(t, NewRoot(p), "registry", verb, "--domain", d)
			if !errors.Is(err, ErrHandled) || !strings.Contains(stderr, want) {
				t.Errorf("registry %s --domain %s: err=%v stderr=%q", verb, d, err, stderr)
			}
		}
	}
	// A loaded registry JSON (Tilt subshells) skips the gate entirely.
	rj := filepath.Join(p.Base, "reg.json")
	testutil.WriteFile(t, rj, `{"registries":[]}`)
	t.Setenv("LOK8S_REGISTRY_JSON", rj)
	if err := registryGate(p, "beta.cloud", os.Stderr); err != nil {
		t.Errorf("gate must be skipped with LOK8S_REGISTRY_JSON: %v", err)
	}
}
