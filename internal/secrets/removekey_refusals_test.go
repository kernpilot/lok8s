package secrets

// removekey_refusals_test.go: the refusals of remove-key that read more than
// the lines of .sops.yaml (the YAML, the rule each .enc matches, the files it
// cannot read, the lock), the recovery after a re-key that stopped, and the
// warnings that name what the run did not re-key. Each refusal must run
// before the first write. tests/unit/secrets_remove_key_test.bats runs the
// same cases through the bash twin.

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
)

// escaped spells a recipient so that YAML decodes it to the recipient while
// the bytes of the file do not hold it: its first letter as the escape
// backslash, u, 0061 in a double-quoted string.
func escaped(rcpt string) string {
	return `"` + string(rune(0x5c)) + "u0061" + rcpt[1:] + `"`
}

// unchanged fails the test when .sops.yaml is not the given content.
func unchanged(t *testing.T, c *Context, before string) {
	t.Helper()
	if readFile(t, c.sopsConfigPath()) != before {
		t.Error(".sops.yaml changed before the refusal")
	}
}

// The YAML read: a recipient that the line edit cannot see or cannot reach
// is refused, with a message that names where it is.
func TestRemoveKeyReadsTheYAML(t *testing.T) {
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	first := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + k + "," + g + "'\n"
	for _, tc := range []struct {
		name, yaml, want string
	}{
		{
			name: "an escaped string",
			yaml: first + "  - path_regex: 'Other\\..*'\n    age: " + escaped(g) + "\n",
			want: "lists the recipient in an escaped string or a YAML list, which remove-key does not edit — remove it there by hand",
		},
		{
			name: "an escaped item of a YAML list",
			yaml: first + "  - path_regex: 'Other\\..*'\n    age: [" + escaped(g) + "]\n",
			want: "lists the recipient in an escaped string or a YAML list, which remove-key does not edit — remove it there by hand",
		},
		{
			name: "key_groups",
			yaml: first + "  - path_regex: 'Other\\..*'\n    key_groups:\n      - age:\n          - " + escaped(g) + "\n",
			want: "lists the recipient in key_groups — remove-key does not edit key groups; remove it there by hand",
		},
		{
			name: "a merge key",
			yaml: "shared: &shared\n  age: " + escaped(g) + "\n" + first + "  - <<: *shared\n    path_regex: 'Other\\..*'\n",
			want: "still lists the recipient after the edit (through an alias or a merge key) — nothing was written; remove it there by hand",
		},
		{
			name: "an alias",
			yaml: "shared: &shared " + escaped(g) + "\n" + first + "  - path_regex: 'Other\\..*'\n    age: *shared\n",
			want: "still lists the recipient after the edit (through an alias or a merge key) — nothing was written; remove it there by hand",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			write(t, c.sopsConfigPath(), tc.yaml)
			if n := strings.Count(tc.yaml, g); n != 1 {
				t.Fatalf("fixture: the recipient must be in the bytes of the first rule only, found %d times", n)
			}

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] " + c.sopsConfigPath() + " " + tc.want + "\n[error]   " + g + "\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, tc.yaml)
		})
	}
}

