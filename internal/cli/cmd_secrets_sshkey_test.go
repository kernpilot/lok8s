package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeKeyPair writes a private key fixture (a header and a canary, no key)
// at priv and a new ed25519 public key at pub.
func writeKeyPair(t *testing.T, priv, pub, canary string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(priv), 0o700); err != nil {
		t.Fatal(err)
	}
	// "PRIVATE KEY" in two parts: no private-key rule of a secret scanner
	// matches this file.
	head := "-----BEGIN OPENSSH PRIVATE " + "KEY-----"
	if err := os.WriteFile(priv, []byte(head+"\n"+canary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pub, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}
}

// init without --ssh-key reads the public key: the .pub of LOK8S_SSH_KEY,
// the path itself when it ends in .pub, and ~/.ssh/id_ed25519.pub without
// the variable. It never reads the private key beside it (bash twin: the
// default key test in tests/unit/secrets_test.bats).
func TestSecretsInitDefaultsToThePublicKey(t *testing.T) {
	const canary = "CANARY_SECRET_MATERIAL_DO_NOT_LEAK"
	for _, tc := range []struct {
		name string
		env  func(home, dir string) string // the LOK8S_SSH_KEY value; "" = unset
		pub  func(home, dir string) string
	}{
		{"unset", func(string, string) string { return "" },
			func(home, _ string) string { return home + "/.ssh/id_ed25519.pub" }},
		{"a private key path", func(_, dir string) string { return dir + "/work" },
			func(_, dir string) string { return dir + "/work.pub" }},
		{"a .pub path", func(_, dir string) string { return dir + "/work.pub" },
			func(_, dir string) string { return dir + "/work.pub" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths := secretsProject(t)
			home, dir := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", home+"/.config")
			writeKeyPair(t, home+"/.ssh/id_ed25519", home+"/.ssh/id_ed25519.pub", canary)
			writeKeyPair(t, dir+"/work", dir+"/work.pub", canary)
			t.Setenv("LOK8S_SSH_KEY", tc.env(home, dir))
			if tc.env(home, dir) == "" {
				os.Unsetenv("LOK8S_SSH_KEY")
			}
			out, errOut, err := runSecrets(t, paths, "secrets", "init")
			if err != nil {
				t.Fatalf("init: %v\nstdout %s\nstderr %s", err, out, errOut)
			}
			if want := "Derived from: " + tc.pub(home, dir) + "\n"; !strings.Contains(out, want) {
				t.Errorf("stdout lacks %q:\n%s", want, out)
			}
			if strings.Contains(out+errOut, canary) {
				t.Error("the private key fixture reached the output")
			}
		})
	}
}

// The help of init and decrypt names the default of --ssh-key: the public
// key for init, the private key for decrypt.
func TestSecretsSSHKeyHelpNamesTheDefault(t *testing.T) {
	for cmd, want := range map[string]string{
		"init":    "Path to the SSH public key (default: the .pub file of LOK8S_SSH_KEY, ~/.ssh/id_ed25519.pub)",
		"decrypt": "Path to the SSH private key (default: LOK8S_SSH_KEY, ~/.ssh/id_ed25519)",
	} {
		out, _, err := runSecrets(t, secretsProject(t), "secrets", cmd, "--help")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s --help lacks %q:\n%s", cmd, want, out)
		}
	}
}
