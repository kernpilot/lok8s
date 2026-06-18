package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/cache"
	"github.com/kernpilot/lok8s/kustomize/pkg/plugin"
	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

func bashCtx(t *testing.T) (*plugin.Context, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := cache.New(dir, "default", "test")
	if err != nil {
		t.Fatal(err)
	}
	return &plugin.Context{
		Name:      "test",
		Namespace: "default",
		Cache:     c,
		Env:       func(k string) (string, bool) { return os.LookupEnv(k) },
		FileRoot:  dir,
	}, dir
}

// ── shorthand exec ──────────────────────────────────────

func TestBash_ExecShorthand(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo hello"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if string(entries[0].Value) != "hello" {
		t.Errorf("expected 'hello', got %q", entries[0].Value)
	}
}

// ── newline modes ───────────────────────────────────────

func TestBash_NewlineStrip(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "printf 'val\\n\\n'", Newline: "strip"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "val" {
		t.Errorf("expected 'val', got %q", entries[0].Value)
	}
}

func TestBash_NewlineKeep(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "printf 'val\\n\\n'", Newline: "keep"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "val\n\n" {
		t.Errorf("expected 'val\\n\\n', got %q", entries[0].Value)
	}
}

func TestBash_NewlineEnsure(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "printf 'val'", Newline: "ensure"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "val\n" {
		t.Errorf("expected 'val\\n', got %q", entries[0].Value)
	}
}

// strip removes the trailing line terminator only — NOT spaces/tabs, which are
// value bytes (and trimming them is what made binary fragile).
func TestBash_NewlineStripKeepsTrailingSpaces(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: `printf 'val  \n'`, Newline: "strip"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "val  " {
		t.Errorf("strip should keep trailing spaces, got %q want %q", entries[0].Value, "val  ")
	}
}

// ── encode ──────────────────────────────────────────────

func TestBash_EncodeHex(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "printf 'AB'", Encode: "hex"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "4142" {
		t.Errorf("expected '4142', got %q", entries[0].Value)
	}
}

func TestBash_EncodeBase64(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "printf 'hello'", Encode: "base64"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "aGVsbG8=" {
		t.Errorf("expected 'aGVsbG8=', got %q", entries[0].Value)
	}
}

// Encoding must preserve the EXACT command bytes. The default newline mode is
// "strip", which trims trailing whitespace-VALUED bytes — applying it before
// encoding silently corrupts binary keys (e.g. `openssl rand` output whose last
// byte is 0x0a/0x20/...). Encode must run before newline handling.
func TestBash_EncodePreservesTrailingWhitespaceBytes(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		// 3 bytes: 'A' 'B' 0x0a — the trailing 0x0a must survive into the base64.
		"KEY": {Exec: `printf 'AB\n'`, Encode: "base64"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// base64("AB\n")=="QUIK"; the strip-then-encode bug yields base64("AB")=="QUI=".
	if string(entries[0].Value) != "QUIK" {
		t.Errorf("trailing byte stripped before encode (binary corruption): got %q, want %q", entries[0].Value, "QUIK")
	}
}

// ── output capture ──────────────────────────────────────

func TestBash_OutputStderr(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo out; echo err >&2", Output: "stderr"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "err" {
		t.Errorf("expected 'err', got %q", entries[0].Value)
	}
}

func TestBash_OutputCombined(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo out; echo err >&2", Output: "combined"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	val := string(entries[0].Value)
	if !strings.Contains(val, "out") || !strings.Contains(val, "err") {
		t.Errorf("expected both out+err, got %q", val)
	}
}

// ── file exec ───────────────────────────────────────────

func TestBash_File(t *testing.T) {
	ctx, dir := bashCtx(t)
	script := filepath.Join(dir, "gen.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho from-script"), 0755); err != nil {
		t.Fatal(err)
	}
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {File: script},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "from-script" {
		t.Errorf("expected 'from-script', got %q", entries[0].Value)
	}
}

// ── caching ─────────────────────────────────────────────

func TestBash_CacheReuse(t *testing.T) {
	ctx, _ := bashCtx(t)
	// Use a command that produces different output each time
	spec := map[string]specpkg.BashEntry{
		"KEY": {Exec: "date +%N"},
	}
	g1 := NewBash(spec)
	entries1, err := g1.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	g2 := NewBash(spec)
	entries2, err := g2.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if string(entries1[0].Value) != string(entries2[0].Value) {
		t.Errorf("cache not reused: %q vs %q", entries1[0].Value, entries2[0].Value)
	}
}

// ── command failure ─────────────────────────────────────

func TestBash_CommandFails(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "exit 1"},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for failing command")
	}
	if !strings.Contains(err.Error(), "bash command failed") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestBash_EmptyOutput(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "true"},
	})
	_, err := g.Generate(ctx)
	if err == nil {
		t.Fatal("expected error for empty output")
	}
	if !strings.Contains(err.Error(), "empty output") {
		t.Errorf("unexpected error: %v", err)
	}
}

// ── security: SHA allow ─────────────────────────────────

