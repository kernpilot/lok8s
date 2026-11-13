// Package spec defines the CRD types for the secrets.lok8s.dev/v1
// Secret resource and the strict YAML parsing logic for each generator
// section.
//
// Every generator section supports both a shorthand and a long form via
// custom UnmarshalYAML implementations. The top-level Decode entrypoint
// applies strict field checking so unknown fields are rejected with
// line numbers.
package spec

import (
	"errors"
	"io"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// APIVersion is the only supported group/version for this CRD.
const APIVersion = "secrets.lok8s.dev/v1"

// Kind is the resource kind.
const Kind = "Secret"

// Secret is the user-facing CRD root type. The Validate field defaults
// to true; users opt out of type-key validation with `validate: false`.
type Secret struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`

	// Type is the k8s Secret type (Opaque, kubernetes.io/tls, ...).
	// Defaults to Opaque when omitted.
	Type string `yaml:"type,omitempty"`

	// Immutable, when true, emits `immutable: true` on the rendered
	// Secret: the apiserver then rejects any apply that would CHANGE the
	// value ("field is immutable") — rotation becomes a deliberate
	// delete+recreate (`lo up --force-recreate`). Use for crown-jewel
	// secrets whose silent rotation orphans stateful data (encryption
	// masterkeys, DB credentials, signing keys). Omitted ⇒ k8s default
	// (mutable).
	Immutable *bool `yaml:"immutable,omitempty"`

	// Validate enables required-key validation for the chosen Type.
	// When false, type-specific key requirements (e.g. tls.crt+tls.key
	// for kubernetes.io/tls) are not enforced. Defaults to true via
	// the validatePtr field below + ValidationEnabled() helper.
	ValidatePtr *bool `yaml:"validate,omitempty"`

	// Generator sections. Each is a map of key → producer spec, with
	// custom UnmarshalYAML support for shorthand forms.
	Literals  map[string]string         `yaml:"literals,omitempty"`
	Passwd    map[string]PasswdEntry    `yaml:"passwd,omitempty"`
	Template  map[string]TemplateEntry  `yaml:"template,omitempty"`
	Bash      map[string]BashEntry      `yaml:"bash,omitempty"`
	Env       map[string]EnvEntry       `yaml:"env,omitempty"`
	SecretRef map[string]SecretRefEntry `yaml:"secretRef,omitempty"`
	Htpasswd  map[string]HtpasswdEntry  `yaml:"htpasswd,omitempty"`
	File      map[string]FileEntry      `yaml:"file,omitempty"`
	B64       map[string]string         `yaml:"b64,omitempty"`
	Key       map[string]KeyEntry       `yaml:"key,omitempty"`

	// Cert is a single development CA or leaf certificate (one cert per Secret),
	// generated with crypto/x509 — no mkcert binary. See CertSpec.
	Cert *CertSpec `yaml:"cert,omitempty"`
}

// Metadata is the standard k8s metadata subset we accept.
type Metadata struct {
	Name        string            `yaml:"name"`
	Namespace   string            `yaml:"namespace,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// ValidationEnabled returns whether type-key validation should run.
// Defaults to true; users opt out via `validate: false`.
func (s *Secret) ValidationEnabled() bool {
	if s.ValidatePtr == nil {
		return true
	}
	return *s.ValidatePtr
}

// Decode reads a Secret from r with strict field checking. Returns a
// structured *errs.Error on parse failures (with line numbers and, for an
// error in a section, the path <section>.<key>). An error never holds a
// value of the spec: a value can be a secret.
func Decode(r io.Reader) (*Secret, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return DecodeBytes(data)
}

// DecodeBytes is the []byte form of Decode.
func DecodeBytes(data []byte) (*Secret, error) {
	var s Secret
	if err := kyaml.DecodeStrictBytes(data, &s); err != nil {
		return nil, withSpecPath(data, err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// withSpecPath sets the path of a decode error from its position: the
// top-level key that holds the position and, in a mapping, the key of the
// entry (passwd.PASSWORD). A section can hold a secret value, so the
// path, not the value, says where the error is.
func withSpecPath(data []byte, err error) error {
	var e *errs.Error
	if !errors.As(err, &e) || e.Line == 0 || e.Path != "" {
		return err
	}
	var doc yaml.Node
	if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return err
	}
	section, value := keyAt(doc.Content[0], e.Line, e.Col)
	if section == "" {
		return err
	}
	e.Path = section
	if value.Kind == yaml.MappingNode {
		if key, _ := keyAt(value, e.Line, e.Col); key != "" {
			e.Path = section + "." + key
		}
	}
	return err
}

// keyAt returns the last key of the mapping m (and its value) that starts
// at or before line:col (col 0: anywhere on the line), or "" when the
// position is before the first key.
func keyAt(m *yaml.Node, line, col int) (string, *yaml.Node) {
	key, value := "", (*yaml.Node)(nil)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Line > line || (k.Line == line && col > 0 && k.Column > col) {
			break
		}
		key, value = k.Value, m.Content[i+1]
	}
	return key, value
}

// validate checks the structural invariants of a decoded Secret. It
// does NOT check generator semantics (those are validated when each
// generator runs).
func (s *Secret) validate() error {
	if s.APIVersion != APIVersion {
		return errs.Newf("apiVersion must be %q, got %q", APIVersion, s.APIVersion)
	}
	if s.Kind != Kind {
		return errs.Newf("kind must be %q, got %q", Kind, s.Kind)
	}
	if s.Metadata.Name == "" {
		return errs.New("metadata.name is required")
	}
	// A `!!null` map value (`R: ~`) skips SecretRefEntry.UnmarshalYAML (yaml.v3
	// quirk), leaving Secret+Key empty — which the string/mapping forms would
	// have rejected. Catch it here so it fails with a clear decode error rather
	// than a confusing "read /default/: no such file" at generation time.
	for _, key := range sortedRefKeys(s.SecretRef) {
		r := s.SecretRef[key]
		if r.Secret == "" {
			return errs.Newf("secretRef %q: secret is required (a null/`~` value?)", key)
		}
		if r.Key == "" {
			return errs.Newf("secretRef %q: key is required (a null/`~` value?)", key)
		}
	}
	return nil
}

// sortedRefKeys returns the sorted keys of a secretRef map for a deterministic
// validation order (stable error messages).
func sortedRefKeys(m map[string]SecretRefEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// nodeKindString is a small helper used by custom unmarshalers in the
// other spec files for clearer error messages.
func nodeKindString(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	}
	return "unknown"
}
