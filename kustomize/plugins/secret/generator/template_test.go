package generator

import (
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// synapseSpec mirrors the Matrix/synapse ed25519 signing-key shape:
//
//	ed25519 a_<4 lowercase> <base64(32 bytes), unpadded>
//
// which is exactly the composite `bash:` was being (mis)used to glue together.
// (encoding: base64-unpadded → standard +/ alphabet, no '=' padding.)
func synapseSpec() map[string]specpkg.TemplateEntry {
	return map[string]specpkg.TemplateEntry{
		"SIGNING_KEY": {
			Pattern: "ed25519 a_{kid} {seed}",
			Fields: map[string]specpkg.TemplateField{
				"kid":  {Length: 4, Chars: "custom:abcdefghijklmnopqrstuvwxyz"},
				"seed": {Bytes: 32, Encoding: "base64-unpadded"},
			},
		},
	}
}

func TestTemplate_SynapseShape(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(synapseSpec())
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "SIGNING_KEY" {
		t.Fatalf("unexpected entries: %+v", got)
	}
	val := string(got[0].Value)
	// 32 bytes → base64-unpadded (RawStdEncoding, standard +/ alphabet) is
	// ceil(32*4/3) = 43 chars. Note base64-unpadded is the standard alphabet,
	// so the seed can contain '+' or '/', not just url-safe chars.
	re := regexp.MustCompile(`^ed25519 a_[a-z]{4} [A-Za-z0-9+/]{43}$`)
	if !re.MatchString(val) {
		t.Fatalf("value %q does not match synapse signing-key shape %s", val, re)
	}
	// The seed segment must decode back to exactly 32 bytes.
	seg := strings.Fields(val)[2]
	raw, err := base64.RawStdEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("seed segment %q is not valid base64-unpadded: %v", seg, err)
	}
	if len(raw) != 32 {
		t.Errorf("seed decoded to %d bytes, want 32", len(raw))
	}
}

func TestTemplate_CacheStableAcrossRuns(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(synapseSpec())
	got1, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got1[0].Value) != string(got2[0].Value) {
		t.Errorf("not byte-stable across runs (cache): %q vs %q", got1[0].Value, got2[0].Value)
	}
}

func TestTemplate_CachedValueReusedVerbatim(t *testing.T) {
	// Pre-seed the cache under the entry key; Generate must return it unchanged
	// (the composed value is what's cached, not per-field state).
	ctx, _ := newCtx(t, nil)
	if err := ctx.Cache.Put("SIGNING_KEY", []byte("ed25519 a_zzzz PRESEEDED")); err != nil {
		t.Fatal(err)
	}
	g := NewTemplate(synapseSpec())
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "ed25519 a_zzzz PRESEEDED" {
		t.Errorf("cached value not reused verbatim: %q", got[0].Value)
	}
}