func TestBash_ShaFileCreated(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo test"},
	})
	if _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
	shaFile := filepath.Join(ctx.Cache.Dir(), "Secret.test.default.KEY.sha")
	if _, err := os.Stat(shaFile); err != nil {
		t.Fatalf(".sha file not created: %v", err)
	}
	data, _ := os.ReadFile(shaFile)
	if len(strings.TrimSpace(string(data))) != 64 {
		t.Errorf("expected 64-char hex hash, got %q", data)
	}
}

func TestBash_BashAllowCreated(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo test"},
	})
	if _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
	allowFile := filepath.Join(ctx.Cache.Dir(), ".bash-allow")
	if _, err := os.Stat(allowFile); err != nil {
		t.Fatalf(".bash-allow not created: %v", err)
	}
}

func TestBash_HashMismatchFails(t *testing.T) {
	ctx, _ := bashCtx(t)

	// First run with "echo original"
	g1 := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo original"},
	})
	if _, err := g1.Generate(ctx); err != nil {
		t.Fatal(err)
	}

	// Second run with changed command — should fail
	g2 := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo CHANGED"},
	})
	_, err := g2.Generate(ctx)
	if err == nil {
		t.Fatal("expected hash mismatch error")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("expected hash mismatch error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "lo secrets allow") {
		t.Errorf("expected 'lo secrets allow' in error, got: %v", err)
	}
}

func TestBash_StaleAllowFails(t *testing.T) {
	ctx, _ := bashCtx(t)

	// First run creates .bash-allow
	g1 := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo test"},
	})
	if _, err := g1.Generate(ctx); err != nil {
		t.Fatal(err)
	}

	// Corrupt .bash-allow
	allowFile := filepath.Join(ctx.Cache.Dir(), ".bash-allow")
	if err := os.WriteFile(allowFile, []byte("badhash\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Re-run — should fail because .bash-allow doesn't match
	g2 := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo test"},
	})
	_, err := g2.Generate(ctx)
	if err == nil {
		t.Fatal("expected stale allow error")
	}
	if !strings.Contains(err.Error(), "lo secrets allow") {
		t.Errorf("expected 'lo secrets allow' in error, got: %v", err)
	}
}

// TestBash_SubsetOfApprovedSetPasses is the regression test for the per-target
// build bug: builds run one target at a time, so a build sees only that target's
// bash entries. The approval set (.bash-allow) holds the UNION across targets,
// and a single target's entries must validate as a subset of it.
func TestBash_SubsetOfApprovedSetPasses(t *testing.T) {
	ctx, _ := bashCtx(t)

	// First run approves KEY's hash into .bash-allow.
	g1 := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo test"},
	})
	if _, err := g1.Generate(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate `lo secrets allow` writing the UNION across targets: keep KEY's
	// approved hash and add an unrelated one (a sibling target's entry).
	allowFile := filepath.Join(ctx.Cache.Dir(), ".bash-allow")
	data, err := os.ReadFile(allowFile)
	if err != nil {
		t.Fatal(err)
	}
	union := strings.TrimSpace(string(data)) + "\n" +
		"0000000000000000000000000000000000000000000000000000000000000000\n"
	if err := os.WriteFile(allowFile, []byte(union), 0600); err != nil {
		t.Fatal(err)
	}

	// Re-run with ONLY KEY (a per-target subset) — must pass: KEY's hash is a
	// member of the approved set even though the set has more entries.
	g2 := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo test"},
	})
	if _, err := g2.Generate(ctx); err != nil {
		t.Fatalf("subset build should pass against a superset .bash-allow: %v", err)
	}
}

// ── pipes work ──────────────────────────────────────────

func TestBash_PipeCommand(t *testing.T) {
	ctx, _ := bashCtx(t)
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "echo hello world | tr ' ' '-'"},
	})
	entries, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(entries[0].Value) != "hello-world" {
		t.Errorf("expected 'hello-world', got %q", entries[0].Value)
	}
}

// ── update: cache bypass (cluster-bound secrets) ────────

func TestBash_UpdateBypassesCache(t *testing.T) {
	ctx, dir := bashCtx(t)
	statefile := filepath.Join(dir, "statefile")
	if err := os.WriteFile(statefile, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "cat statefile", Update: true},
	})
	if e, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	} else if string(e[0].Value) != "v1" {
		t.Fatalf("first run: expected v1, got %q", e[0].Value)
	}
	// Mutate the underlying state; update:true must re-read it (not the cache).
	if err := os.WriteFile(statefile, []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if e, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	} else if string(e[0].Value) != "v2" {
		t.Errorf("update:true must bypass cache: expected v2, got %q", e[0].Value)
	}
}

func TestBash_DefaultCachesValue(t *testing.T) {
	ctx, dir := bashCtx(t)
	statefile := filepath.Join(dir, "statefile")
	if err := os.WriteFile(statefile, []byte("v1"), 0600); err != nil {
		t.Fatal(err)
	}
	g := NewBash(map[string]specpkg.BashEntry{
		"KEY": {Exec: "cat statefile"}, // no Update → cache-first
	})
	if e, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	} else if string(e[0].Value) != "v1" {
		t.Fatalf("first run: expected v1, got %q", e[0].Value)
	}
	if err := os.WriteFile(statefile, []byte("v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if e, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	} else if string(e[0].Value) != "v1" {
		t.Errorf("default must cache: expected v1 (cached), got %q", e[0].Value)
	}
}
