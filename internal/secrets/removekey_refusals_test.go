package secrets

// removekey_refusals_test.go: the refusals of remove-key that read more than
// the lines of .sops.yaml (the YAML, the rule each .enc matches, the files it
// cannot read, the lock), the recovery after a re-key that stopped, and the
// warnings that name what the run did not re-key. Each refusal must run
// before the first write. tests/unit/secrets_remove_key_test.bats runs the
// same cases through the bash twin.

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// A managed file whose name holds a newline is one path to every step: the
// rule check matches the whole name (the first rule matches the part after
// the newline alone, and is not age), and the re-key rewrites the file.
// tests/unit/secrets_remove_key_test.bats runs the same case through the bash
// twin, which hands the paths to yq as one JSON array.
func TestRemoveKeyRekeysANameWithANewline(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	write(t, c.sopsConfigPath(), "creation_rules:\n"+
		"  - path_regex: '^x$'\n    age: '"+k+"'\n    pgp: 'FFFF0000FFFF0000FFFF0000FFFF0000FFFF0000'\n"+
		"  - path_regex: 'Secret\\..*'\n    age: '"+k+","+g+"'\n")
	a := c.Paths.Clusters + "/a.dev/secrets"
	plain := a + "/Secret.app.default.K\nx"
	write(t, plain, "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, plain)
	if !opens(t, plain+".enc", gone, "v") {
		t.Fatal("fixture: the identity to remove cannot open the file before the removal")
	}

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "re-keying 1 cache file(s) across 1 store(s)…\n") {
		t.Errorf("stdout:\n%q", out.String())
	}
	if opens(t, plain+".enc", gone, "v") || !opens(t, plain+".enc", keep, "v") {
		t.Error("the .enc whose name holds a newline was not re-keyed")
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

// lockPath is the lock that a run of c takes: beside .sops.yaml, or beside
// the file that a .sops.yaml link names (sopsConfigDest).
func lockPath(t *testing.T, c *Context) string {
	t.Helper()
	dest, err := resolveConfig(c.sopsConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	return dest + ".lock"
}

// holdLock takes the lock of .sops.yaml as another run would, and makes the
// command under test give up at once.
func holdLock(t *testing.T, c *Context) {
	t.Helper()
	t.Setenv(lockWaitEnv, "0")
	f, err := os.OpenFile(lockPath(t, c), os.O_CREATE|os.O_WRONLY, 0o644)
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
			want := "[error] " + lockPath(t, c) + " is held by another lo secrets add-key or remove-key — nothing was changed; run it again when that one ends\n"
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

// The lock can sit in the directory of the file that a .sops.yaml link
// names, where another user can write. A lock path that is a symlink or a
// directory is refused before the open: both commands print the line of the
// bash twin, change nothing, and the file that the link names keeps its
// bytes.
func TestKeyCommandsRefuseALockThatIsNotARegularFile(t *testing.T) {
	for _, kind := range []string{"a symlink", "a directory"} {
		for _, cmd := range []string{"add-key", "remove-key"} {
			t.Run(kind+"/"+cmd, func(t *testing.T) {
				c, out, errOut := testEnv(t)
				c.Domain = "a.dev"
				sopsRules(t, c, keyA+","+keyB)
				if cmd == "add-key" {
					sopsRules(t, c, keyA)
				}
				lock := c.sopsConfigPath() + ".lock"
				victim := c.Paths.Base + "/elsewhere/victim"
				write(t, victim, "the bytes of another file\n")
				if kind == "a symlink" {
					if err := os.Symlink(victim, lock); err != nil {
						t.Fatal(err)
					}
				} else {
					mkdir(t, lock)
				}
				before := fileStates(t, c.Paths.Base)

				var err error
				if cmd == "add-key" {
					err = c.AddKey(keyB, false, false)
				} else {
					err = c.RemoveKey(keyB, false, false)
				}
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
				}
				want := "[error] " + lock + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was changed — remove it, then run this command again\n"
				if errOut.String() != want || out.String() != "" {
					t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
				}
				sameStates(t, before, fileStates(t, c.Paths.Base))
			})
		}
	}
}

// A link put at the lock path after the check and before the open (the
// lockChecked seam) is not followed: the open has O_NOFOLLOW, and both
// commands refuse with the line of the check. The file that the link names
// keeps its bytes, a link to a path that does not exist makes no file there,
// and .sops.yaml stays as it was (bash: the test after the open, in
// tests/unit/secrets_add_key_test.bats).
func TestKeyCommandsRefuseALockLinkPutThereAfterTheCheck(t *testing.T) {
	for _, victimExists := range []bool{true, false} {
		for _, cmd := range []string{"add-key", "remove-key"} {
			name := cmd + "/a link to a missing file"
			if victimExists {
				name = cmd + "/a link to a file"
			}
			t.Run(name, func(t *testing.T) {
				c, out, errOut := testEnv(t)
				c.Domain = "a.dev"
				sopsRules(t, c, keyA+","+keyB)
				if cmd == "add-key" {
					sopsRules(t, c, keyA)
				}
				lock := c.sopsConfigPath() + ".lock"
				victim := mkdir(t, c.Paths.Base+"/elsewhere") + "/victim"
				const victimBytes = "the bytes of another file\n"
				if victimExists {
					write(t, victim, victimBytes)
				}
				before := readFile(t, c.sopsConfigPath())
				calls := 0
				lockChecked = func(path string) {
					calls++
					if path != lock {
						t.Errorf("the check ran on %s, not on %s", path, lock)
					}
					if err := os.Symlink(victim, path); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() { lockChecked = func(string) {} })

				var err error
				if cmd == "add-key" {
					err = c.AddKey(keyB, false, false)
				} else {
					err = c.RemoveKey(keyB, false, false)
				}
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
				}
				if calls != 1 {
					t.Fatalf("the seam ran %d times, want 1", calls)
				}
				want := "[error] " + lock + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was changed — remove it, then run this command again\n"
				if errOut.String() != want || out.String() != "" {
					t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
				}
				unchanged(t, c, before)
				if victimExists {
					if got := readFile(t, victim); got != victimBytes {
						t.Errorf("the file that the link names changed: %q", got)
					}
				} else if _, err := os.Lstat(victim); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the open made a file through the link: %v", err)
				}
			})
		}
	}
}

// A FIFO put at the lock path after the check and before the open (the
// lockChecked seam). Without a reader, a write-only open that may block
// waits for one: the open has O_NONBLOCK, so it fails (ENXIO) at once. With
// a reader, the open passes and fstat refuses the FIFO (openedPathOK). Both
// commands refuse with the line of the check, and .sops.yaml stays as it
// was. The run goes in a goroutine: an open that blocks fails the test after
// five seconds (the test opens the read end then, so the run ends) instead
// of hanging the suite.
func TestKeyCommandsRefuseALockFIFOPutThereAfterTheCheck(t *testing.T) {
	for _, reader := range []bool{false, true} {
		for _, cmd := range []string{"add-key", "remove-key"} {
			name := cmd + "/a FIFO with no reader"
			if reader {
				name = cmd + "/a FIFO with a reader"
			}
			t.Run(name, func(t *testing.T) {
				c, out, errOut := testEnv(t)
				c.Domain = "a.dev"
				sopsRules(t, c, keyA+","+keyB)
				if cmd == "add-key" {
					sopsRules(t, c, keyA)
				}
				lock := c.sopsConfigPath() + ".lock"
				before := readFile(t, c.sopsConfigPath())
				calls := 0
				lockChecked = func(path string) {
					calls++
					if err := syscall.Mkfifo(path, 0o644); err != nil {
						t.Error(err)
						return
					}
					if !reader {
						return
					}
					r, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
					if err != nil {
						t.Error(err)
						return
					}
					t.Cleanup(func() { _ = r.Close() })
				}
				t.Cleanup(func() { lockChecked = func(string) {} })

				done := make(chan error, 1)
				go func() {
					if cmd == "add-key" {
						done <- c.AddKey(keyB, false, false)
					} else {
						done <- c.RemoveKey(keyB, false, false)
					}
				}()
				var err error
				select {
				case err = <-done:
				case <-time.After(5 * time.Second):
					// Release the open that waits, then fail.
					if r, rerr := os.OpenFile(lock, os.O_RDONLY|syscall.O_NONBLOCK, 0); rerr == nil {
						<-done
						_ = r.Close()
					}
					t.Fatal("the open of the lock waited on a FIFO")
				}
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
				}
				if calls != 1 {
					t.Fatalf("the seam ran %d times, want 1", calls)
				}
				want := "[error] " + lock + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was changed — remove it, then run this command again\n"
				if errOut.String() != want || out.String() != "" {
					t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
				}
				unchanged(t, c, before)
			})
		}
	}
}

