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
	"io"

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

	// Validate enables required-key validation for the chosen Type.
	// When false, type-specific key requirements (e.g. tls.crt+tls.key
	// for kubernetes.io/tls) are not enforced. Defaults to true via
	// the validatePtr field below + ValidationEnabled() helper.
	ValidatePtr *bool `yaml:"validate,omitempty"`

	// Generator sections. Each is a map of key → producer spec, with
	// custom UnmarshalYAML support for shorthand forms.
	Literals  map[string]string         `yaml:"literals,omitempty"`
	Passwd    map[string]PasswdEntry    `yaml:"passwd,omitempty"`
	Bash      map[string]BashEntry      `yaml:"bash,omitempty"`
	Env       map[string]EnvEntry       `yaml:"env,omitempty"`
	SecretRef map[string]SecretRefEntry `yaml:"secretRef,omitempty"`
	Htpasswd  map[string]HtpasswdEntry  `yaml:"htpasswd,omitempty"`
	File      map[string]FileEntry      `yaml:"file,omitempty"`
	B64       map[string]string         `yaml:"b64,omitempty"`

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
// structured *errs.Error on parse failures (with line numbers).
func Decode(r io.Reader) (*Secret, error) {
	var s Secret
	if err := kyaml.DecodeStrict(r, &s); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// DecodeBytes is the []byte form of Decode.
func DecodeBytes(data []byte) (*Secret, error) {
	var s Secret
	if err := kyaml.DecodeStrictBytes(data, &s); err != nil {
		return nil, err
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
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
	return nil
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
