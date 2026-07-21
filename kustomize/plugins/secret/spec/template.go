package spec

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kernpilot/lok8s/kustomize/pkg/charset"
	"github.com/kernpilot/lok8s/kustomize/pkg/kyaml"
)

// TemplateEntry is the spec for a composite secret assembled from a fixed
// pattern with `{name}` placeholders. It is a "mini-Secret + pattern": each
// placeholder is produced by a typed sub-section — reusing the top-level
// generator types (literals, passwd, env, secretRef, key) plus a template-local
// bytes: sub-section — so a multi-part secret with a literal structure can be
// declared instead of glued together in a fragile `bash:` pipeline.
//
// Example (matrix-hookshot registration + passkey):
//
//	template:
//	  registration.yml:
//	    pattern: "id: matrix-hookshot\nas_token: \"{as_token}\"\nhs_token: \"{hs_token}\"\n"
//	    secretRef: { as_token: as_token, hs_token: hs_token }   # sibling keys in THIS secret
//	  passkey.pem:
//	    key: rsa
//
// Every placeholder in the pattern must be produced by exactly one sub-section
// (or fields:), and every declared field must be referenced.
//
// Literal braces are written doubled: `{{` → `{`, `}}` → `}`. Placeholder names
// may carry a bash-style substitution operator (e.g. `{name^^}`, `{name%%.suf}`)
// applied to the resolved value — see Substitute.
type TemplateEntry struct {
	// Pattern is the output template. `{name}` is replaced by the field named
	// "name" (optionally transformed by an operator); `{{`/`}}` are literal
	// braces. Must be non-empty; every placeholder must resolve to a declared
	// field (across all sub-sections + Fields) and vice versa.
	Pattern string `yaml:"pattern"`

	// Fields is the LEGACY shorthand sub-section: charset-mode (length/chars/
	// require, like passwd:) XOR bytes-mode (bytes/encoding). Kept for backward
	// compatibility; prefer the typed sub-sections below. Fields and the typed
	// sub-sections may coexist (a placeholder is produced by exactly one).
	Fields map[string]TemplateField `yaml:"fields,omitempty"`

	// --- typed sub-sections (each: map[placeholder]→<top-level entry type>) ---

	// Literals maps a placeholder to a verbatim string.
	Literals map[string]string `yaml:"literals,omitempty"`
	// Passwd maps a placeholder to a random-password spec (charset draw).
	Passwd map[string]PasswdEntry `yaml:"passwd,omitempty"`
	// Bytes maps a placeholder to a raw-crypto/rand-bytes-then-encode spec.
	Bytes map[string]BytesEntry `yaml:"bytes,omitempty"`
	// Env maps a placeholder to an environment-variable spec.
	Env map[string]EnvEntry `yaml:"env,omitempty"`
	// SecretRef maps a placeholder to another cached key's value. A bare string
	// with no "/" is a SIBLING key in THIS secret (current secret + namespace);
	// "secret/key" and "secret/ns/key" reference another secret.
	SecretRef map[string]TemplateSecretRef `yaml:"secretRef,omitempty"`
	// Key maps a placeholder to a generated private key (PKCS#8 PEM).
	Key map[string]KeyEntry `yaml:"key,omitempty"`
}

// TemplateField is a single LEGACY random field referenced by a Pattern
// placeholder. A field is EITHER charset-mode (draw Length characters from a
// charset, the same DSL as passwd:) OR bytes-mode (read Bytes crypto/rand
// bytes, then encode). Exactly one mode must be configured — both or neither is
// an error.
type TemplateField struct {
	// --- charset mode ---
	// Length is the number of characters to draw. Required (> 0) in charset
	// mode; unlike passwd: there is no default length for a template field —
	// a template's shape is explicit, so a missing length is a config error.
	Length int `yaml:"length,omitempty"`
	// Chars is the passwd charset DSL (alphanum, alphanum+symbols, hex,
	// base64url, custom:<chars>). Defaults to "alphanum" when empty in
	// charset mode.
	Chars string `yaml:"chars,omitempty"`
	// Require lists character classes the draw MUST contain at least one of
	// (upper, lower, digit, symbol) — same semantics as passwd:.require.
	Require []string `yaml:"require,omitempty"`

	// --- bytes mode ---
	// Bytes is the number of raw crypto/rand bytes to read (> 0). Presence of
	// a positive Bytes selects bytes-mode.
	Bytes int `yaml:"bytes,omitempty"`
	// Encoding names how the raw bytes become text: base64, base64url,
	// base64-unpadded, base64url-unpadded, or hex. Defaults to base64 in
	// bytes-mode.
	Encoding string `yaml:"encoding,omitempty"`
}

// TemplateSecretRef is a template's secretRef sub-section value. It stores the
// same (secret, namespace, key) triple as SecretRefEntry (so the same resolver
// applies), but its unmarshaler adds a SIBLING shorthand: a bare string with no
// "/" is a key in THIS secret, left with Secret empty as a sentinel the
// generator fills in from the owning Secret's name.
type TemplateSecretRef SecretRefEntry

