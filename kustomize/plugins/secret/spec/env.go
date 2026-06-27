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
//
// Env-miss fallback (mutually exclusive — at most one). Without one, a
// missing env var is an error (the default "required" behavior):
//
//	ROBOT_USER:  { var: HROBOT_USER, optional: true }   # unset → OMIT the key
//	SOME_ID:     { var: X, default: "literal" }         # unset → literal value
//	SESSION_KEY: { var: SK, passwd: { length: 64 } }    # unset → generate + cache
type EnvEntry struct {
	// Var is the environment variable name to read. Empty means "use
	// the key itself" (i.e. if the spec is `FOO: ~`, read $FOO).
	Var string `yaml:"var,omitempty"`
	// Update bypasses the cache and re-reads the env on every run.
	// Defaults to false (cache-first behavior).
	Update bool `yaml:"update,omitempty"`
	// Optional, when true, OMITS the key entirely if the env var is unset
	// (instead of erroring). Use for keys a consumer treats as optional
	// (e.g. an optional secretKeyRef the chart mounts with optional: true).
	Optional bool `yaml:"optional,omitempty"`
	// Default is a literal fallback used when the env var is unset. A nil
	// pointer means "no default" — which distinguishes an unset default from
	// an explicit empty one (`default: ""`).
	Default *string `yaml:"default,omitempty"`
	// Passwd generates (and caches) a random password when the env var is
	// unset — the "operator-can-override-or-we-mint-one" pattern. Requires
	// PATH_SECRETS; the generated value is cached so it stays stable.
	Passwd *PasswdEntry `yaml:"passwd,omitempty"`
}

// fallbackModes counts how many env-miss fallbacks are configured. At most one
// is allowed (optional / default / passwd are mutually exclusive).
func (e EnvEntry) fallbackModes() int {
	n := 0
	if e.Optional {
		n++
	}
	if e.Default != nil {
		n++
	}
	if e.Passwd != nil {
		n++
	}
	return n
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
		if e.fallbackModes() > 1 {
			return kyaml.NodeErrf(node, "env entry: at most one of optional/default/passwd may be set")
		}
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