func TestTemplate_MultipleEntriesSorted(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"BETA":  {Pattern: "{x}", Fields: map[string]specpkg.TemplateField{"x": {Length: 4, Chars: "hex"}}},
		"ALPHA": {Pattern: "{x}", Fields: map[string]specpkg.TemplateField{"x": {Length: 4, Chars: "hex"}}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "ALPHA" || got[1].Key != "BETA" {
		t.Fatalf("entries not sorted by key: %+v", got)
	}
}

func TestTemplate_LiteralBracesUnescaped(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {
			Pattern: "{{{x}}}",
			Fields:  map[string]specpkg.TemplateField{"x": {Length: 3, Chars: "custom:a"}},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// {{ → {, then {x} → "aaa", then }} → } : "{aaa}"
	if string(got[0].Value) != "{aaa}" {
		t.Errorf("literal-brace unescape wrong: %q, want {aaa}", got[0].Value)
	}
}

func TestTemplate_RepeatedPlaceholderSameDraw(t *testing.T) {
	// A field referenced twice is generated ONCE; the single value fills both
	// sites. A 24-char field re-drawn independently would (astronomically
	// likely) differ, so equal halves prove single-generation semantics.
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {
			Pattern: "{a}-{a}",
			Fields:  map[string]specpkg.TemplateField{"a": {Length: 24, Chars: "alphanum"}},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(got[0].Value), "-", 2)
	if len(parts) != 2 {
		t.Fatalf("expected V-V, got %q", got[0].Value)
	}
	if parts[0] != parts[1] {
		t.Errorf("repeated {a} must be the SAME single draw, got %q vs %q", parts[0], parts[1])
	}
	if len(parts[0]) != 24 {
		t.Errorf("each half should be a 24-char draw, got len %d", len(parts[0]))
	}
}

func TestTemplate_ValueContainingBraceNotReSubstituted(t *testing.T) {
	// Security-relevant single-pass property: a generated value that contains
	// '{...}' must be emitted LITERALLY, never re-expanded — otherwise a field
	// value could inject a placeholder for another field. Drive Substitute
	// directly with adversarial values (the generator produces random bytes, so
	// this exercises the substitution engine deterministically).
	entry := specpkg.TemplateEntry{
		Pattern: "{a} {b}",
		Fields: map[string]specpkg.TemplateField{
			"a": {Length: 1, Chars: "custom:x"},
			"b": {Length: 1, Chars: "custom:y"},
		},
	}
	got, err := entry.Substitute(map[string]string{
		"a": "he}llo", // contains a stray brace
		"b": "{c}",    // looks like a placeholder for a non-existent field "c"
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "he}llo {c}" {
		t.Errorf("single-pass violated: value braces were re-expanded; got %q, want %q", got, "he}llo {c}")
	}
}

func TestTemplate_EncodingShapes(t *testing.T) {
	cases := []struct {
		enc     string
		nbytes  int
		wantLen int // -1 = don't length-check (hex checked explicitly)
		decode  func(string) ([]byte, error)
	}{
		{"base64", 32, base64.StdEncoding.EncodedLen(32), base64.StdEncoding.DecodeString},
		{"base64url", 32, base64.URLEncoding.EncodedLen(32), base64.URLEncoding.DecodeString},
		{"base64-unpadded", 32, base64.RawStdEncoding.EncodedLen(32), base64.RawStdEncoding.DecodeString},
		{"base64url-unpadded", 32, base64.RawURLEncoding.EncodedLen(32), base64.RawURLEncoding.DecodeString},
		{"hex", 16, hex.EncodedLen(16), hex.DecodeString},
	}
	for _, c := range cases {
		t.Run(c.enc, func(t *testing.T) {
			ctx, _ := newCtx(t, nil)
			g := NewTemplate(map[string]specpkg.TemplateEntry{
				"K": {
					Pattern: "{seed}",
					Fields:  map[string]specpkg.TemplateField{"seed": {Bytes: c.nbytes, Encoding: c.enc}},
				},
			})
			got, err := g.Generate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			val := string(got[0].Value)
			if c.wantLen >= 0 && len(val) != c.wantLen {
				t.Errorf("%s: len = %d, want %d (value %q)", c.enc, len(val), c.wantLen, val)
			}
			raw, err := c.decode(val)
			if err != nil {
				t.Fatalf("%s: value %q failed to decode: %v", c.enc, val, err)
			}
			if len(raw) != c.nbytes {
				t.Errorf("%s: decoded %d bytes, want %d", c.enc, len(raw), c.nbytes)
			}
		})
	}
}

func TestTemplate_DefaultEncodingBase64(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "{seed}", Fields: map[string]specpkg.TemplateField{"seed": {Bytes: 12}}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base64.StdEncoding.DecodeString(string(got[0].Value)); err != nil {
		t.Errorf("default encoding should be padded std base64, got %q: %v", got[0].Value, err)
	}
}

func TestTemplate_CharsetRequireHonored(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {
			Pattern: "{x}",
			Fields: map[string]specpkg.TemplateField{
				"x": {Length: 6, Chars: "custom:abc123", Require: []string{"lower", "digit"}},
			},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	val := string(got[0].Value)
	if !strings.ContainsAny(val, "abc") || !strings.ContainsAny(val, "123") {
		t.Errorf("require [lower,digit] not honored: %q", val)
	}
}

func TestTemplate_CharsetFieldLength(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "{x}", Fields: map[string]specpkg.TemplateField{"x": {Length: 20, Chars: "hex"}}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[0].Value) != 20 {
		t.Errorf("charset field length = %d, want 20", len(got[0].Value))
	}
	if m, _ := regexp.MatchString(`^[0-9a-f]{20}$`, string(got[0].Value)); !m {
		t.Errorf("hex field wrong shape: %q", got[0].Value)
	}
}

func TestTemplate_EmptySpec(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty template should return nil, got %+v", got)
	}
}

func TestTemplate_RequiresCache(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	ctx.Cache = nil
	g := NewTemplate(synapseSpec())
	if _, err := g.Generate(ctx); err == nil {
		t.Fatal("expected error when Cache is nil (PATH_SECRETS unset)")
	}
}

func TestTemplate_InfeasibleRequireErrors(t *testing.T) {
	// custom:abc has no digits → require digit can never be satisfied; the
	// feasibility check should surface a clear error, not loop.
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {
			Pattern: "{x}",
			Fields:  map[string]specpkg.TemplateField{"x": {Length: 4, Chars: "custom:abc", Require: []string{"digit"}}},
		},
	})
	if _, err := g.Generate(ctx); err == nil {
		t.Fatal("expected feasibility error for require digit on a digit-less charset")
	}
}

// --- typed sub-sections + operators (new form) ---

// TestTemplate_LegacyFieldsByteStableVsBaseline is the back-compat guard: the
// legacy `fields:` charset+bytes path must keep producing the SAME shape it did
// before the typed sub-sections were added. We pin the synapse regex (the exact
// contract the live synapse-signingkey Secret depends on) so a regression in the
// legacy path is caught here rather than in a live render.
func TestTemplate_LegacyFieldsByteStableVsBaseline(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"signing.key": {
			Pattern: "ed25519 a_{kid} {seed}\n",
			Fields: map[string]specpkg.TemplateField{
				"kid":  {Length: 4, Chars: "custom:abcdefghijklmnopqrstuvwxyz"},
				"seed": {Bytes: 32, Encoding: "base64-unpadded"},
			},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Trailing newline via the pattern (= the old bash: newline: ensure).
	re := regexp.MustCompile(`^ed25519 a_[a-z]{4} [A-Za-z0-9+/]{43}\n$`)
	if !re.MatchString(string(got[0].Value)) {
		t.Errorf("legacy fields: output drifted from the synapse contract: %q", got[0].Value)
	}
}

// TestTemplate_TypedSubsectionsEquivalentToLegacy: the synapse shape expressed
// with the typed sub-sections (bytes: + literals: — no legacy fields:) produces
// the same regex-matching shape, proving the new path is a drop-in.
func TestTemplate_TypedSubsectionsSynapseShape(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"signing.key": {
			Pattern:  "{algo} a_{kid} {seed}\n",
			Literals: map[string]string{"algo": "ed25519"},
			Passwd:   map[string]specpkg.PasswdEntry{"kid": {Length: 4, Chars: "custom:abcdefghijklmnopqrstuvwxyz"}},
			Bytes:    map[string]specpkg.BytesEntry{"seed": {Bytes: 32, Encoding: "base64-unpadded"}},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^ed25519 a_[a-z]{4} [A-Za-z0-9+/]{43}\n$`)
	if !re.MatchString(string(got[0].Value)) {
		t.Errorf("typed sub-sections did not match synapse shape: %q", got[0].Value)
	}
}

// TestTemplate_LiteralsSubsection generates a literal-only field.
func TestTemplate_LiteralsSubsection(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "tag={tag}", Literals: map[string]string{"tag": "matrix-hookshot"}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "tag=matrix-hookshot" {
		t.Errorf("literals sub-section: %q", got[0].Value)
	}
}

// TestTemplate_PasswdSubsectionNotSeparatelyCached: the passwd: sub-section is
// drawn fresh into the COMPOSED value (the composed value is the cached unit);
// it must NOT write a stray per-field cache file (e.g. under an empty key).
func TestTemplate_PasswdSubsectionNoStrayCacheFile(t *testing.T) {
	ctx, dir := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "{pw}", Passwd: map[string]specpkg.PasswdEntry{"pw": {Length: 12, Chars: "hex"}}},
	})
	if _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
	// Only the composed entry (Secret.test-secret.default.K) should exist.
	entries, _ := osReadDir(dir)
	for _, name := range entries {
		if name != "Secret.test-secret.default.K" {
			t.Errorf("unexpected cache file %q — passwd sub-section must not cache per-field", name)
		}
	}
	// Composed value must be stable (cache hit on the entry key).
	got2, _ := g.Generate(ctx)
	first, _ := ctx.Cache.Get("K")
	if string(first) != string(got2[0].Value) {
		t.Error("composed value not stable across runs")
	}
}

// TestTemplate_BytesSubsection produces the synapse seed case.
func TestTemplate_BytesSubsection(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "{seed}", Bytes: map[string]specpkg.BytesEntry{"seed": {Bytes: 32, Encoding: "hex"}}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := regexp.MatchString(`^[0-9a-f]{64}$`, string(got[0].Value)); !m {
		t.Errorf("bytes sub-section (hex 32B) wrong shape: %q", got[0].Value)
	}
}

// TestTemplate_EnvSubsection reads an env var into a placeholder.
func TestTemplate_EnvSubsection(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{"AWS_REGION": "eu-central"})
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "region={region}", Env: map[string]specpkg.EnvEntry{"region": {Var: "AWS_REGION"}}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "region=eu-central" {
		t.Errorf("env sub-section: %q", got[0].Value)
	}
}

func TestTemplate_EnvSubsectionMissingErrors(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "{x}", Env: map[string]specpkg.EnvEntry{"x": {Var: "NOPE"}}},
	})
	if _, err := g.Generate(ctx); err == nil {
		t.Fatal("expected error for missing required env var in template sub-section")
	}
}

func TestTemplate_EnvSubsectionDefaultAndOptional(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	d := "fallback"
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {
			Pattern: "d={d} o={o}",
			Env: map[string]specpkg.EnvEntry{
				"d": {Var: "MISS1", Default: &d},
				"o": {Var: "MISS2", Optional: true}, // unset optional → empty
			},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "d=fallback o=" {
		t.Errorf("env default/optional: %q", got[0].Value)
	}
}

// TestTemplate_KeySubsectionEmbedsPEM inlines a generated key into a pattern.
func TestTemplate_KeySubsectionEmbedsPEM(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"config": {Pattern: "-----\n{pem}", Key: map[string]specpkg.KeyEntry{"pem": {Algorithm: "ed25519"}}},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got[0].Value), "BEGIN PRIVATE KEY") {
		t.Errorf("key sub-section did not embed a PEM key: %q", got[0].Value)
	}
}

// TestTemplate_SiblingSecretRefResolvesOwnSecret: a sibling secretRef (Secret
// empty) reads a key in THIS secret, resolved via the owner name/namespace from
// the Context.
func TestTemplate_SiblingSecretRefResolvesOwnSecret(t *testing.T) {
	dir := t.TempDir()
	c, err := cache.New(dir, "element", "hookshot-secrets")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("as_token", []byte("DEADBEEF")); err != nil {
		t.Fatal(err)
	}
	ctx := newCtxFromCache("element", "hookshot-secrets", c)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"registration.yml": {
			Pattern:   "as_token: {as_token}",
			SecretRef: map[string]specpkg.TemplateSecretRef{"as_token": {Key: "as_token"}},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "as_token: DEADBEEF" {
		t.Errorf("sibling secretRef did not resolve own secret: %q", got[0].Value)
	}
}

// TestTemplate_CrossSecretRefFromTemplate: a cross-secret secretRef in a
// template sub-section reads another secret's cached key.
func TestTemplate_CrossSecretRefFromTemplate(t *testing.T) {
	dir := t.TempDir()
	producer, _ := cache.New(dir, "prod", "db")
	if err := producer.Put("password", []byte("s3cr3t")); err != nil {
		t.Fatal(err)
	}
	consumer, _ := cache.New(dir, "default", "app")
	ctx := newCtxFromCache("default", "app", consumer)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"conn": {
			Pattern:   "pw={pw}",
			SecretRef: map[string]specpkg.TemplateSecretRef{"pw": {Secret: "db", Namespace: "prod", Key: "password"}},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "pw=s3cr3t" {
		t.Errorf("cross-secret ref: %q", got[0].Value)
	}
}

func TestTemplate_SiblingSecretRefMissingErrors(t *testing.T) {
	ctx, _ := newCtx(t, nil) // empty cache
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {Pattern: "{x}", SecretRef: map[string]specpkg.TemplateSecretRef{"x": {Key: "nonexistent"}}},
	})
	if _, err := g.Generate(ctx); err == nil {
		t.Fatal("expected error resolving a missing sibling key")
	}
}

// TestTemplate_OperatorAppliedAtGenerate: an operator in the pattern transforms
// the resolved value at generation time.
func TestTemplate_OperatorAppliedAtGenerate(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewTemplate(map[string]specpkg.TemplateEntry{
		"K": {
			Pattern:  "{tag^^} {file%%.pem}",
			Literals: map[string]string{"tag": "hookshot", "file": "passkey.pem"},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "HOOKSHOT passkey" {
		t.Errorf("operators at generate: %q", got[0].Value)
	}
}

func TestTemplate_SecretRefCanReferenceTemplateKey(t *testing.T) {
	// template runs before secretRef in the plugin dispatch order, so a
	// consumer Secret can pull a template-composed value by reference. Simulate
	// that: a producer runs template: to mint+cache SIGNING_KEY, then a consumer
	// secretRef reads it out of the shared $PATH_SECRETS store.
	dir := t.TempDir()

	producerCache, err := cache.New(dir, "matrix", "synapse")
	if err != nil {
		t.Fatal(err)
	}
	producerCtx := newCtxFromCache("matrix", "synapse", producerCache)
	produced, err := NewTemplate(synapseSpec()).Generate(producerCtx)
	if err != nil {
		t.Fatal(err)
	}
	want := string(produced[0].Value)

	consumerCache, _ := cache.New(dir, "matrix", "consumer")
	consumerCtx := newCtxFromCache("matrix", "consumer", consumerCache)
	got, err := NewSecretRef(map[string]specpkg.SecretRefEntry{
		"KEY": {Secret: "synapse", Namespace: "matrix", Key: "SIGNING_KEY"},
	}).Generate(consumerCtx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != want {
		t.Errorf("secretRef did not resolve the template-composed value: got %q, want %q", got[0].Value, want)
	}
}