// UnmarshalYAML accepts the sibling shorthand (a bare "key" → this secret), the
// cross-secret shorthands ("secret/key", "secret/ns/key"), or a full mapping.
func (r *TemplateSecretRef) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		s := node.Value
		if s == "" {
			return kyaml.NodeErrf(node, "secretRef shorthand must not be empty")
		}
		parts := strings.Split(s, "/")
		switch len(parts) {
		case 1:
			// Sibling: a key in THIS secret. Secret stays empty (resolved by the
			// generator from the owning Secret's name).
			r.Key = parts[0]
		case 2:
			r.Secret = parts[0]
			r.Key = parts[1]
		case 3:
			r.Secret = parts[0]
			r.Namespace = parts[1]
			r.Key = parts[2]
		default:
			return kyaml.NodeErrf(node, "secretRef shorthand must be \"key\", \"secret/key\", or \"secret/namespace/key\", got %q", s)
		}
	case yaml.MappingNode:
		var raw SecretRefEntry
		// SecretRefEntry.UnmarshalYAML requires Secret; a mapping form here is a
		// cross-secret reference, so that requirement is exactly right. Decode via
		// the alias to reuse its strict-field checking + validation.
		if err := (&raw).UnmarshalYAML(node); err != nil {
			return err
		}
		*r = TemplateSecretRef(raw)
		return nil
	default:
		return kyaml.NodeErrf(node, "secretRef entry must be string or mapping, got %s", nodeKindString(node.Kind))
	}
	if r.Key == "" {
		return kyaml.NodeErrf(node, "secretRef.key is required")
	}
	return nil
}

// Recognized bytes-mode encodings.
const (
	EncBase64           = "base64"
	EncBase64URL        = "base64url"
	EncBase64Unpadded   = "base64-unpadded"
	EncBase64URLUnpadds = "base64url-unpadded"
	EncHex              = "hex"
)

// MaxFieldBytes caps a bytes field's raw byte count. Key material is at most a
// few dozen bytes; anything past this is a fat-fingered config (e.g. bytes:
// 32000000 → a ~32 MB Secret), so it's rejected at decode time.
const MaxFieldBytes = 4096

// templateEntryRaw avoids infinite recursion in UnmarshalYAML.
type templateEntryRaw TemplateEntry

// UnmarshalYAML decodes a full mapping (there is no shorthand — a template is
// inherently multi-field) with strict field checking, then validates the
// pattern/field contract.
func (t *TemplateEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return kyaml.NodeErrf(node, "template entry must be a mapping, got %s", nodeKindString(node.Kind))
	}
	var raw templateEntryRaw
	if err := kyaml.DecodeNodeStrict(node, &raw); err != nil {
		return err
	}
	*t = TemplateEntry(raw)
	if err := t.validate(); err != nil {
		return kyaml.NodeErrf(node, "%v", err)
	}
	return nil
}

// declaredFields returns the set of every placeholder name declared across all
// sub-sections (fields + typed), and an error if the same placeholder is
// declared by more than one sub-section (which would make its producer
// ambiguous).
func (t *TemplateEntry) declaredFields() (map[string]bool, error) {
	// One (section, names) pair per sub-section, in a fixed order for stable
	// error messages. Each names list is pre-sorted for determinism.
	sections := []struct {
		name  string
		names []string
	}{
		{"fields", sortedStringKeys(t.Fields)},
		{"literals", sortedStringKeys(t.Literals)},
		{"passwd", sortedStringKeys(t.Passwd)},
		{"bytes", sortedStringKeys(t.Bytes)},
		{"env", sortedStringKeys(t.Env)},
		{"secretRef", sortedStringKeys(t.SecretRef)},
		{"key", sortedStringKeys(t.Key)},
	}
	declared := make(map[string]bool)
	for _, s := range sections {
		for _, name := range s.names {
			if declared[name] {
				return nil, fmt.Errorf("field %q is declared by more than one sub-section (last: %s)", name, s.name)
			}
			declared[name] = true
		}
	}
	return declared, nil
}

