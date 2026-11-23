package secrets

// init_write_test.go: the .sops.yaml write of Init (bash: the init tests of
// tests/unit/secrets_test.bats). Init refuses a key that derives no age
// recipient before any write, takes the lock of .sops.yaml, checks the path
// again under the lock, and writes the file through a temp file and a
// rename.

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
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
	agePubkey, _, err := deriveAgePublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return "# SOPS encryption config for lok8s secret cache files.\n" +
		"# Recipients are age public keys derived from SSH keys via ssh-to-age.\n" +
		"# Add team members with: lo secrets add-key <ssh-pubkey-path|age1…>\n" +
		"creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '" + agePubkey + "'\n"
}

// An RSA key derives no recipient. Init prints the skip note
// of ssh-to-age and the refusal, and writes nothing: no .sops.yaml, no lock
// file and no store (bash: the same lines and status 1).
func TestInitRefusesAKeyThatDerivesNoRecipient(t *testing.T) {
	c, out, errOut := testEnv(t)
	pub := testRSAPublicKey(t)
	_, note, err := deriveAgePublicKey(pub)
	if !errors.Is(err, errUnsupportedKey) || note == "" {
		t.Fatalf("fixture: want a key that derives nothing, with a skip note, got %v %q", err, note)
	}
	before := fileStates(t, c.Paths.Base)

	if err := c.Init(pub); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v", err)
	}
	want := note + "\n[error] no age recipient derived from " + pub +
		" (ed25519 SSH keys only) — nothing written to " + c.sopsConfigPath() + "\n"
	if errOut.String() != want || out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, c.Paths.Base))
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

// Under the lock, init checks the path again without following a link
// (initConfigFree). A file that another run wrote while this run waited
// keeps its bytes. A link that names no file and a directory are not
// replaced, and the file that the link names is not made. Each case prints
// its refusal, and no temp file stays (bash: the same lines and status 1).
func TestInitChecksThePathAgainUnderTheLock(t *testing.T) {
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
			// The lock file exists, so the open of the run adds no entry.
			write(t, cfg+".lock", "")
			before := fileStates(t, c.Paths.Base)

			if err := c.Init(pub); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v", err)
			}
			want := "[error] " + cfg + notRegular
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
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
