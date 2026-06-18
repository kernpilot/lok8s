package spec

import (
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// EnvEntry is the spec for a value sourced from an environment variable.
//
// Shorthand forms:
//
//	GOOGLE_KEY: AUTHENTIK_GOOGLE_KEY      # scalar string → var name
//	GOOGLE_KEY: ~                         # null → use the key itself as var name
//	GOOGLE_KEY:                           # full mapping
//	  var: AUTHENTIK_GOOGLE_KEY
//	  update: true                        # re-read env on every run
//
// When update is false (the default), the env value is read once on
// the first cache miss and cached. Subsequent runs reuse the cached
// value. When update is true, the cache is bypassed entirely and the
// current env value is read every run.
type EnvEntry struct {
	// Var is the environment variable name to read. Empty means "use
	// the key itself" (i.e. if the spec is `FOO: ~`, read $FOO).
	Var string `yaml:"var,omitempty"`
	// Update bypasses the cache and re-reads the env on every run.
	// Defaults to false (cache-first behavior).
	Update bool `yaml:"update,omitempty"`
}

// envEntryRaw avoids infinite recursion in UnmarshalYAML.
type envEntryRaw EnvEntry

// UnmarshalYAML accepts shorthand string (var name), null (use key as
// var name), or a full mapping.
func (e *EnvEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// Null tag (~) → leave Var empty so the generator uses the key.
		if node.Tag == "!!null" {
			return nil
		}
		// Plain string → set Var.
		var s string
		if err := node.Decode(&s); err != nil {
			return kyaml.NodeErrf(node, "env shorthand must be a string or null, got %q", node.Value)
		}
		e.Var = s
		return nil
	case yaml.MappingNode:
		var raw envEntryRaw
		if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
			return err
		}
		*e = EnvEntry(raw)
		return nil
	default:
		return kyaml.NodeErrf(node, "env entry must be string, null, or mapping, got %s", nodeKindString(node.Kind))
	}
}

// EffectiveVar returns the env var name to read for the given key.
// Falls back to the key itself when Var is empty. Value receiver so it
// works on map-element values directly.
func (e EnvEntry) EffectiveVar(key string) string {
	if e.Var == "" {
		return key
	}
	return e.Var
}
