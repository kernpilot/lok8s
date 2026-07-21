package generator

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"sort"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/charset"
	"github.com/kernpilot/lok8s/kustomize/pkg/errs"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	"github.com/kernpilot/lok8s/kustomize/pkg/random"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// Template is the generator for the `template:` field. It composes a fixed
// pattern with `{name}` placeholders into a single secret value, drawing each
// placeholder from a typed sub-section (literals / passwd / bytes / env /
// secretRef / key) or the legacy fields: shorthand. The COMPOSED value is
// cached by the entry key — like passwd:, the store is the source of truth, so
// output is byte-stable across runs. The pattern/fields are NOT hashed (that is
// the bash: approval-gate model this generator exists to avoid): editing the
// pattern only affects freshly-generated keys.
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
// pattern, returning the composed bytes. Fields across all sub-sections are
// generated in a single sorted-by-name pass purely for deterministic dispatch;
// byte-stability across runs rests on the CACHE, not on RNG determinism.
func composeTemplate(ctx *plugin.Context, entry *specpkg.TemplateEntry) ([]byte, error) {
	values, err := generateTemplateValues(ctx, entry)
	if err != nil {
		return nil, err
	}
	s, err := entry.Substitute(values)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// generateTemplateValues resolves every declared placeholder (across fields: +
// the typed sub-sections) to its string value. Names are processed in sorted
// order for deterministic dispatch.
func generateTemplateValues(ctx *plugin.Context, entry *specpkg.TemplateEntry) (map[string]string, error) {
	values := map[string]string{}

	// Legacy fields:
	for _, name := range sortedKeys(entry.Fields) {
		v, err := generateField(ctx, entry.Fields[name])
		if err != nil {
			return nil, errs.Wrap("field "+name, err)
		}
		values[name] = v
	}
	// literals:
	for _, name := range sortedKeys(entry.Literals) {
		values[name] = entry.Literals[name]
	}
	// passwd: (drawn fresh — the composed template value is the cached unit, so
	// there is no per-field cache key to key on)
	for _, name := range sortedKeys(entry.Passwd) {
		v, err := passwdDraw(entry.Passwd[name])
		if err != nil {
			return nil, errs.Wrap("passwd "+name, err)
		}
		values[name] = string(v)
	}
	// bytes:
	for _, name := range sortedKeys(entry.Bytes) {
		v, err := bytesValue(ctx, entry.Bytes[name])
		if err != nil {
			return nil, errs.Wrap("bytes "+name, err)
		}
		values[name] = v
	}
	// env:
	for _, name := range sortedKeys(entry.Env) {
		v, err := templateEnvValue(ctx, name, entry.Env[name])
		if err != nil {
			return nil, errs.Wrap("env "+name, err)
		}
		values[name] = v
	}
	// secretRef:
	for _, name := range sortedKeys(entry.SecretRef) {
		v, err := templateSecretRefValue(ctx, entry.SecretRef[name])
		if err != nil {
			return nil, errs.Wrap("secretRef "+name, err)
		}
		values[name] = string(v)
	}
	// key:
	for _, name := range sortedKeys(entry.Key) {
		v, err := generateKey(ctx, entry.Key[name])
		if err != nil {
			return nil, errs.Wrap("key "+name, err)
		}
		values[name] = string(v)
	}
	return values, nil
}

// templateEnvValue resolves a template env: sub-section value. Unlike the
// top-level env: generator this does NOT cache per-field (the composed template
// value is the cached unit), so it reads the env var directly. Fallbacks on an
// unset var mirror env:'s (default → literal, passwd → generated), except that
// `optional` yields an EMPTY string rather than omitting the key — a placeholder
// must resolve to something, or it would leave a hole in the pattern.
func templateEnvValue(ctx *plugin.Context, key string, e specpkg.EnvEntry) (string, error) {
	varName := e.EffectiveVar(key)
	if v, ok := ctx.Env(varName); ok {
		return v, nil
	}
	switch {
	case e.Default != nil:
		return *e.Default, nil
	case e.Passwd != nil:
		p, err := passwdDraw(*e.Passwd)
		if err != nil {
			return "", err
		}
		return string(p), nil
	case e.Optional:
		// A placeholder must have a value — an omitted key would leave a hole in
		// the pattern. Treat optional+unset as an empty string (documented).
		return "", nil
	default:
		return "", errs.Newf("env var %q not set", varName)
	}
}

// templateSecretRefValue resolves a template secretRef: sub-section value. A
// sibling reference (Secret empty) is resolved against the OWNING secret's name
// and namespace, taken from the Context — so `{ as_token: as_token }` reads this
// secret's own as_token key. Cross-secret refs use the (secret, ns, key) triple.
func templateSecretRefValue(ctx *plugin.Context, r specpkg.TemplateSecretRef) ([]byte, error) {
	// r.Key can be empty only when a `!!null` map value skipped the entry's
	// UnmarshalYAML (yaml.v3 quirk). Fail with a clear message rather than
	// attempting to read the malformed "Secret.<name>.<ns>." cache filename.
	if r.Key == "" {
		return nil, errs.New("secretRef key is empty (a null/`~` value?) — set a sibling key or secret/key")
	}
	secret := r.Secret
	if secret == "" {
		secret = ctx.Name // sibling: a key in THIS secret
	}
	ns := r.Namespace
	if ns == "" {
		ns = ctx.Namespace
	}
	filename := cache.FormatName(secret, ns, r.Key)
	val, err := ctx.Cache.ReadByName(filename)
	if err != nil {
		return nil, errs.Newf("read %s/%s/%s: %v", secret, ns, r.Key, err)
	}
	return val, nil
}

// bytesValue reads a bytes: sub-section's raw crypto/rand bytes and encodes them.
func bytesValue(ctx *plugin.Context, b specpkg.BytesEntry) (string, error) {
	buf := make([]byte, b.Bytes)
	if _, err := io.ReadFull(randReader(ctx), buf); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return encodeBytes(buf, b.EffectiveEncoding())
}

// generateField produces one LEGACY field's string value: a charset draw or an
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
	if _, err := io.ReadFull(randReader(ctx), buf); err != nil {
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

// sortedKeys returns the sorted keys of a string-keyed map (deterministic
// dispatch order for template field generation).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
