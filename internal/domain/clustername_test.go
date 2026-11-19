package domain

import (
	"errors"
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
		list, err := users(clusters, tc.self, tc.name)
		if err != nil {
			t.Fatalf("users(%s, %q, metadata=%v): %v", tc.self, tc.name, tc.metadata, err)
		}
		got := strings.Join(list, ",")
		if got != tc.want {
			t.Errorf("users(%s, %q, metadata=%v) = %q, want %q", tc.self, tc.name, tc.metadata, got, tc.want)
		}
	}
}

func TestClusterNameUsersWithoutClustersDir(t *testing.T) {
	if got, err := ClusterNameUsers(filepath.Join(t.TempDir(), "nope"), "a", "prod"); got != nil || err != nil {
		t.Fatalf("got %v, %v, want nil, nil", got, err)
	}
}

// A merge key (<<) on the path to a cluster_name stops the scan with the
// domains that have one (the guards refuse): the name it brings in is not
// one value (yq and yaml.v3 merge in different ways). A merge key that is
// not on the path, an alias and an anchor read as before.
func TestClusterNameUsersRefuseAMergeKeyOnThePath(t *testing.T) {
	spec := func(provider string) string {
		return "apiVersion: cluster.lok8s.dev/v1beta1\nkind: KubeOne\nmetadata:\n  name: m\nspec:\n  provider:\n    name: hetzner\n" + provider
	}
	cases := []struct {
		name  string
		files map[string]string // path under clusters/o.cloud: content
		// the result of ProviderClusterNameUsers and ClusterNameUsers for
		// the name prod: the users, or "merge" for a *MergeKeyError
		provider, all string
	}{
		{"a merge key that brings cluster_name into hetzner.json", map[string]string{
			"hetzner.json": "base: &b {cluster_name: prod}\n<<: *b\n",
		}, "merge", "merge"},
		{"a merge key that brings cluster_name into a configRef file", map[string]string{
			"cluster.lok8s.yaml": spec("    configRef: desc.yaml\n"),
			"desc.yaml":          "base: &b {cluster_name: prod}\n<<: *b\n",
		}, "merge", "merge"},
		{"a merge key next to its own cluster_name", map[string]string{
			"hetzner.json": "base: &b {cluster_name: prod}\ncluster_name: other\n<<: *b\n",
		}, "merge", "merge"},
		{"a merge list", map[string]string{
			"hetzner.json": "a: &a {cluster_name: one}\nb: &b {cluster_name: prod}\n<<: [*a, *b]\n",
		}, "merge", "merge"},
		{"a merge key in the inline config", map[string]string{
			"cluster.lok8s.yaml": spec("    config:\n      <<: {cluster_name: prod}\n"),
		}, "merge", "merge"},
		{"a merge key in spec.provider (it can give config or configRef)", map[string]string{
			"cluster.lok8s.yaml": "base: &b {config: {cluster_name: prod}}\nspec:\n  provider:\n    <<: *b\n    name: hetzner\n",
		}, "merge", "merge"},
		{"a merge key in spec", map[string]string{
			"cluster.lok8s.yaml": "base: &b {provider: {config: {cluster_name: prod}}}\nspec:\n  <<: *b\n",
		}, "merge", "merge"},
		{"a merge key at the top of the spec", map[string]string{
			"cluster.lok8s.yaml": "base: &b {spec: {provider: {config: {cluster_name: prod}}}}\n<<: *b\n",
		}, "merge", "merge"},
		{"a merge key on the path through an alias", map[string]string{
			"cluster.lok8s.yaml": "p: &p\n  config: {<<: {cluster_name: prod}}\nspec:\n  provider: *p\n",
		}, "merge", "merge"},
		{"a quoted << key (the rule of the recover guard)", map[string]string{
			"hetzner.json": `{"<<": {"cluster_name": "prod"}, "cluster_name": "other"}`,
		}, "merge", "merge"},
		{"a merge key on the path without a cluster_name", map[string]string{
			"hetzner.json": "base: &b {token: x}\n<<: *b\ncluster_name: other\n",
		}, "merge", "merge"},
		{"a merge key in the metadata: only the scan with metadata.name stops", map[string]string{
			"cluster.lok8s.yaml": "base: &b {name: prod}\nmetadata:\n  <<: *b\nspec:\n  provider:\n    config: {cluster_name: other}\n",
		}, "", "merge"},
		{"a merge key that is not on the path", map[string]string{
			"hetzner.json":       "base: &b {type: cx22}\nserver: {<<: *b}\ncluster_name: prod\n",
			"cluster.lok8s.yaml": "base: &b {domain: o.cloud}\nspec:\n  cluster: {<<: *b}\n  provider:\n    name: hetzner\n    configRef: hetzner.json\n",
		}, "o.cloud", "o.cloud"},
		{"an alias as the config, an anchor on the name", map[string]string{
			"cluster.lok8s.yaml": "base: &b {cluster_name: &n prod}\nalso: *n\nspec:\n  provider:\n    name: hetzner\n    config: *b\n",
		}, "o.cloud", "o.cloud"},
		{"a hetzner.json with a YAML body", map[string]string{
			"hetzner.json": "cluster_name: prod\n",
		}, "o.cloud", "o.cloud"},
		{"only the first document counts", map[string]string{
			"hetzner.json": "cluster_name: other\n---\n<<: {cluster_name: prod}\n",
		}, "", ""},
		{"an anchor and an alias, another name", map[string]string{
			"cluster.lok8s.yaml": "base: &b {cluster_name: other}\nspec:\n  provider:\n    name: hetzner\n    config: *b\n",
		}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clusters := filepath.Join(t.TempDir(), "clusters")
			for f, content := range tc.files {
				testutil.WriteFile(t, filepath.Join(clusters, "o.cloud", f), content)
			}
			// A merge key in the domain itself is not part of the scan.
			testutil.WriteFile(t, filepath.Join(clusters, "self.cloud", "hetzner.json"), "base: &b {cluster_name: prod}\n<<: *b\n")
			for _, sc := range []struct {
				label string
				users func(clusters, self, name string) ([]string, error)
				want  string
			}{
				{"ProviderClusterNameUsers", ProviderClusterNameUsers, tc.provider},
				{"ClusterNameUsers", ClusterNameUsers, tc.all},
			} {
				list, err := sc.users(clusters, "self.cloud", "prod")
				var merge *MergeKeyError
				switch {
				case sc.want == "merge":
					if !errors.As(err, &merge) || strings.Join(merge.Domains, ",") != "o.cloud" || list != nil {
						t.Errorf("%s = %v, %v, want a *MergeKeyError for o.cloud", sc.label, list, err)
					}
				case err != nil || strings.Join(list, ",") != sc.want:
					t.Errorf("%s = %v, %v, want %q, nil", sc.label, list, err, sc.want)
				}
			}
		})
	}
}

