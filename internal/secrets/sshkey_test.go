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
	"bom":          "\uFEFF-----BEGIN OPENSSH " + privKey + "-----\n" + keyCanary + "\n",
	"pgp":          "-----BEGIN PGP " + privKey + " BLOCK-----\n" + keyCanary + "\n-----END PGP " + privKey + " BLOCK-----\n",
	"hyphen type":  "-----BEGIN X-TEST " + privKey + "-----\n" + keyCanary + "\n-----END X-TEST " + privKey + "-----\n",
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
// key (bash twin: secrets::_read_public_key).
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

// When the placeholder rule of shownKeyPath replaces a path that does not
// end in .pub (a PEM line, BEGIN or END in any case, or a control byte),
// the fix line of the refusal names ssh-keygen, not <path>.pub: no line of
// the output holds the path (bash twins: the refusal tests of a private key
// file at a PEM path and at a path that ends in a newline).
func TestInitRefusesAPrivateKeyFileAtAHiddenPath(t *testing.T) {
	const fix = "[error] pass the public key file instead: ssh-keygen -y -f <private key> prints the public key\n"
	dashes := strings.Repeat("-", 5)
	for name, tc := range map[string]struct{ dir, file, shown string }{
		"a PEM line":              {pemLine("X"), "key", "(not printed: it holds a PEM block)"},
		"an END line, lower case": {dashes + "end x" + dashes, "key", "(not printed: it holds a PEM block)"},
		"a newline at the end":    {"", "key\n", "(not printed: it holds a line break or a control character)"},
	} {
		t.Run(name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			dir := mkdir(t, filepath.Join(t.TempDir(), tc.dir))
			path := filepath.Join(dir, tc.file)
			write(t, path, privateKeyForms["openssh"])
			if err := c.Init(path); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v", err)
			}
			want := "[error] " + tc.shown + " holds a private key, not a public key: nothing from it is printed\n" + fix
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr %q\nwant %q", out.String(), errOut.String(), want)
			}
			if strings.Contains(out.String()+errOut.String(), "BEGIN") {
				t.Error("a PEM line reached the output")
			}
		})
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
	for _, v := range isolatedHomeVars {
		t.Setenv(v, "/nonexistent-"+v)
	}
	c, _, _ := testEnv(t)
	if msg := realHomeLeak(c.Paths.Base, realHome); msg != "" {
		t.Error(msg)
	}
	if !strings.HasPrefix(os.Getenv("HOME"), c.Paths.Base+"/") {
		t.Errorf("HOME %q is not under the temp dir", os.Getenv("HOME"))
	}
	for _, v := range append(append([]string{}, isolatedIdentityVars...), isolatedHomeVars...) {
		if _, set := os.LookupEnv(v); set {
			t.Errorf("%s is set", v)
		}
	}
}

// The cleanup of testEnv checks again: a test body that moves a key lookup
// under the HOME after testEnv is reported. The guard gets a HOME of the
// test, and a reporter that records instead of failing. os.Setenv, not
// t.Setenv: the restore of a t.Setenv in the body runs before the check.
func TestTestEnvChecksTheHomeAgainAtCleanup(t *testing.T) {
	const home = "/nonexistent-home-of-the-cleanup-test"
	oldHome, oldReport := guardHome, reportHomeLeak
	defer func() { guardHome, reportHomeLeak = oldHome, oldReport }()
	guardHome = home
	var got string
	reportHomeLeak = func(_ *testing.T, msg string) { got = msg }
	t.Run("a body that moves the key under the HOME", func(t *testing.T) {
		testEnv(t)
		os.Setenv("LOK8S_SSH_KEY", home+"/.ssh/id_ed25519")
	})
	if !strings.Contains(got, "LOK8S_SSH_KEY") {
		t.Errorf("the cleanup did not report the key under the HOME: %q", got)
	}
}

