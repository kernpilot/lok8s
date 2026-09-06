package build

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/provision"
	"github.com/kernpilot/lok8s/internal/ui"
)

// AmbientClusterName resolves the cluster name the argsh entrypoint used for
// its unconditional `export KUBECONFIG="${PATH_BASE}/.kubeconfig/<cluster>.yaml"`:
// the domain spec's metadata.name when present, else the --cluster flag, else
// the LOK8S_CLUSTER_NAME env, else "local" (bash: `lo` main()).
func AmbientClusterName(p *config.Paths, domain, clusterFlag string) string {
	name := os.Getenv("LOK8S_CLUSTER_NAME")
	if name == "" {
		name = "local"
	}
	if clusterFlag != "" {
		name = clusterFlag
	}
	if specName := specMetadataName(filepath.Join(p.Clusters, domain, "cluster.lok8s.yaml")); specName != "" {
		name = specName
	}
	return name
}

// AmbientKubeconfig returns the default KUBECONFIG path the bash entrypoint
// exported before dispatching any subcommand.
func AmbientKubeconfig(p *config.Paths, domain, clusterFlag string) string {
	return filepath.Join(p.Base, ".kubeconfig", AmbientClusterName(p, domain, clusterFlag)+".yaml")
}

// resolveClusterRef resolves the clusterRef from a deploy.lok8s.yaml to its
// cluster domain. The canonical implementation (verbatim bash error
// strings, including the historical `.lok8s/` path spelling) now lives with
// the rest of the dispatch layer as provision.ResolveClusterRef; this
// delegate keeps build's call sites and tests unchanged.
func resolveClusterRef(p *config.Paths, domain string, stderr io.Writer) (string, error) {
	return provision.ResolveClusterRef(p, domain, stderr)
}

// ResolveKubeconfigForDomain is kubeconfig pass A (bash:
// _resolve_kubeconfig_for_domain): resolve KUBECONFIG for deploy domains
// that reference another cluster. A deploy-only domain follows
// clusterRef.domain (or the --cluster-override); a cluster domain follows
// the override only. No ref → the ambient KUBECONFIG stays untouched.
//
// The resolved cluster's kubeconfig prefers the canonical
// `secret.<domain>.yaml` (the SOPS-fetched per-domain kubeconfig); falls
// back to `<cluster metadata.name>.yaml` — the name `lo` writes for a
// provisioned cluster (e.g. a domain `example.com` may provision under
// metadata.name `my-cluster`, so its kubeconfig is my-cluster.yaml). Without
// this fallback a deploy domain whose target cluster was provisioned under a
// different metadata.name points KUBECONFIG at a non-existent file and the
// deploy fails.
//
// The result is exported into the process env (bash `export KUBECONFIG`), so
// the later API-endpoint resolution and every child process see it.
func ResolveKubeconfigForDomain(p *config.Paths, domain, clusterOverride string, stderr io.Writer) error {
	domainDir := filepath.Join(p.Clusters, domain)
	refCluster := ""
	deployOnly := fileExists(filepath.Join(domainDir, "deploy.lok8s.yaml")) &&
		!fileExists(filepath.Join(domainDir, "cluster.lok8s.yaml"))
	if deployOnly {
		refCluster = clusterOverride
		if refCluster == "" {
			ref, err := resolveClusterRef(p, domain, stderr)
			if err != nil {
				return err
			}
			refCluster = ref
		}
		ui.Debugf(stderr, "Deploy domain %s -> cluster %s", domain, refCluster)
	} else if clusterOverride != "" {
		refCluster = clusterOverride
	}
	if refCluster == "" {
		return nil
	}

	kubeconfig := filepath.Join(p.Base, ".kubeconfig", "secret."+refCluster+".yaml")
	refSpec := filepath.Join(p.Clusters, refCluster, "cluster.lok8s.yaml")
	if !fileExists(kubeconfig) && fileExists(refSpec) {
		if refName := specMetadataName(refSpec); refName != "" {
			if named := filepath.Join(p.Base, ".kubeconfig", refName+".yaml"); fileExists(named) {
				kubeconfig = named
			}
		}
	}
	return os.Setenv("KUBECONFIG", kubeconfig)
}

// renderKubeconfig is kubeconfig pass B (bash: build::artifacts' local
// resolution, independent of pass A): the kubeconfig handed to the kustomize
// child for cluster-aware plugins (the khelm ChartRenderer kubeVersion
// check). Prefers `.kubeconfig/secret.<domain>.yaml`, falls back to
// `.kubeconfig/<metadata.name>.yaml` when that file exists. May resolve to a
// nonexistent path — tolerated (the plugins degrade gracefully).
func renderKubeconfig(p *config.Paths, domain string) string {
	kubeconfig := filepath.Join(p.Base, ".kubeconfig", "secret."+domain+".yaml")
	if fileExists(kubeconfig) {
		return kubeconfig
	}
	clusterSpec := filepath.Join(p.Clusters, domain, "cluster.lok8s.yaml")
	if fileExists(clusterSpec) {
		if name := specMetadataName(clusterSpec); name != "" {
			if named := filepath.Join(p.Base, ".kubeconfig", name+".yaml"); fileExists(named) {
				return named
			}
		}
	}
	return kubeconfig
}

// resolveAPI resolves the control-plane API endpoint from the domain's
// kubeconfig and exports LOK8S_USER_API_{HOST,PORT} so ${LOK8S_USER_API_*}
// in target manifests (e.g. the host-firewall ccnp) resolve at envsubst
// time. Best-effort: the kubeconfig may not exist yet for a deploy domain
// (resolved secret not fetched); exports only when non-empty. Bash:
// build::_resolve_api.
func resolveAPI(p *config.Paths, domainDir string) {
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" || !fileExists(kubeconfig) {
		// Deploy domains (deploy.lok8s.yaml + clusterRef) have NO
		// cluster.lok8s.yaml — read the name only when the file is present.
		clusterSpec := filepath.Join(domainDir, "cluster.lok8s.yaml")
		if fileExists(clusterSpec) {
			if name := specMetadataName(clusterSpec); name != "" {
				if named := filepath.Join(p.Base, ".kubeconfig", name+".yaml"); fileExists(named) {
					kubeconfig = named
				}
			}
		}
	}
	api := ""
	if fileExists(kubeconfig) {
		api = kubeconfigServer(kubeconfig)
	}
	// Strip the scheme like bash's `${api#http*://}` (shortest glob match).
	if strings.HasPrefix(api, "http") {
		if idx := strings.Index(api, "://"); idx >= 0 {
			api = api[idx+3:]
		}
	}
	if api != "" {
		host := api
		if idx := strings.IndexByte(api, ':'); idx >= 0 {
			host = api[:idx]
		}
		port := "6443"
		if idx := strings.LastIndexByte(api, ':'); idx >= 0 && api[idx+1:] != "" {
			port = api[idx+1:]
		}
		os.Setenv("LOK8S_USER_API_HOST", host)
		os.Setenv("LOK8S_USER_API_PORT", port)
	}
}

// kubeconfigServer reads .clusters[0].cluster.server from a kubeconfig, ""
// on any read/parse failure.
func kubeconfigServer(path string) string {
	var doc struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || yaml.Unmarshal(raw, &doc) != nil || len(doc.Clusters) == 0 {
		return ""
	}
	return doc.Clusters[0].Cluster.Server
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
