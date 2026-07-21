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

// --- typed sub-sections (literals / passwd / bytes / env / secretRef / key) ---

func TestTemplate_TypedSubsectionsParse(t *testing.T) {
	in := []byte(tmplHeader + `template:
  registration.yml:
    pattern: "id: {tag}\nas: \"{as_token}\"\nregion: {region}\nseed: {seed}\npw: {pw}\n"
    literals:  { tag: matrix-hookshot }
    secretRef: { as_token: as_token }
    env:       { region: "$AWS_REGION" }
    bytes:     { seed: { bytes: 32, encoding: base64-unpadded } }
    passwd:    { pw: { length: 16, chars: hex } }
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Template["registration.yml"]
	if e.Literals["tag"] != "matrix-hookshot" {
		t.Errorf("literals: %+v", e.Literals)
	}
	if e.SecretRef["as_token"].Key != "as_token" || e.SecretRef["as_token"].Secret != "" {
		t.Errorf("sibling secretRef: %+v", e.SecretRef["as_token"])
	}
	if e.Env["region"].Var != "$AWS_REGION" {
		t.Errorf("env: %+v", e.Env["region"])
	}
	if e.Bytes["seed"].Bytes != 32 || e.Bytes["seed"].EffectiveEncoding() != "base64-unpadded" {
		t.Errorf("bytes: %+v", e.Bytes["seed"])
	}
	if e.Passwd["pw"].Length != 16 || e.Passwd["pw"].EffectiveChars() != "hex" {
		t.Errorf("passwd: %+v", e.Passwd["pw"])
	}
}

func TestTemplate_SecretRefSiblingAndCrossForms(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{sib} {cross} {crossns}"
    secretRef:
      sib: sibling_key
      cross: other-secret/their_key
      crossns: other-secret/other-ns/their_key
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	sr := s.Template["K"].SecretRef
	if sr["sib"].Secret != "" || sr["sib"].Key != "sibling_key" {
		t.Errorf("sibling: %+v", sr["sib"])
	}
	if sr["cross"].Secret != "other-secret" || sr["cross"].Namespace != "" || sr["cross"].Key != "their_key" {
		t.Errorf("cross: %+v", sr["cross"])
	}
	if sr["crossns"].Secret != "other-secret" || sr["crossns"].Namespace != "other-ns" || sr["crossns"].Key != "their_key" {
		t.Errorf("crossns: %+v", sr["crossns"])
	}
}

func TestTemplate_SecretRefMappingForm(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{r}"
    secretRef:
      r: { secret: db, namespace: prod, key: password }
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Template["K"].SecretRef["r"]
	if r.Secret != "db" || r.Namespace != "prod" || r.Key != "password" {
		t.Errorf("mapping form: %+v", r)
	}
}

func TestTemplate_SecretRefMappingRequiresSecret(t *testing.T) {
	// The mapping form is a cross-secret ref, so secret is required (only the
	// bare-string sibling shorthand may omit it).
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{r}"
    secretRef:
      r: { key: password }
`)
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("expected secret-required error, got %v", err)
	}
}

func TestTemplate_KeySubsectionParse(t *testing.T) {
	in := []byte(tmplHeader + `template:
  config.yml:
    pattern: "key: |\n{pem}\n"
    key: { pem: ed25519 }
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	if s.Template["config.yml"].Key["pem"].EffectiveAlgorithm() != "ed25519" {
		t.Errorf("key subsection: %+v", s.Template["config.yml"].Key["pem"])
	}
}

func TestTemplate_BytesShorthandInSubsection(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{seed}"
    bytes: { seed: 16 }
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	b := s.Template["K"].Bytes["seed"]
	if b.Bytes != 16 || b.EffectiveEncoding() != "base64" {
		t.Errorf("bytes shorthand: %+v", b)
	}
}

func TestTemplate_BytesSubsectionRejectsZero(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{s}"
    bytes: { s: 0 }
`)
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("expected bytes>0 error, got %v", err)
	}
}

func TestTemplate_BytesSubsectionNullRejected(t *testing.T) {
	// A !!null map value skips BytesEntry.UnmarshalYAML (yaml.v3 quirk), leaving
	// Bytes: 0 — the entry-level re-validation in TemplateEntry.validate must
	// still catch it rather than silently emitting an empty field.
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{s}"
    bytes: { s: ~ }
`)
	if err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Errorf("expected bytes>0 error for null bytes entry, got %v", err)
	}
}

func TestTemplate_BytesSubsectionRejectsBadEncoding(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{s}"
    bytes: { s: { bytes: 16, encoding: base32 } }
`)
	if err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Errorf("expected encoding error, got %v", err)
	}
}

// fields: and typed sub-sections may coexist, each producing distinct placeholders.
func TestTemplate_FieldsAndTypedCoexist(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{legacy}-{tag}"
    fields:   { legacy: {length: 4, chars: hex} }
    literals: { tag: v1 }
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatal(err)
	}
	e := s.Template["K"]
	if e.Fields["legacy"].Length != 4 || e.Literals["tag"] != "v1" {
		t.Errorf("coexist: fields=%+v literals=%+v", e.Fields, e.Literals)
	}
}

// A placeholder declared by TWO sub-sections is ambiguous → rejected.
func TestTemplate_DuplicateAcrossSubsectionsRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{dup}"
    literals: { dup: a }
    passwd:   { dup: 8 }
`)
	if err == nil || !strings.Contains(err.Error(), "more than one sub-section") {
		t.Errorf("expected duplicate-declaration error, got %v", err)
	}
}

// A placeholder declared in fields: AND a typed sub-section is also ambiguous.
func TestTemplate_DuplicateFieldsAndTypedRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{dup}"
    fields:   { dup: {length: 4} }
    literals: { dup: a }
`)
	if err == nil || !strings.Contains(err.Error(), "more than one sub-section") {
		t.Errorf("expected duplicate-declaration error, got %v", err)
	}
}

// Placeholders in the pattern that no sub-section produces are rejected.
func TestTemplate_UndeclaredAcrossAllSubsections(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{a}-{missing}"
    literals: { a: x }
`)
	if err == nil || !strings.Contains(err.Error(), "{missing}") {
		t.Errorf("expected undeclared error mentioning {missing}, got %v", err)
	}
}

// A typed sub-section field not referenced by the pattern is dead config.
func TestTemplate_UnusedTypedFieldRejected(t *testing.T) {
	err := decodeTemplateErr(t, `template:
  K:
    pattern: "{a}"
    literals: { a: x, dead: y }
`)
	if err == nil || !strings.Contains(err.Error(), "dead") {
		t.Errorf("expected unused-field error mentioning dead, got %v", err)
	}
}

// Operators in the pattern resolve against the DECLARED field name (before the
// operator), so a typed field is matched even when the placeholder carries one.
func TestTemplate_OperatorPlaceholderMatchesDeclaredField(t *testing.T) {
	in := []byte(tmplHeader + `template:
  K:
    pattern: "{tag^^}"
    literals: { tag: hookshot }
`)
	s, err := DecodeBytes(in)
	if err != nil {
		t.Fatalf("operator placeholder should match declared field: %v", err)
	}
	got, err := s.Template["K"].Substitute(map[string]string{"tag": "hookshot"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "HOOKSHOT" {
		t.Errorf("got %q, want HOOKSHOT", got)
	}
}
