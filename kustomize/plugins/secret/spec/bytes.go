package spec

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// BytesEntry is the spec for a value that is N raw crypto/rand bytes rendered
// through an encoding — the same "read Bytes crypto/rand bytes, then encode"
// behavior as a template charset/bytes field's bytes-mode, but as a first-class
// sub-section so a template can declare `bytes: { seed: {bytes: 32} }` (the
// synapse seed case) alongside the other typed sub-sections.
//
// Shorthand form:
//
//	seed: 32                              # int → bytes, default encoding (base64)
//
// Full mapping:
//
//	seed: { bytes: 32, encoding: base64-unpadded }
type BytesEntry struct {
	// Bytes is the number of raw crypto/rand bytes to read (> 0).
	Bytes int `yaml:"bytes,omitempty"`
	// Encoding names how the raw bytes become text: base64, base64url,
	// base64-unpadded, base64url-unpadded, or hex. Defaults to base64.
	Encoding string `yaml:"encoding,omitempty"`
}

// bytesEntryRaw avoids infinite recursion in UnmarshalYAML.
type bytesEntryRaw BytesEntry

// UnmarshalYAML accepts a shorthand integer (byte count) or a full mapping,
// with strict field checking, then validates the count and encoding.
func (b *BytesEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		var n int
		if err := node.Decode(&n); err != nil {
			return kyaml.NodeErrf(node, "bytes shorthand must be an integer byte count, got %q", node.Value)
		}
		b.Bytes = n
	case yaml.MappingNode:
		var raw bytesEntryRaw
		if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
			return err
		}
		*b = BytesEntry(raw)
	default:
		return kyaml.NodeErrf(node, "bytes entry must be int or mapping, got %s", nodeKindString(node.Kind))
	}
	if err := b.validate(); err != nil {
		return kyaml.NodeErrf(node, "%v", err)
	}
	return nil
}

// validate checks the byte count and encoding, mirroring a template bytes
// field's bytes-mode constraints (positive count, ≤ cap, known encoding).
func (b BytesEntry) validate() error {
	if b.Bytes <= 0 {
		return fmt.Errorf("bytes must be > 0, got %d", b.Bytes)
	}
	if b.Bytes > MaxFieldBytes {
		return fmt.Errorf("bytes must be <= %d, got %d (a secret this large is almost certainly a typo)", MaxFieldBytes, b.Bytes)
	}
	if _, err := validateEncoding(b.EffectiveEncoding()); err != nil {
		return err
	}
	return nil
}

// EffectiveEncoding returns the configured encoding or the default ("base64").
func (b BytesEntry) EffectiveEncoding() string {
	if b.Encoding == "" {
		return EncBase64
	}
	return b.Encoding
}
