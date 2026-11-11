package cli

// cmd_build_test.go — `lo build` and the one-document rule. Bash twin: the
// "One YAML document" block of tests/unit/domain_build_test.bats.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// A spec file with more than one YAML document reads as two different specs
// in the two implementations, so `lo build` stops before it renders: the
// domain's own spec (cluster or deploy) before the announcement, the cluster
// spec that a deploy spec names at the kubeconfig step. Nothing is written.
func TestBuildRefusesASecondDocument(t *testing.T) {
	t.Setenv("DOMAIN_NAME", "")
	t.Setenv("KUBECONFIG", "")
	const twoDocs = "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Lo\nmetadata:\n  name: fixture\n---\n"
	const deploy = "apiVersion: cluster.lok8s.dev/v1beta1\nkind: Deploy\nmetadata:\n  name: fixture\nspec:\n  clusterRef:\n    domain: ref.dev\n"
	for _, tc := range []struct {
		name  string
		files map[string]string
		bad   string // the spec file the error names
		want  string // the stderr after "[error] <bad>"
		intro bool   // the "lo build: domain" line comes first
	}{
		{"cluster spec", map[string]string{"a.dev/cluster.lok8s.yaml": twoDocs}, "a.dev/cluster.lok8s.yaml",
			" holds more than one YAML document. Remove line 5 (a '---' line after the spec).\n", false},
		{"deploy spec", map[string]string{"a.dev/deploy.lok8s.yaml": twoDocs}, "a.dev/deploy.lok8s.yaml",
			" holds more than one YAML document. Remove line 5 (a '---' line after the spec).\n", false},
		{"the cluster spec that a deploy spec names", map[string]string{
			"a.dev/deploy.lok8s.yaml":    deploy,
			"ref.dev/cluster.lok8s.yaml": "kind: Lo\n---\n",
		}, "ref.dev/cluster.lok8s.yaml",
			" holds more than one YAML document. Remove line 2 (a '---' line after the spec).\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := synthProject(t)
			for rel, body := range tc.files {
				testutil.WriteFile(t, filepath.Join(p.Clusters, rel), body)
			}
			testutil.WriteFile(t, filepath.Join(p.Clusters, "a.dev", "kustomization.yaml"), "resources: []\n")
			var stderr bytes.Buffer
			err := runBuild(t.Context(), p, buildOpts{domainFlag: "a.dev"}, &stderr)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("err = %v", err)
			}
			want := "[error] " + filepath.Join(p.Clusters, tc.bad) + tc.want
			if tc.intro {
				want = "lo build: domain a.dev\n" + want
			}
			if stderr.String() != want {
				t.Errorf("stderr = %q\nwant     %q", stderr.String(), want)
			}
			if _, err := os.Stat(filepath.Join(p.Clusters, "a.dev", "artifacts.yaml")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the build wrote artifacts.yaml (stat: %v)", err)
			}
		})
	}
}
