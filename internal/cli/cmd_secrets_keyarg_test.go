package cli

// cmd_secrets_keyarg_test.go: a private key pasted into `lo secrets add-key`
// or `remove-key` reaches no output, through the real command tree. Before
// RunE runs, cobra's flag parser and the argument checks print what they
// reject; the guard of main (GuardPrivateKeyArgv) runs before both.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kernpilot/lok8s/internal/config"
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
// argument is a value, when the argv has the shape of a private key
// (secrets.RefusePrivateKeyValue; bash: secrets::_no_private_value).
const privateKeyValueRefusal = "[error] an argument looks like a private key: it is not printed\n" +
	"[error] pass the value on stdin, not as an argument: lo secrets set --name <name> <key> < <file>\n"

// pemPathRefusal is what every argv check of lo secrets other than set
// prints for a word that holds "-----BEGIN" when the refused words have no
// private key: the first line names the PEM block (secrets.RefusePrivateKey;
// bash: secrets::_no_private_key).
const pemPathRefusal = "[error] a value holds a PEM line (-----BEGIN or -----END): it is not printed\n" +
	"[error] pass the path of the key file instead. add-key and remove-key also take an age public key (age1…)\n"

// pemValueRefusal is what the guard prints for lo secrets set when the argv
// holds "-----BEGIN" and has no private key (a certificate, a public key, a
// static key of OpenVPN): the first line names the PEM block.
const pemValueRefusal = "[error] a value holds a PEM line (-----BEGIN or -----END): it is not printed\n" +
	"[error] pass the value on stdin, not as an argument: lo secrets set --name <name> <key> < <file>\n"