// remove-key reads the own identity from the .pub of LOK8S_SSH_KEY, which is
// LOK8S_SSH_KEY itself when it ends in .pub (bash twin: the own-identity
// test of a .pub LOK8S_SSH_KEY in tests/unit/secrets_remove_key_test.bats).
func TestOwnIdentityReadsAPubSuffixedKey(t *testing.T) {
	testEnv(t)
	_, pub := testSSHKeypair(t, t.TempDir())
	recipient, err := deriveAgePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOK8S_SSH_KEY", pub)
	if got := ownIdentity(recipient); got != pub {
		t.Errorf("LOK8S_SSH_KEY=<key>.pub: own identity %q, want %q", got, pub)
	}
}

// A key file that does not parse prints the lines of each command and no
// library error: the error can quote the input (bash twin: the containment
// tests of a failed ssh-to-age).
func TestKeyParseErrorsPrintNoLibraryError(t *testing.T) {
	const garbage = "ssh-ed25519 AAAA" + keyCanary + "!!\n"
	t.Run("init", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		path := filepath.Join(t.TempDir(), "id.pub")
		write(t, path, garbage)
		if err := c.Init(path); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] Failed to derive age public key from " + path + "\n[error] Only ed25519 SSH keys are supported\n"
		if errOut.String() != want || out.String() != "" {
			t.Errorf("stdout %q\nstderr %q\nwant %q", out.String(), errOut.String(), want)
		}
	})
	t.Run("add-key", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		sopsYAMLWith(t, c, keyA)
		path := filepath.Join(t.TempDir(), "id.pub")
		write(t, path, garbage)
		if err := c.AddKey(path, false, false); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] could not derive an age key from " + path + "\n[error] only ed25519 SSH keys are supported, and it must be the PUBLIC key\n"
		if !strings.HasPrefix(errOut.String(), want) || strings.Contains(out.String()+errOut.String(), keyCanary) ||
			strings.Contains(errOut.String(), "base64") {
			t.Errorf("stdout %q\nstderr %q\nwant the two lines first", out.String(), errOut.String())
		}
	})
}

// An argument of add-key with no marker of a private key passes the argv
// check, so the not-a-file and the derive lines print it through
// shownKeyPath: a key body pasted without its BEGIN line (a line break) and
// a key file at a path with a tab get the placeholder, never the value
// (bash twins: "a pasted key body with no BEGIN line is refused and not
// echoed", "a key file at a path with a tab that does not derive prints a
// placeholder").
func TestRecipientArgHidesAPasteWithNoMarker(t *testing.T) {
	const ctl = "(not printed: it holds a line break or a control character)"
	t.Run("not a file", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		sopsYAMLWith(t, c, keyA)
		body := strings.Repeat("Ab0", 8) + "\n" + keyCanary
		if err := c.AddKey(body, false, false); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] not an age key and not a readable file: " + ctl + "\n" +
			"[error] pass an age public key (age1…) or the path to an ed25519 SSH public key\n"
		if errOut.String() != want || out.String() != "" {
			t.Errorf("stdout %q\nstderr %q\nwant %q", out.String(), errOut.String(), want)
		}
	})
	t.Run("derive", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		sopsYAMLWith(t, c, keyA)
		path := filepath.Join(t.TempDir(), "id\t.pub")
		write(t, path, "ssh-ed25519 AAAA"+keyCanary+"!!\n")
		if err := c.AddKey(path, false, false); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] could not derive an age key from " + ctl + "\n" +
			"[error] only ed25519 SSH keys are supported, and it must be the PUBLIC key\n"
		if !strings.HasPrefix(errOut.String(), want) || strings.Contains(out.String()+errOut.String(), "\t") {
			t.Errorf("stdout %q\nstderr %q\nwant the two lines first, and no tab", out.String(), errOut.String())
		}
	})
}

