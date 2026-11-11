package secrets

// decrypt_entry_test.go: DecryptEntry, the in-memory decrypt of one store
// entry behind LoadProviderCreds, finds its identity through the same chain
// as `lo secrets decrypt`: SOPS_AGE_KEY, the SOPS key file, the identity
// derived from LOK8S_SSH_KEY. Each step on its own, and the cause classes.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
)

// entryStore encrypts one entry to recipient and drops the plaintext.
func entryStore(t *testing.T, c *Context, recipient string) string {
	t.Helper()
	sopsYAMLWith(t, c, recipient)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.hetzner.provisioning.HCLOUD_TOKEN", "tok-enc")
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store + "/Secret.hetzner.provisioning.HCLOUD_TOKEN"); err != nil {
		t.Fatal(err)
	}
	return store + "/Secret.hetzner.provisioning.HCLOUD_TOKEN.enc"
}

func TestDecryptEntryIdentityChain(t *testing.T) {
	t.Run("SOPS_AGE_KEY_FILE", func(t *testing.T) {
		c, _, _ := testEnv(t)
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		enc := entryStore(t, c, id.Recipient().String())
		keyFile := filepath.Join(c.Paths.Base, "keys.txt")
		write(t, keyFile, "# created: test\n"+id.String()+"\n")
		t.Setenv("SOPS_AGE_KEY_FILE", keyFile)
		t.Setenv("LOK8S_SSH_KEY", filepath.Join(c.Paths.Base, "no-ssh-key"))
		if got, err := DecryptEntry(enc); err != nil || string(got) != "tok-enc" {
			t.Fatalf("DecryptEntry = %q, %v", got, err)
		}
	})
	t.Run("LOK8S_SSH_KEY derivation", func(t *testing.T) {
		c, _, _ := testEnv(t)
		privPath, pubPath := testSSHKeypair(t, c.Paths.Base)
		pub, _, err := deriveAgePublicKey(pubPath)
		if err != nil {
			t.Fatal(err)
		}
		enc := entryStore(t, c, pub)
		t.Setenv("LOK8S_SSH_KEY", privPath)
		if got, err := DecryptEntry(enc); err != nil || string(got) != "tok-enc" {
			t.Fatalf("DecryptEntry = %q, %v", got, err)
		}
		// The default key path is ~/.ssh/id_ed25519 when LOK8S_SSH_KEY is unset.
		home := os.Getenv("HOME")
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(privPath)
		write(t, filepath.Join(home, ".ssh", "id_ed25519"), string(raw))
		t.Setenv("LOK8S_SSH_KEY", "")
		os.Unsetenv("LOK8S_SSH_KEY")
		if got, err := DecryptEntry(enc); err != nil || string(got) != "tok-enc" {
			t.Fatalf("default ~/.ssh/id_ed25519: DecryptEntry = %q, %v", got, err)
		}
	})
	t.Run("cause classes", func(t *testing.T) {
		c, _, _ := testEnv(t)
		id, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatal(err)
		}
		enc := entryStore(t, c, id.Recipient().String())
		t.Setenv("LOK8S_SSH_KEY", filepath.Join(c.Paths.Base, "no-ssh-key"))
		if _, err := DecryptEntry(enc); !errors.Is(err, ErrNoIdentity) {
			t.Errorf("no identity: err = %v", err)
		}
		other, _ := age.GenerateX25519Identity()
		t.Setenv("SOPS_AGE_KEY", other.String())
		if _, err := DecryptEntry(enc); !errors.Is(err, ErrDecryptEnc) {
			t.Errorf("another identity: err = %v", err)
		}
		if _, err := DecryptEntry(enc + ".missing"); !errors.Is(err, ErrReadEnc) {
			t.Errorf("missing .enc: err = %v", err)
		}
	})
}
