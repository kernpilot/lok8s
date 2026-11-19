package domain

import (
	"errors"
	"os"
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

// A provider file that cannot be read reads as no name, as a file that does
// not parse (bash: yq gets nothing on stdin, and the error of the
// redirection goes to /dev/null).
func TestClusterNameUsersReadAnUnreadableFileAsNoName(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without the read permission")
	}
	clusters := filepath.Join(t.TempDir(), "clusters")
	f := filepath.Join(clusters, "o.cloud", "hetzner.json")
	testutil.WriteFile(t, f, `{"cluster_name": "prod"}`)
	if err := os.Chmod(f, 0o000); err != nil {
		t.Fatal(err)
	}
	if got, err := ProviderClusterNameUsers(clusters, "self.cloud", "prod"); got != nil || err != nil {
		t.Fatalf("got %v, %v, want nil, nil", got, err)
	}
}

// assertUnreadable runs both scans on clusters and wants an *UnreadableError
// for dir from each (the guards refuse with "could not check" and name dir;
// the bash twin prints dir and returns 1, destroy_guard_test.bats).
func assertUnreadable(t *testing.T, clusters, dir string) {
	t.Helper()
	for _, sc := range []struct {
		label string
		users func(clusters, self, name string) ([]string, error)
	}{
		{"ProviderClusterNameUsers", ProviderClusterNameUsers},
		{"ClusterNameUsers", ClusterNameUsers},
	} {
		list, err := sc.users(clusters, "self.cloud", "prod")
		var ue *UnreadableError
		if !errors.As(err, &ue) || ue.Dir != dir || list != nil {
			t.Errorf("%s(%s) = %v, %v, want nil and an *UnreadableError for %s", sc.label, clusters, list, err, dir)
		}
		if got := UnreadableDir(err, "fallback"); got != dir {
			t.Errorf("UnreadableDir = %q, want %q", got, dir)
		}
	}
}

// A clusters dir that the scan cannot list: the scan cannot tell the other
// domains, so the guards refuse ("could not check").
func TestClusterNameUsersFailWhenTheClustersDirCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a dir without the read permission")
	}
	locked := filepath.Join(t.TempDir(), "clusters")
	testutil.WriteFile(t, filepath.Join(locked, "o.cloud", "hetzner.json"), `{"cluster_name": "prod"}`)
	if err := os.Chmod(locked, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	assertUnreadable(t, locked, locked)
}

// A clusters path that is not a dir fails the scan in the same way. It needs
// no chmod, so root runs it too.
func TestClusterNameUsersFailWhenTheClustersPathIsAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "clusters")
	testutil.WriteFile(t, file, "not a dir\n")
	assertUnreadable(t, file, file)
	if UnreadableDir(errors.New("other"), "fallback") != "fallback" {
		t.Error("UnreadableDir of another error is not the fallback")
	}
}

