package domain

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/testutil"
)

// clusterNameProject is a clusters/ dir with every place a provider
// cluster_name can sit, the shapes `lo init cluster` and the hetzner skill
// write: a hetzner.json next to the spec, a configRef file of another name,
// an inline spec.provider.config, a spec with metadata.name only, and a
// hidden dir.
func clusterNameProject(t *testing.T) string {
	t.Helper()
	clusters := filepath.Join(t.TempDir(), "clusters")
	spec := func(name, provider string) string {
		return "apiVersion: cluster.lok8s.dev/v1beta1\nkind: KubeOne\nmetadata:\n  name: " + name + "\nspec:\n" + provider
	}
	// a.cloud: hetzner.json (configRef), cluster_name prod
	testutil.WriteFile(t, filepath.Join(clusters, "a.cloud", "cluster.lok8s.yaml"),
		spec("a", "  provider:\n    name: hetzner\n    configRef: hetzner.json\n"))
	testutil.WriteFile(t, filepath.Join(clusters, "a.cloud", "hetzner.json"), `{"cluster_name": "prod", "server": []}`)
	// b.cloud: configRef to a YAML file of another name, cluster_name prod
	testutil.WriteFile(t, filepath.Join(clusters, "b.cloud", "cluster.lok8s.yaml"),
		spec("b", "  provider:\n    name: hetzner\n    configRef: infra/desc.yaml\n"))
	testutil.WriteFile(t, filepath.Join(clusters, "b.cloud", "infra", "desc.yaml"), "cluster_name: prod\n")
	// c.cloud: inline config, cluster_name staging
	testutil.WriteFile(t, filepath.Join(clusters, "c.cloud", "cluster.lok8s.yaml"),
		spec("c", "  provider:\n    name: hetzner\n    config:\n      cluster_name: staging\n"))
	// d.dev: no provider; metadata.name prod
	testutil.WriteFile(t, filepath.Join(clusters, "d.dev", "cluster.lok8s.yaml"), spec("prod", "  cluster:\n    domain: d.dev\n"))
	// .hidden: never a domain
	testutil.WriteFile(t, filepath.Join(clusters, ".hidden", "hetzner.json"), `{"cluster_name": "prod"}`)
	return clusters
}

func TestClusterNameUsersFindsEveryProviderConfig(t *testing.T) {
	clusters := clusterNameProject(t)
	cases := []struct {
		self, name string
		metadata   bool
		want       string
	}{
		// hetzner.json and a configRef of another name; metadata.name is
		// not a provider cluster_name, a hidden dir is not a domain.
		{"x.cloud", "prod", false, "a.cloud,b.cloud"},
		// the domain itself never counts
		{"a.cloud", "prod", false, "b.cloud"},
		// an inline spec.provider.config
		{"x.cloud", "staging", false, "c.cloud"},
		// with metadata: metadata.name counts too
		{"x.cloud", "prod", true, "a.cloud,b.cloud,d.dev"},
		{"x.cloud", "c", true, "c.cloud"},
		{"x.cloud", "c", false, ""},
		{"x.cloud", "unused", true, ""},
		{"x.cloud", "", true, ""},
	}
	for _, tc := range cases {
		users := ProviderClusterNameUsers
		if tc.metadata {
			users = ClusterNameUsers
		}
		got := strings.Join(users(clusters, tc.self, tc.name), ",")
		if got != tc.want {
			t.Errorf("users(%s, %q, metadata=%v) = %q, want %q", tc.self, tc.name, tc.metadata, got, tc.want)
		}
	}
}

func TestClusterNameUsersWithoutClustersDir(t *testing.T) {
	if got := ClusterNameUsers(filepath.Join(t.TempDir(), "nope"), "a", "prod"); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}
