// Package provision is the Go port of the cluster-lifecycle dispatch
// (.lok8s/libs/provision): spec resolution, the real-infrastructure gate,
// provider-credential loading, and the provision/destroy/status dispatch
// over the driver registry (internal/driver).
//
// LIBRARY-ONLY for now: no command is wired to it yet — provision/up/down/
// status still shim to bash. The dispatch tail hooks (kubehz, bootstrap,
// inventory, gitops) are injectable function fields; nil hooks are skipped,
// matching the bash `declare -f` probes for the not-loaded libs.
package provision

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/config"
	domainpkg "github.com/kernpilot/lok8s/internal/domain"
	"github.com/kernpilot/lok8s/internal/ui"
)

// Spec kinds (bash: LOK8S_SPEC_KIND).
const (
	SpecKindCluster = "cluster"
	SpecKindDeploy  = "deploy"
)

// Spec is the resolved spec for a domain (bash: the LOK8S_SPEC_FILE /
// LOK8S_SPEC_KIND exports of provision::resolve_spec).
type Spec struct {
	Domain string
	File   string
	Kind   string
}

// ResolveSpec resolves the cluster spec YAML for a domain. Cluster domains
// carry cluster.lok8s.yaml, deployment domains deploy.lok8s.yaml (a domain
// with both is a cluster domain). Error strings verbatim from bash,
// including the historical ".lok8s/<domain>/" spelling AND the two distinct
// error families: the invalid-domain message is the raw `error: …` echo,
// not the [error]-prefixed verbose.sh line.
func ResolveSpec(p *config.Paths, domainName string, stderr io.Writer) (*Spec, error) {
	// No active domain → actionable error instead of a cryptic empty path.
	if domainName == "" {
		ui.Errorf(stderr, "no active domain — set one with 'lo use <domain>' or pass --domain <domain>")
		return nil, errors.New("no active domain")
	}
	// Validate domain name to prevent path traversal and injection.
	if !domainpkg.NameRe.MatchString(domainName) {
		fmt.Fprintf(stderr, "error: invalid domain name: %s\n", domainName)
		return nil, fmt.Errorf("invalid domain name: %s", domainName)
	}
	base := filepath.Join(p.Clusters, domainName)
	if fileExists(filepath.Join(base, "cluster.lok8s.yaml")) {
		return &Spec{Domain: domainName, File: filepath.Join(base, "cluster.lok8s.yaml"), Kind: SpecKindCluster}, nil
	}
	if fileExists(filepath.Join(base, "deploy.lok8s.yaml")) {
		return &Spec{Domain: domainName, File: filepath.Join(base, "deploy.lok8s.yaml"), Kind: SpecKindDeploy}, nil
	}
	ui.Errorf(stderr, "No cluster.lok8s.yaml or deploy.lok8s.yaml found in .lok8s/%s/", domainName)
	return nil, fmt.Errorf("no spec for domain %s", domainName)
}

// ResolveClusterRef resolves the clusterRef of a deploy.lok8s.yaml to its
// cluster domain (spec.clusterRef.domain), validating that the referenced
// domain exists and carries a cluster spec. Error strings verbatim from
// bash (provision::resolve_clusterref), including the historical `.lok8s/`
// path spelling. internal/build's kubeconfig resolution delegates here.
func ResolveClusterRef(p *config.Paths, domainName string, stderr io.Writer) (string, error) {
	specFile := filepath.Join(p.Clusters, domainName, "deploy.lok8s.yaml")
	if !fileExists(specFile) {
		ui.Errorf(stderr, "No deploy.lok8s.yaml found for %s", domainName)
		return "", fmt.Errorf("no deploy spec for %s", domainName)
	}
	info, _ := readSpecInfo(specFile)
	ref := info.Spec.ClusterRef.Domain
	if ref == "" {
		ui.Errorf(stderr, "deploy.lok8s.yaml for %s missing spec.clusterRef.domain", domainName)
		return "", fmt.Errorf("missing clusterRef for %s", domainName)
	}
	if info, err := os.Stat(filepath.Join(p.Clusters, ref)); err != nil || !info.IsDir() {
		ui.Errorf(stderr, "clusterRef domain not found: .lok8s/%s/", ref)
		return "", fmt.Errorf("clusterRef domain not found: %s", ref)
	}
	if !fileExists(filepath.Join(p.Clusters, ref, "cluster.lok8s.yaml")) {
		ui.Errorf(stderr, "clusterRef domain %s has no cluster.lok8s.yaml", ref)
		return "", fmt.Errorf("clusterRef domain %s has no cluster spec", ref)
	}
	return ref, nil
}

// ReadKind reads the driver name for the dispatch, with the errors the
// dispatch wants to print (bash: provision::read_kind — the message layer
// over domain::spec_driver, which owns the reading, coercion, and shape
// guard).
func ReadKind(clusterYAML string, stderr io.Writer) (string, error) {
	kind, err := domainpkg.SpecDriver(clusterYAML, "")
	switch {
	case err == nil:
		return kind, nil
	case errors.Is(err, domainpkg.ErrMalformedDriver):
		// rc 2 = malformed — NEVER defaulted.
		ui.Errorf(stderr, "invalid cluster kind in %s (not a bare driver name)", clusterYAML)
	default:
		ui.Errorf(stderr, "cluster spec has no .kind: %s", clusterYAML)
	}
	return "", err
}

// specInfo is the subset of a cluster/deploy spec the dispatch layer reads.
// Fields that may legally hold non-string scalars decode as `any` and are
// stringified with yq semantics where needed.
type specInfo struct {
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Kubernetes struct {
			Version any `yaml:"version"`
		} `yaml:"kubernetes"`
		Provider struct {
			Name      string    `yaml:"name"`
			Config    yaml.Node `yaml:"config"`
			ConfigRef string    `yaml:"configRef"`
		} `yaml:"provider"`
		Bootstrap []any `yaml:"bootstrap"`
		Gitops    struct {
			Provider string `yaml:"provider"`
		} `yaml:"gitops"`
		ClusterRef struct {
			Domain string `yaml:"domain"`
		} `yaml:"clusterRef"`
	} `yaml:"spec"`
}

// readSpecInfo parses a spec file best-effort: a missing/unreadable/
// unparsable file yields the zero value (matching the bash `yq … 2>/dev/null
// // ""` guards).
func readSpecInfo(path string) (specInfo, error) {
	var doc specInfo
	raw, err := os.ReadFile(path)
	if err != nil {
		return specInfo{}, err
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return specInfo{}, err
	}
	return doc, nil
}

// specMetadataName reads .metadata.name, "" when missing/unreadable
// (bash: yq -r '.metadata.name // ""').
func specMetadataName(path string) string {
	info, _ := readSpecInfo(path)
	return info.Metadata.Name
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
