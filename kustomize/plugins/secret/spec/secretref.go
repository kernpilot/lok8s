package spec

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// SecretRefEntry is the spec for a value pulled from another Secret's
// cache file. Cross-secret references read $PATH_SECRETS files written
// by other generator runs (typically passwd/htpasswd in another Secret
// resource within the same kustomize build).
//
// Shorthand forms:
//
//	DB_PASSWORD: db-secret/password               # secret/key (current namespace)
//	DB_PASSWORD: db-secret/other-ns/password      # secret/namespace/key
//	DB_PASSWORD:                                  # full mapping
//	  secret: db-secret
//	  namespace: other-ns
//	  key: password
type SecretRefEntry struct {
	// Secret is the producer secret's metadata.name.
	Secret string `yaml:"secret"`
	// Namespace defaults to the current Secret's namespace.
	Namespace string `yaml:"namespace,omitempty"`
	// Key is the data key inside the producer secret.
	Key string `yaml:"key"`
}

// secretRefEntryRaw avoids infinite recursion.
type secretRefEntryRaw SecretRefEntry

// UnmarshalYAML accepts shorthand "secret/key" or "secret/ns/key", or
// a full mapping.
func (r *SecretRefEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		s := node.Value
		if s == "" {
			return kyaml.NodeErrf(node, "secretRef shorthand must not be empty")
		}
		parts := strings.Split(s, "/")
		switch len(parts) {
		case 2:
			r.Secret = parts[0]
			r.Key = parts[1]
		case 3:
			r.Secret = parts[0]
			r.Namespace = parts[1]
			r.Key = parts[2]
		default:
			return kyaml.NodeErrf(node, "secretRef shorthand must be \"secret/key\" or \"secret/namespace/key\", got %q", s)
		}
		return r.validateFields(node)
	case yaml.MappingNode:
		var raw secretRefEntryRaw
		if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
			return err
		}
		*r = SecretRefEntry(raw)
		return r.validateFields(node)
	default:
		return kyaml.NodeErrf(node, "secretRef entry must be string or mapping, got %s", nodeKindString(node.Kind))
	}
}

func (r *SecretRefEntry) validateFields(node *yaml.Node) error {
	if r.Secret == "" {
		return kyaml.NodeErrf(node, "secretRef.secret is required")
	}
	if r.Key == "" {
		return kyaml.NodeErrf(node, "secretRef.key is required")
	}
	return nil
}

// EffectiveNamespace returns the explicit namespace, or the fallback if empty.
// Value receiver so it works on map-element values directly.
func (r SecretRefEntry) EffectiveNamespace(fallback string) string {
	if r.Namespace == "" {
		return fallback
	}
	return r.Namespace
}