// A .sops.yaml that does not parse is refused.
func TestRemoveKeyRefusesAConfigThatDoesNotParse(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	yaml := "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + keyA + "," + keyB + "'\n  - path_regex: [\n"
	write(t, c.sopsConfigPath(), yaml)

	if err := c.RemoveKey(keyB, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if want := "[error] " + c.sopsConfigPath() + " does not parse as YAML\n"; errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	unchanged(t, c, yaml)
}

// notARecipient has the length of an age recipient, in a letter that is not
// in the bech32 alphabet.
var notARecipient = "age1" + strings.Repeat("b", 58)

// Every .enc of the selected stores must match a creation rule that sops can
// encrypt to with age: a file with no rule, with a pgp rule, or with an age
// recipient that does not parse would stop the re-key half way. Each is
// refused before any write.
func TestRemoveKeyChecksTheRuleOfEveryEnc(t *testing.T) {
	const badAge = "[error] the creation rule for clusters/a.dev/secrets/Secret.other.default.K.enc has an age recipient that does not parse\n"
	for _, tc := range []struct {
		name, rule, want string
	}{
		{"no rule", "", "[error] no creation rule matches clusters/a.dev/secrets/Secret.other.default.K.enc\n"},
		{"a pgp rule", "  - path_regex: 'Secret\\.other\\..*'\n    age: 'KEEP'\n    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'\n",
			"[error] the creation rule for clusters/a.dev/secrets/Secret.other.default.K.enc has a recipient that is not age (pgp, kms, …)\n"},
		{"a pgp key group", "  - path_regex: 'Secret\\.other\\..*'\n    key_groups:\n      - age: ['KEEP']\n        pgp: ['FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000']\n",
			"[error] the creation rule for clusters/a.dev/secrets/Secret.other.default.K.enc has a recipient that is not age (pgp, kms, …)\n"},
		{"an age recipient that does not parse", "  - path_regex: 'Secret\\.other\\..*'\n    age: 'KEEP,BAD'\n", badAge},
		{"an age recipient of a key group that does not parse", "  - path_regex: 'Secret\\.other\\..*'\n    key_groups:\n      - age: ['KEEP', 'BAD']\n", badAge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			write(t, c.sopsConfigPath(), "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '"+k+","+g+"'\n")
			a := c.Paths.Clusters + "/a.dev/secrets"
			write(t, a+"/Secret.app.default.K", "v")
			write(t, a+"/Secret.other.default.K", "o")
			encryptStore(t, c, "a.dev", out, errOut)
			freshen(t, a+"/Secret.app.default.K", a+"/Secret.other.default.K")
			yaml := "creation_rules:\n  - path_regex: 'Secret\\.app\\..*'\n    age: '" + k + "," + g + "'\n" +
				strings.NewReplacer("KEEP", k, "BAD", notARecipient).Replace(tc.rule)
			write(t, c.sopsConfigPath(), yaml)

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := tc.want + "[error]   the re-key encrypts with age through the first matching rule: fix the creation rules first. Nothing was written.\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, yaml)
			if !opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
				t.Error("an .enc was re-keyed before the refusal")
			}
		})
	}
}

// The age recipients of the matched rule, read as sops reads them. The rows
// match the _rules_check case of tests/unit/secrets_remove_key_test.bats,
// which checks the same values by their shape.
func TestRuleForReadsTheAgeRecipients(t *testing.T) {
	v := newAgeIdentity(t).Recipient().String()
	_, pubPath := testSSHKeypair(t, t.TempDir())
	sshRcpt := strings.TrimSpace(readFile(t, pubPath))
	filler := "AGE-SECRET-KEY-1" + strings.Repeat("O", 58)
	const pgp = "\n    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'"
	for _, tc := range []struct {
		name, value string
		want        ruleProblem
	}{
		{"one recipient", "age: 'V'", ruleOK},
		{"a recipient that does not parse", "age: 'V,BAD'", ruleBadAge},
		{"an empty part of a string", "age: 'V, ,V'", ruleOK},
		{"an empty item of a list", "age: ['V', '']", ruleBadAge},
		{"a list of one empty item", "age: ['']", ruleOK},
		{"an item that is not a string", "age: ['V', 5]", ruleBadAge},
		{"an ssh-ed25519 public key", "age: 'SSH'", ruleOK},
		{"an ssh key of another type", "age: 'ssh-dss AAAAB3NzaC1kc3M'", ruleBadAge},
		{"a private key in the rule", "age: 'FILLER'", ruleBadAge},
		{"a key group", "key_groups:\n      - age: ['V']", ruleOK},
		{"a key group with a recipient that does not parse", "key_groups:\n      - age: ['V', 'BAD']", ruleBadAge},
		{"a key group with pgp and a recipient that does not parse", "key_groups:\n      - age: ['BAD']\n        pgp: ['FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000']", ruleNotAge},
		{"pgp and a recipient that does not parse", "age: 'BAD'" + pgp, ruleBadAge},
		{"pgp", "age: 'V'" + pgp, ruleNotAge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/.sops.yaml"
			value := strings.NewReplacer("BAD", notARecipient, "SSH", sshRcpt, "FILLER", filler, "V", v).Replace(tc.value)
			write(t, path, "creation_rules:\n  - path_regex: 'Secret\\..*'\n    "+value+"\n")
			// The error is not printed: its text can quote the filler.
			got, err := ruleFor(path, "Secret.app.default.K")
			if err != nil || got != tc.want {
				t.Errorf("got %d (an error: %t); want %d", got, err != nil, tc.want)
			}
		})
	}
}

