package spec

import (
	"strings"
	"testing"
)

// header is the minimal valid Secret preamble shared by the template parse
// tests, so each case only carries its own `template:` block.
const tmplHeader = `apiVersion: secrets.lok8s.dev/v1
kind: Secret
metadata: {name: t}
`

func TestTemplate_ParseFull(t *testing.T) {
	in := []byte(tmplHeader + `template:
  SIGNING_KEY:
    pattern: "ed25519 a_{kid} {seed}"
    fields:
      kid:  {length: 4, chars: "custom:abcdefghijklmnopqrstuvwxyz"}
      seed: {bytes: 32, encoding: base64-unpadded}
  API_TOKEN:
    pattern: "sk-{body}"
    fields:
      body: {length: 40, chars: base64url, require: [lower, digit]}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	sk, ok := s.Template["SIGNING_KEY"]
	if !ok {
		t.Fatal("SIGNING_KEY not decoded")
	}
	if sk.Pattern != "ed25519 a_{kid} {seed}" {
		t.Errorf("pattern = %q", sk.Pattern)
	}
	kid := sk.Fields["kid"]
	if kid.Length != 4 || kid.EffectiveChars() != "custom:abcdefghijklmnopqrstuvwxyz" {
		t.Errorf("kid field = %+v", kid)
	}
	if kid.IsBytes() {
		t.Error("kid should be a charset field, not bytes")
	}
	seed := sk.Fields["seed"]
	if !seed.IsBytes() || seed.Bytes != 32 || seed.EffectiveEncoding() != "base64-unpadded" {
		t.Errorf("seed field = %+v", seed)
	}
	body := s.Template["API_TOKEN"].Fields["body"]
	if body.Length != 40 || len(body.Require) != 2 {
		t.Errorf("body field = %+v", body)
	}
}

func TestTemplate_DefaultEncodingIsBase64(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{seed}"
    fields:
      seed: {bytes: 16}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Template["K"].Fields["seed"].EffectiveEncoding(); got != "base64" {
		t.Errorf("default encoding = %q, want base64", got)
	}
}

func TestTemplate_DefaultCharsIsAlphanum(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 8}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Template["K"].Fields["x"].EffectiveChars(); got != "alphanum" {
		t.Errorf("default chars = %q, want alphanum", got)
	}
}

func TestTemplate_EscapedBraces(t *testing.T) {
	// `{{` and `}}` are literal braces and must NOT be treated as placeholders,
	// so a template of only escaped braces needs no fields.
	in := []byte(tmplHeader + `template:
  K:
    pattern: "prefix{{{x}}}suffix"
    fields:
      x: {length: 3, chars: hex}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatalf("escaped braces should parse: %v", err)
	}
	// Sanity: substitution unescapes to literal braces around the value.
	got, err := s.Template["K"].Substitute(map[string]string{"x": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "prefix{abc}suffix" {
		t.Errorf("substitute = %q, want prefix{abc}suffix", got)
	}
}

func TestTemplate_EscapedBracesNoFields(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "just {{literal}} braces"
    fields: {}
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatalf("a pattern with only escaped braces and no fields should parse: %v", err)
	}
	got, err := s.Template["K"].Substitute(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "just {literal} braces" {
		t.Errorf("substitute = %q", got)
	}
}

// --- validation errors ---

func decodeTemplateErr(t *testing.T, body string) error {
	t.Helper()
	_, err := DecodeBytes([]byte(tmplHeader + body))
	return err
}

func TestTemplate_EmptyPatternRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: ""
    fields:
      x: {length: 4}
`)
	if err == nil || !strings.Contains(err.Error(), "pattern") {
		t.Errorf("expected empty-pattern error, got %v", err)
	}
}

func TestTemplate_UndeclaredPlaceholderRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{a}-{b}"
    fields:
      a: {length: 4}
`)
	if err == nil || !strings.Contains(err.Error(), "{b}") {
		t.Errorf("expected undeclared-placeholder error mentioning {b}, got %v", err)
	}
}

func TestTemplate_UnusedFieldRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{a}"
    fields:
      a: {length: 4}
      dead: {length: 8}
