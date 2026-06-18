package generator

import (
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/kustomize/pkg/htpasswdfmt"

	specpkg "github.com/kernpilot/lok8s/kustomize/plugins/secret/spec"
)

func TestHtpasswd_GeneratesUserAndPassword(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"smtp.htpasswd": {
			Username: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 12}},
			Password: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 24}},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d entries", len(got))
	}
	line := got[0].Value
	// Should have form "username:$2..." (bcrypt prefix).
	if !strings.Contains(string(line), ":$2") {
		t.Errorf("expected bcrypt line, got %q", line)
	}
	// Cache files should exist for username, password, and bcrypt.
	if !ctx.Cache.Has("smtp.htpasswd.username") {
		t.Error("missing .username cache")
	}
	if !ctx.Cache.Has("smtp.htpasswd.password") {
		t.Error("missing .password cache")
	}
	if !ctx.Cache.Has("smtp.htpasswd.bcrypt") {
		t.Error("missing .bcrypt cache")
	}
}

func TestHtpasswd_LiteralUsername(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"k": {
			Username: specpkg.UserOrGen{Literal: "alice"},
			Password: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 16}},
		},
	})
	got, _ := g.Generate(ctx)
	line := string(got[0].Value)
	if !strings.HasPrefix(line, "alice:") {
		t.Errorf("expected alice: prefix, got %q", line)
	}
	cached, _ := ctx.Cache.Get("k.username")
	if string(cached) != "alice" {
		t.Errorf("cached username = %q", cached)
	}
}

func TestHtpasswd_LiteralPassword(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"k": {
			Username: specpkg.UserOrGen{Literal: "bob"},
			Password: specpkg.UserOrGen{Literal: "hunter2"},
		},
	})
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	line := got[0].Value
	if err := htpasswdfmt.Verify(line, []byte("hunter2")); err != nil {
		t.Errorf("bcrypt verify failed: %v", err)
	}
}

func TestHtpasswd_DeterministicAcrossRuns(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"k": {
			Username: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 12}},
			Password: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 24}},
		},
	})
	got1, _ := g.Generate(ctx)
	got2, _ := g.Generate(ctx)
	// Output must be byte-stable (this is the cache-first guarantee).
	if string(got1[0].Value) != string(got2[0].Value) {
		t.Errorf("not deterministic:\n%s\nvs\n%s", got1[0].Value, got2[0].Value)
	}
}

func TestHtpasswd_UsernameStartsWithLetter(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"k": {
			Username: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 8}},
			Password: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 16}},
		},
	})
	if _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
	user, _ := ctx.Cache.Get("k.username")
	if user[0] < 'a' || user[0] > 'z' {
		t.Errorf("username should start with a letter, got %q", user[0])
	}
}

func TestHtpasswd_PasswordCanIncludeSymbols(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"k": {
			Username: specpkg.UserOrGen{Literal: "u"},
			Password: specpkg.UserOrGen{Gen: &specpkg.PasswdEntry{Length: 64, Chars: "alphanum+symbols"}},
		},
	})
	if _, err := g.Generate(ctx); err != nil {
		t.Fatal(err)
	}
	pw, _ := ctx.Cache.Get("k.password")
	if len(pw) != 64 {
		t.Errorf("password length = %d", len(pw))
	}
}

func TestHtpasswd_RotateByDeletingBcryptCache(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(map[string]specpkg.HtpasswdEntry{
		"k": {
			Username: specpkg.UserOrGen{Literal: "u"},
			Password: specpkg.UserOrGen{Literal: "p"},
		},
	})
	got1, _ := g.Generate(ctx)
	// Delete just the .bcrypt cache (simulate rotation).
	bcryptPath := ctx.Cache.(interface{ Dir() string }).Dir() + "/Secret.test-secret.default.k.bcrypt"
	_ = removeFile(bcryptPath)
	got2, _ := g.Generate(ctx)
	// New bcrypt hash (different salt) but verifies the same password.
	if string(got1[0].Value) == string(got2[0].Value) {
		t.Error("after deleting .bcrypt, hash should be regenerated with new salt")
	}
	if err := htpasswdfmt.Verify(got2[0].Value, []byte("p")); err != nil {
		t.Errorf("regenerated hash should still verify: %v", err)
	}
}

func TestHtpasswd_EmptySpec(t *testing.T) {
	ctx, _ := newCtx(t, nil)
	g := NewHtpasswd(nil)
	got, err := g.Generate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("empty htpasswd should return nil, got %+v", got)
	}
}

// removeFile is a tiny helper used by the rotation test. We avoid
// importing "os" at the top of this file just for this one call.
func removeFile(path string) error {
	return osRemove(path)
}
