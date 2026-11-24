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

// isolatedIdentityVars are the age identity sources sops reads from the
// environment besides SOPS_AGE_KEY_FILE: testEnv unsets each one, so that an
// identity of the caller cannot open a file a test expects to stay closed.
var isolatedIdentityVars = []string{"SOPS_AGE_KEY", "SOPS_AGE_KEY_CMD", "SOPS_AGE_SSH_PRIVATE_KEY_FILE"}

// isolatedHomeVars name a git or GnuPG config outside HOME: testEnv unsets
// each one, so that git and gpg read the HOME of the test (bash twin:
// isolate_secrets_home).
var isolatedHomeVars = []string{"GNUPGHOME", "GIT_CONFIG_GLOBAL"}

// realHome is the HOME that the test binary started with. No key or
// identity lookup of a test may resolve under it (realHomeLeak).
var realHome = os.Getenv("HOME")

// guardHome is the HOME that testEnv guards, and reportHomeLeak reports a
// lookup under it at the cleanup of a test. A test of the guard replaces
// both.
var (
	guardHome      = realHome
	reportHomeLeak = func(t *testing.T, msg string) { t.Error(msg + " (at the cleanup of the test)") }
)

// realHomeLeak names the first key or identity lookup that resolves to home
// or under it, and "" when none does. A path under base (the temp dir of the
// test) is no leak, also when the temp dirs are under home. Without a HOME
// (empty or /) there is nothing to guard. Bash twin: assert_no_real_home.
func realHomeLeak(base, home string) string {
	if home == "" || home == "/" {
		return ""
	}
	for _, l := range []struct{ name, path string }{
		{"HOME", os.Getenv("HOME")},
		{"XDG_CONFIG_HOME", os.Getenv("XDG_CONFIG_HOME")},
		{"SOPS_AGE_KEY_FILE", os.Getenv("SOPS_AGE_KEY_FILE")},
		{"GNUPGHOME", os.Getenv("GNUPGHOME")},
		{"GIT_CONFIG_GLOBAL", os.Getenv("GIT_CONFIG_GLOBAL")},
		{"LOK8S_SSH_KEY (DefaultSSHKey)", DefaultSSHKey()},
		{"LOK8S_SSH_KEY (DefaultSSHPublicKey)", DefaultSSHPublicKey()},
	} {
		if l.path == base || strings.HasPrefix(l.path, base+"/") {
			continue
		}
		if l.path == home || strings.HasPrefix(l.path, home+"/") {
			return l.name + " resolves under the real HOME: run the test with testEnv"
		}
	}
	return ""
}

// testEnv builds a throwaway project and a Context writing into buffers.
// It ends with the sentinel realHomeLeak: a test whose setup leaves a key
// lookup under the real HOME fails before it reads a key. The cleanup of the
// test runs it again (reportHomeLeak), because a test body can set a
// variable after testEnv.
func testEnv(t *testing.T) (*Context, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	base := t.TempDir()
	// Isolate every ambient identity source.
	t.Setenv("HOME", filepath.Join(base, "home"))
	// sops also reads <user config dir>/sops/age/keys.txt, and on Linux that
	// dir is $XDG_CONFIG_HOME when it is set, not $HOME/.config.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(base, "home", ".config"))
	for _, v := range append(append([]string{}, isolatedIdentityVars...), isolatedHomeVars...) {
		t.Setenv(v, "")
		os.Unsetenv(v)
	}
	t.Setenv("SOPS_AGE_KEY_FILE", filepath.Join(base, "no-keys.txt"))
	// remove-key reads the .pub of the SSH key lo decrypts with.
	t.Setenv("LOK8S_SSH_KEY", filepath.Join(base, "home", ".ssh", "id_ed25519"))
	if msg := realHomeLeak(base, guardHome); msg != "" {
		t.Fatal(msg)
	}
	// Registered after the t.Setenv calls above, so it runs before they are
	// undone: it sees the variables as the test left them.
	t.Cleanup(func() {
		if msg := realHomeLeak(base, guardHome); msg != "" {
			reportHomeLeak(t, msg)
		}
	})
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

// encEnvelope is the shape of a binary-mode .enc that lists rcpts in its sops
// metadata, with no real ciphertext: enough for the recipient scans, not for
// a decrypt.
func encEnvelope(rcpts ...string) string {
	entries := make([]string, 0, len(rcpts))
	for _, r := range rcpts {
		entries = append(entries, `{"recipient":"`+r+`","enc":"x"}`)
	}
	return `{"data":"ENC[x]","sops":{"age":[` + strings.Join(entries, ",") + `]}}` + "\n"
}