// A directory that the scan lists but cannot search (read, no search
// permission): it cannot tell whether a file is there, so it refuses and
// names that directory. The clusters dir (an entry cannot be read), the dir
// of a domain, and a dir on the way to a configRef file. The domain itself
// is never read, and a provider file behind a link into a dir that cannot be
// searched is no file (the walk reads each name with Lstat).
func TestClusterNameUsersFailWhenADirCannotBeSearched(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root searches a dir without the search permission")
	}
	lock := func(t *testing.T, dir string) {
		t.Helper()
		if err := os.Chmod(dir, 0o400); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}
	t.Run("the clusters dir", func(t *testing.T) {
		clusters := filepath.Join(t.TempDir(), "clusters")
		testutil.WriteFile(t, filepath.Join(clusters, "o.cloud", "hetzner.json"), `{"cluster_name": "other"}`)
		lock(t, clusters)
		assertUnreadable(t, clusters, clusters)
	})
	t.Run("the clusters dir with the domain itself only", func(t *testing.T) {
		clusters := filepath.Join(t.TempDir(), "clusters")
		testutil.WriteFile(t, filepath.Join(clusters, "self.cloud", "hetzner.json"), `{"cluster_name": "prod"}`)
		lock(t, clusters)
		if got, err := ClusterNameUsers(clusters, "self.cloud", "prod"); got != nil || err != nil {
			t.Fatalf("got %v, %v, want nil, nil", got, err)
		}
	})
	t.Run("the dir of a domain", func(t *testing.T) {
		clusters := filepath.Join(t.TempDir(), "clusters")
		testutil.WriteFile(t, filepath.Join(clusters, "o.cloud", "hetzner.json"), `{"cluster_name": "other"}`)
		lock(t, filepath.Join(clusters, "o.cloud"))
		assertUnreadable(t, clusters, filepath.Join(clusters, "o.cloud"))
	})
	t.Run("a dir on the way to the configRef file", func(t *testing.T) {
		clusters := filepath.Join(t.TempDir(), "clusters")
		testutil.WriteFile(t, filepath.Join(clusters, "o.cloud", "cluster.lok8s.yaml"), "spec:\n  provider:\n    configRef: infra/desc.yaml\n")
		testutil.WriteFile(t, filepath.Join(clusters, "o.cloud", "infra", "desc.yaml"), "cluster_name: other\n")
		lock(t, filepath.Join(clusters, "o.cloud", "infra"))
		assertUnreadable(t, clusters, filepath.Join(clusters, "o.cloud", "infra"))
	})
	t.Run("a provider file behind a link into a dir that cannot be searched", func(t *testing.T) {
		base := t.TempDir()
		testutil.WriteFile(t, filepath.Join(base, "locked", "x.json"), `{"cluster_name": "prod"}`)
		clusters := filepath.Join(base, "clusters")
		if err := os.MkdirAll(filepath.Join(clusters, "o.cloud"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(base, "locked", "x.json"), filepath.Join(clusters, "o.cloud", "hetzner.json")); err != nil {
			t.Fatal(err)
		}
		lock(t, filepath.Join(base, "locked"))
		if got, err := ProviderClusterNameUsers(clusters, "self.cloud", "prod"); got != nil || err != nil {
			t.Fatalf("got %v, %v, want nil, nil", got, err)
		}
	})
}

