package cache

// enc_test.go: an entry whose plaintext is missing while its encrypted twin
// (<entry>.enc) exists is not a miss (B425). A `lo build` in a fresh
// worktree before `lo secrets decrypt` generated 32 new values over the
// committed ones, and a raw kustomize build generated 2 database passwords
// into a shared store.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeEnc writes an encrypted twin the way `lo secrets encrypt` names it.
// The plugin never reads its content, so a fake sops body is enough.
func fakeEnc(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+EncSuffix), []byte(`{"data":"ENC[AES256_GCM,data:fake]","sops":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEncryptedTwinIsNotAMiss(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	fakeEnc(t, dir, "Secret.app.default.PASSWORD")
	c, err := New(dir, "default", "app")
	if err != nil {
		t.Fatal(err)
	}

	called := false
	_, err = c.GetOrCreate("PASSWORD", func() ([]byte, error) {
		called = true
		return []byte("minted"), nil
	})
	var eo *EncryptedOnlyError
	if !errors.As(err, &eo) {
		t.Fatalf("GetOrCreate err = %v, want *EncryptedOnlyError", err)
	}
	if called {
		t.Error("GetOrCreate generated a value over the encrypted twin")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Error("the error reads as a miss (os.ErrNotExist)")
	}
	want := "cannot read Secret.app.default.PASSWORD: the store " + dir +
		" has Secret.app.default.PASSWORD.enc but no plaintext." +
		" The plugin does not generate a new value over an encrypted value." +
		` Run "lo secrets decrypt --domain alpha.dev", then build again`
	if err.Error() != want {
		t.Errorf("message:\n got %s\nwant %s", err, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "Secret.app.default.PASSWORD")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a plaintext was written: %v", err)
	}

	// The entry exists for Has (an env entry without update reads it), and
	// Get and ReadByName (secretRef) stop the same way.
	if !c.Has("PASSWORD") {
		t.Error("Has = false for an encrypted-only entry")
	}
	if _, err := c.Get("PASSWORD"); !errors.As(err, &eo) {
		t.Errorf("Get err = %v", err)
	}
	if _, err := c.ReadByName("Secret.app.default.PASSWORD"); !errors.As(err, &eo) {
		t.Errorf("ReadByName err = %v", err)
	}
}

// The deliberate rotation path: with the plaintext AND the .enc deleted, the
// next build generates a new value. A plain miss stays os.ErrNotExist.
func TestRotationDeletesBothThenGenerates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	fakeEnc(t, dir, "Secret.app.default.PASSWORD")
	if err := os.Remove(filepath.Join(dir, "Secret.app.default.PASSWORD.enc")); err != nil {
		t.Fatal(err)
	}
	c, _ := New(dir, "default", "app")
	if _, err := c.Get("PASSWORD"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get err = %v, want os.ErrNotExist", err)
	}
	if c.Has("PASSWORD") {
		t.Error("Has = true with neither file")
	}
	v, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("new"), nil })
	if err != nil || string(v) != "new" {
		t.Fatalf("GetOrCreate = %q, %v", v, err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "Secret.app.default.PASSWORD")); string(raw) != "new" {
		t.Errorf("stored = %q", raw)
	}
}

// A plaintext next to its .enc is a normal hit: the twin changes nothing.
func TestPlaintextWithTwinIsAHit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	fakeEnc(t, dir, "Secret.app.default.PASSWORD")
	c, _ := New(dir, "default", "app")
	if err := c.Put("PASSWORD", []byte("stored")); err != nil {
		t.Fatal(err)
	}
	v, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("new"), nil })
	if err != nil || string(v) != "stored" {
		t.Fatalf("GetOrCreate = %q, %v", v, err)
	}
}

func TestDecryptCommand(t *testing.T) {
	cases := map[string]string{
		"/p/clusters/alpha.dev/secrets": "lo secrets decrypt --domain alpha.dev",
		"/p/.secrets":                   "lo secrets decrypt",
		"/p/custom-store":               "lo secrets decrypt",
		"/tmp/x/secrets":                "lo secrets decrypt", // a copy outside clusters/
		"secrets":                       "lo secrets decrypt",
		"/secrets":                      "lo secrets decrypt",
	}
	for dir, want := range cases {
		if got := DecryptCommand(dir); got != want {
			t.Errorf("DecryptCommand(%q) = %q, want %q", dir, got, want)
		}
	}
}

// A dry run stops the same way: the real run would stop, so the dry run
// reports nothing as generated and writes nothing.
func TestOverlayEncryptedTwinStops(t *testing.T) {
	root := t.TempDir()
	report := filepath.Join(root, "report")
	if err := os.WriteFile(report, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(root, "clusters", "alpha.dev", "secrets")
	fakeEnc(t, store, "Secret.app.default.PASSWORD")
	before := dirListing(t, store)
	s, err := Open(store, "default", "app", DryRunFromEnv(dryRunEnvStore(report, filepath.Join(root, "run"))))
	if err != nil {
		t.Fatal(err)
	}
	var eo *EncryptedOnlyError
	if _, err := s.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("minted"), nil }); !errors.As(err, &eo) {
		t.Fatalf("GetOrCreate err = %v", err)
	}
	if _, err := s.ReadByName("Secret.app.default.PASSWORD"); !errors.As(err, &eo) {
		t.Fatalf("ReadByName err = %v", err)
	}
	if raw, _ := os.ReadFile(report); len(raw) != 0 {
		t.Errorf("report = %q, want empty", raw)
	}
	if after := dirListing(t, store); after != before {
		t.Errorf("the store changed:\nbefore %s\nafter  %s", before, after)
	}
}

// A dangling symlink is no plaintext: with the .enc next to it, the entry is
// encrypted-only, and GetOrCreate does not generate through the link.
func TestDanglingPlaintextSymlinkIsNotAPlaintext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	fakeEnc(t, dir, "Secret.app.default.PASSWORD")
	if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "Secret.app.default.PASSWORD")); err != nil {
		t.Fatal(err)
	}
	c, _ := New(dir, "default", "app")
	called := false
	_, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { called = true; return []byte("minted"), nil })
	var eo *EncryptedOnlyError
	if !errors.As(err, &eo) || called {
		t.Fatalf("GetOrCreate err = %v, generated = %v", err, called)
	}
	if fi, err := os.Lstat(filepath.Join(dir, "Secret.app.default.PASSWORD")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced: %v", err)
	}
}
