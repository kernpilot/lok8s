package spec

import (
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// PasswdEntry is the spec for a generated random password.
//
// Shorthand forms:
//
//	REDIS_PASSWORD: 32                    # int → length
//	WP_TOKEN: {}                          # empty mapping → all defaults
//	NUXT_PASSWORD:                        # full mapping
//	  length: 128
//	  chars: alphanum+symbols
type PasswdEntry struct {
	// Length is the password length in characters. Defaults to 32 when zero.
	Length int `yaml:"length"`
	// Chars is a charset DSL string (alphanum, alphanum+symbols, hex,
	// base64url, custom:<chars>). Defaults to "alphanum" when empty.
	Chars string `yaml:"chars,omitempty"`
}

// passwdEntryRaw is used to avoid infinite recursion in UnmarshalYAML.
// We can't decode into PasswdEntry from inside its own unmarshaler
// without an alias type.
type passwdEntryRaw PasswdEntry

// UnmarshalYAML accepts shorthand integer (length) or a full mapping.
func (p *PasswdEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var n int
		if err := node.Decode(&n); err != nil {
			return kyaml.NodeErrf(node, "passwd shorthand must be an integer length, got %q", node.Value)
		}
		if n <= 0 {
			return kyaml.NodeErrf(node, "passwd length must be > 0, got %d", n)
		}
		p.Length = n
		return nil
	case yaml.MappingNode:
		var raw passwdEntryRaw
		if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
			return err
		}
		*p = PasswdEntry(raw)
		if p.Length < 0 {
			return kyaml.NodeErrf(node, "passwd length must be >= 0, got %d", p.Length)
		}
		return nil
	default:
		return kyaml.NodeErrf(node, "passwd entry must be int or mapping, got %s", nodeKindString(node.Kind))
	}
}

// EffectiveLength returns the configured length or the default (32).
// Value receiver so it works on map-element values directly.
func (p PasswdEntry) EffectiveLength() int {
	if p.Length == 0 {
		return 32
	}
	return p.Length
}

// EffectiveChars returns the configured charset or the default ("alphanum").
func (p PasswdEntry) EffectiveChars() string {
	if p.Chars == "" {
		return "alphanum"
	}
	return p.Chars
}