// The key file is read once and at most 64 KiB: a bigger file, a file that
// holds a NUL byte and a file that cannot be read are refused with the path
// and the cause, and nothing from the file, by init, add-key and remove-key.
// A file over 64 KiB of NUL bytes is a file over 64 KiB, as in bash (bash
// twin: the cap, the NUL and the read refusals of secrets::_read_public_key).
func TestKeyFileReadIsCappedAndFailsClosed(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.pub")
	write(t, big, "ssh-ed25519 AAAA "+keyCanary+strings.Repeat("x", maxPublicKeyFile))
	atCap := filepath.Join(dir, "at-cap.pub")
	write(t, atCap, "ssh-ed25519 AAAA"+strings.Repeat("x", maxPublicKeyFile-len("ssh-ed25519 AAAA")))
	nul := filepath.Join(dir, "nul.pub")
	write(t, nul, "ssh-ed25519 AAAA\x00"+keyCanary+"\n")
	bigNUL := filepath.Join(dir, "big-nul.pub")
	write(t, bigNUL, strings.Repeat("\x00", maxPublicKeyFile+1))
	for path, want := range map[string]error{big: errKeyFileTooBig, bigNUL: errKeyFileTooBig, nul: errKeyFileNUL} {
		if _, err := deriveAgePublicKey(path); !errors.Is(err, want) {
			t.Errorf("%s: want %v, got %v", filepath.Base(path), want, err)
		}
	}
	if _, err := deriveAgePublicKey(atCap); errors.Is(err, errKeyFileTooBig) || errors.Is(err, errKeyFileUnreadable) {
		t.Errorf("a file of exactly 64 KiB was refused by the cap: %v", err)
	}
	cases := map[string]string{
		big:    "[error] " + big + " is bigger than 64 KiB, which no public key file is: nothing from it is printed\n",
		bigNUL: "[error] " + bigNUL + " is bigger than 64 KiB, which no public key file is: nothing from it is printed\n",
		nul:    "[error] " + nul + " holds a NUL byte, which no public key file holds: nothing from it is printed\n",
	}
	if os.Geteuid() != 0 {
		closed := filepath.Join(dir, "closed.pub")
		write(t, closed, "ssh-ed25519 AAAA "+keyCanary+"\n")
		if err := os.Chmod(closed, 0); err != nil {
			t.Fatal(err)
		}
		cases[closed] = "[error] cannot read the key file " + closed + ": nothing from it is printed\n"
	}
	for path, want := range cases {
		c, out, errOut := testEnv(t)
		if err := c.Init(path); !errors.Is(err, ErrHandled) {
			t.Fatalf("%s: want ErrHandled, got %v", path, err)
		}
		if errOut.String() != want || out.String() != "" {
			t.Errorf("init: stdout %q\nstderr %q\nwant %q", out.String(), errOut.String(), want)
		}
		for name, run := range map[string]func(*Context) func(string, bool, bool) error{
			"add-key":    func(c *Context) func(string, bool, bool) error { return c.AddKey },
			"remove-key": func(c *Context) func(string, bool, bool) error { return c.RemoveKey },
		} {
			c, _, errOut = testEnv(t)
			sopsYAMLWith(t, c, keyA)
			if err := run(c)(path, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("%s %s: want ErrHandled, got %v", name, path, err)
			}
			if !strings.HasPrefix(errOut.String(), want) || strings.Contains(errOut.String(), keyCanary) {
				t.Errorf("%s: stderr %q\nwant %q first", name, errOut.String(), want)
			}
		}
	}
}