// controlByteRefusal is what the guard prints for an argv of lo secrets that
// holds a control byte and no key (secrets.RefuseControlByte; bash:
// secrets::_no_control_byte).
const controlByteRefusal = "[error] an argument holds a line break or a control character: it is not printed\n" +
	"[error] in lo secrets, only the value of set can hold one: check the command, and pass the path of a key file, not the key\n"

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
// refuse, and print the two refusal lines only. The flag-error func sees
// the first word of a paste split into words only ("-----BEGIN"), so its
// first line names the PEM block. The guard of main sees the whole argv and
// runs first in the binary: its first line names the private key, as in
// the bash tree.
func TestSecretsKeyCommandsDoNotEchoAPrivateKey(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		for _, argv := range privateKeyArgvs(cmd) {
			paths := secretsProject(t)
			out, errOut, err := runSecrets(t, paths, argv...)
			if err == nil {
				t.Errorf("%s: no error", strings.Join(argv[:2], " "))
			}
			want := privateKeyRefusal
			if argv[2] == "-----BEGIN" {
				want = pemPathRefusal
			}
			if out != "" || errOut != want {
				t.Errorf("argv %d words (%s): stdout %d bytes, stderr %q", len(argv), cmd, len(out), errOut)
			}
			// The refusal names the marker "-----BEGIN" itself: look for an
			// echo in what is left of the output without it.
			rest := strings.Replace(out+errOut, want, "", 1)
			for _, a := range argv[1:] {
				if strings.Contains(a, "BEGIN") || strings.Contains(strings.ToUpper(a), "AGE-SECRET-KEY-") {
					if strings.Contains(rest, a) {
						t.Errorf("%s: an argument was echoed", cmd)
					}
				}
			}
			var guard bytes.Buffer
			if !GuardPrivateKeyArgv(NewRoot(paths), argv, &guard) || guard.String() != privateKeyRefusal {
				t.Errorf("argv %d words (%s): the guard of main printed %q", len(argv), cmd, guard.String())
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

// A key body pasted without its BEGIN line has no marker of a key, and the
// parsers print it: as an extra argument, as a command that they do not
// know. The guard refuses an argv of lo secrets that holds a control byte
// (a line break first of all), before cobra parses: as a positional, a flag
// value of the command, of the group and of the root, a command, the first
// word of the argv, and with a command that cobra cannot resolve. A key or
// a PEM line in the argv keeps its own refusal. The value of set is any
// text: set passes, and a key in it is still refused. A command outside lo
// secrets is left to cobra. The rule reads bytes: a C1 character (U+0085)
// is no control byte. The same in a project that routes lo secrets to the
// bash tree. Bash twin: "secrets: an argument with a control byte is
// refused, for set it is not" in tests/unit/secrets_test.bats.
func TestGuardPrivateKeyArgvRefusesAControlByte(t *testing.T) {
	const canary = "CANARY_SECRET_MATERIAL_DO_NOT_LEAK"
	roots := map[string]*config.Paths{
		"go":             secretsProject(t),
		"default bash":   routedProject(t, "    default: bash\n", true),
		"secrets routed": routedProject(t, "    bash:\n      commands: [secrets]\n", true),
	}
	values := []string{
		strings.Repeat("Ab0", 8) + "\n" + canary,
		canary + "\r",
		canary + "\tx",
		"\x1b[31m" + canary,
		canary + "\x7f",
		"\x01" + canary,
		"\x1f" + canary,
	}
	for name, paths := range roots {
		guard := func(argv []string) (bool, string) {
			var errOut bytes.Buffer
			refused := GuardPrivateKeyArgv(NewRoot(paths), argv, &errOut)
			return refused, errOut.String()
		}
		for _, value := range values {
			for i, argv := range [][]string{
				{"secrets", "add-key", value},
				{"secrets", "remove-key", value},
				{"secrets", "add-key", fixtureRcpt, value},
				{"secrets", "init", "--ssh-key", value},
				{"secrets", "decrypt", "--ssh-key=" + value},
				{"secrets", value},
				{"secrets", value, "list"},
				{"secrets", "list", value},
				{"secrets", "print", value},
				{"secrets", "--domain", value, "list"},
				{"secrets", "--domain", "set", "list", value},
				{"secrets", "st", "KEY", value},
				{"s", "l", value},
				{"--domain", value, "secrets", "list"},
				{value},
				{value, "secrets", "list"},
				{value, "up"},
				{"no-such-command", value},
				// Find does not reach set (fail closed).
				{"--no-such-flag", "secrets", "set", "KEY", value},
				{"--", "secrets", "set", "KEY", value},
			} {
				if refused, errOut := guard(argv); !refused || errOut != controlByteRefusal {
					t.Errorf("%s, argv %d with %q: refused %v, printed %q", name, i, value, refused, errOut)
				}
			}
			// An unknown flag before set: Find stops at the shim of a project
			// that routes secrets to the bash tree, and the words of the argv
			// name set there, so it is left to that parser. Else Find does not
			// reach set (fail closed).
			want := name == "go"
			refused, errOut := guard([]string{"secrets", "-z", "set", "list", value})
			if refused != want || (errOut == controlByteRefusal) != want {
				t.Errorf("%s, -z before set with %q: refused %v, printed %q, want refused %v", name, value, refused, errOut, want)
			}
			for i, argv := range [][]string{
				{"secrets", "set", "-n", "app", "KEY", value},
				{"s", "s", "-n", "app", "KEY", value},
				{"--domain", "alpha.dev", "secrets", "set", "-n", "app", "KEY", value},
				{"secrets", "--domain", "alpha.dev", "set", "KEY", value},
				{"secrets", "--no-such-flag", "set", "KEY", value},
				{"version", value},
				{"--domain", "secrets", "lint", value},
				{"--domain", value, "version"},
			} {
				if refused, errOut := guard(argv); refused || errOut != "" {
					t.Errorf("%s, argv %d with %q: refused %v, printed %q, want it left to cobra", name, i, value, refused, errOut)
				}
			}
			// A key or a PEM line beside the control byte: its own refusal.
			for i, tc := range []struct {
				argv []string
				want string
			}{
				{[]string{"secrets", "add-key", fillerIdentity, value}, privateKeyRefusal},
				{[]string{"secrets", "add-key", "-----BEGIN CERTIFICATE-----", value}, pemPathRefusal},
				{[]string{"secrets", "set", "-n", "app", "KEY", fillerIdentity + value}, privateKeyValueRefusal},
				{[]string{"secrets", "set", "-n", "app", "KEY", "-----END CERTIFICATE-----" + value}, pemValueRefusal},
			} {
				if refused, errOut := guard(tc.argv); !refused || errOut != tc.want {
					t.Errorf("%s, key argv %d with %q: refused %v, printed %q", name, i, value, refused, errOut)
				}
			}
		}
		for _, argv := range [][]string{
			{"secrets", "add-key", "/k/a b.pub"},
			{"secrets", "list", "a\u0085b"},
			{"secrets", "add-key", "/k/schl\u00fcssel.pub"},
			{"secrets", "list", ""},
		} {
			if refused, errOut := guard(argv); refused || errOut != "" {
				t.Errorf("%s, %q: refused %v, printed %q", name, argv, refused, errOut)
			}
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
