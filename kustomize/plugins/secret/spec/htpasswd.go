package spec

import (
	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// HtpasswdEntry is the spec for an htpasswd-formatted secret entry.
// Each entry produces a single output value of the form
// "username:$2y$10$<bcrypt hash>" and writes the username, plaintext
// password, and bcrypt hash to three separate cache files.
//
// Both username and password can be either generated or literal:
//
//	smtp.htpasswd:
//	  username: {length: 16}              # generate alphanumeric of length 16
//	  password: {length: 32}              # generate alphanumeric of length 32
//
//	alt.htpasswd:
//	  username: explicit-user             # literal
//	  password:
//	    length: 64
//	    chars: alphanum+symbols
type HtpasswdEntry struct {
	Username UserOrGen `yaml:"username"`
	Password UserOrGen `yaml:"password"`
}

// UserOrGen is a sum type: either a literal value or a generation spec.
// In YAML, scalar strings → literal, mappings → PasswdEntry generation
// spec.
type UserOrGen struct {
	Literal string       // set if user provided a literal scalar
	Gen     *PasswdEntry // set if user provided a length/chars mapping
}

// IsLiteral reports whether the user provided a literal value.
// Value receiver so it works on map-element values directly.
func (u UserOrGen) IsLiteral() bool { return u.Gen == nil }

// htpasswdEntryRaw avoids infinite recursion.
type htpasswdEntryRaw HtpasswdEntry

// UnmarshalYAML for HtpasswdEntry: must be a mapping.
func (h *HtpasswdEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return kyaml.NodeErrf(node, "htpasswd entry must be a mapping, got %s", nodeKindString(node.Kind))
	}
	var raw htpasswdEntryRaw
	if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
		return err
	}
	*h = HtpasswdEntry(raw)
	return nil
}

// UnmarshalYAML for UserOrGen: scalar → Literal, mapping → Gen.
func (u *UserOrGen) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		// Treat null as "no literal" (caller must provide a Gen via mapping).
		if node.Tag == "!!null" || node.Value == "" {
			return kyaml.NodeErrf(node, "htpasswd username/password must not be empty (use a literal string or {length: N})")
		}
		u.Literal = node.Value
		return nil
	case yaml.MappingNode:
		gen := &PasswdEntry{}
		// Reuse PasswdEntry's UnmarshalYAML logic so length/chars/strict
		// checking is identical to the passwd generator.
		if err := gen.UnmarshalYAML(node); err != nil {
			return err
		}
		u.Gen = gen
		return nil
	default:
		return kyaml.NodeErrf(node, "htpasswd username/password must be string or mapping, got %s", nodeKindString(node.Kind))
	}
}
