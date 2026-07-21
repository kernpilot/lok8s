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
// pattern with `{name}` placeholders, each substituted by a named random
// field. It exists so multi-part secrets with a literal structure — e.g. a
// Matrix/synapse ed25519 signing key `ed25519 a_<kid> <base64 seed>` — can be
// declared instead of glued together in a fragile `bash:` pipeline.
//
// Why not bash: a shell one-liner like `printf 'ed25519 a_%s %s' "$(tr ... </dev/urandom | head)" ...`
// (1) SIGPIPEs in non-tty renders (`tr | head` closes the pipe early → exit 141)
// and (2) is SHA-pinned, so any edit forces a `lo secrets allow` re-approval and
// re-keys every plane. template: composes the value in-process from crypto/rand,
// caches it by the entry key like passwd:, and needs no approval gate.
//
// Example:
//
//	template:
//	  SIGNING_KEY:
//	    pattern: "ed25519 a_{kid} {seed}"
//	    fields:
//	      kid:  {length: 4, chars: "custom:abcdefghijklmnopqrstuvwxyz"}
//	      seed: {bytes: 32, encoding: base64-unpadded}
//
// Literal braces are written doubled: `{{` → `{`, `}}` → `}`.
type TemplateEntry struct {
	// Pattern is the output template. `{name}` is replaced by field "name";
	// `{{`/`}}` are literal braces. Must be non-empty and every placeholder
	// must have a matching Fields entry (and vice versa).
	Pattern string `yaml:"pattern"`
	// Fields maps each placeholder name to its random-field spec.
	Fields map[string]TemplateField `yaml:"fields"`
}

// TemplateField is a single random field referenced by a Pattern placeholder.
// A field is EITHER charset-mode (draw Length characters from a charset, the
// same DSL as passwd:) OR bytes-mode (read Bytes crypto/rand bytes, then
// encode). Exactly one mode must be configured — both or neither is an error.
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

// Recognized bytes-mode encodings.
const (
	EncBase64           = "base64"
	EncBase64URL        = "base64url"
	EncBase64Unpadded   = "base64-unpadded"
	EncBase64URLUnpadds = "base64url-unpadded"
	EncHex              = "hex"
)

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

// validate checks the pattern/field contract: non-empty pattern, balanced
// braces, every placeholder declared, every declared field referenced, and
// each field individually valid.
func (t *TemplateEntry) validate() error {
	if t.Pattern == "" {
		return fmt.Errorf("template pattern must not be empty")
	}
	used, err := parsePlaceholders(t.Pattern)
	if err != nil {
		return err
	}
	// Every placeholder must have a matching field.
	for _, name := range used {
		if _, ok := t.Fields[name]; !ok {
			return fmt.Errorf("pattern references {%s} but no such field is declared", name)
		}
	}
	// Every declared field must be referenced (catch dead config / typos).
	usedSet := make(map[string]bool, len(used))
	for _, name := range used {
		usedSet[name] = true
	}
	// Deterministic error order for stable messages/tests.
	names := make([]string, 0, len(t.Fields))
	for name := range t.Fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !usedSet[name] {
			return fmt.Errorf("field %q is declared but not referenced in the pattern", name)
		}
		if err := t.Fields[name].validate(); err != nil {
			return fmt.Errorf("field %q: %v", name, err)
		}
	}
	return nil
}

// validate checks a single field: exactly one mode, and that mode's fields
// are well-formed (positive length/bytes, known encoding, parseable classes).
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
		if _, err := validateEncoding(f.EffectiveEncoding()); err != nil {
			return err
		}
		return nil
	default: // charset mode
		if f.Length <= 0 {
			return fmt.Errorf("length must be > 0 (a template field needs an explicit length)")
		}
		if _, err := charset.Resolve(f.EffectiveChars()); err != nil {
			return err
		}
		for _, r := range f.Require {
			if _, err := charset.ParseClass(r); err != nil {
				return err
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

// parsePlaceholders extracts the ordered list of `{name}` placeholder names
// from a pattern, treating `{{` and `}}` as literal braces. It also validates
// that every brace is part of a placeholder or an escape — an unmatched `{` or
// a stray `}` is a config error (so a mistyped pattern fails loudly instead of
// silently emitting a broken value). Placeholder names must be non-empty.
func parsePlaceholders(pattern string) ([]string, error) {
	var names []string
	for i := 0; i < len(pattern); {
		c := pattern[i]
		switch c {
		case '{':
			if i+1 < len(pattern) && pattern[i+1] == '{' {
				i += 2 // literal "{"
				continue
			}
			end := strings.IndexByte(pattern[i+1:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unterminated placeholder: '{' at offset %d has no closing '}'", i)
			}
			name := pattern[i+1 : i+1+end]
			if name == "" {
				return nil, fmt.Errorf("empty placeholder {} at offset %d", i)
			}
			if strings.ContainsAny(name, "{}") {
				return nil, fmt.Errorf("placeholder name %q at offset %d must not contain braces", name, i)
			}
			names = append(names, name)
			i += 1 + end + 1
		case '}':
			if i+1 < len(pattern) && pattern[i+1] == '}' {
				i += 2 // literal "}"
				continue
			}
			return nil, fmt.Errorf("stray '}' at offset %d (write '}}' for a literal brace)", i)
		default:
			i++
		}
	}
	return names, nil
}

// Substitute renders the pattern, replacing each `{name}` with values[name]
// and unescaping `{{`/`}}`. It assumes the pattern already passed validation
// (every placeholder has a value); a missing value is treated as an internal
// error so the generator surfaces it rather than emitting a partial string.
func (t TemplateEntry) Substitute(values map[string]string) (string, error) {
	var b strings.Builder
	b.Grow(len(t.Pattern))
	for i := 0; i < len(t.Pattern); {
		c := t.Pattern[i]
		switch c {
		case '{':
			if i+1 < len(t.Pattern) && t.Pattern[i+1] == '{' {
				b.WriteByte('{')
				i += 2
				continue
			}
			end := strings.IndexByte(t.Pattern[i+1:], '}')
			if end < 0 {
				return "", fmt.Errorf("internal: unterminated placeholder at offset %d", i)
			}
			name := t.Pattern[i+1 : i+1+end]
			v, ok := values[name]
			if !ok {
				return "", fmt.Errorf("internal: no value for field %q", name)
			}
			b.WriteString(v)
			i += 1 + end + 1
		case '}':
			if i+1 < len(t.Pattern) && t.Pattern[i+1] == '}' {
				b.WriteByte('}')
				i += 2
				continue
			}
			return "", fmt.Errorf("internal: stray '}' at offset %d", i)
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}
