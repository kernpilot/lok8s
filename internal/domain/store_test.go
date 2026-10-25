package domain

// store_test.go: a domain directory without its own secrets store, in a
// project that keeps a store per domain, must not fall back to the flat
// .secrets/ store (the recreate: the first run of a new domain directory
// would generate a new sops-age key and new tokens into the flat store).

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernpilot/lok8s/internal/ui"
)

func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMissingSecretsStore(t *testing.T) {
	cases := []struct {
		name      string
		dirs      []string // relative to clusters/
		domain    string
		wantOK    bool
		wantOther string
	}{
		{"legacy project, flat store only", []string{"new.dev"}, "new.dev", false, ""},
		{"per-domain project, domain without a store", []string{"b.dev/secrets", "a.dev/secrets", "new.dev"}, "new.dev", true, "a.dev/secrets"},
		{"domain with its own store", []string{"a.dev/secrets", "new.dev/secrets"}, "new.dev", false, ""},
		{"no domain directory (the lok8s.dev default)", []string{"a.dev/secrets"}, "lok8s.dev", false, ""},
		{"no domain", []string{"a.dev/secrets"}, "", false, ""},
		{"a dot-directory store does not count", []string{".hidden/secrets", "new.dev"}, "new.dev", false, ""},
		{"a secrets FILE does not count", []string{"a.dev", "new.dev"}, "new.dev", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clusters := filepath.Join(t.TempDir(), "clusters")
			for _, d := range c.dirs {
				mkdirs(t, filepath.Join(clusters, d))
			}
			if c.name == "a secrets FILE does not count" {
				if err := os.WriteFile(filepath.Join(clusters, "a.dev", "secrets"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			other, ok := MissingSecretsStore(clusters, c.domain)
			want := ""
			if c.wantOther != "" {
				want = clusters + "/" + c.wantOther
			}
			if ok != c.wantOK || other != want {
				t.Errorf("MissingSecretsStore = (%q, %v), want (%q, %v)", other, ok, want, c.wantOK)
			}
		})
	}
}

func TestCheckSecretsStoreMessage(t *testing.T) {
	clusters := filepath.Join(t.TempDir(), "clusters")
	mkdirs(t, filepath.Join(clusters, "kubehz.dev", "secrets"), filepath.Join(clusters, "green.dev"))
	var buf bytes.Buffer
	// The message names the store lo would otherwise use, here an explicit
	// PATH_SECRETS: it does not override the refusal.
	err := CheckSecretsStore(clusters, "green.dev", "/custom/flat", &buf)
	if !errors.Is(err, ui.ErrHandled) {
		t.Fatalf("err = %v, want ui.ErrHandled", err)
	}
	store := clusters + "/green.dev/secrets"
	want := "[error] domain green.dev has no secrets store: " + store + " does not exist, and this project keeps a store per domain (" + clusters + "/kubehz.dev/secrets). lo does not fall back to /custom/flat.\n" +
		"[error]   Create the empty directory " + store + " to generate new values for green.dev.\n"
	if buf.String() != want {
		t.Errorf("message:\n got %q\nwant %q", buf.String(), want)
	}
	// Creating the empty store is the documented way out.
	mkdirs(t, store)
	buf.Reset()
	if err := CheckSecretsStore(clusters, "green.dev", "/custom/flat", &buf); err != nil || buf.Len() != 0 {
		t.Errorf("with the store: err = %v, output %q", err, buf.String())
	}
}

// The other store is the first in byte order (bash: LC_ALL=C sort), so both
// implementations name the same one whatever the locale.
func TestMissingSecretsStoreNamesTheByteOrderFirst(t *testing.T) {
	clusters := filepath.Join(t.TempDir(), "clusters")
	mkdirs(t, filepath.Join(clusters, "a.dev", "secrets"), filepath.Join(clusters, "B.dev", "secrets"), filepath.Join(clusters, "new.dev"))
	if other, ok := MissingSecretsStore(clusters, "new.dev"); !ok || other != clusters+"/B.dev/secrets" {
		t.Errorf("MissingSecretsStore = (%q, %v), want B.dev (byte order)", other, ok)
	}
}
