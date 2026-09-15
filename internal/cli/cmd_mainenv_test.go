package cli

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// TestApplyMainEnvSetsKubeconfigLikeTheEntrypoint: the bash entrypoint
// exported KUBECONFIG=<project>/.kubeconfig/<cluster>.yaml for every
// command; the binary does the same in applyMainEnv, so `lo up` in a shell
// without KUBECONFIG reads and prints the same file the driver writes.
func TestApplyMainEnvSetsKubeconfigLikeTheEntrypoint(t *testing.T) {
	for _, k := range []string{"KUBECONFIG", "DOMAIN_NAME", "LOK8S_DOMAIN_EXPLICIT", "KIND_EXPERIMENTAL_DOCKER_NETWORK", "LOK8S_FORCE_RECREATE", "LOK8S_REMOTE"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	p := projectPaths(t.TempDir())
	testutil.WriteFile(t, filepath.Join(p.Clusters, "d1", "cluster.lok8s.yaml"), "kind: Lo\nmetadata:\n  name: clu\n")
	if d := applyMainEnv(p, io.Discard, false, false, false, "d1", true, ""); d != "d1" {
		t.Fatalf("domain = %q", d)
	}
	if got, want := os.Getenv("KUBECONFIG"), filepath.Join(p.Base, ".kubeconfig", "clu.yaml"); got != want {
		t.Errorf("KUBECONFIG = %q, want %q", got, want)
	}
}
