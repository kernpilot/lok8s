package domain

// clustername.go: which other domain in the project already uses a cluster
// name. Two names identify a cluster: the cluster_name of the provider
// config, by which the hetzner provider deletes (`lo destroy` deletes every
// resource in the token's project that carries
// lok8s.dev/cluster=<cluster_name>), and the spec's metadata.name (the
// kubeconfig file, the kind cluster). A domain copied from another one keeps
// both. `lo destroy` refuses a provider cluster_name that another domain
// uses (ProviderClusterNameUsers; bash twin: provision::_cluster_name_users);
// `lo init cluster` refuses a name that another domain uses as either
// (ClusterNameUsers).
//
// The provider deletes by the cluster_name of its config and nothing else
// (hetzner::_destroy_cluster_name refuses a descriptor without one), so the
// scan reads the same key: one name resolution for both.

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// providerClusterNames reads the cluster_name of a domain dir's provider
// config: clusters/<d>/hetzner.json, the file of spec.provider.configRef and
// an inline spec.provider.config (bash: yq -r '.cluster_name // ""' on each).
func providerClusterNames(dir string, spec *yaml.Node) []string {
	var names []string
	if f := filepath.Join(dir, "hetzner.json"); fsutil.IsRegular(f) {
		names = append(names, yqsem.Or(yqsem.MapGet(yqsem.LoadNode(f), "cluster_name"), ""))
	}
	if spec == nil {
		return names
	}
	if ref := yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "configRef"), ""); ref != "" {
		if f := filepath.Join(dir, ref); fsutil.IsRegular(f) {
			names = append(names, yqsem.Or(yqsem.MapGet(yqsem.LoadNode(f), "cluster_name"), ""))
		}
	}
	return append(names, yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "config", "cluster_name"), ""))
}

// nameUsers lists the domains under clusters, other than self, for which
// names(dir, spec) holds name. Sorted.
func nameUsers(clusters, self, name string, names func(dir string, spec *yaml.Node) []string) []string {
	if name == "" {
		return nil
	}
	entries, err := os.ReadDir(clusters)
	if err != nil {
		return nil
	}
	var users []string
	for _, e := range entries {
		other := e.Name()
		if other == self || strings.HasPrefix(other, ".") {
			continue
		}
		dir := filepath.Join(clusters, other)
		// fsutil.DirExists follows a link, as the bash glob clusters/*/ does.
		if !fsutil.DirExists(dir) {
			continue
		}
		var spec *yaml.Node
		if f := filepath.Join(dir, "cluster.lok8s.yaml"); fsutil.IsRegular(f) {
			spec = yqsem.LoadNode(f)
		}
		if slices.Contains(names(dir, spec), name) {
			users = append(users, other)
		}
	}
	sort.Strings(users)
	return users
}

// ProviderClusterNameUsers lists the domains under clusters, other than
// self, whose provider config carries cluster_name name. Sorted.
func ProviderClusterNameUsers(clusters, self, name string) []string {
	return nameUsers(clusters, self, name, providerClusterNames)
}

// ClusterNameUsers lists the domains under clusters, other than self, that
// use name as their provider cluster_name or as the metadata.name of their
// cluster.lok8s.yaml. Sorted.
func ClusterNameUsers(clusters, self, name string) []string {
	return nameUsers(clusters, self, name, func(dir string, spec *yaml.Node) []string {
		names := providerClusterNames(dir, spec)
		if spec != nil {
			names = append(names, yqsem.Or(yqsem.Lookup(spec, "metadata", "name"), ""))
		}
		return names
	})
}
