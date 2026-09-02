package kubeone

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/build"
	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/ui"
)

// coreTemplateVars is the whitelisted variable set of the core-template
// render (bash: the template::envsubst positional whitelist — restricting
// substitution keeps arbitrary `$…` in the template untouched).
var coreTemplateVars = []string{
	"CLUSTER_NAME", "K8S_VERSION", "CLOUD_PROVIDER",
	"POD_SUBNET", "SERVICE_SUBNET", "CNI_PLUGIN", "KUBE_PROXY_SKIP",
}

var (
	poolNameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)
	numericRe    = regexp.MustCompile(`^[0-9]+$`)
	safeTokenRe  = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	regHostRe    = regexp.MustCompile(`^[a-zA-Z0-9._:-]+$`)
	secretNameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
)

// GenerateConfig ports kubeone::generate_config: render kubeone.yaml from
// the core template + cluster spec, append a dynamicWorkers block for each
// pool in spec.workers, then merge spec.oidc (features.openidConnect) and
// spec.registries (containerd registry auth) into the manifest. Writes
// <outputDir>/kubeone.yaml.
func (d *Driver) GenerateConfig(ctx context.Context, clusterYAML, provider, outputDir string) error {
	stderr := d.stderr()
	coreTmpl := filepath.Join(d.deps.Paths.Lok8s, "drivers", "kubeone", "cluster", "core", "kubeone.yaml")
	if !fileExists(coreTmpl) {
		ui.Errorf(stderr, "KubeOne core template not found: %s", coreTmpl)
		return fmt.Errorf("kubeone: core template not found: %s", coreTmpl)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return err
	}

	// Ensure the template variables are exported. Guarded: extract_vars can
	// fail (e.g. an invalid spec.network.kubeProxy) and must propagate
	// cleanly instead of rendering with stale env (issue #91's family).
	if err := d.ExtractVars(ctx, clusterYAML); err != nil {
		return err
	}

	raw, err := os.ReadFile(coreTmpl)
	if err != nil {
		return err
	}
	rendered := build.Envsubst(raw, coreTemplateVars)
	if len(bytes.TrimSpace(rendered)) == 0 {
		ui.Errorf(stderr, "the KubeOne manifest rendered EMPTY from %s — refusing to continue", coreTmpl)
		return fmt.Errorf("kubeone: manifest rendered empty from %s", coreTmpl)
	}
	out := strings.TrimRight(string(rendered), "\n")

	spec, err := loadClusterSpec(clusterYAML)
	if err != nil {
		return err
	}
	pools := workerPools(&spec.Spec.Workers)
	if len(pools) > 0 {
		out += "\n\ndynamicWorkers:"
		for _, pool := range pools {
			// The ONE pool-name rule (utils/spec.sh): the name lands in
			// rendered YAML, so it is constrained to what a Kubernetes
			// object name can hold anyway.
			if !poolNameRe.MatchString(pool.name) {
				ui.Errorf(stderr, "Invalid worker pool name: %s (must be alphanumeric with hyphens)", pool.name)
				return fmt.Errorf("kubeone: invalid pool name %q", pool.name)
			}
			replicas := pool.field("replicas", "1")
			poolType := pool.field("type", "")
			// Spec-controlled values are interpolated into YAML — validate
			// before use so newlines/quotes/colons can't inject fields.
			if !numericRe.MatchString(replicas) {
				ui.Errorf(stderr, "Invalid replicas for pool %s: %s (must be numeric)", pool.name, replicas)
				return fmt.Errorf("kubeone: invalid replicas for pool %s", pool.name)
			}
			if !safeTokenRe.MatchString(poolType) {
				ui.Errorf(stderr, "Invalid server/instance type for pool %s: %s", pool.name, poolType)
				return fmt.Errorf("kubeone: invalid type for pool %s", pool.name)
			}

			out += "\n- name: " + pool.name
			out += "\n  replicas: " + replicas
			out += "\n  providerSpec:"
			out += "\n    cloudProviderSpec:"

			switch provider {
			case "hetzner":
				datacenter := defaultStr(spec.Spec.Datacenter, "fsn1")
				image := pool.field("image", "ubuntu-22.04")
				if !safeTokenRe.MatchString(datacenter) {
					ui.Errorf(stderr, "Invalid datacenter: %s", datacenter)
					return fmt.Errorf("kubeone: invalid datacenter %q", datacenter)
				}
				if !safeTokenRe.MatchString(image) {
					ui.Errorf(stderr, "Invalid image for pool %s: %s", pool.name, image)
					return fmt.Errorf("kubeone: invalid image for pool %s", pool.name)
				}
				out += "\n      serverType: \"" + poolType + "\""
				out += "\n      location: \"" + datacenter + "\""
				out += "\n      image: \"" + image + "\""
				out += "\n      networks:"
				out += "\n        - \"" + os.Getenv("CLUSTER_NAME") + "\""
			case "aws":
				region := defaultStr(spec.Spec.AWS.Region, "eu-central-1")
				ami := pool.field("ami", "")
				if !safeTokenRe.MatchString(region) {
					ui.Errorf(stderr, "Invalid region: %s", region)
					return fmt.Errorf("kubeone: invalid region %q", region)
				}
				if ami != "" && !safeTokenRe.MatchString(ami) {
					ui.Errorf(stderr, "Invalid ami for pool %s: %s", pool.name, ami)
					return fmt.Errorf("kubeone: invalid ami for pool %s", pool.name)
				}
				out += "\n      instanceType: \"" + poolType + "\""
				out += "\n      region: \"" + region + "\""
				if ami != "" {
					out += "\n      ami: \"" + ami + "\""
				}
			default:
				ui.Errorf(stderr, "Unsupported provider for dynamic workers: %s", provider)
				return fmt.Errorf("kubeone: unsupported provider %q for dynamic workers", provider)
			}
		}
	}

	manifest := filepath.Join(outputDir, "kubeone.yaml")
	if err := os.WriteFile(manifest, []byte(out+"\n"), 0o644); err != nil {
		return err
	}

	// spec.oidc → features.openidConnect, merged into the manifest's
	// features block (no-op without spec.oidc). KubeOne-native: propagates
	// to joiners, no file delivery.
	if err := d.injectOIDC(manifest); err != nil {
		return err
	}
	// spec.registries → containerd registry auth (optional secretRef).
	if err := d.injectRegistryAuth(manifest, clusterYAML); err != nil {
		return err
	}
	ui.Debugf(stderr, "Generated kubeone.yaml at %s", manifest)
	return nil
}

