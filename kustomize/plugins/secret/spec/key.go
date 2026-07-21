package spec

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// KeyEntry is the spec for a generated asymmetric private key, emitted as
// PKCS#8 PEM (the format openssl's `genpkey` default produces). It replaces the
// `bash: { exec: "openssl genpkey ..." }` idiom for private keys: no shell, no
// approval gate, and cache-first so an existing key is reused verbatim (never
// re-keyed).
//
// Shorthand forms:
//
//	passkey.pem: rsa                      # algorithm string → all defaults
//	signing.pem: ed25519
//
// Full mapping:
//
//	passkey.pem:
//	  algorithm: rsa                       # rsa (default) | ed25519
//	  bits: 4096                           # RSA modulus size (rsa only; default 4096)
//	  encoding: pkcs8                      # pkcs8 (default; the only supported form)
type KeyEntry struct {
	// Algorithm selects the key type: "rsa" (default) or "ed25519".
	Algorithm string `yaml:"algorithm,omitempty"`
	// Bits is the RSA modulus size in bits (rsa only). Defaults to 4096.
	// Must be unset (or 0) for ed25519, which has a fixed key size.
	Bits int `yaml:"bits,omitempty"`
	// Encoding is the private-key serialization. Only "pkcs8" (the default,
	// matching openssl genpkey) is supported.
	Encoding string `yaml:"encoding,omitempty"`
}

// Key algorithm + encoding names.
const (
	KeyAlgoRSA     = "rsa"
	KeyAlgoEd25519 = "ed25519"
	KeyEncPKCS8    = "pkcs8"
)

// DefaultRSABits is the RSA modulus size used when a key: entry omits bits.
const DefaultRSABits = 4096

// MinRSABits rejects fat-fingered / insecure RSA sizes at decode time.
const MinRSABits = 2048

// keyEntryRaw avoids infinite recursion in UnmarshalYAML.
type keyEntryRaw KeyEntry

// UnmarshalYAML accepts a shorthand algorithm string or a full mapping, with
// strict field checking, then validates the algorithm/bits/encoding contract.
func (k *KeyEntry) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag == "!!null" || node.Value == "" {
			return kyaml.NodeErrf(node, "key shorthand must be an algorithm name (rsa or ed25519)")
		}
		k.Algorithm = node.Value
	case yaml.MappingNode:
		var raw keyEntryRaw
		if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
			return err
		}
		*k = KeyEntry(raw)
	default:
		return kyaml.NodeErrf(node, "key entry must be a string or mapping, got %s", nodeKindString(node.Kind))
	}
	if err := k.validate(); err != nil {
		return kyaml.NodeErrf(node, "%v", err)
	}
	return nil
}

// validate checks the algorithm, bits, and encoding are a coherent combination.
func (k KeyEntry) validate() error {
	switch k.EffectiveAlgorithm() {
	case KeyAlgoRSA:
		if k.Bits != 0 && k.Bits < MinRSABits {
			return fmt.Errorf("rsa bits must be >= %d, got %d", MinRSABits, k.Bits)
		}
	case KeyAlgoEd25519:
		if k.Bits != 0 {
			return fmt.Errorf("ed25519 has a fixed key size — remove bits (got %d)", k.Bits)
		}
	default:
		return fmt.Errorf("unknown algorithm %q (valid: rsa, ed25519)", k.Algorithm)
	}
	if enc := k.EffectiveEncoding(); enc != KeyEncPKCS8 {
		return fmt.Errorf("unknown encoding %q (only pkcs8 is supported)", enc)
	}
	return nil
}

// EffectiveAlgorithm returns the configured algorithm or the default ("rsa").
func (k KeyEntry) EffectiveAlgorithm() string {
	if k.Algorithm == "" {
		return KeyAlgoRSA
	}
	return k.Algorithm
}

// EffectiveBits returns the configured RSA bit size or the default (4096).
// Meaningful only for RSA keys.
func (k KeyEntry) EffectiveBits() int {
	if k.Bits == 0 {
		return DefaultRSABits
	}
	return k.Bits
}

// EffectiveEncoding returns the configured encoding or the default ("pkcs8").
func (k KeyEntry) EffectiveEncoding() string {
	if k.Encoding == "" {
		return KeyEncPKCS8
	}
	return k.Encoding
}