// The replace keeps the mode, leaves a hard link to the old file with the
// old content, and, given the path that resolveConfig makes of a symlink,
// replaces the target of the link, not the link.
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

	dest, err := resolveConfig(dir + "/link")
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(dest, []byte("new\n")); err != nil {
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
// resumes it. Each file that it re-keyed is left with the .enc the newer file
// of its pair, and the rerun takes it all the same: the compare reads the
// values, not the times. Without --all a rerun re-keys the store again and
// names, in a warning, the store that still lists the recipient; the printed
// command (with --all) resumes the run.
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
	// The encrypt of the second store fails: its directory takes no temp
	// file. (A read-only .enc no longer stops it: the encrypt writes a temp
	// file and renames it over the .enc.)
	if err := os.Chmod(b, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(b, 0o700) })

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, true, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	wantTail := "[error] Failed to encrypt Secret.app.default.K\n" +
		"[error] the re-key did not finish. .sops.yaml no longer lists the recipient, and running the same command again, with an identity that decrypts every file, resumes it:\n" +
		"[error]   lo secrets remove-key " + g + " --domain a.dev --all\n"
	if !strings.HasSuffix(errOut.String(), wantTail) {
		t.Errorf("stderr:\n%q\nwant the tail:\n%q", errOut.String(), wantTail)
	}
	if strings.Contains(readFile(t, c.sopsConfigPath()), g) || opens(t, a+"/Secret.app.default.K.enc", gone, "v") {
		t.Fatal("fixture: the first store was expected to be re-keyed")
	}
	if !newerThan(a+"/Secret.app.default.K.enc", a+"/Secret.app.default.K") {
		t.Error("the re-key left the twin the newer file: a later encrypt sweep would encrypt it again")
	}
	if err := os.Chmod(b, 0o700); err != nil {
		t.Fatal(err)
	}

	// Without --all the key is in no rule and on no label of the store: the
	// rerun warns, re-keys the store again, ends, and names the store that
	// still lists the recipient.
	out.Reset()
	errOut.Reset()
	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("rerun without --all: %v\n%s", err, errOut.String())
	}
	want := "[warn] " + g + " is in no creation rule and in no .enc label: remove-key re-keys every managed .enc anyway, and changes no rule\n" +
		scopeWarn +
		"[warn] still listed in 1 .enc file(s) of a store this run did not re-key: clusters/b.dev/secrets\n" +
		"[warn]   rerun with --all to re-key every domain store\n" +
		rotateWarn
	if errOut.String() != want || !strings.HasSuffix(out.String(), rmSummary(0, 0, 1)) {
		t.Errorf("stdout:\n%q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}

	out.Reset()
	errOut.Reset()
	if err := c.RemoveKey(g, true, false); err != nil {
		t.Fatalf("rerun with --all: %v\n%s", err, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "recipient not in .sops.yaml: .sops.yaml is not changed\n") ||
		!strings.HasSuffix(out.String(), rmSummary(0, 1, 2)) {
		t.Errorf("stdout:\n%q", out.String())
	}
	if opens(t, b+"/Secret.app.default.K.enc", gone, "w") || !opens(t, b+"/Secret.app.default.K.enc", keep, "w") {
		t.Error("the rerun did not re-key the second store")
	}
}

// treeBytes reads every regular file under dir, by its path.
func treeBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files[path] = readFile(t, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// Before the first write, remove-key decrypts each file that it re-keys, in
// memory, with the identity of the caller, and compares it with its twin: the
// re-key encrypts the twin, so a twin with another value would replace the
// value of the .enc. A twin that differs, a file that the caller cannot
// decrypt and a twin that it cannot read are refused by their path, and
// .sops.yaml and every file of the store stay byte-equal. No value is
// printed: stdout and stderr are pinned whole. An identity that is in no
// rule is read and does not decrypt (not the error of no identity), also for
// an empty twin: an empty failed decrypt is not a twin that matches.
// tests/unit/secrets_remove_key_test.bats runs the same cases.
func TestRemoveKeyRefusesATwinThatDiffersFromItsEnc(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caller  bool // the caller holds the identity that stays
		wrong   bool // the caller holds an identity that is in no rule
		empty   bool // the twin of app is empty
		differs bool // the twin gets another value, newer than its .enc
		unread  bool // the twin gets mode 000
		want    string
	}{
		{
			name:    "a twin that differs",
			caller:  true,
			differs: true,
			want:    "[error] clusters/a.dev/secrets/Secret.app.default.K.enc differs from its decrypted twin: run lo secrets decrypt for this store, or encrypt the twin first\n",
		},
		{
			name: "a file that the caller cannot decrypt",
			want: "[error] cannot decrypt clusters/a.dev/secrets/Secret.app.default.K.enc to compare it with its twin\n" +
				"[error] cannot decrypt clusters/a.dev/secrets/Secret.other.default.K.enc to compare it with its twin\n",
		},
		{
			name:  "an identity that does not decrypt the files, with an empty twin",
			wrong: true,
			empty: true,
			want: "[error] cannot decrypt clusters/a.dev/secrets/Secret.app.default.K.enc to compare it with its twin\n" +
				"[error] cannot decrypt clusters/a.dev/secrets/Secret.other.default.K.enc to compare it with its twin\n",
		},
		{
			name:   "a twin that it cannot read",
			caller: true,
			unread: true,
			want:   "[error] cannot read clusters/a.dev/secrets/Secret.app.default.K\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.unread {
				skipUnlessPermissionsHold(t)
			}
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep, gone := newAgeIdentity(t), newAgeIdentity(t)
			k, g := keep.Recipient().String(), gone.Recipient().String()
			sopsRules(t, c, k+","+g)
			a := c.Paths.Clusters + "/a.dev/secrets"
			app, other := a+"/Secret.app.default.K", a+"/Secret.other.default.K"
			value := "v"
			if tc.empty {
				value = ""
			}
			write(t, app, value)
			write(t, other, "w")
			encryptStore(t, c, "a.dev", out, errOut)
			freshen(t, app, other)
			if tc.caller {
				callerIdentity(t, keep)
			}
			if tc.wrong {
				callerIdentity(t, newAgeIdentity(t))
			}
			// The marker of the identity: an identity that is read and does
			// not decrypt, or no identity.
			if !tc.caller {
				mark := ErrNoIdentity
				if tc.wrong {
					mark = ErrDecryptEnc
				}
				if _, err := DecryptEntry(app + ".enc"); !errors.Is(err, mark) {
					t.Fatalf("the identity: want %v, got %v", mark, err)
				}
			}
			if tc.differs {
				write(t, app, "x")
				freshen(t, app)
			}
			yaml, before := readFile(t, c.sopsConfigPath()), treeBytes(t, c.Paths.Clusters)
			if tc.unread {
				if err := os.Chmod(app, 0o000); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(app, 0o600) })
			}

			if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			if want := tc.want + twinTail; errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			if tc.unread {
				_ = os.Chmod(app, 0o600)
			}
			unchanged(t, c, yaml)
			if !maps.Equal(treeBytes(t, c.Paths.Clusters), before) {
				t.Error("a file of the store changed before the refusal")
			}
			if !opens(t, app+".enc", gone, value) || !opens(t, other+".enc", gone, "w") {
				t.Error("an .enc was re-keyed before the refusal")
			}
		})
	}
}

