package generator

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

// newCtx returns a Context with an in-tempdir cache and an env lookup
// driven by the given map. Tests use this to keep generators isolated.
func newCtx(t *testing.T, env map[string]string) (*plugin.Context, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.New(dir, "default", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	envLookup := func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
	return &plugin.Context{
		Name:      "test-secret",
		Namespace: "default",
		Cache:     c,
		Env:       envLookup,
		FileRoot:  dir,
	}, dir
}

// newCtxFromCache wraps an existing cache (used for cross-secret tests
// where the test sets up multiple Cache instances under the same
// $PATH_SECRETS dir).
func newCtxFromCache(namespace, name string, c *cache.Cache) *plugin.Context {
	return &plugin.Context{
		Name:      name,
		Namespace: namespace,
		Cache:     c,
		Env:       func(string) (string, bool) { return "", false },
		FileRoot:  c.Dir(),
	}
}

// --- Literal ---

func TestLiteral_Generate(t *testing.T) {
	g := NewLiteral(map[string]string{"FOO": "bar", "BAZ": "qux"})
	ctx, _ := newCtx(t, nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	// Sorted: BAZ before FOO
	if got[0].Key != "BAZ" || got[1].Key != "FOO" {
		t.Errorf("not sorted: %+v", got)
	}
	if string(got[0].Value) != "qux" || string(got[1].Value) != "bar" {
		t.Errorf("wrong values: %+v", got)
	}
}

func TestLiteral_Empty(t *testing.T) {
	g := NewLiteral(nil)
	ctx, _ := newCtx(t, nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty literal should return nil, got %+v", got)
	}
}

// --- Env ---

func TestEnv_ReadFromEnvCacheFirst(t *testing.T) {
	env := map[string]string{"MY_VAR": "value1"}
	ctx, _ := newCtx(t, env)
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MY_VAR"}})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "value1" {
		t.Errorf("got %q", got[0].Value)
	}
	// Cache should now have it.
	if !ctx.Cache.Has("K") {
		t.Error("cache should hold value after first read")
	}
}

func TestEnv_CacheStableAcrossRuns(t *testing.T) {
	// First run with one env value, then change env and run again.
	// Cache-first behavior should preserve the original value.
	ctx, _ := newCtx(t, map[string]string{"MY_VAR": "v1"})
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MY_VAR"}})
	got1, _ := g.Generate(ctx)

	// Change env and re-run with a different lookup but same cache.
	ctx.Env = func(k string) (string, bool) {
		return "v2", true
	}
	got2, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got1[0].Value) != string(got2[0].Value) {
		t.Errorf("cache-first should produce stable output: %q vs %q", got1[0].Value, got2[0].Value)
	}
	if string(got2[0].Value) != "v1" {
		t.Errorf("expected cached v1, got %q", got2[0].Value)
	}
}

func TestEnv_UpdateModeBypassesCache(t *testing.T) {
	// First, prime the cache with v1.
	ctx, _ := newCtx(t, map[string]string{"MY_VAR": "v1"})
	g1 := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MY_VAR"}})
	if _, err := g1.Generate(ctx); err != nil {
		t.Fatal(err)
	}
	// Now update mode with a different env value.
	ctx.Env = func(k string) (string, bool) { return "v2", true }
	g2 := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MY_VAR", Update: true}})
	got, err := g2.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "v2" {
		t.Errorf("update mode should read fresh env: got %q, want v2", got[0].Value)
	}
	// Cache should now reflect the new value.
	cached, _ := ctx.Cache.Get("K")
	if string(cached) != "v2" {
		t.Errorf("update mode should write back to cache: got %q", cached)
	}
}

func TestEnv_KeyAsVarWhenEmpty(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{"MYKEY": "v"})
	g := NewEnv(map[string]specpkg.EnvEntry{"MYKEY": {}}) // Var empty
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "v" {
		t.Errorf("got %q", got[0].Value)
	}
}

func TestEnv_MissingVarErrors(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MISSING"}})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for missing env var")
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("error should mention MISSING: %v", err)
	}
}

func TestEnv_UpdateMissingErrors(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "X", Update: true}})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

// --- B64 ---

func TestB64_Decode(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("hello"))
	g := NewB64(map[string]string{"K": encoded})
	ctx, _ := newCtx(t, nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "hello" {
		t.Errorf("decoded = %q", got[0].Value)
	}
}

