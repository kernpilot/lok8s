package generator

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/charset"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/pkg/random"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Template is the generator for the `template:` field. It composes a fixed
// pattern with `{name}` placeholders into a single secret value, drawing each
// placeholder from a named random field (charset or raw bytes). The COMPOSED
// value is cached by the entry key — like passwd:, the store is the source of
// truth, so output is byte-stable across runs. The pattern/fields are NOT
// hashed (that is the bash: approval-gate model this generator exists to
// avoid): editing the pattern only affects freshly-generated keys.
type Template struct {
	spec map[string]specpkg.TemplateEntry
}

// NewTemplate wraps a template map.
func NewTemplate(spec map[string]specpkg.TemplateEntry) *Template { return &Template{spec: spec} }

// Name returns the generator's spec field name.
func (g *Template) Name() string { return "template" }

// Generate produces one Entry per template spec, using GetOrCreate so an
// existing cached (composed) value is reused verbatim.
func (g *Template) Generate(ctx *plugin.Context) ([]plugin.Entry, error) {
	if len(g.spec) == 0 {
		return nil, nil
	}
	if ctx.Cache == nil {
		return nil, errs.New("template generator requires PATH_SECRETS to be set")
	}
	keys := make([]string, 0, len(g.spec))
	for k := range g.spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]plugin.Entry, 0, len(keys))
	for _, k := range keys {
		entry := g.spec[k]
		val, err := ctx.Cache.GetOrCreate(k, func() ([]byte, error) {
			return composeTemplate(ctx, &entry)
		})
		if err != nil {
			return nil, errs.Wrap(k, err)
		}
		out = append(out, plugin.Entry{Key: k, Value: val})
	}
	return out, nil
}

// composeTemplate generates every field's value and substitutes them into the
// pattern, returning the composed bytes. Fields are generated in sorted name
// order purely for deterministic dispatch — charset fields draw from the
// package-global crypto/rand reader (random.Reader), and only bytes fields use
// ctx.Rand, so byte-stability across runs rests on the CACHE, not on RNG
// determinism.
func composeTemplate(ctx *plugin.Context, entry *specpkg.TemplateEntry) ([]byte, error) {
	names := make([]string, 0, len(entry.Fields))
	for name := range entry.Fields {
		names = append(names, name)
	}
	sort.Strings(names)

	values := make(map[string]string, len(entry.Fields))
	for _, name := range names {
		v, err := generateField(ctx, entry.Fields[name])
		if err != nil {
			return nil, errs.Wrap("field "+name, err)
		}
		values[name] = v
	}

	s, err := entry.Substitute(values)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// generateField produces one field's string value: a charset draw or an
// encoded run of crypto/rand bytes.
func generateField(ctx *plugin.Context, f specpkg.TemplateField) (string, error) {
	if f.IsBytes() {
		return generateBytesField(ctx, f)
	}
	return generateCharsetField(f)
}

// generateCharsetField draws Length characters from the resolved charset,
// honoring any require: classes (validated for feasibility up front, mirroring
// the passwd generator).
func generateCharsetField(f specpkg.TemplateField) (string, error) {
	length := f.Length
	chars, err := charset.Resolve(f.EffectiveChars())
	if err != nil {
		return "", err
	}
	required, err := f.RequireClasses()
	if err != nil {
		return "", err
	}
	if len(required) > length {
		return "", fmt.Errorf("require lists %d classes but length is %d", len(required), length)
	}
	for _, c := range required {
		if !charset.PoolContains(chars, c) {
			return "", fmt.Errorf("charset %q has no %q characters to satisfy require", f.EffectiveChars(), c)
		}
	}
	if len(required) == 0 {
		p, err := random.Password(length, chars)
		if err != nil {
			return "", err
		}
		return string(p), nil
	}
	p, err := random.PasswordSatisfying(length, chars, func(p []byte) bool {
		return charset.SatisfiesAll(p, required)
	})
	if err != nil {
		return "", err
	}
	return string(p), nil
}

// generateBytesField reads f.Bytes crypto/rand bytes (from ctx.Rand when set,
// so tests can inject a deterministic reader) and encodes them per the
// configured encoding.
func generateBytesField(ctx *plugin.Context, f specpkg.TemplateField) (string, error) {
	buf := make([]byte, f.Bytes)
	r := ctx.Rand
	if r == nil {
		r = rand.Reader
	}
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return encodeBytes(buf, f.EffectiveEncoding())
}

// encodeBytes applies a bytes-mode encoding to raw bytes.
func encodeBytes(b []byte, enc string) (string, error) {
	switch enc {
	case specpkg.EncBase64:
		return base64.StdEncoding.EncodeToString(b), nil
	case specpkg.EncBase64URL:
		return base64.URLEncoding.EncodeToString(b), nil
	case specpkg.EncBase64Unpadded:
		return base64.RawStdEncoding.EncodeToString(b), nil
	case specpkg.EncBase64URLUnpadds:
		return base64.RawURLEncoding.EncodeToString(b), nil
	case specpkg.EncHex:
		return hex.EncodeToString(b), nil
	default:
		return "", fmt.Errorf("unknown encoding %q", enc)
	}
}
