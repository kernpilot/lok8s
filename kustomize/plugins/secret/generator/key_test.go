package generator

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// keyCtx builds a Context with crypto/rand for the key generator.
func keyCtx(t *testing.T, dir string) *plugin.Context {
	t.Helper()
	c, err := cache.New(dir, "default", "keys")
	if err != nil {
		t.Fatal(err)
	}
	return &plugin.Context{
		Name:      "keys",
		Namespace: "default",
		Cache:     c,
		Env:       func(string) (string, bool) { return "", false },
		Rand:      rand.Reader,
	}
}

// parsePKCS8PEM decodes a "PRIVATE KEY" PEM block into a crypto private key.
func parsePKCS8PEM(t *testing.T, pemBytes []byte) any {
	t.Helper()
	blk, _ := pem.Decode(pemBytes)
	if blk == nil {
		t.Fatalf("not a PEM block: %q", pemBytes)
	}
	if blk.Type != "PRIVATE KEY" {
		t.Fatalf("PEM type = %q, want PRIVATE KEY (PKCS#8)", blk.Type)
	}
	key, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		t.Fatalf("not valid PKCS#8: %v", err)
	}
	return key
}

func TestKey_RSA_DefaultBits(t *testing.T) {
	dir := t.TempDir()
	out, err := NewKey(map[string]specpkg.KeyEntry{"k.pem": {}}).Generate(keyCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Key != "k.pem" {
		t.Fatalf("unexpected entries: %+v", out)
	}
	key := parsePKCS8PEM(t, out[0].Value)
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("default key type = %T, want *rsa.PrivateKey", key)
	}
	if rsaKey.N.BitLen() != 4096 {
		t.Errorf("default RSA size = %d bits, want 4096", rsaKey.N.BitLen())
	}
}

func TestKey_RSA_ExplicitBits(t *testing.T) {
	dir := t.TempDir()
	out, err := NewKey(map[string]specpkg.KeyEntry{"k": {Algorithm: "rsa", Bits: 2048}}).Generate(keyCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	rsaKey := parsePKCS8PEM(t, out[0].Value).(*rsa.PrivateKey)
	if rsaKey.N.BitLen() != 2048 {
		t.Errorf("RSA size = %d, want 2048", rsaKey.N.BitLen())
	}
}

func TestKey_Ed25519(t *testing.T) {
	dir := t.TempDir()
	out, err := NewKey(map[string]specpkg.KeyEntry{"k": {Algorithm: "ed25519"}}).Generate(keyCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	key := parsePKCS8PEM(t, out[0].Value)
	if _, ok := key.(ed25519.PrivateKey); !ok {
		// x509.ParsePKCS8PrivateKey returns ed25519.PrivateKey (not a pointer).
		if _, okPtr := key.(*ecdsa.PrivateKey); okPtr {
			t.Fatalf("got ecdsa, want ed25519")
		}
		t.Fatalf("key type = %T, want ed25519.PrivateKey", key)
	}
}

func TestKey_CacheReusedVerbatim(t *testing.T) {
	dir := t.TempDir()
	ctx := keyCtx(t, dir)
	g := NewKey(map[string]specpkg.KeyEntry{"k": {Algorithm: "rsa", Bits: 2048}})
	got1, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got1[0].Value) != string(got2[0].Value) {
		t.Error("key not stable across runs (cache): a re-key happened")
	}
}

func TestKey_PreseededCacheReusedVerbatim(t *testing.T) {
	// An existing cached key (e.g. an openssl-minted one migrated in) must be
	// returned unchanged — the whole point of the NO-re-key guarantee.
	dir := t.TempDir()
	ctx := keyCtx(t, dir)
	existing := []byte("-----BEGIN PRIVATE KEY-----\nPRESEEDED\n-----END PRIVATE KEY-----\n")
	if err := ctx.Cache.Put("k", existing); err != nil {
		t.Fatal(err)
	}
	out, err := NewKey(map[string]specpkg.KeyEntry{"k": {Algorithm: "rsa"}}).Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0].Value) != string(existing) {
		t.Errorf("cached key not reused verbatim: %q", out[0].Value)
	}
}

func TestKey_DifferentKeysDiffer(t *testing.T) {
	dir := t.TempDir()
	out, err := NewKey(map[string]specpkg.KeyEntry{
		"a": {Algorithm: "ed25519"},
		"b": {Algorithm: "ed25519"},
	}).Generate(keyCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0].Value) == string(out[1].Value) {
		t.Error("two key entries produced identical keys")
	}
}

func TestKey_SortedOutput(t *testing.T) {
	dir := t.TempDir()
	out, err := NewKey(map[string]specpkg.KeyEntry{
		"zeta":  {Algorithm: "ed25519"},
		"alpha": {Algorithm: "ed25519"},
	}).Generate(keyCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Key != "alpha" || out[1].Key != "zeta" {
		t.Errorf("not sorted by key: %+v", entryKeys(out))
	}
}

func TestKey_EmptySpec(t *testing.T) {
	dir := t.TempDir()
	out, err := NewKey(nil).Generate(keyCtx(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Errorf("empty key spec should return nil, got %+v", out)
	}
}

func TestKey_RequiresCache(t *testing.T) {
	ctx := keyCtx(t, t.TempDir())
	ctx.Cache = nil
	if _, err := NewKey(map[string]specpkg.KeyEntry{"k": {}}).Generate(ctx); err == nil {
		t.Fatal("expected error when Cache is nil (PATH_SECRETS unset)")
	}
}