// The own-identity check reads the .pub through the same read: a file that
// holds a valid key line and more than 64 KiB, or a NUL byte after the key
// line, is no match, although the library would parse its first line (bash
// twin: the own-identity cases of the cap and NUL test in
// tests/unit/secrets_remove_key_test.bats).
func TestOwnIdentityReadIsCappedAndFailsClosed(t *testing.T) {
	testEnv(t)
	dir := t.TempDir()
	_, pub := testSSHKeypair(t, dir)
	raw, err := os.ReadFile(pub)
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := deriveAgePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOK8S_SSH_KEY", pub)
	if got := ownIdentity(recipient); got != pub {
		t.Fatalf("the plain .pub: own identity %q, want %q", got, pub)
	}
	for name, content := range map[string]string{
		"over 64 KiB": string(raw) + "#" + strings.Repeat("x", maxPublicKeyFile) + "\n",
		"a NUL byte":  string(raw) + "\x00\n",
	} {
		write(t, pub, content)
		if got := ownIdentity(recipient); got != "" {
			t.Errorf("%s: own identity %q, want no match", name, got)
		}
	}
}

// A key path with the shape of a private key is not printed: every message
// of init, decrypt and remove-key that names the key file prints a
// placeholder (shownKeyPath). The path is a real file in a directory whose
// name holds an age identity marker. Bash twin: the shaped-path test in
// tests/unit/secrets_test.bats.
func TestKeyPathMessagesHideAPrivateKeyShape(t *testing.T) {
	const shown = "(not printed: it looks like a private key)"
	marker := "AGE-SECRET-" + "KEY-1SHAPEDPATH"
	newShaped := func(t *testing.T) string {
		return mkdir(t, filepath.Join(t.TempDir(), marker))
	}
	check := func(t *testing.T, out, errOut string) {
		t.Helper()
		if strings.Contains(out+errOut, marker) || strings.Contains(out+errOut, keyCanary) {
			t.Errorf("the path or the file reached the output:\nstdout %q\nstderr %q", out, errOut)
		}
	}

	t.Run("init refusals", func(t *testing.T) {
		dir := newShaped(t)
		cases := map[string][2]string{
			"key":     {privateKeyForms["openssh"], "[error] " + shown + " holds a private key, not a public key: nothing from it is printed\n[error] pass the public key file instead: ssh-keygen -y -f <private key> prints the public key\n"},
			"big.pub": {strings.Repeat("x", maxPublicKeyFile+1), "[error] " + shown + " is bigger than 64 KiB, which no public key file is: nothing from it is printed\n"},
			"nul.pub": {"ssh-ed25519 AAAA\x00\n", "[error] " + shown + " holds a NUL byte, which no public key file holds: nothing from it is printed\n"},
			"bad.pub": {"ssh-ed25519 AAAA" + keyCanary + "!!\n", "[error] Failed to derive age public key from " + shown + "\n[error] Only ed25519 SSH keys are supported\n"},
		}
		if os.Geteuid() != 0 {
			cases["closed.pub"] = [2]string{"ssh-ed25519 AAAA\n", "[error] cannot read the key file " + shown + ": nothing from it is printed\n"}
		}
		for name, tc := range cases {
			c, out, errOut := testEnv(t)
			path := filepath.Join(dir, name)
			write(t, path, tc[0])
			if name == "closed.pub" {
				if err := os.Chmod(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Init(path); !errors.Is(err, ErrHandled) {
				t.Fatalf("%s: want ErrHandled, got %v", name, err)
			}
			if errOut.String() != tc[1] || out.String() != "" {
				t.Errorf("%s: stdout %q\nstderr %q\nwant %q", name, out.String(), errOut.String(), tc[1])
			}
			check(t, out.String(), errOut.String())
		}
	})

	t.Run("init with an rsa key", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		raw, err := os.ReadFile(testRSAPublicKey(t))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(newShaped(t), "rsa.pub")
		write(t, path, string(raw))
		if err := c.Init(path); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		if want := "[error] no age recipient derived from " + shown + " (ed25519 SSH keys only)"; !strings.HasPrefix(errOut.String(), want) {
			t.Errorf("stderr %q\nwant it to start with %q", errOut.String(), want)
		}
		check(t, out.String(), errOut.String())
	})

	t.Run("init, and init again with another key in .sops.yaml", func(t *testing.T) {
		_, pub := testSSHKeypair(t, newShaped(t))
		c, out, errOut := testEnv(t)
		if err := c.Init(pub); err != nil {
			t.Fatalf("%v\n%s", err, errOut.String())
		}
		if want := "Derived from: " + shown + "\n"; !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q lacks %q", out.String(), want)
		}
		check(t, out.String(), errOut.String())
		c, out, errOut = testEnv(t)
		sopsYAMLWith(t, c, keyA)
		if err := c.Init(pub); err != nil {
			t.Fatalf("%v\n%s", err, errOut.String())
		}
		if want := "[warn] Add it with: lo secrets add-key " + shown + "\n"; !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q lacks %q", errOut.String(), want)
		}
		check(t, out.String(), errOut.String())
	})

	t.Run("decrypt with a key that does not derive", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		c.Domain = "a.dev"
		mkdir(t, c.Paths.Clusters+"/a.dev/secrets")
		path := filepath.Join(newShaped(t), "id_ed25519")
		write(t, path, "not a key "+keyCanary+"\n")
		if err := c.Decrypt(path); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
		}
		for _, want := range []string{
			"[error] Failed to derive age key from " + shown + "\n",
			"[error]   ssh-to-age -private-key -i " + shown + " > ~/.config/sops/age/keys.txt\n",
		} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("stderr %q lacks %q", errOut.String(), want)
			}
		}
		check(t, out.String(), errOut.String())
	})

	t.Run("remove-key of the own identity", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		c.Domain = "a.dev"
		keep, gone := newAgeIdentity(t), newAgeIdentity(t)
		k, g := keep.Recipient().String(), gone.Recipient().String()
		file := filepath.Join(newShaped(t), "keys.txt")
		t.Setenv("SOPS_AGE_KEY_FILE", file)
		write(t, file, "# created: test\n# public key: "+g+"\n"+gone.String()+"\n")
		sopsRules(t, c, k+","+g)
		a := c.Paths.Clusters + "/a.dev/secrets"
		write(t, a+"/Secret.app.default.K", "v")
		encryptStore(t, c, "a.dev", out, errOut)
		freshen(t, a+"/Secret.app.default.K")
		if err := c.RemoveKey(g, false, false); err != nil {
			t.Fatalf("%v\n%s", err, errOut.String())
		}
		if want := "[warn] the recipient is your own decrypt identity (" + shown + "): once it is removed"; !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr %q lacks %q", errOut.String(), want)
		}
		if strings.Contains(out.String()+errOut.String(), marker) {
			t.Error("the path of the keys file reached the output")
		}
	})
}

