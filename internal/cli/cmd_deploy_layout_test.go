package cli

// cmd_deploy_layout_test.go — `lo deploy` and the one-document rule. Bash
// twin: "main::deploy refuses a spec file with a second document" in
// tests/unit/deploy_test.bats.

import (
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// A spec file with more than one YAML document reads as two different specs
// in the two implementations, so `lo deploy` stops before it applies: for
// the domain's own spec (cluster or deploy), and for the cluster spec that a
// deploy spec names. Only the [error] line is printed, and no kubectl runs
// (the artifact is never read).
func TestDeployRefusesASecondDocument(t *testing.T) {
	for _, env := range []string{"DOMAIN_NAME", "LOK8S_DOMAIN_EXPLICIT", "KUBECONFIG", "KIND_EXPERIMENTAL_DOCKER_NETWORK"} {
		t.Setenv(env, "")
	}
	const twoDocs = "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: fixture\n---\n"
	const deploy = "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: fixture\nspec:\n  clusterRef:\n    domain: ref.dev\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		bad   string // the spec file the error names
		want  string // the stderr after "[error] <bad>"
	}{
		{"cluster spec", map[string]string{"a.dev/cluster.lok8s.yaml": twoDocs}, "a.dev/cluster.lok8s.yaml",
			" holds more than one YAML document. Remove line 5 (a '---' line after the spec).\n"},
		{"deploy spec", map[string]string{"a.dev/deploy.lok8s.yaml": twoDocs}, "a.dev/deploy.lok8s.yaml",
			" holds more than one YAML document. Remove line 5 (a '---' line after the spec).\n"},
		{"the cluster spec that a deploy spec names", map[string]string{
			"a.dev/deploy.lok8s.yaml":    deploy,
			"ref.dev/cluster.lok8s.yaml": "kind: Lo\n---\n",
		}, "ref.dev/cluster.lok8s.yaml",
			" holds more than one YAML document. Remove line 2 (a '---' line after the spec).\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := synthProject(t)
			for rel, body := range tc.files {
				testutil.WriteFile(t, filepath.Join(p.Clusters, rel), body)
			}
			_, stderr, err := runLo(t, NewRoot(p), "deploy", "--domain", "a.dev")
			if err == nil {
				t.Fatal("lo deploy passed")
			}
			if want := "[error] " + filepath.Join(p.Clusters, tc.bad) + tc.want; stderr != want {
				t.Errorf("stderr = %q\nwant     %q", stderr, want)
			}
		})
	}
}
