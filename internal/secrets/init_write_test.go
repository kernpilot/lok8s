package secrets

// init_write_test.go: the .sops.yaml write of Init (bash: the init tests of
// tests/unit/secrets_test.bats). Init refuses a key that derives no age
// recipient before any other step, checks the path, takes the lock of
// .sops.yaml, checks the path again under the lock, makes the store and its
// .gitignore, and writes the file through a temp file and a rename.

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testRSAPublicKey writes the public half of a new RSA SSH key: a
// well-formed ssh- key that derives no age recipient (ssh-to-age skips it
// and exits 0). An ECDSA key does not get there: ssh-to-age fails to parse
// it, which is the other refusal of init.
func testRSAPublicKey(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_rsa.pub")
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// initConfigTemps lists the temp files of the .sops.yaml write in dir.
func initConfigTemps(t *testing.T, dir string) []string {
	t.Helper()
	left, err := filepath.Glob(dir + "/..sops.yaml.tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	return left
}

// wantInitConfig is the .sops.yaml that Init writes for the key at pub.
func wantInitConfig(t *testing.T, pub string) string {
	t.Helper()
	agePubkey, err := deriveAgePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return "# SOPS encryption config for lok8s secret cache files.\n" +
		"# Recipients are age public keys derived from SSH keys via ssh-to-age.\n" +
		"# Add team members with: lo secrets add-key <ssh-pubkey-path|age1…>\n" +
		"creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + agePubkey + "'\n"
}

// An RSA key derives no recipient. Init prints the refusal only (no skip
// note: bash keeps the stderr of ssh-to-age from the terminal), and writes
// nothing: no .sops.yaml, no lock file and no store. A rerun (a .sops.yaml
// is there) refuses the key the same way: the empty recipient is in every
// config, so a rerun took it for a key that is configured (bash: the same
// line and status 1).
func TestInitRefusesAKeyThatDerivesNoRecipient(t *testing.T) {
	for _, rerun := range []bool{false, true} {
		t.Run(fmt.Sprintf("rerun=%v", rerun), func(t *testing.T) {
			c, out, errOut := testEnv(t)
			pub := testRSAPublicKey(t)
			if _, err := deriveAgePublicKey(pub); !errors.Is(err, errUnsupportedKey) {
				t.Fatalf("fixture: want a key that derives nothing, got %v", err)
			}
			if rerun {
				_, ed := testSSHKeypair(t, t.TempDir())
				write(t, c.sopsConfigPath(), wantInitConfig(t, ed))
			}
			before := fileStates(t, c.Paths.Base)

			if err := c.Init(pub); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\nstdout %q", err, out.String())
			}
			want := "[error] no age recipient derived from " + pub +
				" (ed25519 SSH keys only) — nothing written to " + c.sopsConfigPath() + "\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// While another run holds the lock of .sops.yaml, init refuses with the line
// of add-key and remove-key and writes nothing. After the release, init
// writes the config.
func TestInitRefusesWhileTheLockIsHeld(t *testing.T) {
	c, out, errOut := testEnv(t)
	_, pub := testSSHKeypair(t, t.TempDir())
	t.Setenv(lockWaitEnv, "0")
	lock := c.sopsConfigPath() + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	before := fileStates(t, c.Paths.Base)

	if err := c.Init(pub); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	want := "[error] " + lock + " is held by another lo secrets init, add-key or remove-key: nothing was changed. When that run ends, run this command again\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, c.Paths.Base))

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if err := c.Init(pub); err != nil {
		t.Fatalf("after the release: %v\n%s", err, errOut.String())
	}
	if got := readFile(t, c.sopsConfigPath()); got != wantInitConfig(t, pub) {
		t.Errorf("after the release: .sops.yaml %q", got)
	}
}

// Init checks the path without following a link (initConfigFree), before
// the lock and again under it. A file that another run wrote while this run
// waited for the lock keeps its bytes. A link that names no file and a
// directory are refused before the lock: they are not replaced, the file
// that the link names is not made, and no lock file is made. Each case
// prints its refusal, and no temp file stays (bash: the same lines and
// status 1).
func TestInitChecksThePathBeforeAndUnderTheLock(t *testing.T) {
	notRegular := " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was written. Remove it, then run lo secrets init again\n"

	t.Run("another run wrote it", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		_, pub := testSSHKeypair(t, t.TempDir())
		cfg := c.sopsConfigPath()
		const other = "# written by another run\n"
		orig := lockOpened
		t.Cleanup(func() { lockOpened = orig })
		calls := 0
		lockOpened = func(string) {
			calls++
			write(t, cfg, other)
		}

		if err := c.Init(pub); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		if calls != 1 {
			t.Fatalf("the seam ran %d times, want 1", calls)
		}
		want := "[error] another run wrote " + cfg + " after this run checked it: this run wrote nothing. Run lo secrets init again\n"
		if errOut.String() != want || out.String() != "" {
			t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
		}
		if got := readFile(t, cfg); got != other {
			t.Errorf("the file of the other run changed: %q", got)
		}
		if left := initConfigTemps(t, c.Paths.Base); len(left) != 0 {
			t.Errorf("temp files stay: %q", left)
		}
	})

	for _, kind := range []string{"a link that names no file", "a directory"} {
		t.Run(kind, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			_, pub := testSSHKeypair(t, t.TempDir())
			cfg := c.sopsConfigPath()
			if kind == "a directory" {
				mkdir(t, cfg)
			} else {
				// The directory of the target exists: a write through the
				// link would make the file.
				mkdir(t, c.Paths.Base+"/elsewhere")
				if err := os.Symlink(c.Paths.Base+"/elsewhere/sops.yaml", cfg); err != nil {
					t.Fatal(err)
				}
			}
			before := fileStates(t, c.Paths.Base)

			if err := c.Init(pub); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v", err)
			}
			want := "[error] " + cfg + notRegular
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			if _, err := os.Lstat(cfg + ".lock"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the refusal made the lock file: %v", err)
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// A .sops.yaml write that stops part way (the writeInitConfig seam writes
// half of the content, then fails, as on a full disk): init prints one line
// that names the file, and leaves no part of a config and no temp file. The
// next init then runs as a first run, not as a rerun: it writes the whole
// config, mode 0644 (bash: tests/unit/secrets_test.bats).
func TestInitLeavesNoPartOfAConfigOnAPartialWrite(t *testing.T) {
	c, out, errOut := testEnv(t)
	_, pub := testSSHKeypair(t, t.TempDir())
	cfg := c.sopsConfigPath()
	orig := writeInitConfig
	t.Cleanup(func() { writeInitConfig = orig })
	calls := 0
	writeInitConfig = func(w io.Writer, content []byte) error {
		calls++
		if _, err := w.Write(content[:len(content)/2]); err != nil {
			return err
		}
		return errors.New("no space left on device")
	}

	if err := c.Init(pub); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("the seam ran %d times, want 1", calls)
	}
	want := "[error] cannot write " + cfg + ": no config was written\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	if _, err := os.Lstat(cfg); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("an entry is at %s after the failed write: %v", cfg, err)
	}
	if left := initConfigTemps(t, c.Paths.Base); len(left) != 0 {
		t.Errorf("temp files stay: %q", left)
	}

	writeInitConfig = orig
	out.Reset()
	errOut.Reset()
	if err := c.Init(pub); err != nil {
		t.Fatalf("the next init: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "Created "+cfg+"\n") {
		t.Errorf("the next init did not write the config:\n%s", out.String())
	}
	if got := readFile(t, cfg); got != wantInitConfig(t, pub) {
		t.Errorf(".sops.yaml %q", got)
	}
	info, err := os.Lstat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o644 {
		t.Errorf(".sops.yaml mode %v, want a regular file with 0644", info.Mode())
	}
	if left := initConfigTemps(t, c.Paths.Base); len(left) != 0 {
		t.Errorf("temp files stay: %q", left)
	}
}

// A temp file that a stopped run left (..sops.yaml.tmp-*) is not the config:
// init writes .sops.yaml as a first run does, and the temp file keeps its
// bytes.
func TestInitDoesNotTakeALeftTempFileForTheConfig(t *testing.T) {
	c, out, errOut := testEnv(t)
	_, pub := testSSHKeypair(t, t.TempDir())
	cfg := c.sopsConfigPath()
	left := c.Paths.Base + "/..sops.yaml.tmp-123456"
	const part = "# SOPS encryption config for lok8s secret cache files.\ncreation_rules:\n  - path_regex: 'Sec"
	write(t, left, part)

	if err := c.Init(pub); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "Created "+cfg+"\n") {
		t.Errorf("init did not write the config:\n%s", out.String())
	}
	if got := readFile(t, cfg); got != wantInitConfig(t, pub) {
		t.Errorf(".sops.yaml %q", got)
	}
	if got := readFile(t, left); got != part {
		t.Errorf("the left temp file changed: %q", got)
	}
}

// A .sops.yaml write that fails and whose temp file cannot be removed (the
// project directory is made read-only during the write): init names the
// temp file, then the file that it did not write (bash: the same two
// lines).
func TestInitNamesATempFileThatItCannotRemove(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, out, errOut := testEnv(t)
	_, pub := testSSHKeypair(t, t.TempDir())
	base := c.Paths.Base
	t.Cleanup(func() { _ = os.Chmod(base, 0o700) })
	orig := writeInitConfig
	t.Cleanup(func() { writeInitConfig = orig })
	writeInitConfig = func(io.Writer, []byte) error {
		if err := os.Chmod(base, 0o500); err != nil {
			t.Fatal(err)
		}
		return errors.New("stopped half way")
	}

	if err := c.Init(pub); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	left := initConfigTemps(t, base)
	if len(left) != 1 {
		t.Fatalf("want one temp file that stays, got %q", left)
	}
	want := "[error] cannot remove the temp file " + c.rel(left[0]) + ": remove it by hand\n" +
		"[error] cannot write " + c.sopsConfigPath() + ": no config was written\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
}

// heldElsewhere reports whether another open file description holds the
// flock of path: a flock with LOCK_NB on a new descriptor fails with
// EWOULDBLOCK. A lock that it gets is released at once.
func heldElsewhere(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatal(err)
	}
	return true
}

// probeWriter passes each write to w, and calls probe with it first.
type probeWriter struct {
	w     io.Writer
	probe func(p []byte)
}

func (p probeWriter) Write(b []byte) (int, error) {
	p.probe(b)
	return p.w.Write(b)
}

// Init holds the lock of .sops.yaml during the write of the temp file and
// after the rename: a flock with LOCK_NB on the lock file fails in the
// writeInitConfig seam and when Init prints "Created", which is after the
// rename (bash: the same test with a printf and a rename wrapper).
func TestInitHoldsTheLockAcrossTheRename(t *testing.T) {
	c, out, errOut := testEnv(t)
	_, pub := testSSHKeypair(t, t.TempDir())
	cfg := c.sopsConfigPath()
	orig := writeInitConfig
	t.Cleanup(func() { writeInitConfig = orig })
	var inWrite, afterRename []bool
	writeInitConfig = func(w io.Writer, content []byte) error {
		inWrite = append(inWrite, heldElsewhere(t, cfg+".lock"))
		return orig(w, content)
	}
	c.Out = probeWriter{w: out, probe: func(b []byte) {
		if bytes.HasPrefix(b, []byte("Created ")) {
			if _, err := os.Lstat(cfg); err != nil {
				t.Errorf("Init printed Created before the rename: %v", err)
			}
			afterRename = append(afterRename, heldElsewhere(t, cfg+".lock"))
		}
	}}

	if err := c.Init(pub); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if fmt.Sprint(inWrite) != "[true]" || fmt.Sprint(afterRename) != "[true]" {
		t.Errorf("the lock was held: in the write %v, after the rename %v; want [true] each", inWrite, afterRename)
	}
	if heldElsewhere(t, cfg+".lock") {
		t.Error("the lock is still held after Init returned")
	}
}

// The mode of .sops.yaml is 0644 under each umask: the temp file gets the
// mode with a chmod, which the umask does not change. Not parallel: the
// umask is of the process.
func TestInitConfigModeIsTheSameUnderEachUmask(t *testing.T) {
	for _, mask := range []int{0o077, 0o000} {
		t.Run(fmt.Sprintf("umask %03o", mask), func(t *testing.T) {
			old := syscall.Umask(mask)
			t.Cleanup(func() { syscall.Umask(old) })
			c, _, errOut := testEnv(t)
			_, pub := testSSHKeypair(t, t.TempDir())
			if err := c.Init(pub); err != nil {
				t.Fatalf("%v\n%s", err, errOut.String())
			}
			info, err := os.Lstat(c.sopsConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode() != 0o644 {
				t.Errorf(".sops.yaml mode %v under umask %03o, want -rw-r--r--", info.Mode(), mask)
			}
		})
	}
}

// Init makes the store and its .gitignore before .sops.yaml. A store step
// that fails leaves no .sops.yaml and no temp file, so the next init runs as
// a first run and writes the config (bash: the same cases).
func TestInitLeavesNoConfigWhenAStoreStepFails(t *testing.T) {
	t.Run("the .gitignore write", func(t *testing.T) {
		c, out, errOut := testEnv(t)
		_, pub := testSSHKeypair(t, t.TempDir())
		cfg := c.sopsConfigPath()
		orig := writeGitignore
		t.Cleanup(func() { writeGitignore = orig })
		writeGitignore = func(io.Writer, []byte) error { return errors.New("no space left on device") }

		if err := c.Init(pub); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v", err)
		}
		want := "[error] cannot write .secrets/.gitignore: it was not changed\n"
		if errOut.String() != want || out.String() != "" {
			t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
		}
		if _, err := os.Lstat(cfg); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an entry is at %s after the failed .gitignore write: %v", cfg, err)
		}
		if left := initConfigTemps(t, c.Paths.Base); len(left) != 0 {
			t.Errorf("temp files stay: %q", left)
		}

		writeGitignore = orig
		out.Reset()
		errOut.Reset()
		if err := c.Init(pub); err != nil {
			t.Fatalf("the next init: %v\n%s", err, errOut.String())
		}
		if got := readFile(t, cfg); got != wantInitConfig(t, pub) {
			t.Errorf("the next init: .sops.yaml %q", got)
		}
	})

	t.Run("the store directory", func(t *testing.T) {
		skipUnlessPermissionsHold(t)
		c, out, errOut := testEnv(t)
		_, pub := testSSHKeypair(t, t.TempDir())
		cfg := c.sopsConfigPath()
		c.Domain = "alpha.dev"
		domainDir := mkdir(t, c.Paths.Clusters+"/alpha.dev")
		if err := os.Chmod(domainDir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(domainDir, 0o755) })

		if err := c.Init(pub); err == nil {
			t.Fatalf("want the error of the store step, got nil\nstdout %q", out.String())
		}
		if out.String() != "" {
			t.Errorf("stdout %q", out.String())
		}
		if _, err := os.Lstat(cfg); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an entry is at %s after the failed store step: %v", cfg, err)
		}

		if err := os.Chmod(domainDir, 0o755); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		errOut.Reset()
		if err := c.Init(pub); err != nil {
			t.Fatalf("the next init: %v\n%s", err, errOut.String())
		}
		if got := readFile(t, cfg); got != wantInitConfig(t, pub) {
			t.Errorf("the next init: .sops.yaml %q", got)
		}
		if got := readFile(t, domainDir+"/secrets/.gitignore"); got != litBlock {
			t.Errorf("the next init: the .gitignore of the store %q", got)
		}
	})
}