// pemLine is the first line of a PEM block with the label, and syntheticPEM
// a whole block with the label and the body of a test: no key and no
// certificate. Both are built at run time, so no block is in the source.
// Bash twin: _synthetic_pem in tests/unit/secrets_test.bats.
func pemLine(label string) string {
	return strings.Repeat("-", 5) + "BEGIN " + label + strings.Repeat("-", 5)
}

func syntheticPEM(label, body string) string {
	return pemLine(label) + "\n" + body + "\n" + strings.Repeat("-", 5) + "END " + label + strings.Repeat("-", 5)
}

// A key path that holds a PEM block which is not a private key (a
// certificate, a static key of OpenVPN, a block with any other label: the
// rule is no list of labels) is not printed either: shownKeyPath and
// QuoteArg give the PEM placeholder, never the block. A private key keeps
// its own placeholder. A plain path, and a path with BEGIN in it but
// no PEM line, print as they are. Bash twin: "secrets init and decrypt: the
// not-found line does not print a PEM block from LOK8S_SSH_KEY".
func TestShownKeyPathHidesAPEMBlock(t *testing.T) {
	const pem = "(not printed: it holds a PEM block)"
	const private = "(not printed: it looks like a private key)"
	for name, tc := range map[string]struct{ path, want string }{
		"a certificate":                  {syntheticPEM("CERTIFICATE", keyCanary), pem},
		"a static key":                   {syntheticPEM("OpenVPN Static key V1", strings.Repeat("0f", 8)+"\n"+keyCanary), pem},
		"a PEM line in a directory name": {"/keys/" + pemLine("CERTIFICATE") + "/id.pub", pem},
		"another label":                  {syntheticPEM("SOME DATA", keyCanary), pem},
		"a private key":                  {syntheticPEM("OPENSSH "+privKey, keyCanary), private},
		"a plain path":                   {"/home/user/.ssh/id_ed25519.pub", "/home/user/.ssh/id_ed25519.pub"},
		"BEGIN without the dashes":       {"/keys/BEGIN CERTIFICATE/id.pub", "/keys/BEGIN CERTIFICATE/id.pub"},
		"an END line only":               {strings.Repeat("-", 5) + "END CERTIFICATE" + strings.Repeat("-", 5), pem},
		"a PEM line in lower case":       {strings.Repeat("-", 5) + "begin certificate" + strings.Repeat("-", 5), pem},
		"a PEM line in mixed case":       {"/k/" + strings.Repeat("-", 5) + "End X" + strings.Repeat("-", 5), pem},
		"a dotted capital I":             {"/k/" + strings.Repeat("-", 5) + "BEG\u0130N X" + strings.Repeat("-", 5), "/k/" + strings.Repeat("-", 5) + "BEG\u0130N X" + strings.Repeat("-", 5)},
	} {
		if got := shownKeyPath(tc.path); got != tc.want {
			t.Errorf("%s: shownKeyPath gives %q, want %q", name, got, tc.want)
		}
		if tc.want != tc.path {
			if got := QuoteArg(tc.path); got != tc.want {
				t.Errorf("%s: QuoteArg gives %q, want %q", name, got, tc.want)
			}
		}
	}
}

