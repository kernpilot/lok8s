package cli

// cmd_addons_test.go covers `lo addons --detail` and `lo addons show`.

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/testutil"
)

func addonsProject(t *testing.T) *config.Paths {
	t.Helper()
	repo := repoRootDir(t)
	p := synthProject(t)
	// The REAL addon tree so category/version come from the shipped labels.
	p.Lok8s = filepath.Join(repo, ".lok8s")
	return p
}

func TestAddonsDetailInventory(t *testing.T) {
	p := addonsProject(t)
	os.MkdirAll(filepath.Join(p.Clusters, "inv", "targets", "networking"), 0o755)
	testutil.WriteFile(t, filepath.Join(p.Clusters, "inv", "cluster.lok8s.yaml"), `apiVersion: cluster.lok8s.dev/v1beta1
kind: KubeOne
metadata: { name: inv }
spec:
  bootstrap:
    - cilium: { wait: true }
    - cert-manager
    - ./targets/networking: { dependsOn: [cert-manager] }
`)
	stdout, _, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", "inv")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Addons deployed by inv (kind=kubeone)",
		"cilium", "networking", "policyAuditMode",
		"cert-manager", "infrastructure",
		"per-cluster glue in clusters/inv/targets/networking",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}

	// Map-form entry stays ONE addon (no shattering into reserved keys).
	testutil.WriteFile(t, filepath.Join(p.Clusters, "mapform", "cluster.lok8s.yaml"), `kind: KubeOne
metadata: { name: m }
spec:
  bootstrap:
    - ccm:
        values:
          env:
            ROBOT_ENABLED: { value: "true" }
        wait: true
`)
	stdout, _, err = runLo(t, NewRoot(p), "a", "--detail", "--domain", "mapform")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "ccm") || !strings.Contains(stdout, "hcloud CCM") {
		t.Errorf("got:\n%s", stdout)
	}
	if regexp.MustCompile(`(?m)^(values|env|wait|dependsOn)[[:space:]]`).MatchString(stdout) {
		t.Errorf("map entry shattered:\n%s", stdout)
	}

	// Empty bootstrap, missing spec, injected domain, malformed kind.
	testutil.WriteFile(t, filepath.Join(p.Clusters, "empty", "cluster.lok8s.yaml"), "kind: KubeOne\nmetadata: { name: e }\nspec:\n  bootstrap: []\n")
	stdout, _, _ = runLo(t, NewRoot(p), "addons", "--detail", "--domain", "empty")
	if !strings.Contains(stdout, "deploys no addons") {
		t.Errorf("empty:\n%s", stdout)
	}
	_, stderr, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", "nonexistent-domain")
	if err != nil || !strings.Contains(stderr, "nothing to inventory") {
		t.Errorf("missing spec: err=%v stderr=%q", err, stderr)
	}
	for _, d := range []string{"../etc", "/abs", "foo/../bar", "a/b", ".hidden"} {
		stdout, stderr, err := runLo(t, NewRoot(p), "addons", "--detail", "--domain", d)
		if err != nil || !strings.Contains(stderr, "Invalid domain") || strings.Contains(stdout, "Addons deployed by") {
			t.Errorf("%q: err=%v out=%q stderr=%q", d, err, stdout, stderr)
		}
	}
	testutil.WriteFile(t, filepath.Join(p.Clusters, "bad2", "cluster.lok8s.yaml"), "kind: \"a b\"\nmetadata: { name: bad2 }\nspec:\n  bootstrap:\n    - cilium\n")
	stdout, _, err = runLo(t, NewRoot(p), "addons", "--detail", "--domain", "bad2")
	if !errors.Is(err, ErrHandled) || strings.Contains(stdout, "kind=lo") {
		t.Errorf("malformed kind: err=%v out=%q", err, stdout)
	}
}

func TestAddonsShowSeparator(t *testing.T) {
	p := addonsProject(t)
	stdout, _, err := runLo(t, NewRoot(p), "addons", "cilium", "metallb")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "\n\n---\nname:    metallb\n") {
		t.Errorf("separator:\n%s", stdout)
	}
	if !strings.HasPrefix(stdout, "name:    cilium\ndriver:  lo\npath:    "+p.Lok8s+"/addons/cilium\ntype:    khelm\n") {
		t.Errorf("show:\n%s", stdout)
	}
}
