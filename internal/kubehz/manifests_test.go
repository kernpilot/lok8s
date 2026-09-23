package kubehz

// manifests_test.go pins the embedded manifest tree to the bash tree
// (.lok8s/libs/kubehz/manifests/**) byte for byte, in BOTH directions: a
// file edited on one side without the other fails here. The digest-pin and
// RBAC least-privilege gates of kubehz_live_agent_test.bats keep running
// against the bash tree, and this equality is what extends them to the
// embedded copy.

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestEmbeddedManifestsMatchBashTree(t *testing.T) {
	t.Parallel()
	bashTree := filepath.Join(testutil.RepoRoot(t), ".lok8s", "libs", "kubehz", "manifests")
	if _, err := os.Stat(bashTree); err != nil {
		t.Skipf("bash tree not present: %v", err)
	}
	testutil.Drift{
		Want: testutil.ReadFS(t, "internal/kubehz/manifests", Manifests(), nil),
		Got:  testutil.ReadDir(t, ".lok8s/libs/kubehz/manifests", bashTree, nil),
		Sync: "copy the file to the other side; there is no sync script for the kubehz manifests",
	}.Check(t)
	for _, must := range []string{"agent/cronjob.yaml", "live-agent/UPSTREAM.sha256", "live-agent/managed/rbac-managed.yaml"} {
		if _, err := fs.Stat(Manifests(), must); err != nil {
			t.Fatalf("%s missing from the embed", must)
		}
	}
}

func TestEmbeddedManifestsCarryOnlyTheThreePlaceholders(t *testing.T) {
	t.Parallel()
	want := map[string]int{"KUBEHZ_API_URL_PLACEHOLDER": 2, "CLUSTER_ID_PLACEHOLDER": 2, "HEARTBEAT_OWNER_PLACEHOLDER": 1}
	got := map[string]int{}
	_ = fs.WalkDir(Manifests(), ".", func(path string, d fs.DirEntry, err error) error {
		if d.IsDir() {
			return nil
		}
		raw, _ := fs.ReadFile(Manifests(), path)
		for token := range want {
			got[token] += countNonComment(string(raw), token)
		}
		return nil
	})
	for token, n := range want {
		if got[token] != n {
			t.Fatalf("%s: %d occurrences, want %d", token, got[token], n)
		}
	}
}

func countNonComment(s, token string) int {
	n := 0
	for _, line := range splitLines(s) {
		trimmed := trimLeftSpace(line)
		if len(trimmed) > 0 && trimmed[0] == '#' {
			continue
		}
		if contains(line, token) {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func trimLeftSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	return s
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// B242: a self-hosted cluster can be control-plane-only, or its workers can
// join after the deploy; both agent pods must tolerate the control-plane
// taint or the cluster reports nothing. The manifests are parsed and the
// tolerations read at the pod spec, so a block that is commented out, or
// placed at the wrong level, fails here.
func TestAgentManifestsTolerateTheControlPlaneTaint(t *testing.T) {
	t.Parallel()
	root := filepath.Join(testutil.RepoRoot(t), "internal", "assets", "lok8s", "libs", "kubehz", "manifests")
	cases := []struct {
		rel  string
		path []string // the pod spec under the document's spec
	}{
		{"live-agent/base/deployment.yaml", []string{"spec", "template", "spec"}},
		{"agent/cronjob.yaml", []string{"spec", "jobTemplate", "spec", "template", "spec"}},
	}
	for _, tc := range cases {
		b, err := os.ReadFile(filepath.Join(root, tc.rel))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: %v", tc.rel, err)
		}
		var node any = doc
		for _, k := range tc.path {
			m, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("%s: no map at %q", tc.rel, k)
			}
			node = m[k]
		}
		podSpec, ok := node.(map[string]any)
		if !ok {
			t.Fatalf("%s: no pod spec at %v", tc.rel, tc.path)
		}
		tols, _ := podSpec["tolerations"].([]any)
		seen := map[string]bool{}
		for _, raw := range tols {
			tol, _ := raw.(map[string]any)
			if tol["operator"] == "Exists" && tol["effect"] == "NoSchedule" {
				if key, _ := tol["key"].(string); key != "" {
					seen[key] = true
				}
			}
		}
		for _, key := range []string{"node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master"} {
			if !seen[key] {
				t.Errorf("%s: no Exists/NoSchedule toleration for %s at the pod spec", tc.rel, key)
			}
		}
	}
}
