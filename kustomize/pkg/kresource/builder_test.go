package kresource

import (
	"encoding/base64"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNewSecret_DefaultsToOpaque(t *testing.T) {
	b := NewSecret("name", "ns", "")
	if b.Type != SecretTypeOpaque {
		t.Errorf("type = %q, want Opaque", b.Type)
	}
}

func TestSecretBuilder_AddAndMarshal(t *testing.T) {
	b := NewSecret("test", "default", "")
	b.Add("hello", []byte("world"))
	b.Add("foo", []byte("bar"))
	out, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "name: test") {
		t.Errorf("missing name: %s", s)
	}
	if !strings.Contains(s, "type: Opaque") {
		t.Errorf("missing type: %s", s)
	}
	// Sorted: foo before hello
	if strings.Index(s, "foo:") > strings.Index(s, "hello:") {
		t.Errorf("data keys not sorted: %s", s)
	}
	// Base64 encoded
	if !strings.Contains(s, base64.StdEncoding.EncodeToString([]byte("world"))) {
		t.Errorf("missing base64 of 'world': %s", s)
	}
}

func TestSecretBuilder_AddBase64_RoundTrip(t *testing.T) {
	b := NewSecret("test", "default", SecretTypeOpaque)
	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	if err := b.AddBase64("k", []byte(encoded)); err != nil {
		t.Fatal(err)
	}
	out, _ := b.Marshal()
	// The output should contain the base64 encoding of "hello", which
	// is the same as `encoded` (round-trip is consistent).
	if !strings.Contains(string(out), encoded) {
		t.Errorf("expected %q in output, got: %s", encoded, out)
	}
}

func TestSecretBuilder_AddBase64_Invalid(t *testing.T) {
	b := NewSecret("test", "default", "")
	if err := b.AddBase64("k", []byte("not!valid!base64!")); err == nil {
		t.Error("expected error for invalid base64")
	}
}

func TestSecretBuilder_Marshal_Deterministic(t *testing.T) {
	build := func() []byte {
		b := NewSecret("t", "ns", "")
		b.Add("c", []byte("3"))
		b.Add("a", []byte("1"))
		b.Add("b", []byte("2"))
		out, _ := b.Marshal()
		return out
	}
	if string(build()) != string(build()) {
		t.Error("non-deterministic Marshal output")
	}
}

func TestValidateSecretKeys_TLSRequired(t *testing.T) {
	dataKeys := map[string]struct{}{
		"tls.crt": {},
	}
	err := ValidateSecretKeys(SecretTypeTLS, dataKeys)
	if err == nil {
		t.Error("expected error for missing tls.key")
	}
	if !strings.Contains(err.Error(), "tls.key") {
		t.Errorf("error should mention tls.key: %v", err)
	}
}

func TestValidateSecretKeys_TLSComplete(t *testing.T) {
	dataKeys := map[string]struct{}{
		"tls.crt": {},
		"tls.key": {},
	}
	if err := ValidateSecretKeys(SecretTypeTLS, dataKeys); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateSecretKeys_OpaqueAlwaysValid(t *testing.T) {
	if err := ValidateSecretKeys(SecretTypeOpaque, nil); err != nil {
		t.Error("Opaque should never have required keys")
	}
}

func TestValidateSecretKeys_BasicAuth(t *testing.T) {
	if err := ValidateSecretKeys(SecretTypeBasicAuth, map[string]struct{}{"username": {}}); err == nil {
		t.Error("expected error for missing password")
	}
}

func TestValidateSecretKeys_UnknownTypePassesThrough(t *testing.T) {
	if err := ValidateSecretKeys("custom.example.com/v1", nil); err != nil {
		t.Error("unknown types should not be validated")
	}
}

func TestIsKnownType(t *testing.T) {
	if !IsKnownType(SecretTypeTLS) {
		t.Error("TLS should be known")
	}
	if IsKnownType("custom.example.com/v1") {
		t.Error("unknown type should not be known")
	}
}

func TestSecretBuilder_HasAndKeys(t *testing.T) {
	b := NewSecret("t", "ns", "")
	b.Add("a", []byte("1"))
	b.Add("b", []byte("2"))
	if !b.Has("a") {
		t.Error("Has(a) should be true")
	}
	if b.Has("nope") {
		t.Error("Has(nope) should be false")
	}
	keys := b.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys() returned %d, want 2", len(keys))
	}
	if _, ok := keys["a"]; !ok {
		t.Error("Keys missing a")
	}
}

func TestSecretBuilder_Immutable_Emitted(t *testing.T) {
	b := NewSecret("sealed", "ns", "")
	tr := true
	b.Immutable = &tr
	b.Add("k", []byte("v"))
	out, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Decode-and-check: the field must be TOP-LEVEL (a string-contains would
	// also pass if it leaked under metadata or into data).
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if got, ok := doc["immutable"].(bool); !ok || !got {
		t.Errorf("top-level immutable must be true, got %v:\n%s", doc["immutable"], out)
	}
	meta, _ := doc["metadata"].(map[string]any)
	if _, leaked := meta["immutable"]; leaked {
		t.Errorf("immutable must not appear under metadata:\n%s", out)
	}
}

func TestSecretBuilder_Immutable_FalseEmittedAsIs(t *testing.T) {
	// An explicit false survives omitempty (non-nil pointer) — same k8s
	// semantics as omitting it, but the user's declaration is preserved.
	b := NewSecret("unsealed", "ns", "")
	fa := false
	b.Immutable = &fa
	b.Add("k", []byte("v"))
	out, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if got, ok := doc["immutable"].(bool); !ok || got {
		t.Errorf("top-level immutable must be false, got %v:\n%s", doc["immutable"], out)
	}
}

func TestSecretBuilder_Immutable_OmittedWhenNil(t *testing.T) {
	b := NewSecret("plain", "ns", "")
	b.Add("k", []byte("v"))
	out, err := b.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Decode-and-check: substring matching could false-positive on
	// base64-encoded data that happens to contain "immutable".
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, present := doc["immutable"]; present {
		t.Errorf("nil Immutable must be omitted entirely:\n%s", out)
	}
}
