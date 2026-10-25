package provision

// creds_enc_test.go: a worktree store holds only the committed .enc of the
// provider credentials until `lo secrets decrypt` runs. LoadProviderCreds
// read plaintext only, so provisioning from such a store failed (the
// recreate). It now decrypts the .enc in memory and never writes the
// plaintext.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/kernpilot/lok8s/internal/config"
	"github.com/kernpilot/lok8s/internal/secrets"
	"github.com/kernpilot/lok8s/internal/testutil"
)

// encStore writes the creds the way production does: plaintext in the
// domain store, `lo secrets encrypt` (secrets.Context.Encrypt), then the
// plaintext removed (what a fresh checkout or worktree holds). It isolates
// every ambient identity source.
func encStore(t *testing.T, creds map[string]string) (*config.Paths, string, *age.X25519Identity) {
	t.Helper()
	clearCredsEnv(t)
	p := testPaths(t)
	t.Setenv("HOME", filepath.Join(p.Base, "home"))
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(p.Base, "no-keys.txt"))
	t.Setenv("LOK8S_SSH_KEY", filepath.Join(p.Base, "no-ssh-key"))
	t.Setenv("SOPS_AGE_KEY", "")
	os.Unsetenv("SOPS_AGE_KEY")
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(p.Base, ".sops.yaml"),
		"creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '"+id.Recipient().String()+"'\n")
	store := filepath.Join(p.Clusters, "test.lok8s.dev", "secrets")
	for k, v := range creds {
		testutil.WriteFile(t, filepath.Join(store, "Secret.hetzner.provisioning."+k), v)
	}
	c := &secrets.Context{Paths: p, Domain: "test.lok8s.dev", Out: io.Discard, ErrOut: io.Discard}
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	for k := range creds {
		if err := os.Remove(filepath.Join(store, "Secret.hetzner.provisioning."+k)); err != nil {
			t.Fatal(err)
		}
	}
	return p, store, id
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	return string(out)
}

// plaintextsIn lists the plaintext entries of the store.
func plaintextsIn(t *testing.T, store string) []string {
	t.Helper()
	ents, err := os.ReadDir(store)
	if err != nil {
		t.Fatal(err)
	}
	var plain []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "Secret.") && !strings.HasSuffix(e.Name(), ".enc") {
			plain = append(plain, e.Name())
		}
	}
	return plain
}

func TestLoadProviderCredsDecryptsAnEncOnlyStore(t *testing.T) {
	p, store, id := encStore(t, map[string]string{
		"HCLOUD_TOKEN": "tok-enc\n", "HROBOT_USER": "rob-usr", "HROBOT_PASSWORD": "rob-pwd",
	})
	t.Setenv("SOPS_AGE_KEY", id.String())

	stderr := captureStderr(t, func() {
		if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
			t.Fatal(err)
		}
	})
	want := map[string]string{
		"HCLOUD_TOKEN":           "tok-enc", // trailing newline trimmed, as from a plaintext
		"HROBOT_USER":            "rob-usr",
		"ROBOT_USER":             "rob-usr",
		"HETZNER_ROBOT_PASSWORD": "rob-pwd",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	if plain := plaintextsIn(t, store); len(plain) != 0 {
		t.Errorf("plaintext written to the store: %v", plain)
	}
}

// An .enc that no local identity decrypts is skipped with a warning by name
// (never a value), and LoadProviderCreds still returns nil.
func TestLoadProviderCredsWarnsOnAnUnreadableEnc(t *testing.T) {
	p, store, _ := encStore(t, map[string]string{"HCLOUD_TOKEN": "tok-enc"})
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", other.String()} { // no identity; a wrong one
		os.Unsetenv("SOPS_AGE_KEY")
		if key != "" {
			t.Setenv("SOPS_AGE_KEY", key)
		}
		os.Unsetenv("HCLOUD_TOKEN")
		stderr := captureStderr(t, func() {
			if err := LoadProviderCreds(p, "test.lok8s.dev"); err != nil {
				t.Fatal(err)
			}
		})
		want := "[warn] cannot read Secret.hetzner.provisioning.HCLOUD_TOKEN: the plaintext is missing, and no local age identity decrypts its .enc. Set HCLOUD_TOKEN, or set SOPS_AGE_KEY or LOK8S_SSH_KEY to an identity of the store.\n"
		if stderr != want {
			t.Errorf("key=%t: stderr = %q, want %q", key != "", stderr, want)
		}
		if got := os.Getenv("HCLOUD_TOKEN"); got != "" {
			t.Errorf("key=%t: HCLOUD_TOKEN = %q", key != "", got)
		}
		if plain := plaintextsIn(t, store); len(plain) != 0 {
			t.Errorf("plaintext written to the store: %v", plain)
		}
	}
}