// skipUnlessPermissionsHold skips a test that needs a file the process
// cannot read: root reads every file.
func skipUnlessPermissionsHold(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
}

// An .enc it cannot read, or a store it cannot list, may still list the
// recipient: the command fails closed before any write.
func TestRemoveKeyFailsClosedOnWhatItCannotRead(t *testing.T) {
	skipUnlessPermissionsHold(t)
	for _, tc := range []struct {
		name, path, want string
		mode             os.FileMode
	}{
		{"an .enc", "/Secret.app.default.K.enc", "clusters/a.dev/secrets/Secret.app.default.K.enc", 0o000},
		{"a store", "", "clusters/a.dev/secrets", 0o300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			sopsRules(t, c, k+","+g)
			a := c.Paths.Clusters + "/a.dev/secrets"
			write(t, a+"/Secret.app.default.K", "v")
			encryptStore(t, c, "a.dev", out, errOut)
			freshen(t, a+"/Secret.app.default.K")
			before := readFile(t, c.sopsConfigPath())
			if err := os.Chmod(a+tc.path, tc.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(a+tc.path, 0o700) })

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] cannot read " + tc.want + "\n" +
				"[error]   a file that is not read may still list the recipient — fix the permissions first; nothing was written\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, before)
		})
	}
}

// holdLock takes the lock of .sops.yaml as another run would, and makes the
// command under test give up at once.
func holdLock(t *testing.T, c *Context) {
	t.Helper()
	t.Setenv(lockWaitEnv, "0")
	f, err := os.OpenFile(c.sopsConfigLock(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

// Two runs at the same time can read the same .sops.yaml, and the second
// write puts back what the first removed. While another run holds the lock,
// both commands refuse and change nothing.
func TestKeyCommandsRefuseWhileTheLockIsHeld(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Context) error
	}{
		{"add-key", func(c *Context) error { return c.AddKey(keyB, false, false) }},
		{"remove-key", func(c *Context) error { return c.RemoveKey(keyB, false, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			sopsRules(t, c, keyA+","+keyB)
			if tc.name == "add-key" {
				sopsRules(t, c, keyA)
			}
			before := readFile(t, c.sopsConfigPath())
			holdLock(t, c)

			if err := tc.run(c); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v", err)
			}
			want := "[error] " + c.sopsConfigLock() + " is held by another lo secrets add-key or remove-key — nothing was changed; run it again when that one ends\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			unchanged(t, c, before)
		})
	}
}

// The lock is released at the end of a run: a second run does not wait.
func TestKeyCommandsReleaseTheLock(t *testing.T) {
	c, _, errOut := testEnv(t)
	c.Domain = "a.dev"
	t.Setenv(lockWaitEnv, "0")
	sopsRules(t, c, keyA)
	for range 2 {
		if err := c.AddKey(keyA, false, false); err != nil {
			t.Fatalf("%v\n%s", err, errOut.String())
		}
	}
}

// The replace keeps the mode, leaves a hard link to the old file with the
// old content, and replaces the target of a symlink, not the link.
func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	target := dir + "/target.yaml"
	write(t, target, "old\n")
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, dir+"/hard"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.yaml", dir+"/link"); err != nil {
		t.Fatal(err)
	}

	if err := replaceFile(dir+"/link", []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, target); got != "new\n" {
		t.Errorf("target: %q", got)
	}
	if got := readFile(t, dir+"/hard"); got != "old\n" {
		t.Errorf("the hard link sees %q: the file was rewritten in place", got)
	}
	if info, err := os.Lstat(dir + "/link"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by a file")
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o640 {
		t.Errorf("mode not kept: %v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 3 {
		t.Errorf("a temp file was left: %d entries", len(entries))
	}
}

// A re-key that stops after the .sops.yaml write prints the command that
// resumes it. Without --all a rerun refuses, and names the store that still
// lists the recipient; the printed command (with --all) resumes the run.
func TestRemoveKeyResumesAStoppedRekey(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	b := c.Paths.Clusters + "/b.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	write(t, b+"/Secret.app.default.K", "w")
	encryptStore(t, c, "a.dev", out, errOut)
	encryptStore(t, c, "b.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K", b+"/Secret.app.default.K")
	// The encrypt of the second store fails: its .enc cannot be written.
	if err := os.Chmod(b+"/Secret.app.default.K.enc", 0o400); err != nil {
		t.Fatal(err)
	}

	if err := c.RemoveKey(g, true, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	wantTail := "[error] Failed to encrypt Secret.app.default.K\n" +
		"[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again resumes it:\n" +
		"[error]   lo secrets remove-key " + g + " --domain a.dev --all\n"
	if !strings.HasSuffix(errOut.String(), wantTail) {
		t.Errorf("stderr:\n%q\nwant the tail:\n%q", errOut.String(), wantTail)
	}
	if strings.Contains(readFile(t, c.sopsConfigPath()), g) || opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Fatal("fixture: the first store was expected to be re-keyed")
	}
	if err := os.Chmod(b+"/Secret.app.default.K.enc", 0o600); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errOut.Reset()
	if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("rerun without --all: want ErrHandled, got %v", err)
	}
	want := "[error] not a recipient in .sops.yaml or in any .enc file of the store(s) — nothing to remove\n" +
		"[error]   " + g + "\n" +
		"[error] still listed in 1 .enc file(s) of a store this run did not re-key: clusters/b.dev/secrets\n" +
		"[error]   rerun with --all to re-key every domain store\n"
	if errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}

	out.Reset()
	errOut.Reset()
	if err := c.RemoveKey(g, true, false); err != nil {
		t.Fatalf("rerun with --all: %v\n%s", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "recipient not in .sops.yaml; 1 .enc file(s) still list it — re-keying them\n") {
		t.Errorf("stdout:\n%q", out.String())
	}
	if opens(t, b+"/Secret.app.default.K.enc", gone, "w") || !opens(t, b+"/Secret.app.default.K.enc", keep, "w") {
		t.Error("the rerun did not re-key the second store")
	}
}

