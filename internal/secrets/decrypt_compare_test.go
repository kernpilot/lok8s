package secrets

// decrypt_compare_test.go: the failure paths of the decrypt compare (review
// round 1 of lok8s#298). A MAC mismatch must not print the two MACs: they
// are unkeyed SHA-512 digests of the plaintext. A plaintext that cannot be
// read is named as such, not as a decrypt failure.

import (
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/getsops/sops/v3/aes"
	sopsage "github.com/getsops/sops/v3/age"
	"github.com/getsops/sops/v3/keyservice"
)

// hexRun matches a digest-length hex run (a SHA-512 MAC is 128 hex chars).
var hexRun = regexp.MustCompile(`[0-9a-fA-F]{32,}`)

// macMismatch rewrites encPath so that its values decrypt but no longer
// match its MAC: the value is replaced and encrypted again with the same
// data key, and the old encrypted MAC is kept (a changed .enc).
func macMismatch(t *testing.T, encPath string, id *age.X25519Identity, value string) {
	t.Helper()
	raw, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatal(err)
	}
	store := binaryStore()
	tree, err := store.LoadEncryptedFile(raw)
	if err != nil {
		t.Fatal(err)
	}
	var ids sopsage.ParsedIdentities
	if err := ids.Import(id.String()); err != nil {
		t.Fatal(err)
	}
	svc := keyservice.NewCustomLocalClient(ageDecryptor{identities: ids})
	dataKey, err := tree.Metadata.GetDataKeyWithKeyServices([]keyservice.KeyServiceClient{svc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cipher := aes.NewCipher()
	if _, err := tree.Decrypt(dataKey, cipher); err != nil {
		t.Fatal(err)
	}
	tree.Branches[0][0].Value = value
	if _, err := tree.Encrypt(dataKey, cipher); err != nil { // the new MAC is dropped
		t.Fatal(err)
	}
	out, err := store.EmitEncryptedFile(tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(encPath, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDecryptMACMismatchPrintsNoDigest(t *testing.T) {
	c, _, errOut := testEnv(t)
	id := testAgeIdentity(t, c)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.K", "committed-value")
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	macMismatch(t, store+"/Secret.app.default.K.enc", id, "changed-value")
	t.Setenv("SOPS_AGE_KEY", id.String())

	// The library error itself carries no digest.
	raw, _ := os.ReadFile(store + "/Secret.app.default.K.enc")
	if _, err := sopsDecryptData(raw, id.String()); !errors.Is(err, errMACMismatch) || hexRun.MatchString(err.Error()) {
		t.Fatalf("sopsDecryptData err = %v, want errMACMismatch without a digest", err)
	}

	// The compare path (a kept, newer plaintext): one fixed line.
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(store+"/Secret.app.default.K", future, future)
	if err := c.Decrypt("/nonexistent-ssh-key"); err == nil {
		t.Fatal("a MAC mismatch decrypted")
	}
	if errOut.String() != "[error] Failed to decrypt Secret.app.default.K.enc\n" {
		t.Errorf("compare path stderr = %q", errOut.String())
	}

	// The decrypt path (no plaintext): the library error, still no digest.
	os.Remove(store + "/Secret.app.default.K")
	errOut.Reset()
	if err := c.Decrypt("/nonexistent-ssh-key"); err == nil {
		t.Fatal("a MAC mismatch decrypted")
	}
	if hexRun.MatchString(errOut.String()) || errOut.Len() == 0 {
		t.Errorf("decrypt path stderr = %q", errOut.String())
	}
	if _, err := os.Stat(store + "/Secret.app.default.K"); err == nil {
		t.Error("a plaintext was written from a MAC-mismatched .enc")
	}
}

func TestDecryptCompareNamesAnUnreadablePlaintext(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	c, _, errOut := testEnv(t)
	id := testAgeIdentity(t, c)
	store := c.Paths.Base + "/.secrets"
	write(t, store+"/Secret.app.default.K", "v")
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	os.Chtimes(store+"/Secret.app.default.K", future, future)
	if err := os.Chmod(store+"/Secret.app.default.K", 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(store+"/Secret.app.default.K", 0o600) })
	t.Setenv("SOPS_AGE_KEY", id.String())
	if err := c.Decrypt("/nonexistent-ssh-key"); err == nil {
		t.Fatal("an unreadable plaintext was compared")
	}
	if errOut.String() != "[error] Failed to read Secret.app.default.K\n" {
		t.Errorf("stderr = %q", errOut.String())
	}
}