// Every domain with a merge key is named, sorted, and a plain user does
// not hide them.
func TestClusterNameUsersNameEveryMergeKey(t *testing.T) {
	clusters := filepath.Join(t.TempDir(), "clusters")
	testutil.WriteFile(t, filepath.Join(clusters, "z.cloud", "hetzner.json"), "base: &b {cluster_name: x}\n<<: *b\n")
	testutil.WriteFile(t, filepath.Join(clusters, "a.cloud", "hetzner.json"), "<<: {cluster_name: y}\n")
	testutil.WriteFile(t, filepath.Join(clusters, "m.cloud", "hetzner.json"), `{"cluster_name": "prod"}`)
	_, err := ProviderClusterNameUsers(clusters, "self.cloud", "prod")
	var merge *MergeKeyError
	if !errors.As(err, &merge) || strings.Join(merge.Domains, ",") != "a.cloud,z.cloud" {
		t.Fatalf("err = %v, want a *MergeKeyError for a.cloud, z.cloud", err)
	}
	if want := "the provider config of: a.cloud, z.cloud has a merge key (<<) on the path to its cluster_name"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	// No name, no scan.
	if got, err := ProviderClusterNameUsers(clusters, "self.cloud", ""); got != nil || err != nil {
		t.Errorf("empty name = %v, %v, want nil, nil", got, err)
	}
}