`)
	if err == nil || !strings.Contains(err.Error(), "dead") {
		t.Errorf("expected unused-field error mentioning dead, got %v", err)
	}
}

func TestTemplate_BothModesRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 4, bytes: 8}
`)
	if err == nil || !strings.Contains(err.Error(), "mix") {
		t.Errorf("expected both-mode error, got %v", err)
	}
}

func TestTemplate_NeitherModeRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {}
`)
	if err == nil || !strings.Contains(err.Error(), "either") {
		t.Errorf("expected neither-mode error, got %v", err)
	}
}

func TestTemplate_BadEncodingRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {bytes: 16, encoding: base32}
`)
	if err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Errorf("expected bad-encoding error, got %v", err)
	}
}

func TestTemplate_CharsetLengthRequiredRejected(t *testing.T) {
	// A charset field with chars but no length is a config error (no default
	// length for a template field).
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {chars: hex}
`)
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("expected length-required error, got %v", err)
	}
}

func TestTemplate_ZeroBytesRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {bytes: 0, encoding: hex}
`)
	// bytes:0 with encoding still selects bytes-mode (encoding set) → must be > 0.
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("expected bytes>0 error, got %v", err)
	}
}

func TestTemplate_BadCharsetRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 8, chars: nope}
`)
	if err == nil || !strings.Contains(err.Error(), "charset") {
		t.Errorf("expected bad-charset error, got %v", err)
	}
}

func TestTemplate_BadRequireClassRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 8, chars: alphanum, require: [bogus]}
`)
	if err == nil || !strings.Contains(err.Error(), "class") {
		t.Errorf("expected bad-class error, got %v", err)
	}
}

// --- parse-time feasibility (parity with the generator's runtime guards) ---

func TestTemplate_RequireMoreClassesThanLengthRejectedAtParse(t *testing.T) {
	// 3 required classes cannot fit in length 1 — must fail at DECODE, not only
	// on a cache-miss generation.
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 1, chars: alphanum, require: [upper, lower, digit]}
`)
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Errorf("expected feasibility (length) error at parse, got %v", err)
	}
}

func TestTemplate_RequireInfeasibleCharsetRejectedAtParse(t *testing.T) {
	// custom:abc has no digits → require digit is impossible; caught at decode.
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 8, chars: "custom:abc", require: [digit]}
`)
	if err == nil || !strings.Contains(err.Error(), "digit") {
		t.Errorf("expected feasibility (charset) error at parse, got %v", err)
	}
}

func TestTemplate_BytesUpperBoundRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {bytes: 32000000, encoding: base64}
`)
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("expected bytes-upper-bound error, got %v", err)
	}
}

func TestTemplate_BytesAtUpperBoundAllowed(t *testing.T) {
	// The cap itself is valid (boundary).
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{x}"
    fields:
      x: {bytes: 4096, encoding: hex}
`)
	if _, err := DecodeBytes(in); err != nil {
		t.Errorf("bytes at the cap (4096) should be allowed, got %v", err)
	}
}

func TestTemplate_StrayBraceRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "a } b"
    fields: {}
`)
	if err == nil || !strings.Contains(err.Error(), "}") {
		t.Errorf("expected stray-brace error, got %v", err)
	}
}

func TestTemplate_UnterminatedPlaceholderRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "a {x b"
    fields:
      x: {length: 4}
`)
	if err == nil {
		t.Errorf("expected unterminated-placeholder error, got nil")
	}
}

func TestTemplate_RejectsScalarEntry(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K: just-a-string
`)
	if err == nil || !strings.Contains(err.Error(), "mapping") {
		t.Errorf("expected mapping error, got %v", err)
	}
}

func TestTemplate_RejectsUnknownField(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 4}
    nope: bad
`)
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected unknown-field error, got %v", err)
	}
}

func TestTemplate_RejectsUnknownFieldKey(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{x}"
    fields:
      x: {length: 4, weird: 1}
`)
	if err == nil || !strings.Contains(err.Error(), "weird") {
		t.Errorf("expected unknown field-key error, got %v", err)
	}
}
