package generator

import (
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// --- Passwd ---

func TestPasswd_Generates(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{
		"PASS": {Length: 32},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	if len(got[0].Value) != 32 {
		t.Errorf("password length = %d", len(got[0].Value))
	}
}

func TestPasswd_DefaultLength(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{"K": {}})
	got, _ := g.Generate(ctx)
	if len(got[0].Value) != 32 {
		t.Errorf("default length = %d, want 32", len(got[0].Value))
	}
}

func TestPasswd_CacheFirstStable(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{"K": {Length: 16}})
	got1, _ := g.Generate(ctx)
	got2, _ := g.Generate(ctx)
	if string(got1[0].Value) != string(got2[0].Value) {
		t.Errorf("not stable: %q vs %q", got1[0].Value, got2[0].Value)
	}
}

func TestPasswd_DifferentKeysDifferentValues(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{
		"A": {Length: 16},
		"B": {Length: 16},
	})
	got, _ := g.Generate(ctx)
	if string(got[0].Value) == string(got[1].Value) {
		t.Errorf("different keys produced same value (collision is astronomically unlikely)")
	}
}

func TestPasswd_BadCharset(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(map[string]specpkg.PasswdEntry{"K": {Chars: "nope"}})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Error("expected error for unknown charset")
	}
}

func TestPasswd_EmptySpec(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewPasswd(nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty passwd should return nil, got %+v", got)
	}
}

// --- SecretRef ---

func TestSecretRef_ReadsAnotherCache(t *testing.T) {
	dir := t.TempDir()
	// Producer: write a cache entry as if another secret had stored it.
	producer, err := cache.New(dir, "default", "db-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := producer.Put("password", []byte("hunter2")); err != nil {
		t.Fatal(err)
	}
	// Consumer: secretRef points at producer's entry.
	consumer, _ := cache.New(dir, "default", "consumer")
	ctx := newCtxFromCache("default", "consumer", consumer)
	g := NewSecretRef(map[string]specpkg.SecretRefEntry{
		"DB_PASSWORD": {Secret: "db-secret", Key: "password"},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "hunter2" {
		t.Errorf("got %q", got[0].Value)
	}
}

func TestSecretRef_CrossNamespace(t *testing.T) {
	dir := t.TempDir()
	producer, _ := cache.New(dir, "other-ns", "db")
	if err := producer.Put("password", []byte("p4ss")); err != nil {
		t.Fatal(err)
	}
	consumer, _ := cache.New(dir, "default", "consumer")
	ctx := newCtxFromCache("default", "consumer", consumer)
	g := NewSecretRef(map[string]specpkg.SecretRefEntry{
		"DB": {Secret: "db", Namespace: "other-ns", Key: "password"},
	})
	got, _ := g.Generate(ctx)
	if string(got[0].Value) != "p4ss" {
		t.Errorf("got %q", got[0].Value)
	}
}

func TestSecretRef_DefaultsToConsumerNamespace(t *testing.T) {
	dir := t.TempDir()
	producer, _ := cache.New(dir, "ns", "db")
	if err := producer.Put("k", []byte("v")); err != nil {
		t.Fatal(err)
	}
	consumer, _ := cache.New(dir, "ns", "consumer")
	ctx := newCtxFromCache("ns", "consumer", consumer)
	g := NewSecretRef(map[string]specpkg.SecretRefEntry{
		"DB": {Secret: "db", Key: "k"}, // Namespace empty → uses ctx.Namespace
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "v" {
		t.Errorf("got %q", got[0].Value)
	}
}

func TestSecretRef_MissingProducerErrors(t *testing.T) {
	dir := t.TempDir()
	consumer, _ := cache.New(dir, "default", "consumer")
	ctx := newCtxFromCache("default", "consumer", consumer)
	g := NewSecretRef(map[string]specpkg.SecretRefEntry{
		"DB": {Secret: "nope", Key: "missing"},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should mention producer secret: %v", err)
	}
}

func TestSecretRef_EmptySpec(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewSecretRef(nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty secretRef should return nil, got %+v", got)
	}
}
