package secrets

// testutil_test.go holds the fixtures the secrets tests share: a Context
// over a temp project, generated age and SSH identities, and the two
// canned age recipients.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"golang.org/x/crypto/ssh"

	"github.com/kernpilot/lok8s/internal/config"
)

// testEnv builds a throwaway project and a Context writing into buffers.
func testEnv(t *testing.T) (*Context, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	// Isolate every ambient identity source.
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("SOPS_AGE_KEY", "")
	os.Unsetenv("SOPS_AGE_KEY")
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(base, "no-keys.txt"))
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	c := &Context{
		Paths: &config.Paths{
			Base:     base,
			Bin:      filepath.Join(base, ".bin"),
			Lok8s:    filepath.Join(base, ".lok8s"),
			Clusters: filepath.Join(base, "clusters"),
		},
		Out:          out,
		ErrOut:       errOut,
		Stdin:        strings.NewReader(""),
		StdinIsTTY:   func() bool { return false },
		ReadPassword: func() (string, error) { return "", nil },
	}
	return c, out, errOut
}

// testAgeIdentity generates an age keypair and installs it: recipient into
// .sops.yaml, identity into SOPS_AGE_KEY.
func testAgeIdentity(t *testing.T, c *Context) *age.X25519Identity {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sopsYAML := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + id.Recipient().String() + "'\n"
	if err := os.WriteFile(c.sopsConfigPath(), []byte(sopsYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return id
}

// testSSHKeypair writes an ed25519 SSH keypair and returns (privPath, pubPath).
func testSSHKeypair(t *testing.T, dir string) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	privPath := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(privPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	pubPath := privPath + ".pub"
	if err := os.WriteFile(pubPath, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}
	return privPath, pubPath
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const (
	keyA = "age1zvkyg2lc7kjhpnjwqpjkwzr9qkxnwqp5xzdmqvxqjhqx0nqwqjqs3xqzq7"
	keyB = "age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqz"
)

func sopsYAMLWith(t *testing.T, c *Context, key string) {
	t.Helper()
	write(t, c.sopsConfigPath(), "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '"+key+"'\n")
}