// twinTail is the last line of the refusal of the compare (twinsMatch).
const twinTail = "[error]   the re-key encrypts each twin, so a twin that differs would replace the value of its .enc (lo secrets decrypt keeps a twin that is newer than its .enc: to take the .enc value, remove that twin first). Nothing was written.\n"

// A twin that holds another value than its .enc is refused by the compare
// also when it is the older file of the two, and also for a file whose
// labels do not list the removed key: the re-key encrypts every managed
// twin. The times of the files do not count. .sops.yaml and every file of
// the store stay byte-equal. tests/unit/secrets_remove_key_test.bats runs
// the same case.
func TestRemoveKeyRefusesAnOlderTwinThatDiffersFromItsEnc(t *testing.T) {
	c, out, errOut := testEnv(t)
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	app, other := labelStore(t, c, k, g, out, errOut)
	write(t, other, "x")
	info, err := os.Stat(other + ".enc")
	if err != nil {
		t.Fatal(err)
	}
	ts := info.ModTime().Add(-time.Hour)
	if err := os.Chtimes(other, ts, ts); err != nil {
		t.Fatal(err)
	}
	if !newerThan(other+".enc", other) {
		t.Fatal("fixture: the twin is not the older file")
	}
	yaml, before := readFile(t, c.sopsConfigPath()), treeBytes(t, c.Paths.Clusters)

	callerIdentity(t, keep)
	if err := c.RemoveKey(g, false, false); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	want := "[error] clusters/a.dev/secrets/Secret.other.default.K.enc differs from its decrypted twin: run lo secrets decrypt for this store, or encrypt the twin first\n" + twinTail
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	unchanged(t, c, yaml)
	if !maps.Equal(treeBytes(t, c.Paths.Clusters), before) {
		t.Error("a file of the store changed before the refusal")
	}
	if !opens(t, app+".enc", gone, "v") {
		t.Error("an .enc was re-keyed before the refusal")
	}
}

