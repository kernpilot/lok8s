package kubeone

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/internal/oidc"
	"github.com/kernpilot/lok8s/internal/ui"
)

// clusterSpec is the subset of a cluster.lok8s.yaml the KubeOne driver
// reads. Fields that may legally hold non-string YAML scalars decode as
// `any` and are stringified with yq's `-r` + `//` semantics (yqStr).
type clusterSpec struct {
	Metadata struct {
		Name any `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Kubernetes struct {
			Version any `yaml:"version"`
		} `yaml:"kubernetes"`
		Cluster struct {
			Domain string `yaml:"domain"`
		} `yaml:"cluster"`
		DNS struct {
			DomainFilter string `yaml:"domainFilter"`
		} `yaml:"dns"`
		Network struct {
			PodSubnet     string `yaml:"podSubnet"`
			ServiceSubnet string `yaml:"serviceSubnet"`
			CNI           string `yaml:"cni"`
			CSI           string `yaml:"csi"`
			KubeProxy     any    `yaml:"kubeProxy"`
		} `yaml:"network"`
		ControlPlane struct {
			Replicas any `yaml:"replicas"`
		} `yaml:"controlPlane"`
		Provider struct {
			Name string `yaml:"name"`
		} `yaml:"provider"`
		AWS struct {
			Region string `yaml:"region"`
		} `yaml:"aws"`
		SSH struct {
			User string `yaml:"user"`
			Port any    `yaml:"port"`
		} `yaml:"ssh"`
		Addons struct {
			Enabled any    `yaml:"enabled"`
			Path    string `yaml:"path"`
		} `yaml:"addons"`
		Datacenter string    `yaml:"datacenter"`
		Workers    yaml.Node `yaml:"workers"`
		Registries yaml.Node `yaml:"registries"`
	} `yaml:"spec"`

	// awsPresent/hcloudPresent back provider detection (yq -e '.spec.aws').
	awsPresent    bool
	hcloudPresent bool
}

// loadClusterSpec parses the cluster spec; unlike the dispatch's
// best-effort reads, the driver's own reads fail loud (they feed a manifest
// the apiserver trusts).
func loadClusterSpec(path string) (*clusterSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc clusterSpec
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	// Presence probes for the legacy provider inference.
	var probe struct {
		Spec struct {
			Hcloud yaml.Node `yaml:"hcloud"`
			AWS    yaml.Node `yaml:"aws"`
		} `yaml:"spec"`
	}
	if yaml.Unmarshal(raw, &probe) == nil {
		doc.hcloudPresent = !probe.Spec.Hcloud.IsZero() && probe.Spec.Hcloud.Tag != "!!null"
		doc.awsPresent = !probe.Spec.AWS.IsZero() && probe.Spec.AWS.Tag != "!!null"
	}
	return &doc, nil
}

// yqStr stringifies a decoded YAML scalar with yq's `value // "default"`
// semantics: the default fires on null/missing AND on `false` (jq/yq
// alternative-operator falsiness — this is why `kubeProxy: false` reads as
// the DEFAULT "enabled", exactly as the bash comment documents).
func yqStr(v any, def string) string {
	if v == nil {
		return def
	}
	if b, ok := v.(bool); ok && !b {
		return def
	}
	return fmt.Sprint(v)
}

// yqScalar stringifies like a bare `yq -r '.path'` (no `//`): missing/null
// prints "null".
func yqScalar(v any) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprint(v)
}

// detectProvider ports provider::detect: explicit spec.provider.name wins;
// legacy specs are inferred from spec.hcloud / spec.aws.
//
// DELIBERATE DEVIATION, fail-loud: the bash call inside extract_vars was
// unguarded under a disabled errexit, so a spec with no detectable provider
// silently rendered `cloudProvider: "": {}` garbage. Here it errors.
func detectProvider(spec *clusterSpec, clusterYAML string, stderr io.Writer) (string, error) {
	if spec.Spec.Provider.Name != "" {
		return spec.Spec.Provider.Name, nil
	}
	if spec.hcloudPresent {
		return "hetzner", nil
	}
	if spec.awsPresent {
		return "aws", nil
	}
	ui.Errorf(stderr, "No provider found in cluster spec: %s", clusterYAML)
	return "", fmt.Errorf("kubeone: no provider in %s", clusterYAML)
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ExtractVars ports kubeone::extract_vars: export the spec-derived template
// and bootstrap env (CLUSTER_NAME, K8S_VERSION, POD_SUBNET, SERVICE_SUBNET,
// CP_REPLICAS, CNI_PLUGIN, CSI_PLUGIN, CLOUD_PROVIDER, KUBE_PROXY_SKIP,
// SSH_USER, SSH_PORT, ADDONS_ENABLED, ADDONS_PATH,
// LOK8S_SPEC_CLUSTER_DOMAIN, LOK8S_SPEC_DNS_DOMAINFILTER, and the
// LOK8S_SPEC_OIDC_* family). Pure spec→env, idempotent, no side effects
// beyond the process environment.
func (d *Driver) ExtractVars(ctx context.Context, clusterYAML string) error {
	stderr := d.stderr()

	// spec.oidc first — internal/oidc is the single source of truth for
	// the reads + defaults (bash: oidc::load_spec), shared by both drivers
	// and the OIDC kubeconfig.
	if err := oidc.LoadSpec(clusterYAML, stderr); err != nil {
		return err
	}

	spec, err := loadClusterSpec(clusterYAML)
	if err != nil {
		return err
	}

	os.Setenv("CLUSTER_NAME", yqScalar(spec.Metadata.Name))
	os.Setenv("K8S_VERSION", yqScalar(spec.Spec.Kubernetes.Version))
	os.Setenv("LOK8S_SPEC_CLUSTER_DOMAIN", spec.Spec.Cluster.Domain)
	os.Setenv("LOK8S_SPEC_DNS_DOMAINFILTER", spec.Spec.DNS.DomainFilter)
	os.Setenv("POD_SUBNET", defaultStr(spec.Spec.Network.PodSubnet, "10.244.0.0/16"))
	os.Setenv("SERVICE_SUBNET", defaultStr(spec.Spec.Network.ServiceSubnet, "10.96.0.0/12"))
	os.Setenv("CP_REPLICAS", yqStr(spec.Spec.ControlPlane.Replicas, "1"))
	os.Setenv("CNI_PLUGIN", defaultStr(spec.Spec.Network.CNI, "canal"))
	// spec.network.csi — provider-CSI selector. Default "external" =
	// Ceph-first: lok8s owns storage and KubeOne's embedded provider CSI is
	// shadowed (validated later, in the addon render).
	os.Setenv("CSI_PLUGIN", defaultStr(spec.Spec.Network.CSI, "external"))

	// spec.network.kubeProxy — STRING enum (a YAML bool `kubeProxy: false`
	// is NOT recognized as disable: yq's `// "enabled"` treats false as
	// falsy and yields the default).
	kubeProxy := yqStr(spec.Spec.Network.KubeProxy, "enabled")
	switch kubeProxy {
	case "enabled":
		os.Setenv("KUBE_PROXY_SKIP", "false")
	case "disabled":
		os.Setenv("KUBE_PROXY_SKIP", "true")
	default:
		ui.Errorf(stderr, "extract_vars: invalid spec.network.kubeProxy '%s' (expected 'enabled' or 'disabled')", kubeProxy)
		return fmt.Errorf("kubeone: invalid spec.network.kubeProxy %q", kubeProxy)
	}

	provider, err := detectProvider(spec, clusterYAML, stderr)
	if err != nil {
		return err
	}
	os.Setenv("CLOUD_PROVIDER", provider)

	// SSH config — read from the provider output access[] when a provider
	// is loaded, fall back to spec.ssh for backward compatibility. The
	// provider read is best-effort (bash: `|| true`).
	sshUser, sshPort := "", ""
	if d.deps.Provider != nil && d.deps.ProviderConfigFile != "" {
		if out, oerr := d.deps.Provider.Output(ctx, d.deps.ProviderConfigFile); oerr == nil && len(out) > 0 {
			var inv struct {
				Access []struct {
					User       string `json:"user"`
					Port       any    `json:"port"`
					PrivateKey string `json:"privateKey"`
					PublicKey  string `json:"publicKey"`
				} `json:"access"`
			}
			if json.Unmarshal(out, &inv) == nil && len(inv.Access) > 0 {
				a := inv.Access[0]
				sshUser = defaultStr(a.User, "root")
				sshPort = yqStr(a.Port, "22")
				os.Setenv("SSH_PRIVATE_KEY", a.PrivateKey)
				os.Setenv("SSH_PUBLIC_KEY", a.PublicKey)
			}
		}
	}
	// `: "${SSH_USER:=fallback}"` — an already-exported non-empty value is
	// kept when the provider path didn't set one.
	if sshUser == "" {
		sshUser = os.Getenv("SSH_USER")
	}
	if sshUser == "" {
		sshUser = defaultStr(spec.Spec.SSH.User, "root")
	}
	if sshPort == "" {
		sshPort = os.Getenv("SSH_PORT")
	}
	if sshPort == "" {
		sshPort = yqStr(spec.Spec.SSH.Port, "22")
	}
	os.Setenv("SSH_USER", sshUser)
	os.Setenv("SSH_PORT", sshPort)

	os.Setenv("ADDONS_ENABLED", yqStr(spec.Spec.Addons.Enabled, "false"))
	os.Setenv("ADDONS_PATH", defaultStr(spec.Spec.Addons.Path, "./addons"))
	return nil
}

// metadataName reads .metadata.name for path construction, "" when
// missing/unreadable.
func metadataName(clusterYAML string) string {
	spec, err := loadClusterSpec(clusterYAML)
	if err != nil || spec.Metadata.Name == nil {
		return ""
	}
	return fmt.Sprint(spec.Metadata.Name)
}
