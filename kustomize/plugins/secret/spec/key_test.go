package spec

import (
	"strings"
	"testing"
)

const keyHeader = `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
`

func decodeKey(t *testing.T, body string) (*Secret, error) {
	t.Helper()
	return DecodeBytes([]byte(keyHeader + body))
}

func TestKey_ShorthandAlgorithm(t *testing.T) {
	s, err := decodeKey(t, `key:
  passkey.pem: rsa
  signing.pem: ed25519
`)
	if err != nil {
		t.Fatal(err)
	}
	if s.Key["passkey.pem"].EffectiveAlgorithm() != "rsa" {
		t.Errorf("rsa shorthand: %+v", s.Key["passkey.pem"])
	}
	if s.Key["passkey.pem"].EffectiveBits() != 4096 {
		t.Errorf("default bits = %d, want 4096", s.Key["passkey.pem"].EffectiveBits())
	}
	if s.Key["signing.pem"].EffectiveAlgorithm() != "ed25519" {
		t.Errorf("ed25519 shorthand: %+v", s.Key["signing.pem"])
	}
}

func TestKey_FullMapping(t *testing.T) {
	s, err := decodeKey(t, `key:
  k:
    algorithm: rsa
    bits: 2048
    encoding: pkcs8
`)
	if err != nil {
		t.Fatal(err)
	}
	k := s.Key["k"]
	if k.EffectiveBits() != 2048 || k.EffectiveAlgorithm() != "rsa" || k.EffectiveEncoding() != "pkcs8" {
		t.Errorf("full mapping: %+v", k)
	}
}

func TestKey_DefaultsAlgorithmRSA(t *testing.T) {
	// An empty mapping defaults to rsa/4096/pkcs8.
	s, err := decodeKey(t, `key:
  k: {}
`)
	if err != nil {
		t.Fatal(err)
	}
	k := s.Key["k"]
	if k.EffectiveAlgorithm() != "rsa" || k.EffectiveBits() != 4096 || k.EffectiveEncoding() != "pkcs8" {
		t.Errorf("defaults: %+v", k)
	}
}

func TestKey_RejectsUnknownAlgorithm(t *testing.T) {
	_, err := decodeKey(t, `key:
  k: dsa
`)
	if err == nil || !strings.Contains(err.Error(), "algorithm") {
		t.Errorf("expected algorithm error, got %v", err)
	}
}

func TestKey_RejectsBitsOnEd25519(t *testing.T) {
	_, err := decodeKey(t, `key:
  k:
    algorithm: ed25519
    bits: 256
`)
	if err == nil || !strings.Contains(err.Error(), "ed25519") {
		t.Errorf("expected ed25519+bits error, got %v", err)
	}
}

func TestKey_RejectsTinyRSA(t *testing.T) {
	_, err := decodeKey(t, `key:
  k:
    algorithm: rsa
    bits: 512
`)
	if err == nil || !strings.Contains(err.Error(), "bits") {
		t.Errorf("expected small-bits error, got %v", err)
	}
}

func TestKey_RejectsUnknownEncoding(t *testing.T) {
	_, err := decodeKey(t, `key:
  k:
    encoding: pkcs1
`)
	if err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Errorf("expected encoding error, got %v", err)
	}
}

func TestKey_RejectsUnknownField(t *testing.T) {
	_, err := decodeKey(t, `key:
  k:
    algorithm: rsa
    nope: 1
`)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected unknown-field error, got %v", err)
	}
}

func TestKey_NullShorthandDefaultsToRSA(t *testing.T) {
	// yaml.v3 does not invoke UnmarshalYAML for a !!null map value, so `k: ~`
	// leaves the zero KeyEntry, which resolves to the rsa/4096/pkcs8 default —
	// consistent with the other entry types' null handling.
	s, err := decodeKey(t, `key:
  k: ~
`)
	if err != nil {
		t.Fatal(err)
	}
	k := s.Key["k"]
	if k.EffectiveAlgorithm() != "rsa" || k.EffectiveBits() != 4096 {
		t.Errorf("null shorthand should default to rsa/4096, got %+v", k)
	}
}

func TestKey_RejectsSequence(t *testing.T) {
	_, err := decodeKey(t, `key:
  k: [rsa]
`)
	if err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Errorf("expected string-or-mapping error, got %v", err)
	}
}