// An entry that is a link to nothing, or a link loop, is no domain: the scan
// reads on (bash: -d is false).
func TestClusterNameUsersSkipALinkToNothing(t *testing.T) {
	clusters := filepath.Join(t.TempDir(), "clusters")
	testutil.WriteFile(t, filepath.Join(clusters, "o.cloud", "hetzner.json"), `{"cluster_name": "prod"}`)
	for _, l := range []struct{ name, dest string }{{"gone.cloud", "nowhere"}, {"loop.cloud", "loop.cloud"}} {
		if err := os.Symlink(l.dest, filepath.Join(clusters, l.name)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ProviderClusterNameUsers(clusters, "self.cloud", "prod")
	if err != nil || strings.Join(got, ",") != "o.cloud" {
		t.Fatalf("got %v, %v, want [o.cloud], nil", got, err)
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
		// The merge key is in the mapping that the alias names, not below it.
		{"a merge key in the config that an alias names", map[string]string{
			"cluster.lok8s.yaml": "b: &b {cluster_name: prod, <<: {cluster_name: other}}\nspec:\n  provider:\n    config: *b\n",
		}, "merge", "merge"},
		{"a merge key in the provider that an alias names", map[string]string{
			"cluster.lok8s.yaml": "p: &p\n  config: {cluster_name: prod}\n  <<: {config: {cluster_name: other}}\nspec:\n  provider: *p\n",
		}, "merge", "merge"},
		// An anchor name used twice: the alias names the last one before it.
		{"a redefined anchor: the merge key is in the mapping that the alias names", map[string]string{
			"cluster.lok8s.yaml": "b: &b {cluster_name: other}\nb2: &b {cluster_name: prod, <<: {x: 1}}\nspec:\n  provider:\n    config: *b\n",
		}, "merge", "merge"},
		{"an anchor name used again after the alias: the alias names the one before it", map[string]string{
			"cluster.lok8s.yaml": "b: &b {cluster_name: prod, <<: {x: 1}}\nspec:\n  provider:\n    config: *b\nlater: &b {cluster_name: other}\n",
		}, "merge", "merge"},
		{"a redefined anchor: the merge key is in the mapping that the alias does not name", map[string]string{
			"cluster.lok8s.yaml": "b: &b {cluster_name: other, <<: {x: 1}}\nb2: &b {cluster_name: prod}\nspec:\n  provider:\n    config: *b\n",
		}, "o.cloud", "o.cloud"},
		// An anchor on a complex key (a mapping as a key): yq's `..` skips
		// the keys, `...` visits them.
		{"an anchor on a complex key: the merge key is in the mapping that the alias names", map[string]string{
			"cluster.lok8s.yaml": "? &m {<<: {cluster_name: prod}}\n: x\nspec:\n  provider:\n    config: *m\n",
		}, "merge", "merge"},
		{"an anchor on a value, then on a key: the alias names the key", map[string]string{
			"cluster.lok8s.yaml": "a: &m {x: 1}\n? &m {<<: {cluster_name: prod}}\n: y\nspec: {provider: {config: *m}}\n",
		}, "merge", "merge"},
		{"an anchor on a key, then on a value: the alias names the value", map[string]string{
			"cluster.lok8s.yaml": "? &m {<<: {cluster_name: prod}}\n: y\na: &m {x: 1}\nspec: {provider: {config: *m}}\n",
		}, "", ""},
		{"an anchor on a scalar key after a mapping anchor of that name: the alias names the scalar", map[string]string{
			"cluster.lok8s.yaml": "base: &b {<<: {cluster_name: prod}}\n&b k: v\nspec: {provider: {config: *b}}\n",
		}, "", ""},
		// An anchor and its alias on one line: the column decides.
		{"an anchor and its alias on one line", map[string]string{
			"cluster.lok8s.yaml": "{a: &s {<<: {cluster_name: prod}}, spec: {provider: {config: *s}}}\n",
		}, "merge", "merge"},
		{"a key with the explicit tag !!merge", map[string]string{
			"hetzner.json": "x: {cluster_name: other}\n!!merge base: {cluster_name: prod}\n",
		}, "merge", "merge"},
		{"an alias key that names a plain <<", map[string]string{
			"cluster.lok8s.yaml": "x: &k <<\nspec:\n  provider:\n    config:\n      *k : {cluster_name: prod}\n",
		}, "merge", "merge"},
		{"an alias key that names a quoted <<", map[string]string{
			"cluster.lok8s.yaml": "x: &k '<<'\nspec:\n  provider:\n    config:\n      *k : {cluster_name: prod}\n",
		}, "merge", "merge"},
		{"a merge key in a mapping with its own tag", map[string]string{
			"cluster.lok8s.yaml": spec("    config: !cfg {<<: {cluster_name: prod}}\n"),
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
		{"the first document has the name, a second document follows", map[string]string{
			"hetzner.json": "cluster_name: prod\n---\ncluster_name: x\n",
		}, "o.cloud", "o.cloud"},
		{"a spec with a second document: the inline config of the first counts", map[string]string{
			"cluster.lok8s.yaml": spec("    config: {cluster_name: prod}\n") + "---\nspec: {provider: {config: {cluster_name: x}}}\n",
		}, "o.cloud", "o.cloud"},
		{"a spec with a second document: the configRef of the first counts", map[string]string{
			"cluster.lok8s.yaml": spec("    configRef: desc.yaml\n") + "---\nspec: {provider: {configRef: other.yaml}}\n",
			"desc.yaml":          "cluster_name: prod\n",
		}, "o.cloud", "o.cloud"},
		// yaml.v3 stops after the first document; yq prints its value and
		// exits 1.
		{"a spec whose second document does not parse: the configRef of the first counts", map[string]string{
			"cluster.lok8s.yaml": spec("    configRef: desc.yaml\n") + "---\nb: [\n",
			"desc.yaml":          "cluster_name: prod\n",
		}, "o.cloud", "o.cloud"},
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
	if want := "the cluster config of: a.cloud, z.cloud has a merge key (<<) on the path to a cluster name"; err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	// No name, no scan.
	if got, err := ProviderClusterNameUsers(clusters, "self.cloud", ""); got != nil || err != nil {
		t.Errorf("empty name = %v, %v, want nil, nil", got, err)
	}
}
