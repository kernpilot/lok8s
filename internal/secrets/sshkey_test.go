package secrets

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// keyCanary is the body of every private key fixture: no message may hold
// it.
const keyCanary = "CANARY_SECRET_MATERIAL_DO_NOT_LEAK"

// privKey is "PRIVATE KEY" in two parts, so that the private-key rule of a
// secret scanner does not match a fixture of this file.
const privKey = "PRIVATE " + "KEY"

// privateKeyForms are the first lines of the private key files that a key
// path must not hold, each with the canary as its body (bash twin: the same
// list in tests/unit/secrets_test.bats).
var privateKeyForms = map[string]string{
	"openssh":      "-----BEGIN OPENSSH " + privKey + "-----\n" + keyCanary + "\n-----END OPENSSH " + privKey + "-----\n",
	"rsa pem":      "-----BEGIN RSA " + privKey + "-----\n" + keyCanary + "\n-----END RSA " + privKey + "-----\n",
	"ec pem":       "-----BEGIN EC " + privKey + "-----\n" + keyCanary + "\n-----END EC " + privKey + "-----\n",
	"pkcs8":        "-----BEGIN " + privKey + "-----\n" + keyCanary + "\n-----END " + privKey + "-----\n",
	"pkcs8 enc":    "-----BEGIN ENCRYPTED " + privKey + "-----\n" + keyCanary + "\n-----END ENCRYPTED " + privKey + "-----\n",
	"ssh2":         "---- BEGIN SSH2 ENCRYPTED " + privKey + " ----\n" + keyCanary + "\n---- END SSH2 ENCRYPTED " + privKey + " ----\n",
	"putty":        "PuTTY-User-Key-File-3: ssh-ed25519\n" + keyCanary + "\n",
	"age identity": "AGE-SECRET-KEY-1" + keyCanary + "\n",
	"age lower":    "age-secret-key-1" + strings.ToLower(keyCanary) + "\n",
	"crlf":         "-----BEGIN OPENSSH " + privKey + "-----\r\n" + keyCanary + "\r\n",
	"indented":     "  -----BEGIN OPENSSH " + privKey + "-----\n" + keyCanary + "\n",
	"comment":      "# my laptop key\n-----BEGIN OPENSSH " + privKey + "-----\n" + keyCanary + "\n",
}

// testRSAPrivateKey writes a new RSA private key in the OpenSSH form (the
// form of ssh-keygen) and returns its path.
func testRSAPrivateKey(t *testing.T, dir string) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The default key paths: LOK8S_SSH_KEY names the private key (decrypt), and
// the public key is its .pub, or the path itself when it ends in .pub (bash
// twin: the secrets::_ssh_public_key test).
func TestDefaultSSHKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, tc := range []struct {
		env, key, pub string
	}{
		{"", home + "/.ssh/id_ed25519", home + "/.ssh/id_ed25519.pub"},
		{"/k/work", "/k/work", "/k/work.pub"},
		{"/k/work.pub", "/k/work.pub", "/k/work.pub"},
	} {
		t.Setenv("LOK8S_SSH_KEY", tc.env)
		if tc.env == "" {
			os.Unsetenv("LOK8S_SSH_KEY")
		}
		if got := DefaultSSHKey(); got != tc.key {
			t.Errorf("LOK8S_SSH_KEY=%q: DefaultSSHKey %q, want %q", tc.env, got, tc.key)
		}
		if got := DefaultSSHPublicKey(); got != tc.pub {
			t.Errorf("LOK8S_SSH_KEY=%q: DefaultSSHPublicKey %q, want %q", tc.env, got, tc.pub)
		}
	}
}

// The derivation refuses a file that holds a private key before the
// library parses it, in every form; a public key file passes the check.
func TestDeriveRefusesAPrivateKeyFile(t *testing.T) {
	dir := t.TempDir()
	priv, pub := testSSHKeypair(t, dir)
	paths := map[string]string{"ed25519 ssh-keygen form": priv, "rsa ssh-keygen form": testRSAPrivateKey(t, dir)}
	for name, content := range privateKeyForms {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		write(t, path, content)
		paths[name] = path
	}
	for name, path := range paths {
		if _, err := deriveAgePublicKey(path); !errors.Is(err, errPrivateKeyFile) {
			t.Errorf("%s: want errPrivateKeyFile, got %v", name, err)
		}
	}

	// Public key files: no private key refusal.
	raw, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	commented := filepath.Join(dir, "commented.pub")
	write(t, commented, strings.TrimSpace(string(raw))+" my "+privKey+" backup\n")
	ssh2 := filepath.Join(dir, "ssh2.pub")
	write(t, ssh2, "---- BEGIN SSH2 PUBLIC KEY ----\nAAAA\n---- END SSH2 PUBLIC KEY ----\n")
	for _, p := range []string{pub, commented, testRSAPublicKey(t), ssh2} {
		if _, err := deriveAgePublicKey(p); errors.Is(err, errPrivateKeyFile) {
			t.Errorf("%s: a public key file was refused as a private key", p)
		}
	}
	if got, err := deriveAgePublicKey(commented); err != nil || !agePubkeyRe.MatchString(got) {
		t.Errorf("a public key with PRIVATE KEY in its comment: %q, %v", got, err)
	}
}