// ── worker pools (the utils/spec.sh reader, natively) ─────

type pool struct {
	name string
	node *yaml.Node // the pool's mapping node
}

// field reads one field of the pool with the shared default rule: the
// default is applied for missing AND null values, and a legitimate `false`
// is preserved (bash spec::pool_field applies the default itself for
// exactly that reason).
func (p pool) field(name, def string) string {
	if p.node == nil || p.node.Kind != yaml.MappingNode {
		return def
	}
	for i := 0; i+1 < len(p.node.Content); i += 2 {
		if p.node.Content[i].Value == name {
			v := p.node.Content[i+1]
			if v.Tag == "!!null" || v.Value == "" && len(v.Content) == 0 {
				return def
			}
			return v.Value
		}
	}
	return def
}

// workerPools iterates spec.workers in DOCUMENT ORDER (bash: yq keys —
// mikefarah yq preserves map order).
func workerPools(workers *yaml.Node) []pool {
	if workers == nil || workers.Kind != yaml.MappingNode {
		return nil
	}
	var pools []pool
	for i := 0; i+1 < len(workers.Content); i += 2 {
		pools = append(pools, pool{name: workers.Content[i].Value, node: workers.Content[i+1]})
	}
	return pools
}

// ── OIDC injection (kubeone::_inject_oidc) ────────────────

func (d *Driver) injectOIDC(manifest string) error {
	stderr := d.stderr()
	if !oidc.Enabled() {
		return nil
	}
	if !fileExists(manifest) {
		ui.Errorf(stderr, "OIDC: manifest not found: %s", manifest)
		return fmt.Errorf("kubeone: OIDC manifest not found: %s", manifest)
	}
	doc, err := loadYAMLDoc(manifest)
	if err != nil {
		ui.Errorf(stderr, "OIDC: failed to inject features.openidConnect")
		return fmt.Errorf("kubeone: OIDC injection failed: %w", err)
	}
	oc := ensureMapPath(doc, "features", "openidConnect")
	setKey(oc, "enable", boolNode(true))
	cfg := ensureMapPath(doc, "features", "openidConnect", "config")
	setKey(cfg, "issuerUrl", strNode(os.Getenv(oidc.EnvIssuer)))
	setKey(cfg, "clientId", strNode(os.Getenv(oidc.EnvClientID)))
	setKey(cfg, "usernameClaim", strNode(os.Getenv(oidc.EnvUsernameClaim)))
	setKey(cfg, "usernamePrefix", strNode(os.Getenv(oidc.EnvUsernamePrefix)))
	setKey(cfg, "groupsClaim", strNode(os.Getenv(oidc.EnvGroupsClaim)))
	setKey(cfg, "groupsPrefix", strNode(os.Getenv(oidc.EnvGroupsPrefix)))

	// caBundle (optional): KubeOne auto-uploads + mounts the caFile, so a
	// custom-CA issuer just needs the local file + caFile path.
	if ca := os.Getenv(oidc.EnvCABundle); ca != "" {
		caDir := filepath.Join(filepath.Dir(manifest), ".oidc")
		if err := os.MkdirAll(caDir, 0o755); err != nil {
			return err
		}
		caFile := filepath.Join(caDir, "oidc-ca.pem")
		if err := os.WriteFile(caFile, []byte(ca+"\n"), 0o644); err != nil {
			ui.Errorf(stderr, "OIDC: failed to set caFile")
			return fmt.Errorf("kubeone: OIDC caFile write failed: %w", err)
		}
		setKey(cfg, "caFile", strNode(caFile))
	}
	if err := saveYAMLDoc(manifest, doc); err != nil {
		ui.Errorf(stderr, "OIDC: failed to inject features.openidConnect")
		return fmt.Errorf("kubeone: OIDC injection failed: %w", err)
	}
	ui.Debugf(stderr, "OIDC: features.openidConnect injected (issuer %s)", os.Getenv(oidc.EnvIssuer))
	return nil
}

