package cache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFormatName(t *testing.T) {
	cases := []struct {
		secret, ns, key, want string
	}{
		{"ut-user", "default", "PASSWORD", "Secret.ut-user.default.PASSWORD"},
		{"db", "", "user", "Secret.db.default.user"},  // empty ns → "default"
		{"a", "b", "c.d.e", "Secret.a.b.c.d.e"},       // dotted keys allowed
	}
	for _, c := range cases {
		if got := FormatName(c.secret, c.ns, c.key); got != c.want {
			t.Errorf("FormatName(%q,%q,%q) = %q, want %q", c.secret, c.ns, c.key, got, c.want)
		}
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in, defaultNS string
		secret, ns, key string
		ok bool
	}{
		{"db/password", "myns", "db", "myns", "password", true},
		{"db/other-ns/password", "myns", "db", "other-ns", "password", true},
		{"Secret.db.other-ns.password", "myns", "db", "other-ns", "password", true},
		{"", "myns", "", "", "", false},
		{"single", "myns", "", "", "", false},
		{"too/many/parts/here", "myns", "", "", "", false},
	}
	for _, c := range cases {
		s, n, k, ok := ParseRef(c.in, c.defaultNS)
		if ok != c.ok || s != c.secret || n != c.ns || k != c.key {
			t.Errorf("ParseRef(%q) = (%q,%q,%q,%v), want (%q,%q,%q,%v)",
				c.in, s, n, k, ok, c.secret, c.ns, c.key, c.ok)
		}
	}
}

func TestNew_RejectsEmptyDir(t *testing.T) {
	if _, err := New("", "default", "x"); err == nil {
		t.Error("expected error for empty dir")
	}
}

func TestNew_RejectsEmptyName(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir, "default", ""); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestPutAndGet(t *testing.T) {
	dir := t.TempDir()
	c, err := New(dir, "default", "ut-user")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("PASSWORD", []byte("s3cr3t")); err != nil {
		t.Fatal(err)
	}
	if !c.Has("PASSWORD") {
		t.Error("Has should be true after Put")
	}
	got, err := c.Get("PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "s3cr3t" {
		t.Errorf("Get = %q", got)
	}
	// Verify mode is 0600.
	st, err := os.Stat(filepath.Join(dir, "Secret.ut-user.default.PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file perm = %o, want 0600", st.Mode().Perm())
	}
}

func TestGet_Missing(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir, "default", "x")
	_, err := c.Get("nope")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestGetOrCreate_FirstCall_Generates(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir, "default", "x")
	called := false
	got, err := c.GetOrCreate("KEY", func() ([]byte, error) {
		called = true
		return []byte("generated"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Error("gen should have been called on first GetOrCreate")
	}
	if string(got) != "generated" {
		t.Errorf("got %q", got)
	}
}

func TestGetOrCreate_SecondCall_Cached(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir, "default", "x")
	if _, err := c.GetOrCreate("KEY", func() ([]byte, error) {
		return []byte("first"), nil
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	got, err := c.GetOrCreate("KEY", func() ([]byte, error) {
		called = true
		return []byte("second"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("gen should NOT be called on second GetOrCreate")
	}
	if string(got) != "first" {
		t.Errorf("got %q, want cached 'first'", got)
	}
}

func TestReadByName_Cross(t *testing.T) {
	dir := t.TempDir()
	// Producer secret writes its own cache entry.
	producer, _ := New(dir, "ns", "producer")
	if err := producer.Put("password", []byte("p4ss")); err != nil {
		t.Fatal(err)
	}
	// Consumer reads it via the canonical filename.
	consumer, _ := New(dir, "ns", "consumer")
	got, err := consumer.ReadByName(FormatName("producer", "ns", "password"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "p4ss" {
		t.Errorf("ReadByName = %q", got)
	}
}

func TestReadByName_RejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	// Create a file outside the cache that an attacker might want to read.
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	_ = os.WriteFile(outside, []byte("secret"), 0o600)
	defer os.Remove(outside)

	c, _ := New(dir, "ns", "name")
	cases := []string{
		"../outside.txt",
		"../../etc/passwd",
		"./..",
		"sub/dir/file",
		"/etc/passwd",
	}
	for _, name := range cases {
		_, err := c.ReadByName(name)
		if err == nil {
			t.Errorf("ReadByName(%q) should have rejected", name)
		}
	}
}

func TestReadByName_EmptyRejected(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir, "ns", "name")
	if _, err := c.ReadByName(""); err == nil {
		t.Error("empty filename should be rejected")
	}
}

func TestDir(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir, "ns", "name")
	if c.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", c.Dir(), dir)
	}
}

func TestGetOrCreate_GenError(t *testing.T) {
	dir := t.TempDir()
	c, _ := New(dir, "ns", "name")
	wantErr := errors.New("gen failed")
	_, err := c.GetOrCreate("KEY", func() ([]byte, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Errorf("expected wrapped %v, got %v", wantErr, err)
	}
	// Cache should NOT have an entry for the failed key.
	if c.Has("KEY") {
		t.Error("failed gen should not write to cache")
	}
}

func TestNew_MkdirFailure(t *testing.T) {
	// Try to create a cache under a path that's not writable.
	// We use /proc which is read-only on Linux.
	if _, err := New("/proc/sys/fake-path-cannot-be-created", "ns", "name"); err == nil {
		t.Error("expected mkdir failure")
	}
}