// wantPrivateKeyRefusal is the refusal of a key file that holds a private
// key (bash twin: secrets::_refuse_private_key_file).
func wantPrivateKeyRefusal(path string) string {
	fix := "pass the public key file instead: " + path + ".pub"
	if strings.HasSuffix(path, ".pub") {
		fix = "pass the public key file instead: ssh-keygen -y -f <private key> prints the public key"
	}
	return "[error] " + path + " holds a private key, not a public key: nothing from it is printed\n" +
		"[error] " + fix + "\n"
}

// Init refuses a private key file with the path and the fix, prints nothing
// from the file and writes nothing; also when the path ends in .pub.
func TestInitRefusesAPrivateKeyFile(t *testing.T) {
	for name, content := range privateKeyForms {
		for _, suffix := range []string{"", ".pub"} {
			t.Run(name+suffix, func(t *testing.T) {
				c, out, errOut := testEnv(t)
				path := filepath.Join(t.TempDir(), "key"+suffix)
				write(t, path, content)
				before := fileStates(t, c.Paths.Base)
				if err := c.Init(path); !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v", err)
				}
				if errOut.String() != wantPrivateKeyRefusal(path) || out.String() != "" {
					t.Errorf("stdout %q\nstderr %q\nwant %q", out.String(), errOut.String(), wantPrivateKeyRefusal(path))
				}
				sameStates(t, before, fileStates(t, c.Paths.Base))
			})
		}
	}
}

// add-key and remove-key refuse a key file that holds a private key the
// same way, before any write.
func TestKeyCommandsRefuseAPrivateKeyFile(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		t.Run(cmd, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			sopsYAMLWith(t, c, keyA)
			path := filepath.Join(t.TempDir(), "id_ed25519")
			write(t, path, privateKeyForms["openssh"])
			before := fileStates(t, c.Paths.Base)
			run := c.AddKey
			if cmd == "remove-key" {
				run = c.RemoveKey
			}
			if err := run(path, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v", err)
			}
			if !strings.HasPrefix(errOut.String(), wantPrivateKeyRefusal(path)) || strings.Contains(out.String()+errOut.String(), keyCanary) {
				t.Errorf("stdout %q\nstderr %q\nwant the refusal first", out.String(), errOut.String())
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// testEnv moves every key and identity lookup under the temp dir of the
// test: none resolves under the HOME the tests started with (bash twin:
// isolate_secrets_home and assert_no_real_home in tests/test_helper.bash).
func TestTestEnvIsolatesTheKeyLookups(t *testing.T) {
	c, _, _ := testEnv(t)
	if msg := realHomeLeak(c.Paths.Base, realHome); msg != "" {
		t.Error(msg)
	}
	if !strings.HasPrefix(os.Getenv("HOME"), c.Paths.Base+"/") {
		t.Errorf("HOME %q is not under the temp dir", os.Getenv("HOME"))
	}
	for _, v := range isolatedIdentityVars {
		if _, set := os.LookupEnv(v); set {
			t.Errorf("%s is set", v)
		}
	}
}

// The guard itself, with a HOME of the test as the real one: it names a
// lookup under that HOME, and the HOME itself.
func TestRealHomeLeakNamesTheLookup(t *testing.T) {
	c, _, _ := testEnv(t)
	const home = "/nonexistent-home-of-the-guard-test"
	if msg := realHomeLeak(c.Paths.Base, home); msg != "" {
		t.Fatalf("testEnv: %s", msg)
	}
	t.Setenv("LOK8S_SSH_KEY", home+"/.ssh/id_ed25519")
	if msg := realHomeLeak(c.Paths.Base, home); !strings.Contains(msg, "LOK8S_SSH_KEY") {
		t.Errorf("the guard did not name LOK8S_SSH_KEY: %q", msg)
	}
	t.Setenv("LOK8S_SSH_KEY", filepath.Join(c.Paths.Base, "key"))
	t.Setenv("HOME", home)
	if msg := realHomeLeak(c.Paths.Base, home); !strings.Contains(msg, "HOME") {
		t.Errorf("the guard did not name HOME: %q", msg)
	}
}