// ── Registry auth injection (kubeone::_inject_registry_auth) ─

// injectRegistryAuth resolves spec.registries secretRefs from the
// per-domain secret store and writes containerRuntime.containerd.
// registries.<host>.auth so EVERY node authenticates — creds never enter
// the spec. No-op without spec.registries. A spec that ASKS for auth but
// ends up with NONE configured is a breaking misconfig, not a silent
// fall-back to anonymous pulls.
func (d *Driver) injectRegistryAuth(manifest, clusterYAML string) error {
	stderr := d.stderr()
	spec, err := loadClusterSpec(clusterYAML)
	if err != nil {
		// bash: reads are 2>/dev/null-guarded — an unreadable spec reads as
		// "no registries".
		return nil
	}
	entries := registryEntries(&spec.Spec.Registries)
	if len(entries) == 0 {
		return nil
	}
	// DOMAIN_NAME anchors the per-domain secrets path; without it the
	// secretRefs would resolve under the wrong store — fail loud instead.
	domainName := os.Getenv("DOMAIN_NAME")
	if domainName == "" {
		ui.Errorf(stderr, "registry auth: DOMAIN_NAME not exported; cannot resolve registry secrets")
		return fmt.Errorf("kubeone: registry auth without DOMAIN_NAME")
	}
	secretsDir := filepath.Join(d.deps.Paths.Clusters, domainName, "secrets")

	doc, err := loadYAMLDoc(manifest)
	if err != nil {
		return err
	}
	nSpecified, nConfigured := 0, 0
	for _, e := range entries {
		if e.host == "" || e.secretName == "" {
			continue
		}
		nSpecified++
		if !regHostRe.MatchString(e.host) {
			ui.Warnf(stderr, "registry auth: skipping invalid host '%s'", e.host)
			continue
		}
		if !secretNameRe.MatchString(e.secretName) {
			ui.Warnf(stderr, "registry auth: skipping invalid secret name '%s'", e.secretName)
			continue
		}
		if e.namespace != "" && !secretNameRe.MatchString(e.namespace) {
			ui.Warnf(stderr, "registry auth: skipping invalid namespace '%s'", e.namespace)
			continue
		}
		ns := defaultStr(e.namespace, "provisioning")
		username := readSecretFile(filepath.Join(secretsDir, "Secret."+e.secretName+"."+ns+".username"))
		password := readSecretFile(filepath.Join(secretsDir, "Secret."+e.secretName+"."+ns+".password"))
		if username == "" || password == "" {
			ui.Warnf(stderr, "registry auth: secret %s/%s lacks username/password — '%s' left anonymous", ns, e.secretName, e.host)
			continue
		}
		auth := ensureMapPath(doc, "containerRuntime", "containerd", "registries", e.host, "auth")
		setKey(auth, "username", strNode(username))
		setKey(auth, "password", strNode(password))
		nConfigured++
		ui.Debugf(stderr, "registry auth: %s ← %s/%s", e.host, ns, e.secretName)
	}
	if nSpecified > 0 && nConfigured == 0 {
		ui.Errorf(stderr, "registry auth: %d registry(ies) declared in spec.registries but none could be configured (missing/invalid secretRef creds) — populate the secret(s) or remove spec.registries", nSpecified)
		return fmt.Errorf("kubeone: registry auth configured nothing")
	}
	if nConfigured > 0 {
		return saveYAMLDoc(manifest, doc)
	}
	return nil
}

type registryEntry struct {
	host       string
	secretName string
	namespace  string
}

// registryEntries reads spec.registries in document order:
// host → auth.secretRef.{name, namespace // "provisioning"}.
func registryEntries(regs *yaml.Node) []registryEntry {
	if regs == nil || regs.Kind != yaml.MappingNode {
		return nil
	}
	var out []registryEntry
	for i := 0; i+1 < len(regs.Content); i += 2 {
		e := registryEntry{host: regs.Content[i].Value, namespace: "provisioning"}
		var val struct {
			Auth struct {
				SecretRef struct {
					Name      string `yaml:"name"`
					Namespace string `yaml:"namespace"`
				} `yaml:"secretRef"`
			} `yaml:"auth"`
		}
		if regs.Content[i+1].Decode(&val) == nil {
			e.secretName = val.Auth.SecretRef.Name
			if val.Auth.SecretRef.Namespace != "" {
				e.namespace = val.Auth.SecretRef.Namespace
			}
		}
		out = append(out, e)
	}
	return out
}

// readSecretFile reads a per-domain secret cache file like the bash
// `$(cat …)` — trailing newlines stripped, "" on any error.
func readSecretFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimRight(string(raw), "\n")
}
