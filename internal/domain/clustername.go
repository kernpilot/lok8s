package domain

// clustername.go: which other domain in the project already uses a cluster
// name. Two names identify a cluster: the cluster_name of the provider
// config, by which the hetzner provider finds what it deletes (`lo destroy`
// deletes the resources in the token's project that carry
// lok8s.dev/cluster=<cluster_name> and the owner mark of the domain), and
// the spec's metadata.name (the
// kubeconfig file, the kind cluster). A domain copied from another one keeps
// both. `lo destroy` and `lo recover` (the rebuild finds its servers by the
// same label) refuse a provider cluster_name that another domain uses
// (ProviderClusterNameUsers; bash twin: provision::_cluster_name_users);
// `lo init cluster` refuses a name that another domain uses as either
// (ClusterNameUsers).
//
// The provider deletes by the cluster_name of its config and nothing else
// (hetzner::_destroy_cluster_name refuses a descriptor without one), so the
// scan reads the same key: one name resolution for both.
//
// A merge key (<<) on the path to a name stops the scan (MergeKeyError, fail
// closed): the name it brings in is not one value. yq gives the merged value
// over the own key, and from a merge list the last mapping on a path read
// (.cluster_name) but the first one on the JSON read of the provider
// (template::descriptor_json); --yaml-fix-merge-anchor-to-spec changes both,
// and yq says that this will become its default. yaml.v3 does not merge at
// all. So a scan cannot tell which name the provider of that domain uses.
// The rule is the one of the recover guard's own read (configClusterName):
// a key whose text is "<<", quoted or not. A key with the explicit tag
// !!merge (`!!merge base: *b`) is a merge key for yq too, so it counts. An
// alias on the path counts with the mapping that it names.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/fsutil"
	"github.com/kernpilot/lok8s/internal/yqsem"
)

// MergeKeyError: the cluster config of each of Domains (sorted) has a merge
// key (<<) on the path to a cluster name: the cluster_name of its provider
// config, or (ClusterNameUsers) the metadata.name of its cluster.lok8s.yaml.
// So the scan cannot tell the name that the domain uses. The guards refuse
// with it.
type MergeKeyError struct {
	Domains []string
}

func (e *MergeKeyError) Error() string {
	return "the cluster config of: " + strings.Join(e.Domains, ", ") + " has a merge key (<<) on the path to a cluster name"
}

// IsMergeKey: a key whose text is "<<" (a merge key, or the quoted text) or
// whose tag is !!merge (bash: select(. == "<<" or tag == "!!merge")). The
// rule of both cluster_name reads: this scan (mergeOnPath) and the recover
// guard's own read of the provider config (configClusterName).
func IsMergeKey(k *yaml.Node) bool {
	return k.Value == "<<" || k.ShortTag() == "!!merge"
}

// mergeOnPath reports a merge key (IsMergeKey) in n or in a mapping on path
// from n: each mapping that a merge key in it can give the next key of the
// path or the name (bash: the yq keys of each, in
// provision::_cluster_name_users). Each node of the path is dereferenced, so
// an alias counts with the mapping that it names.
func mergeOnPath(n *yaml.Node, path ...string) bool {
	n = yqsem.Deref(n)
	for i := 0; n != nil && n.Kind == yaml.MappingNode; i++ {
		for k := 0; k+1 < len(n.Content); k += 2 {
			if IsMergeKey(n.Content[k]) {
				return true
			}
		}
		if i == len(path) {
			return false
		}
		n = yqsem.MapGet(n, path[i])
	}
	return false
}

// providerClusterNames reads the cluster_name of a domain dir's provider
// config: clusters/<d>/hetzner.json, the file of spec.provider.configRef and
// an inline spec.provider.config (bash: yq -r '.cluster_name // ""' on each).
// merge is true when a merge key is on the path to one of them: the top of
// each file, and the top, spec, spec.provider and spec.provider.config of
// the spec.
func providerClusterNames(dir string, spec *yaml.Node) (names []string, merge bool) {
	read := func(f string) {
		n := yqsem.LoadNode(f)
		merge = merge || mergeOnPath(n)
		names = append(names, yqsem.Or(yqsem.MapGet(n, "cluster_name"), ""))
	}
	if f := filepath.Join(dir, "hetzner.json"); fsutil.IsRegular(f) {
		read(f)
	}
	if spec == nil {
		return names, merge
	}
	merge = merge || mergeOnPath(spec, "spec", "provider", "config")
	if ref := yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "configRef"), ""); ref != "" {
		if f := filepath.Join(dir, ref); fsutil.IsRegular(f) {
			read(f)
		}
	}
	return append(names, yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "config", "cluster_name"), "")), merge
}

// nameUsers lists the domains under clusters, other than self, for which
// names(dir, spec) holds name. Sorted. A *MergeKeyError when names reports
// a merge key for a domain. No clusters dir is no other domain; a clusters
// dir that cannot be read is an error (bash: the scan returns 1), so the
// guards refuse.
func nameUsers(clusters, self, name string, names func(dir string, spec *yaml.Node) ([]string, bool)) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(clusters)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var users, merged []string
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
		got, merge := names(dir, spec)
		switch {
		case merge:
			merged = append(merged, other)
		case slices.Contains(got, name):
			users = append(users, other)
		}
	}
	if len(merged) > 0 {
		sort.Strings(merged)
		return nil, &MergeKeyError{Domains: merged}
	}
	sort.Strings(users)
	return users, nil
}

// ProviderClusterNameUsers lists the domains under clusters, other than
// self, whose provider config carries cluster_name name. Sorted. A
// *MergeKeyError when a merge key is on the path to the cluster_name of a
// domain: the caller refuses.
func ProviderClusterNameUsers(clusters, self, name string) ([]string, error) {
	return nameUsers(clusters, self, name, providerClusterNames)
}

// ClusterNameUsers lists the domains under clusters, other than self, that
// use name as their provider cluster_name or as the metadata.name of their
// cluster.lok8s.yaml. Sorted. A *MergeKeyError as ProviderClusterNameUsers,
// and for a merge key in the top or the metadata of the spec too.
func ClusterNameUsers(clusters, self, name string) ([]string, error) {
	return nameUsers(clusters, self, name, func(dir string, spec *yaml.Node) ([]string, bool) {
		names, merge := providerClusterNames(dir, spec)
		if spec != nil {
			merge = merge || mergeOnPath(spec, "metadata")
			names = append(names, yqsem.Or(yqsem.Lookup(spec, "metadata", "name"), ""))
		}
		return names, merge
	})
}