// A key path that holds a control byte (0x00-0x1F, 0x7F) is not printed: a
// line break means a paste of more than one line, for example a key body
// without its BEGIN line, and an escape can move the cursor of the terminal.
// The rule reads bytes: a C1 character (U+0085) and a UTF-8 letter print as
// they are, as in the bash twin under LC_ALL=C. Bash twin:
// "secrets::_shown_path: an END line, a PEM line in any case and a control
// byte get a placeholder".
func TestShownKeyPathHidesAControlByte(t *testing.T) {
	const ctl = "(not printed: it holds a line break or a control character)"
	for name, tc := range map[string]struct{ path, want string }{
		"a body with no BEGIN line": {strings.Repeat("Ab0", 8) + "\n" + keyCanary, ctl},
		"a newline at the end":      {"/k/id\n", ctl},
		"a carriage return":         {"/k/id\r", ctl},
		"a tab":                     {"/k/a\tb", ctl},
		"an escape":                 {"/k/\x1b[31mred", ctl},
		"a DEL":                     {"/k/a\x7fb", ctl},
		"a C1 character":            {"/k/a\u0085b", "/k/a\u0085b"},
		"a UTF-8 letter":            {"/k/\u00e9", "/k/\u00e9"},
	} {
		if got := shownKeyPath(tc.path); got != tc.want {
			t.Errorf("%s: shownKeyPath gives %q, want %q", name, got, tc.want)
		}
		if tc.want == ctl {
			if got := QuoteArg(tc.path); got != ctl {
				t.Errorf("%s: QuoteArg gives %q, want %q", name, got, ctl)
			}
		}
	}
}