// Removing the caller's own identity is allowed, with a warning: the sops
// age keys file by its public key line, and the SSH key lo decrypts with by
// the age key of its .pub. The second line names the two sources the check
// reads and those it does not. No private key is read into any output.
func TestRemoveKeyWarnsAboutTheCallersOwnIdentity(t *testing.T) {
	for _, source := range []string{"keys file", "ssh key"} {
		t.Run(source, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			keep := newAgeIdentity(t)
			k := keep.Recipient().String()
			var g, file string
			if source == "keys file" {
				// The keys file of the identity to remove: before the
				// first write it still decrypts the store.
				gone := newAgeIdentity(t)
				g, file = gone.Recipient().String(), os.Getenv("SOPS_AGE_KEY_FILE")
				write(t, file, "# created: test\n# public key: "+g+"\n"+gone.String()+"\n")
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
			want := "[warn] the recipient is your own decrypt identity (" + file + "): once it is removed, that identity cannot decrypt the re-keyed files, and a resume of a stopped run needs another identity that decrypts every file\n" +
				scopeWarn + rotateWarn
			if errOut.String() != want {
				t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
			}
		})
	}
}

// A removal whose recipient matches neither source of the own-identity check
// prints the scope line of the check all the same: no match warning does not
// mean that the key is not the caller's. The keys file (of the identity that
// stays) and the SSH .pub hold other keys.
func TestRemoveKeyPrintsTheIdentityScopeWithoutAMatch(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep := newAgeIdentity(t)
	k, g := keep.Recipient().String(), newAgeIdentity(t).Recipient().String()
	write(t, os.Getenv("SOPS_AGE_KEY_FILE"), "# created: test\n# public key: "+k+"\n"+keep.String()+"\n")
	testSSHKeypair(t, mkdir(t, os.Getenv("HOME")+"/.ssh"))
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")

	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if errOut.String() != scopeWarn+rotateWarn {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), scopeWarn+rotateWarn)
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