// Removing the caller's own identity is allowed, with a warning: the sops
// age keys file by its public key line, and the SSH key lo decrypts with by
// the age key of its .pub. No private key is read into any output.
func TestRemoveKeyWarnsAboutTheCallersOwnIdentity(t *testing.T) {
	for _, source := range []string{"keys file", "ssh key"} {
		t.Run(source, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep := newAgeIdentity(t)
			k := keep.Recipient().String()
			var g, file string
			if source == "keys file" {
				g, file = newAgeIdentity(t).Recipient().String(), os.Getenv("SOPS_AGE_KEY_FILE")
				write(t, file, "# created: test\n# public key: "+g+"\n")
			} else {
				_, pub := testSSHKeypair(t, mkdir(t, os.Getenv("HOME")+"/.ssh"))
				derived, _, err := deriveAgePublicKey(pub)
				if err != nil {
					t.Fatal(err)
				}
				g, file = derived, pub
			}
			sopsRules(t, c, k+","+g)
			a := c.Paths.Clusters + "/a.dev/secrets"
			write(t, a+"/Secret.app.default.K", "v")
			encryptStore(t, c, "a.dev", out, errOut)
			freshen(t, a+"/Secret.app.default.K")

			if err := c.RemoveKey(g, false, false); err != nil {
				t.Fatalf("%v\n%s", err, errOut.String())
			}
			want := "[warn] the recipient is your own decrypt identity (" + file + "): once it is removed, that identity cannot decrypt the re-keyed files\n" + rotateWarn
			if errOut.String() != want {
				t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
			}
		})
	}
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testEnv unsets every age identity source sops reads from the environment.
func TestTestEnvIsolatesEveryIdentitySource(t *testing.T) {
	for _, v := range []string{"SOPS_AGE_KEY", "SOPS_AGE_KEY_CMD", "SOPS_AGE_SSH_PRIVATE_KEY_FILE"} {
		t.Setenv(v, "set by the caller")
	}
	testEnv(t)
	for _, v := range []string{"SOPS_AGE_KEY", "SOPS_AGE_KEY_CMD", "SOPS_AGE_SSH_PRIVATE_KEY_FILE"} {
		if _, set := os.LookupEnv(v); set {
			t.Errorf("%s is still set", v)
		}
	}
	if !strings.HasPrefix(os.Getenv("LOK8S_SSH_KEY"), os.Getenv("HOME")+"/") {
		t.Error("LOK8S_SSH_KEY is not in the test's HOME")
	}
}