// init with a missing .pub whose private key is a file at a path with a PEM
// line in it: the not-found line prints the PEM placeholder, and there is no
// hint, which would hold the placeholder (publicKeyHint). Bash twin: the
// hint part of "secrets init and decrypt: the not-found line does not print
// a PEM block from LOK8S_SSH_KEY".
func TestInitMissingKeyAtAPEMPath(t *testing.T) {
	c, out, errOut := testEnv(t)
	dir := mkdir(t, filepath.Join(c.Paths.Base, pemLine("CERTIFICATE")))
	write(t, dir+"/id", keyCanary+"\n")
	if err := c.Init(dir + "/id.pub"); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	want := "[error] SSH public key not found: (not printed: it holds a PEM block)\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr %q\nwant %q", out.String(), errOut.String(), want)
	}
	if strings.Contains(out.String()+errOut.String(), "BEGIN") || strings.Contains(out.String()+errOut.String(), keyCanary) {
		t.Error("the path or the file reached the output")
	}
}

// remove-key of the own identity, from a keys file at a path with a PEM line
// in it: the warning prints the PEM placeholder, not the path. Bash twin:
// "removing the caller's own identity from a keys file at a PEM path warns
// with the placeholder" in tests/unit/secrets_remove_key_test.bats.
func TestRemoveKeyOwnIdentityAtAPEMPath(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	file := filepath.Join(mkdir(t, filepath.Join(t.TempDir(), pemLine("CERTIFICATE"))), "keys.txt")
	t.Setenv("SOPS_AGE_KEY_FILE", file)
	write(t, file, "# created: test\n# public key: "+g+"\n"+gone.String()+"\n")
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	want := "[warn] the recipient is your own decrypt identity ((not printed: it holds a PEM block)): once it is removed"
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr %q lacks %q", errOut.String(), want)
	}
	if strings.Contains(out.String()+errOut.String(), "BEGIN") {
		t.Error("the path of the keys file reached the output")
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

// The argv checks refuse the first line of a private key block in each
// form, a paste of it split into words (joined with spaces), an age
// identity and the PuTTY lines. Another PEM block (a certificate, a public
// key) and a bare "-----BEGIN" pass: lo secrets set takes a certificate as a
// value. The check of the key-path commands (LooksLikeKeyArg) refuses every
// PEM block. Bash twin: "secrets set: a certificate passes the argv checks, a
// private key is refused with the stdin line".
func TestLooksLikePrivateKeyArgvMarkers(t *testing.T) {
	for _, arg := range []string{
		"-----BEGIN OPENSSH " + privKey + "-----",
		strings.Join([]string{"-----BEGIN", "OPENSSH", "PRIVATE", "KEY-----"}, " "),
		"-----BEGIN RSA " + privKey + "-----\n" + keyCanary,
		"-----BEGIN " + privKey + "-----",
		"-----BEGIN PGP " + privKey + " BLOCK-----",
		"---- BEGIN SSH2 ENCRYPTED " + privKey + " ----",
		"AGE-SECRET-KEY-1" + keyCanary,
		"age-secret-key-1" + keyCanary,
		"PuTTY-User-Key-File-3: ssh-ed25519",
		"Private-Lines: 1",
	} {
		if !LooksLikePrivateKey(arg) || !LooksLikeKeyArg(arg) {
			t.Errorf("not refused: %q", arg)
		}
	}
	for arg, pem := range map[string]bool{
		"-----BEGIN CERTIFICATE-----\nMIIBfake\n-----END CERTIFICATE-----": true,
		"-----BEGIN PUBLIC KEY-----\nMCowfake\n-----END PUBLIC KEY-----":   true,
		"-----BEGIN":                      true,
		"-----END CERTIFICATE-----":       true,
		"-----begin certificate-----":     true,
		"-----End":                        true,
		"BEGIN CERTIFICATE":               false,
		"---- BEGIN SSH2 PUBLIC KEY ----": false,
		"/k/id_ed25519.pub":               false,
		"age1" + strings.Repeat("q", 58):  false,
	} {
		if LooksLikePrivateKey(arg) || LooksLikeKeyArg(arg) != pem {
			t.Errorf("%q: LooksLikePrivateKey %v, LooksLikeKeyArg %v", arg, LooksLikePrivateKey(arg), LooksLikeKeyArg(arg))
		}
	}
}
