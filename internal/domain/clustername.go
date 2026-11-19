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
// alias on the path counts with the mapping that it names, and an alias as
// a key with the key that it names.
//
// When the scan cannot read a directory that it must list or search, it
// cannot tell the names of the other domains: an *UnreadableError, and the
// guards refuse.

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

// UnreadableError: the cluster_name scan cannot read Dir, a directory that
// it must list or search: the clusters dir (it cannot be listed, or an entry
// in it cannot be read), the dir of a domain, or a dir on the way to its
// configRef file. So the scan cannot tell the names of the other domains,
// and the guards refuse (bash: the scan prints Dir and returns 1).
type UnreadableError struct {
	Dir string
	Err error
}

func (e *UnreadableError) Error() string {
	return "cannot read the directory " + e.Dir + ": " + e.Err.Error()
}
func (e *UnreadableError) Unwrap() error { return e.Err }

// UnreadableDir is the Dir of an *UnreadableError in err, else fallback:
// the directory that a guard names in its refusal.
func UnreadableDir(err error, fallback string) string {
	if ue := (*UnreadableError)(nil); errors.As(err, &ue) {
		return ue.Dir
	}
	return fallback
}

// IsMergeKey: a key whose text is "<<" (a merge key, or the quoted text) or
// whose tag is !!merge, an alias key counting with the key that it names
// (bash: PROVISION_YQ_IS_MERGE). The rule of both cluster_name reads: this
// scan (mergeOnPath) and the recover guard's own read of the provider config
// (configClusterName).
func IsMergeKey(k *yaml.Node) bool {
	k = yqsem.Deref(k)
	return k != nil && (k.Value == "<<" || k.ShortTag() == "!!merge")
}

// regularIn reports whether dir/rel is a regular file (fsutil.IsRegular: a
// link counts by its target). It reads each name on the way from dir with
// Lstat first: an Lstat that fails with EACCES means that the directory
// before the name cannot be searched, so the scan cannot tell whether the
// file is there. That is an *UnreadableError for that directory (bash:
// provision::_unsearchable). A name that does not exist, or a file on the
// way, is no file.
func regularIn(dir, rel string) (bool, error) {
	p := dir
	for name := range strings.SplitSeq(rel, "/") {
		if name == "" {
			continue
		}
		next := p + "/" + name
		if _, err := os.Lstat(next); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return false, &UnreadableError{Dir: p, Err: err}
			}
			return false, nil
		}
		p = next
	}
	return fsutil.IsRegular(filepath.Join(dir, rel)), nil
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
// the spec. An *UnreadableError when a directory on the way to a file
// cannot be searched (regularIn).
func providerClusterNames(dir string, spec *yaml.Node) (names []string, merge bool, err error) {
	read := func(rel string) error {
		ok, err := regularIn(dir, rel)
		if err != nil || !ok {
			return err
		}
		n := yqsem.LoadNode(filepath.Join(dir, rel))
		merge = merge || mergeOnPath(n)
		names = append(names, yqsem.Or(yqsem.MapGet(n, "cluster_name"), ""))
		return nil
	}
	if err := read("hetzner.json"); err != nil {
		return nil, false, err
	}
	if spec == nil {
		return names, merge, nil
	}
	merge = merge || mergeOnPath(spec, "spec", "provider", "config")
	if ref := yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "configRef"), ""); ref != "" {
		if err := read(ref); err != nil {
			return nil, false, err
		}
	}
	return append(names, yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "config", "cluster_name"), "")), merge, nil
}

// nameUsers lists the domains under clusters, other than self, for which
// names(dir, spec) holds name. Sorted. A *MergeKeyError when names reports
// a merge key for a domain. No clusters dir is no other domain. An
// *UnreadableError (bash: the scan prints the directory and returns 1), so
// the guards refuse, when clusters cannot be listed, when an entry that it
// lists cannot be read (clusters has no search permission: the Lstat fails
// with EACCES), or when names cannot search a directory.
func nameUsers(clusters, self, name string, names func(dir string, spec *yaml.Node) ([]string, bool, error)) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(clusters)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, &UnreadableError{Dir: clusters, Err: err}
	}
	var users, merged []string
	for _, e := range entries {
		other := e.Name()
		if other == self || strings.HasPrefix(other, ".") {
			continue
		}
		dir := filepath.Join(clusters, other)
		if _, err := os.Lstat(dir); err != nil {
			if errors.Is(err, fs.ErrPermission) {
				return nil, &UnreadableError{Dir: clusters, Err: err}
			}
			continue
		}
		// fsutil.DirExists follows a link, as the bash -d does: a link to
		// nothing is no domain.
		if !fsutil.DirExists(dir) {
			continue
		}
		var spec *yaml.Node
		ok, err := regularIn(dir, "cluster.lok8s.yaml")
		if err != nil {
			return nil, err
		}
		if ok {
			spec = yqsem.LoadNode(filepath.Join(dir, "cluster.lok8s.yaml"))
		}
		got, merge, err := names(dir, spec)
		if err != nil {
			return nil, err
		}
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
	return nameUsers(clusters, self, name, func(dir string, spec *yaml.Node) ([]string, bool, error) {
		names, merge, err := providerClusterNames(dir, spec)
		if err != nil {
			return nil, false, err
		}
		if spec != nil {
			merge = merge || mergeOnPath(spec, "metadata")
			names = append(names, yqsem.Or(yqsem.Lookup(spec, "metadata", "name"), ""))
		}
		return names, merge, nil
	})
}
