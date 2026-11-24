package cli

// cmd_secrets_keyarg_test.go: a private key pasted into `lo secrets add-key`
// or `remove-key` reaches no output, through the real command tree. Before
// RunE runs, cobra's flag parser and the argument checks print what they
// reject; the guard of main (GuardPrivateKeyArgv) runs before both.

import (
	"bytes"
	"strings"
	"testing"
)

// fillerIdentity has the shape of an age identity, made of a filler
// character that is not in the bech32 alphabet: not a key.
var fillerIdentity = "AGE-SECRET-KEY-1" + strings.Repeat("O", 58)

// pemLine is the first line of an OpenSSH private key block, no key. It is
// built from two parts, so that the private-key rule of a secret scanner does
// not match from it to the PEM words below.
const pemLine = "-----BEGIN OPENSSH PRIVATE " + "KEY-----"

// fixtureRcpt has the shape of an age public key.
var fixtureRcpt = "age1" + strings.Repeat("q", 57) + "z"

// privateKeyRefusal is what every argv check of lo secrets prints
// (secrets.RefusePrivateKey; bash: secrets::_no_private_key).
const privateKeyRefusal = "[error] an argument looks like a private key: it is not printed\n" +
	"[error] pass the path of the key file instead. add-key and remove-key also take an age public key (age1…)\n"

// privateKeyValueRefusal is what the guard prints for lo secrets set, whose
// argument is a value (secrets.RefusePrivateKeyValue; bash:
// secrets::_no_private_value).
const privateKeyValueRefusal = "[error] an argument looks like a private key: it is not printed\n" +
	"[error] pass the value on stdin, not as an argument: lo secrets set --name <name> <key> < <file>\n"

// privateKeyArgvs are the argvs of one key command with a private key shape
// in each place a parser could print it: a flag-like PEM line, the PEM block
// split into words, an extra argument, a bool flag value, and a --domain
// value before and after the command.
func privateKeyArgvs(cmd string) [][]string {
	return [][]string{
		{"secrets", cmd, pemLine},
		{"secrets", cmd, "-----BEGIN", "OPENSSH", "PRIVATE", "KEY-----"},
		{"secrets", cmd, fixtureRcpt, fillerIdentity},
		{"secrets", cmd, "--all=" + fillerIdentity, fixtureRcpt},
		{"secrets", "--domain", fillerIdentity, cmd, fixtureRcpt},
		{"secrets", cmd, fixtureRcpt, "--domain", strings.ToLower(fillerIdentity)},
	}
}

// Through cobra Execute (no main): the flag-error func and the Args check
// refuse, and print the two refusal lines only.
func TestSecretsKeyCommandsDoNotEchoAPrivateKey(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		for _, argv := range privateKeyArgvs(cmd) {
			paths := secretsProject(t)
			out, errOut, err := runSecrets(t, paths, argv...)
			if err == nil {
				t.Errorf("%s: no error", strings.Join(argv[:2], " "))
			}
			if out != "" || errOut != privateKeyRefusal {
				t.Errorf("argv %d words (%s): stdout %d bytes, stderr %q", len(argv), cmd, len(out), errOut)
			}
			for _, a := range argv[1:] {
				if strings.Contains(a, "BEGIN") || strings.Contains(strings.ToUpper(a), "AGE-SECRET-KEY-") {
					if strings.Contains(out+errOut, a) {
						t.Errorf("%s: an argument was echoed", cmd)
					}
				}
			}
		}
	}
}

// The raw argv guard of main: it refuses before cobra parses. The argvs of
// add-key and remove-key here; the rest of lo secrets in
// TestGuardPrivateKeyArgvCoversTheSecretsTree.
func TestGuardPrivateKeyArgv(t *testing.T) {
	paths := secretsProject(t)
	for _, cmd := range []string{"add-key", "remove-key"} {
		for _, argv := range privateKeyArgvs(cmd) {
			var errOut bytes.Buffer
			if !GuardPrivateKeyArgv(NewRoot(paths), argv, &errOut) {
				t.Errorf("%s (%d words): not refused", cmd, len(argv))
			}
			if errOut.String() != privateKeyRefusal {
				t.Errorf("%s: stderr %q", cmd, errOut.String())
			}
		}
	}
	for _, argv := range [][]string{
		{"secrets", "remove-key", fixtureRcpt},
		{"secrets", "add-key", fixtureRcpt, "--all"},
		{"version"},
	} {
		var errOut bytes.Buffer
		if GuardPrivateKeyArgv(NewRoot(paths), argv, &errOut) || errOut.Len() != 0 {
			t.Errorf("%q: refused, or printed %q", argv[:2], errOut.String())
		}
	}
}

// The help of remove-key names every refusal, the lock and the revocation
// caveat. The compare of each twin with its .enc refuses a twin that holds
// another value; the times of the files are no refusal.
func TestSecretsRemoveKeyHelpNamesTheRefusals(t *testing.T) {
	out, _, err := runSecrets(t, secretsProject(t), "secrets", "remove-key", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "newer than its plaintext") {
		t.Error("the help names a refusal by the times of the files")
	}
	for _, want := range []string{
		"looks like a private key (it is not printed)",
		"outside a single-quoted age: list (an unquoted list",
		"key_groups, an alias or a merge key",
		"the last recipient of a rule",
		"holds other bytes than its decrypted twin",
		"matches no creation rule",
		"without --skip-orphans",
		"flat store is not re-keyed",
		".sops.yaml.lock",
		"LO_SECRETS_LOCK_WAIT",
		"run the same command again to resume it",
		"A removal does not revoke what the key already read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}