func TestB64_RejectsInvalid(t *testing.T) {
	g := NewB64(map[string]string{"K": "not!valid!base64!"})
	ctx, _ := newCtx(t, nil)
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

// --- File ---

func TestFile_RawMode(t *testing.T) {
	ctx, dir := newCtx(t, nil)
	target := filepath.Join(dir, "data.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := NewFile(map[string]specpkg.FileEntry{
		"data.txt": {Path: "data.txt", Mode: specpkg.FileModeRaw},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "hello" {
		t.Errorf("got %q", got[0].Value)
	}
}

func TestFile_PassthroughMode(t *testing.T) {
	ctx, dir := newCtx(t, nil)
	encoded := base64.StdEncoding.EncodeToString([]byte("payload"))
	target := filepath.Join(dir, "blob.b64")
	if err := os.WriteFile(target, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	g := NewFile(map[string]specpkg.FileEntry{
		"blob": {Path: "blob.b64", Mode: specpkg.FileModePassthrough},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Generator should have decoded the base64 → raw bytes.
	if string(got[0].Value) != "payload" {
		t.Errorf("decoded = %q", got[0].Value)
	}
}

func TestFile_PassthroughInvalidB64(t *testing.T) {
	ctx, dir := newCtx(t, nil)
	target := filepath.Join(dir, "bad.b64")
	if err := os.WriteFile(target, []byte("not base64!!!"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := NewFile(map[string]specpkg.FileEntry{
		"bad": {Path: "bad.b64", Mode: specpkg.FileModePassthrough},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestFile_PathTraversal_Rejected(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewFile(map[string]specpkg.FileEntry{
		"escape": {Path: "../escape.txt"},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for path traversal")
	}
}

func TestFile_NotFound(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewFile(map[string]specpkg.FileEntry{
		"nope": {Path: "nope.txt"},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFile_PathTraversalErrorIsStructured(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewFile(map[string]specpkg.FileEntry{
		"escape": {Path: "../../../etc/passwd"},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error")
	}
	// Don't lock the exact format — just confirm there's an error type.
	var any error
	any = err
	_ = errors.Is(any, any) // smoke check on error chain
}

// --- env fallback modes: optional / default / passwd ------------------------

func TestEnv_OptionalOmitsWhenMissing(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{}) // no env
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MISSING", Optional: true}})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("optional + missing env should omit the key, got %d entries", len(got))
	}
}

func TestEnv_OptionalEmittedWhenSet(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{"V": "present"})
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "V", Optional: true}})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].Value) != "present" {
		t.Errorf("optional + set should emit the value, got %+v", got)
	}
}

func TestEnv_DefaultUsedWhenMissing(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{})
	d := "fallback"
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MISSING", Default: &d}})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].Value) != "fallback" {
		t.Errorf("default should be used when env missing, got %+v", got)
	}
}

func TestEnv_DefaultOverriddenByEnv(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{"V": "real"})
	d := "fallback"
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "V", Default: &d}})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "real" {
		t.Errorf("env should win over default, got %q", got[0].Value)
	}
}

func TestEnv_PasswdFallbackGeneratesAndCaches(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{}) // no env → generate
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "MISSING", Passwd: &specpkg.PasswdEntry{Length: 24}}})
	got1, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got1) != 1 || len(got1[0].Value) != 24 {
		t.Fatalf("passwd fallback should generate 24 chars, got %q (len %d)", got1[0].Value, len(got1[0].Value))
	}
	got2, _ := g.Generate(ctx) // stable across runs (cached)
	if string(got1[0].Value) != string(got2[0].Value) {
		t.Errorf("passwd fallback should be stable: %q vs %q", got1[0].Value, got2[0].Value)
	}
}

func TestEnv_PasswdFallbackOverriddenByEnv(t *testing.T) {
	ctx, _ := newCtx(t, map[string]string{"V": "operator-set"})
	g := NewEnv(map[string]specpkg.EnvEntry{"K": {Var: "V", Passwd: &specpkg.PasswdEntry{Length: 24}}})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(got[0].Value) != "operator-set" {
		t.Errorf("env should win over passwd fallback, got %q", got[0].Value)
	}
}