// validate checks the pattern/field contract: non-empty pattern, balanced
// braces, every placeholder declared by exactly one sub-section, every declared
// field referenced, and each legacy field individually valid. (The typed
// sub-sections validate themselves in their own unmarshalers.)
func (t *TemplateEntry) validate() error {
	if t.Pattern == "" {
		return fmt.Errorf("template pattern must not be empty")
	}
	refs, err := parsePlaceholders(t.Pattern)
	if err != nil {
		return err
	}
	declared, err := t.declaredFields()
	if err != nil {
		return err
	}
	// Every placeholder must resolve to a declared field.
	usedSet := make(map[string]bool, len(refs))
	for _, ref := range refs {
		usedSet[ref.name] = true
		if !declared[ref.name] {
			return fmt.Errorf("pattern references {%s} but no such field is declared", ref.name)
		}
	}
	// Every declared field must be referenced (catch dead config / typos).
	declaredNames := make([]string, 0, len(declared))
	for name := range declared {
		declaredNames = append(declaredNames, name)
	}
	sort.Strings(declaredNames)
	for _, name := range declaredNames {
		if !usedSet[name] {
			return fmt.Errorf("field %q is declared but not referenced in the pattern", name)
		}
	}
	// Validate each legacy field's mode (typed sub-sections already validated).
	for _, name := range sortedStringKeys(t.Fields) {
		if err := t.Fields[name].validate(); err != nil {
			return fmt.Errorf("field %q: %v", name, err)
		}
	}
	// Re-validate the typed sub-sections whose per-entry UnmarshalYAML can be
	// SKIPPED by yaml.v3 (a !!null map value leaves the zero struct, bypassing
	// the unmarshaler). bytes: has a "> 0" invariant and secretRef: needs a
	// non-empty key — a zero value would otherwise slip past decode and only
	// surface as a confusing runtime error.
	for _, name := range sortedStringKeys(t.Bytes) {
		if err := t.Bytes[name].validate(); err != nil {
			return fmt.Errorf("bytes field %q: %v", name, err)
		}
	}
	for _, name := range sortedStringKeys(t.SecretRef) {
		if t.SecretRef[name].Key == "" {
			return fmt.Errorf("secretRef field %q: key is required (a null/`~` value?)", name)
		}
	}
	return nil
}

// validate checks a single legacy field: exactly one mode, and that mode's
// fields are well-formed (positive length/bytes, known encoding, parseable
// classes).
func (f TemplateField) validate() error {
	charsetMode := f.Length != 0 || f.Chars != "" || len(f.Require) > 0
	bytesMode := f.Bytes != 0 || f.Encoding != ""
	switch {
	case charsetMode && bytesMode:
		return fmt.Errorf("cannot mix charset mode (length/chars/require) with bytes mode (bytes/encoding)")
	case !charsetMode && !bytesMode:
		return fmt.Errorf("must set either a charset field (length + optional chars/require) or a bytes field (bytes + optional encoding)")
	case bytesMode:
		if f.Bytes <= 0 {
			return fmt.Errorf("bytes must be > 0, got %d", f.Bytes)
		}
		if f.Bytes > MaxFieldBytes {
			return fmt.Errorf("bytes must be <= %d, got %d (a secret this large is almost certainly a typo)", MaxFieldBytes, f.Bytes)
		}
		if _, err := validateEncoding(f.EffectiveEncoding()); err != nil {
			return err
		}
		return nil
	default: // charset mode
		if f.Length <= 0 {
			return fmt.Errorf("length must be > 0 (a template field needs an explicit length)")
		}
		chars, err := charset.Resolve(f.EffectiveChars())
		if err != nil {
			return err
		}
		req, err := f.RequireClasses()
		if err != nil {
			return err
		}
		// Feasibility, checked statically so an impossible config fails at
		// decode instead of only on a cache-miss generation. The generator
		// re-checks these (belt-and-suspenders).
		if len(req) > f.Length {
			return fmt.Errorf("require lists %d classes but length is %d", len(req), f.Length)
		}
		for _, c := range req {
			if !charset.PoolContains(chars, c) {
				return fmt.Errorf("charset %q has no %q characters to satisfy require", f.EffectiveChars(), c)
			}
		}
		return nil
	}
}

// IsBytes reports whether the field is configured in bytes mode.
func (f TemplateField) IsBytes() bool { return f.Bytes > 0 }

// EffectiveChars returns the configured charset or the default ("alphanum").
func (f TemplateField) EffectiveChars() string {
	if f.Chars == "" {
		return charset.DefaultName
	}
	return f.Chars
}

// EffectiveEncoding returns the configured encoding or the default ("base64").
func (f TemplateField) EffectiveEncoding() string {
	if f.Encoding == "" {
		return EncBase64
	}
	return f.Encoding
}

// RequireClasses parses Require into charset.Class values (validated +
// de-duplicated, first-seen order). Returns nil when Require is empty.
func (f TemplateField) RequireClasses() ([]charset.Class, error) {
	if len(f.Require) == 0 {
		return nil, nil
	}
	out := make([]charset.Class, 0, len(f.Require))
	seen := make(map[charset.Class]bool, len(f.Require))
	for _, r := range f.Require {
		c, err := charset.ParseClass(r)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// validateEncoding checks that enc is a recognized bytes-mode encoding.
func validateEncoding(enc string) (string, error) {
	switch enc {
	case EncBase64, EncBase64URL, EncBase64Unpadded, EncBase64URLUnpadds, EncHex:
		return enc, nil
	default:
		return "", fmt.Errorf("unknown encoding %q (valid: base64, base64url, base64-unpadded, base64url-unpadded, hex)", enc)
	}
}

// sortedStringKeys returns the sorted keys of any string-keyed map — used for
// deterministic iteration in validation.
func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
