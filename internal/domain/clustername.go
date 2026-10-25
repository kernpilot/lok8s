package domain

// clustername.go: which other domain in the project already uses a cluster
// name. Two names identify a cluster: the cluster_name of the provider
// config, by which the hetzner provider deletes (`lo destroy` deletes every
// resource in the token's project that carries
// lok8s.dev/cluster=<cluster_name>), and the spec's metadata.name (the
// kubeconfig file, the kind cluster). A domain copied from another one keeps
// both. `lo destroy` refuses a provider cluster_name that another domain
// uses (bash twin: provision::_cluster_name_users); `lo init cluster`
// refuses a name that another domain uses as either.

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/yqsem"
)

// readYAML parses a YAML (or JSON) file; nil when it is missing or broken.
func readYAML(file string) *yaml.Node {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return &doc
}

// isFile reports a regular file (or a link to one).
func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// providerClusterNames reads the cluster_name of a domain dir's provider
// config: clusters/<d>/hetzner.json, the file of spec.provider.configRef and
// an inline spec.provider.config (bash: yq -r '.cluster_name // ""' on each).
func providerClusterNames(dir string, spec *yaml.Node) []string {
	var names []string
	if f := filepath.Join(dir, "hetzner.json"); isFile(f) {
		names = append(names, yqsem.Or(yqsem.MapGet(readYAML(f), "cluster_name"), ""))
	}
	if spec == nil {
		return names
	}
	if ref := yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "configRef"), ""); ref != "" {
		if f := filepath.Join(dir, ref); isFile(f) {
			names = append(names, yqsem.Or(yqsem.MapGet(readYAML(f), "cluster_name"), ""))
		}
	}
	return append(names, yqsem.Or(yqsem.Lookup(spec, "spec", "provider", "config", "cluster_name"), ""))
}

// ClusterNameUsers lists the domains under clusters, other than self, whose
// provider config carries cluster_name name, and with metadata also the
// domains whose cluster.lok8s.yaml has metadata.name name. Sorted.
func ClusterNameUsers(clusters, self, name string, metadata bool) []string {
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
		// os.Stat follows a link, as the bash glob clusters/*/ does.
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			continue
		}
		var spec *yaml.Node
		if f := filepath.Join(dir, "cluster.lok8s.yaml"); isFile(f) {
			spec = readYAML(f)
		}
		names := providerClusterNames(dir, spec)
		if metadata && spec != nil {
			names = append(names, yqsem.Or(yqsem.Lookup(spec, "metadata", "name"), ""))
		}
		if slices.Contains(names, name) {
			users = append(users, other)
		}
	}
	sort.Strings(users)
	return users
}
