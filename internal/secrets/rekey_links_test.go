package secrets

// rekey_links_test.go: add-key, remove-key and symlinks. The scan of the
// stores follows a symlinked .enc, but the re-key never writes through one
// (encryptFile refuses it), so a link in the files that a run re-keys is
// refused before the first write. add-key refuses every other .enc that the
// encrypt refuses too (a directory, a FIFO, a socket, a path that it cannot
// look up), and its encrypt reads the list that it checked. A .sops.yaml
// that is a symlink stays a link: both commands write the edit to its
// target. The encrypt refuses a .gitignore of the store that is a symlink or
// a directory (tests/unit/secrets_test.bats runs the bash twin).
// tests/unit/secrets_add_key_test.bats and
// tests/unit/secrets_remove_key_test.bats run the same cases through the bash
// twin.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/kernpilot/lok8s/internal/fsutil"
)

// linkFixture is a store a.dev with two pairs, Secret.app.default.K and
// Secret.db.default.K. remove-key removes gone from the rule "keep,gone";
// add-key adds gone to the rule "keep". Every .enc and plaintext gets a time
// in the past, so that a write shows in its mtime.
type linkFixture struct {
	t           *testing.T
	cmd         string
	c           *Context
	out, errOut *bytes.Buffer
	keep, gone  *age.X25519Identity
	store       string
}

func newLinkFixture(t *testing.T, cmd string) *linkFixture {
	t.Helper()
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	f := &linkFixture{t: t, cmd: cmd, c: c, out: out, errOut: errOut,
		keep: newAgeIdentity(t), gone: newAgeIdentity(t),
		store: c.Paths.Clusters + "/a.dev/secrets"}
	k, g := f.keep.Recipient().String(), f.gone.Recipient().String()
	if cmd == "remove-key" {
		sopsRules(t, c, k+","+g)
	} else {
		sopsRules(t, c, k)
	}
	write(t, f.store+"/Secret.app.default.K", "v")
	write(t, f.store+"/Secret.db.default.K", "w")
	encryptStore(t, c, "a.dev", out, errOut)
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{"app", "db"} {
		plain := f.store + "/Secret." + p + ".default.K"
		if err := os.Chtimes(plain+".enc", old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(plain, old.Add(time.Second), old.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	callerIdentity(t, f.keep)
	return f
}

func (f *linkFixture) run() error {
	f.out.Reset()
	f.errOut.Reset()
	if f.cmd == "remove-key" {
		return f.c.RemoveKey(f.gone.Recipient().String(), false, false)
	}
	return f.c.AddKey(f.gone.Recipient().String(), false, false)
}

// rekeyed fails the test unless the run re-keyed the .enc of name: after
// remove-key gone cannot open it and keep can; after add-key both can.
func (f *linkFixture) rekeyed(name, plain string) {
	f.t.Helper()
	enc := f.store + "/Secret." + name + ".default.K.enc"
	if !opens(f.t, enc, f.keep, plain) || opens(f.t, enc, f.gone, plain) != (f.cmd == "add-key") {
		f.t.Errorf("%s: %s was not re-keyed", f.cmd, f.c.rel(enc))
	}
}

// fileStates reads every entry under the paths: its type, its link target,
// its mtime and, for a regular file, its bytes. Any write shows in it.
func fileStates(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	states := map[string]string{}
	for _, root := range paths {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			s := info.Mode().String() + " " + strconv.FormatInt(info.ModTime().UnixNano(), 10)
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := os.Readlink(path)
				if err != nil {
					return err
				}
				s += " -> " + target
			case info.Mode().IsRegular():
				s += " " + readFile(t, path)
			}
			states[path] = s
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return states
}

// sameStates fails the test when an entry was added, removed or changed.
func sameStates(t *testing.T, before, after map[string]string) {
	t.Helper()
	for path, s := range before {
		if after[path] != s {
			t.Errorf("%s changed:\n%q\nnow:\n%q", path, s, after[path])
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was added", path)
		}
	}
}

// A store whose .enc of app is a symlink to a regular file that lists the
// recipient: both commands refuse it before any write. .sops.yaml, every
// .enc (its bytes and its mtime), the link and every plaintext stay as they
// were, and the run prints no command to resume: nothing was started. The
// next action that the message names (the file in the place of the link)
// is the control: the run with a regular .enc passes and re-keys both
// pairs.
func TestKeyCommandsRefuseASymlinkedEncBeforeAnyWrite(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		t.Run(cmd, func(t *testing.T) {
			f := newLinkFixture(t, cmd)
			enc := f.store + "/Secret.app.default.K.enc"
			target := f.c.Paths.Base + "/elsewhere/Secret.app.default.K.enc"
			mkdir(t, filepath.Dir(target))
			if err := os.Rename(enc, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, enc); err != nil {
				t.Fatal(err)
			}
			// The scan reads the recipients through the link.
			if cmd == "remove-key" && !strings.Contains(readFile(t, enc), f.gone.Recipient().String()) {
				t.Fatal("fixture: the linked .enc does not list the recipient")
			}
			paths := []string{f.c.sopsConfigPath(), f.c.Paths.Clusters, filepath.Dir(target)}
			before := fileStates(t, paths...)

			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
			}
			want := "[error] clusters/a.dev/secrets/Secret.app.default.K.enc is a symlink: the re-key does not write an .enc through a link\n" +
				"[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written\n"
			if f.errOut.String() != want || f.out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, paths...))

			// The file in the place of the link: the run passes.
			if err := os.Rename(target, enc); err != nil {
				t.Fatal(err)
			}
			if err := f.run(); err != nil {
				t.Fatalf("with the link replaced: %v\n%s", err, f.errOut.String())
			}
			f.rekeyed("app", "v")
			f.rekeyed("db", "w")
		})
	}
}

// The .enc of a later pair that the encrypt refuses, a directory, a FIFO, a
// socket or a symlink, also the .enc of a .sha file: add-key refuses it
// before any write, with the list of the encrypt sweep. .sops.yaml, the
// other .enc (its bytes and its mtime), the entry and every plaintext stay
// as they were, and no temp file appears in the project or the store. A
// device node needs root to make, so no case makes one: encDestinationOK
// refuses it as it refuses a FIFO. The control: with the entry removed, the
// run passes.
func TestAddKeyRefusesEveryEncThatTheEncryptRefusesBeforeAnyWrite(t *testing.T) {
	const kind = " is not a regular file (a directory, a FIFO, a socket or a device): the re-key does not write an .enc there\n"
	const other = "[error]   remove or fix each entry named above, then run this command again; nothing was written\n"
	cases := []struct {
		name, plain string // plain: the plaintext whose .enc is the entry
		make        func(t *testing.T, f *linkFixture, enc string)
		want        string
	}{
		{"a directory", "Secret.db.default.K", func(t *testing.T, _ *linkFixture, enc string) {
			mkdir(t, enc)
		}, "[error] clusters/a.dev/secrets/Secret.db.default.K.enc" + kind + other},
		{"a FIFO", "Secret.db.default.K", func(t *testing.T, _ *linkFixture, enc string) {
			if err := syscall.Mkfifo(enc, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "[error] clusters/a.dev/secrets/Secret.db.default.K.enc" + kind + other},
		{"a socket", "Secret.db.default.K", func(t *testing.T, _ *linkFixture, enc string) {
			bindSocket(t, enc)
		}, "[error] clusters/a.dev/secrets/Secret.db.default.K.enc" + kind + other},
		{"a symlink", "Secret.db.default.K", func(t *testing.T, f *linkFixture, enc string) {
			target := f.c.Paths.Base + "/elsewhere/Secret.db.default.K.enc"
			write(t, target, "x")
			if err := os.Symlink(target, enc); err != nil {
				t.Fatal(err)
			}
		}, "[error] clusters/a.dev/secrets/Secret.db.default.K.enc is a symlink: the re-key does not write an .enc through a link\n" +
			"[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written\n"},
		{"the .enc of a .sha file", "Secret.db.default.K.sha", func(t *testing.T, _ *linkFixture, enc string) {
			mkdir(t, enc)
		}, "[error] clusters/a.dev/secrets/Secret.db.default.K.sha.enc" + kind + other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			plain := f.store + "/" + tc.plain
			if !fsutil.IsRegular(plain) {
				write(t, plain, "s")
			}
			enc := plain + ".enc"
			if err := os.Remove(enc); err != nil && !errors.Is(err, fs.ErrNotExist) {
				t.Fatal(err)
			}
			tc.make(t, f, enc)
			// The lock file is there before the run, so that the listing of
			// the project shows any other new entry.
			write(t, lockPath(t, f.c), "")
			before := fileStates(t, f.c.Paths.Base)

			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
			}
			if f.errOut.String() != tc.want || f.out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), tc.want)
			}
			sameStates(t, before, fileStates(t, f.c.Paths.Base))

			if err := os.RemoveAll(enc); err != nil {
				t.Fatal(err)
			}
			if err := f.run(); err != nil {
				t.Fatalf("with the entry removed: %v\n%s", err, f.errOut.String())
			}
			f.rekeyed("app", "v")
			f.rekeyed("db", "w")
		})
	}
}

// A regular plaintext whose name is so long that the name of its .enc is
// over NAME_MAX: the lstat of the .enc fails with ENAMETOOLONG, not with
// "does not exist". add-key refuses it before any write, with the line of a
// lookup error. .sops.yaml, every .enc and every plaintext stay as they
// were, and no temp file appears in the project or the store. The case
// skips when the filesystem refuses the plaintext name or takes the .enc
// name. The control: with the long plaintext removed, the run passes.
func TestAddKeyRefusesAnEncPathThatItCannotLookUpBeforeAnyWrite(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	name := "Secret.app.default."
	name += strings.Repeat("K", 253-len(name))
	plain := f.store + "/" + name
	if err := os.WriteFile(plain, []byte("s"), 0o600); err != nil {
		t.Skipf("the filesystem refuses a name of %d bytes: %v", len(name), err)
	}
	if _, err := os.Lstat(plain + ".enc"); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Skipf("the lstat of a name of %d bytes does not fail with another error: %v", len(name)+len(".enc"), err)
	}
	write(t, lockPath(t, f.c), "")
	before := fileStates(t, f.c.Paths.Base)

	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	want := "[error] clusters/a.dev/secrets/" + name + ".enc cannot be looked up (an error other than \"no such file or directory\", for example a name that is too long): the re-key does not write an .enc there\n" +
		"[error]   remove or fix each entry named above, then run this command again; nothing was written\n"
	if f.errOut.String() != want || f.out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, f.c.Paths.Base))

	if err := os.Remove(plain); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatalf("with the long plaintext removed: %v\n%s", err, f.errOut.String())
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
}

// bindSocket makes a unix socket at path. A socket path is limited to about
// 108 bytes, and the temp dir of a test can be longer, so it binds the base
// name from the directory of path (t.Chdir). It skips the test when it
// cannot bind a socket there.
func bindSocket(t *testing.T, path string) {
	t.Helper()
	t.Chdir(filepath.Dir(path))
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Skipf("cannot make a unix socket: %v", err)
	}
	defer func() { _ = syscall.Close(fd) }()
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: filepath.Base(path)}); err != nil {
		t.Skipf("cannot bind a unix socket in the test dir: %v", err)
	}
}

// A store whose .enc of app is a symlink and whose .enc of db is a
// directory: add-key refuses both before any write, and the last line names
// both kinds. Nothing changes in the project.
func TestAddKeyRefusesALinkAndADirectoryInOneStore(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	app := f.store + "/Secret.app.default.K.enc"
	target := f.c.Paths.Base + "/elsewhere/Secret.app.default.K.enc"
	mkdir(t, filepath.Dir(target))
	if err := os.Rename(app, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, app); err != nil {
		t.Fatal(err)
	}
	db := f.store + "/Secret.db.default.K.enc"
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	mkdir(t, db)
	write(t, lockPath(t, f.c), "")
	before := fileStates(t, f.c.Paths.Base)

	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	want := "[error] clusters/a.dev/secrets/Secret.app.default.K.enc is a symlink: the re-key does not write an .enc through a link\n" +
		"[error] clusters/a.dev/secrets/Secret.db.default.K.enc is not a regular file (a directory, a FIFO, a socket or a device): the re-key does not write an .enc there\n" +
		"[error]   replace each link with the file it names, and remove or fix each other entry, then run this command again; nothing was written\n"
	if f.errOut.String() != want || f.out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, f.c.Paths.Base))
}

// The store has no .gitignore, a dangling plaintext link Secret.app.default.K
// -> .gitignore and a dangling Secret.app.default.K.enc -> missing, next to
// the pair db. When add-key checks the list of the sweep, the plaintext link
// names nothing, so it is not in the list. The encrypt writes the
// .gitignore, and the link then names a regular file. The encrypt reads the
// list that add-key checked: it leaves both links as they were and re-keys
// db. A new listing of the store would take the link and stop at its .enc,
// after the .sops.yaml write.
func TestAddKeyEncryptsTheListThatItChecked(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	app := f.store + "/Secret.app.default.K"
	for _, p := range []string{app, app + ".enc", f.store + "/.gitignore"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	links := map[string]string{app: ".gitignore", app + ".enc": "missing"}
	for path, target := range links {
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.run(); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	for path, target := range links {
		if got, err := os.Readlink(path); err != nil || got != target {
			t.Errorf("%s: %q, %v; want the link to %s", f.c.rel(path), got, err, target)
		}
	}
	if !fsutil.IsRegular(f.store + "/.gitignore") {
		t.Error("the encrypt wrote no .gitignore")
	}
	f.rekeyed("db", "w")
}

// add-key in a domain whose store does not resolve (the domain has no
// store, and the project keeps a store per domain), with --all and
// without: it stops before its first write. ResolveStore says why, and the
// last line says that .sops.yaml did not change and what to do. stdout is
// empty, and .sops.yaml, every .enc and every plaintext stay as they were.
// Before, the run added the recipient, then stopped, and a rerun found the
// recipient present and re-keyed nothing. The control: with the store
// made, the run adds the recipient.
func TestAddKeyRefusesAStoreThatDoesNotResolveBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		all  bool
	}{{"without --all", false}, {"with --all", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			mkdir(t, f.c.Paths.Clusters+"/c.dev")
			f.c.Domain = "c.dev"
			write(t, lockPath(t, f.c), "")
			before := fileStates(t, f.c.Paths.Base)
			f.out.Reset()
			f.errOut.Reset()

			err := f.c.AddKey(f.gone.Recipient().String(), tc.all, false)
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
			}
			store := f.c.Paths.Clusters + "/c.dev/secrets"
			want := "[error] domain c.dev has no secrets store: " + store + " does not exist, and this project keeps a store per domain (" + f.store + "). lo does not fall back to " + f.c.flatStore() + ".\n" +
				"[error]   Create the empty directory " + store + " to generate new values for c.dev.\n" +
				"[error]   add-key did not change .sops.yaml: create that directory, or select a domain that has a store, then run this command again\n"
			if f.out.String() != "" || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, f.c.Paths.Base))

			mkdir(t, store)
			if err := f.c.AddKey(f.gone.Recipient().String(), tc.all, false); err != nil {
				t.Fatalf("with the store made: %v\n%s", err, f.errOut.String())
			}
			if !strings.Contains(readFile(t, f.c.sopsConfigPath()), f.gone.Recipient().String()) {
				t.Error("with the store made, the recipient was not added")
			}
		})
	}
}

// remove-key resumes a stopped re-key: .sops.yaml no longer lists the
// recipient, the .enc of app was re-keyed and is now a symlink to a file
// that does not list it, and the .enc of db still lists it. The labels do
// not choose the files, so the link is in the re-key list, and it is
// refused before any write.
func TestRemoveKeyResumeRefusesASymlinkedEncThatDoesNotListTheKey(t *testing.T) {
	f := newLinkFixture(t, "remove-key")
	g := f.gone.Recipient().String()
	sopsRules(t, f.c, f.keep.Recipient().String())
	app := f.store + "/Secret.app.default.K"
	if err := f.c.encryptFile(app); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	target := f.c.Paths.Base + "/elsewhere/Secret.app.default.K.enc"
	mkdir(t, filepath.Dir(target))
	if err := os.Rename(app+".enc", target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, app+".enc"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(readFile(t, target), g) || !strings.Contains(readFile(t, f.store+"/Secret.db.default.K.enc"), g) {
		t.Fatal("fixture: the link lists the recipient, or the .enc of db does not")
	}
	write(t, lockPath(t, f.c), "")
	before := fileStates(t, f.c.Paths.Base)

	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	want := "[error] clusters/a.dev/secrets/Secret.app.default.K.enc is a symlink: the re-key does not write an .enc through a link\n" +
		"[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written\n"
	if f.errOut.String() != want || f.out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
	}
	sameStates(t, before, fileStates(t, f.c.Paths.Base))
}

// A .gitignore of the store that is a symlink or a directory: the encrypt
// refuses it before it reads or writes it, and encrypts nothing. The file
// that the link names and every entry of the project stay as they were.
func TestEncryptRefusesAGitignoreThatIsALinkOrNotARegularFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(t *testing.T, gitignore, outside string)
	}{
		{"a symlink", func(t *testing.T, gitignore, outside string) {
			write(t, outside, "# not the rules of lok8s\n")
			if err := os.Symlink(outside, gitignore); err != nil {
				t.Fatal(err)
			}
		}},
		{"a directory", func(t *testing.T, gitignore, _ string) {
			mkdir(t, gitignore)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			testAgeIdentity(t, c)
			store := c.Paths.Base + "/.secrets"
			write(t, store+"/Secret.app.default.K", "v")
			tc.make(t, store+"/.gitignore", c.Paths.Base+"/elsewhere/gitignore")
			before := fileStates(t, c.Paths.Base)

			if err := c.Encrypt(""); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] .secrets/.gitignore is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before the .enc writes. Remove it, then run this command again\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// A .sops.yaml that is a symlink stays a link: both commands write the edit
// to its target, the target keeps its mode, and no temp file is left next to
// it.
func TestKeyCommandsEditTheTargetOfASymlinkedSopsConfig(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		t.Run(cmd, func(t *testing.T) {
			f := newLinkFixture(t, cmd)
			conf := f.c.Paths.Base + "/conf/sops.yaml"
			mkdir(t, filepath.Dir(conf))
			if err := os.Rename(f.c.sopsConfigPath(), conf); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(conf, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("conf/sops.yaml", f.c.sopsConfigPath()); err != nil {
				t.Fatal(err)
			}

			if err := f.run(); err != nil {
				t.Fatalf("%v\n%s", err, f.errOut.String())
			}
			if info, err := os.Lstat(f.c.sopsConfigPath()); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Fatal("the link was replaced by a file")
			}
			if target, err := os.Readlink(f.c.sopsConfigPath()); err != nil || target != "conf/sops.yaml" {
				t.Errorf("the link names %q", target)
			}
			list := f.keep.Recipient().String()
			if cmd == "add-key" {
				list += "," + f.gone.Recipient().String()
			}
			if got, want := readFile(t, conf), "creation_rules:\n  - path_regex: 'Secret\\..*'\n    age: '"+list+"'\n"; got != want {
				t.Errorf("the target:\n%q\nwant:\n%q", got, want)
			}
			info, err := os.Stat(conf)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Errorf("the mode of the target was not kept: %v", info.Mode().Perm())
			}
			// No temp file is left, and the lock is beside the target.
			var names []string
			entries, _ := os.ReadDir(filepath.Dir(conf))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			if strings.Join(names, " ") != "sops.yaml sops.yaml.lock" {
				t.Errorf("the dir of the target holds %q, want the file and its lock", names)
			}
			if _, err := os.Lstat(f.c.sopsConfigPath() + ".lock"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a lock beside the link: %v", err)
			}
			f.rekeyed("app", "v")
		})
	}
}

// With a .sops.yaml link, the lock goes beside the file that the link names.
// In a directory that lo cannot write, both commands refuse, also add-key of
// a recipient that is already there and remove-key of a key in no rule. The
// line names that directory and says where the lock goes; nothing changes.
func TestKeyCommandsNameTheDirectoryOfALockThatCannotBeOpened(t *testing.T) {
	skipUnlessPermissionsHold(t)
	for _, cmd := range []string{"add-key", "remove-key"} {
		t.Run(cmd, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			c.Domain = "a.dev"
			sopsRules(t, c, keyA)
			dir := mkdir(t, c.Paths.Base+"/conf")
			if err := os.Rename(c.sopsConfigPath(), dir+"/sops.yaml"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("conf/sops.yaml", c.sopsConfigPath()); err != nil {
				t.Fatal(err)
			}
			real, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
			before := fileStates(t, c.Paths.Base)

			if cmd == "add-key" {
				err = c.AddKey(keyA, false, false)
			} else {
				err = c.RemoveKey(keyB, false, false)
			}
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] cannot open the lock file " + real + "/sops.yaml.lock — the lock goes beside the file that .sops.yaml names, so lo needs write access to " + real + "; nothing was changed\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// Two .sops.yaml links that name one file take one lock, beside that file,
// also when one of them gets there through a second link: while a run
// through the other link holds it, a run is refused and changes nothing, and
// no lock is made beside a link. When the lock is free again, the same run
// goes on and writes the file at the end of the chain.
func TestKeyCommandsTakeOneLockForTwoLinksToOneSopsConfig(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		t.Run(cmd, func(t *testing.T) {
			f := newLinkFixture(t, cmd)
			dir := t.TempDir()
			conf := filepath.Join(dir, "sops.yaml")
			if err := os.Rename(f.c.sopsConfigPath(), conf); err != nil {
				t.Fatal(err)
			}
			mid := filepath.Join(dir, "mid.yaml")
			if err := os.Symlink("sops.yaml", mid); err != nil {
				t.Fatal(err)
			}
			other := *f.c
			paths := *f.c.Paths
			paths.Base = t.TempDir()
			other.Paths = &paths
			// .sops.yaml -> mid.yaml -> sops.yaml, and the other project's
			// .sops.yaml -> sops.yaml.
			if err := os.Symlink(mid, f.c.sopsConfigPath()); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(conf, other.sopsConfigPath()); err != nil {
				t.Fatal(err)
			}
			t.Setenv(lockWaitEnv, "0")
			dest, err := other.sopsConfigDest()
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := other.lockSopsConfig(dest)
			if err != nil {
				t.Fatal(err)
			}
			released := false
			defer func() {
				if !released {
					unlock()
				}
			}()
			target, err := filepath.EvalSymlinks(conf)
			if err != nil {
				t.Fatal(err)
			}
			before := fileStates(t, f.c.Paths.Base, other.Paths.Base, dir)

			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
			}
			want := "[error] " + target + ".lock is held by another lo secrets add-key or remove-key — nothing was changed; run it again when that one ends\n"
			if f.errOut.String() != want || f.out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, f.c.Paths.Base, other.Paths.Base, dir))

			unlock()
			released = true
			if err := f.run(); err != nil {
				t.Fatalf("the run after the release: %v\n%s", err, f.errOut.String())
			}
			has := strings.Contains(readFile(t, conf), f.gone.Recipient().String())
			if has != (cmd == "add-key") {
				t.Errorf("%s after the release: the file at the end of the chain was not edited:\n%s", cmd, readFile(t, conf))
			}
			for _, p := range []string{f.c.sopsConfigPath(), mid} {
				if info, err := os.Lstat(p); err != nil || info.Mode()&fs.ModeSymlink == 0 {
					t.Errorf("%s is no longer a link", p)
				}
			}
			for _, p := range []string{f.c.sopsConfigPath(), other.sopsConfigPath(), mid} {
				if _, err := os.Lstat(p + ".lock"); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("a lock beside the link %s: %v", p, err)
				}
			}
			f.rekeyed("app", "v")
		})
	}
}

// The .gitignore write goes to a temp file in the store and a rename: a hard
// link to the old file keeps its bytes, the new file has mode 0644, and no
// temp file stays (bash: tests/unit/secrets_test.bats).
func TestEnsureGitignoreReplacesAHardLink(t *testing.T) {
	c, _, errOut := testEnv(t)
	dir := mkdir(t, c.Paths.Base+"/.secrets")
	outside := c.Paths.Base + "/elsewhere"
	write(t, outside, "# old\n")
	if err := os.Link(outside, dir+"/.gitignore"); err != nil {
		t.Fatal(err)
	}
	if err := c.ensureGitignore(dir, stepPlaintext); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if got := readFile(t, outside); got != "# old\n" {
		t.Errorf("the hard link sees %q: the write went through it", got)
	}
	// The old file has no block: the block goes on top of its line.
	if got := readFile(t, dir+"/.gitignore"); got != litBlock+"# old\n" {
		t.Errorf(".gitignore: %q", got)
	}
	info, err := os.Stat(dir + "/.gitignore")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v, want 0644", info.Mode().Perm())
	}
	if left, _ := filepath.Glob(dir + "/..gitignore.tmp-*"); len(left) != 0 {
		t.Errorf("temp files stay: %q", left)
	}
}

// A .gitignore that it cannot write (the store is read-only): the old file
// stays, and ensureGitignore prints one line that names the file, with no
// error text of the file system, and returns ErrHandled (bash:
// tests/unit/secrets_test.bats).
func TestEnsureGitignoreReportsAWriteThatFails(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, _, errOut := testEnv(t)
	dir := mkdir(t, c.Paths.Base+"/.secrets")
	write(t, dir+"/.gitignore", "# old\n")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := c.ensureGitignore(dir, stepPlaintext); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	if errOut.String() != "[error] cannot write .secrets/.gitignore: it was not changed\n" {
		t.Errorf("stderr:\n%s", errOut.String())
	}
	if got := readFile(t, dir+"/.gitignore"); got != "# old\n" {
		t.Errorf(".gitignore: %q", got)
	}
}

// Init returns the refusal of its .gitignore write (bash:
// tests/unit/secrets_test.bats).
func TestInitReturnsAGitignoreWriteThatFails(t *testing.T) {
	skipUnlessPermissionsHold(t)
	c, _, errOut := testEnv(t)
	_, pubPath := testSSHKeypair(t, c.Paths.Base)
	dir := mkdir(t, c.Paths.Base+"/.secrets")
	write(t, dir+"/.gitignore", "# old\n")
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if err := c.Init(pubPath); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	if errOut.String() != "[error] cannot write .secrets/.gitignore: it was not changed\n" {
		t.Errorf("stderr:\n%s", errOut.String())
	}
}

// add-key checks the .gitignore of the store before its first write, the
// lock file included: a .gitignore that is a symlink is refused with one
// line, also for a recipient that is already there, and nothing is
// written (bash: tests/unit/secrets_add_key_test.bats).
func TestAddKeyRefusesABadGitignoreBeforeAnyWrite(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run("present="+strconv.FormatBool(present), func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			gitignore := f.store + "/.gitignore"
			if err := os.Remove(gitignore); err != nil {
				t.Fatal(err)
			}
			outside := f.c.Paths.Base + "/elsewhere/gitignore"
			write(t, outside, "# not the rules of lok8s\n")
			if err := os.Symlink(outside, gitignore); err != nil {
				t.Fatal(err)
			}
			if present {
				sopsRules(t, f.c, f.keep.Recipient().String()+","+f.gone.Recipient().String())
			}
			before := fileStates(t, f.c.Paths.Base)

			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
			}
			want := "[error] " + f.c.rel(gitignore) + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before its first write. Remove it, then run this command again\n"
			if f.errOut.String() != want || f.out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, f.c.Paths.Base))
		})
	}
}

// add-key writes the .gitignore of the store before .sops.yaml: a .gitignore
// that it cannot write (the store is read-only) stops the run with
// .sops.yaml and the store as they were. The run after the fix adds the
// recipient, re-keys both pairs and writes the .gitignore (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyWritesTheGitignoreBeforeTheConfig(t *testing.T) {
	skipUnlessPermissionsHold(t)
	f := newLinkFixture(t, "add-key")
	write(t, f.store+"/.gitignore", "# old\n")
	info, err := os.Stat(f.store)
	if err != nil {
		t.Fatal(err)
	}
	mode := info.Mode().Perm()
	before := readFile(t, f.c.sopsConfigPath())
	states := fileStates(t, f.store)
	if err := os.Chmod(f.store, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.store, mode) })

	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	want := "[error] cannot write " + f.c.rel(f.store) + "/.gitignore: it was not changed\n" +
		"[error]   add-key did not change .sops.yaml or an .enc file: fix the cause above, then run this command again\n"
	if f.errOut.String() != want || f.out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
	}
	unchanged(t, f.c, before)
	if err := os.Chmod(f.store, mode); err != nil {
		t.Fatal(err)
	}
	sameStates(t, states, fileStates(t, f.store))

	if err := f.run(); err != nil {
		t.Fatalf("the run after the fix: %v\n%s", err, f.errOut.String())
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
	if got := readFile(t, f.store+"/.gitignore"); got != litBlock+"# old\n" {
		t.Errorf(".gitignore: %q", got)
	}
}

// A run that stops after its write of .sops.yaml (here at the orphan check:
// an .enc with no plaintext twin) leaves the recipient in .sops.yaml and no
// .enc re-keyed. The rerun that the message names (--skip-orphans) finds the
// recipient present and the .enc files of the sweep without it: it finishes
// the re-key and does not write .sops.yaml again. The orphan still does not
// list the recipient, so a run without --skip-orphans refuses it as the
// first run did, before it decides that nothing is left to do. With
// --skip-orphans, the run after that changes nothing and warns about the
// orphan. No rerun writes .sops.yaml: its file keeps its inode and its mtime,
// not only its bytes (bash: tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunFinishesAReKeyThatAnEarlierRunLeft(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	gone := f.gone.Recipient().String()
	write(t, f.store+"/Secret.orphan.default.K.enc", readFile(t, f.store+"/Secret.app.default.K.enc"))
	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	config := readFile(t, f.c.sopsConfigPath())
	if !strings.Contains(config, gone) {
		t.Fatal("fixture: the first run did not write .sops.yaml before it stopped")
	}
	id := configFile(t, f.c)

	f.out.Reset()
	f.errOut.Reset()
	if err := f.c.AddKey(gone, false, true); err != nil {
		t.Fatalf("the rerun: %v\n%s", err, f.errOut.String())
	}
	want := "recipient already present in .sops.yaml, but 2 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key\n  " + gone + "\n" +
		"Encrypted 2 secret(s)\n" +
		"done — commit .sops.yaml and the updated .enc files together\n"
	if got := f.out.String(); got != want {
		t.Errorf("stdout:\n%q\nwant:\n%q", got, want)
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
	unchanged(t, f.c, config)
	sameConfigFile(t, f.c, id)

	orphan := "[warn] orphan (no decrypted twin, cannot re-key): " + f.c.rel(f.store) + "/Secret.orphan.default.K.enc\n"
	states := fileStates(t, f.store)
	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("the run without --skip-orphans: want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	refusal := orphan +
		"[error] 1 encrypted file(s) have no decrypted twin — they would keep the OLD\n" +
		"[error]   recipient list while .sops.yaml claims the new key was added.\n" +
		"[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.\n"
	if f.out.String() != "" || f.errOut.String() != refusal {
		t.Errorf("stdout %q\nstderr:\n%q\nwant stderr:\n%q", f.out.String(), f.errOut.String(), refusal)
	}
	sameStates(t, states, fileStates(t, f.store))

	f.out.Reset()
	f.errOut.Reset()
	if err := f.c.AddKey(gone, false, true); err != nil {
		t.Fatalf("the last run: %v\n%s", err, f.errOut.String())
	}
	if want := "recipient already present in .sops.yaml — nothing to add\n  " + gone + "\n"; f.out.String() != want || f.errOut.String() != orphan {
		t.Errorf("stdout %q\nstderr %q\nwant stdout %q and stderr %q", f.out.String(), f.errOut.String(), want, orphan)
	}
	sameStates(t, states, fileStates(t, f.store))
	unchanged(t, f.c, config)
	sameConfigFile(t, f.c, id)
}

// configFile is the identity of the file of .sops.yaml: its inode and its
// mtime. A write in place changes the mtime, and a replace (a rename over
// the name) changes the inode, also when the bytes stay the same.
func configFile(t *testing.T, c *Context) string {
	t.Helper()
	info, err := os.Stat(c.sopsConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no inode in the stat of .sops.yaml")
	}
	return fmt.Sprintf("inode %d, mtime %d", st.Ino, info.ModTime().UnixNano())
}

// sameConfigFile fails the test when the file of .sops.yaml is not the one
// of id (configFile): the run wrote or replaced it.
func sameConfigFile(t *testing.T, c *Context, id string) {
	t.Helper()
	if got := configFile(t, c); got != id {
		t.Errorf(".sops.yaml was written or replaced: %s, before %s", got, id)
	}
}

// finished runs add-key in the fixture, so that each .enc of the store
// lists both identities, and returns .sops.yaml.
func (f *linkFixture) finished() string {
	f.t.Helper()
	if err := f.run(); err != nil {
		f.t.Fatalf("the first run: %v\n%s", err, f.errOut.String())
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
	return readFile(f.t, f.c.sopsConfigPath())
}

// envelope reads the JSON envelope of the .enc at path, lets edit change
// it, and writes it back.
func envelope(t *testing.T, path string, edit func(top, meta map[string]any)) {
	t.Helper()
	var top map[string]any
	if err := json.Unmarshal([]byte(readFile(t, path)), &top); err != nil {
		t.Fatal(err)
	}
	meta, ok := top["sops"].(map[string]any)
	if !ok {
		t.Fatalf("%s: no sops metadata", path)
	}
	edit(top, meta)
	raw, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(raw))
}

// ageEntry is the age entry of recipient in the sops metadata meta.
func ageEntry(t *testing.T, meta map[string]any, recipient string) map[string]any {
	t.Helper()
	list, _ := meta["age"].([]any)
	for _, e := range list {
		if entry, _ := e.(map[string]any); entry["recipient"] == recipient {
			return entry
		}
	}
	t.Fatalf("no age entry for %s", recipient)
	return nil
}

// A rerun after a finished run, with the .enc of app in a state that is not
// finished: it is missing, it holds the label of the recipient alone, its
// envelope has no mac or no lastmodified, the entry of the recipient has
// no enc (and the entry of the other recipient has a good one), the enc of
// the entry is not an armored age block, or it lists the old recipients
// only. The rerun re-keys
// that file only: the .enc of db keeps its bytes and its mtime, and
// .sops.yaml stays as it was. The run after it changes nothing (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunReKeysOnlyTheFilesThatAreNotFinished(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(f *linkFixture, enc, old string)
	}{
		{"a missing .enc", func(f *linkFixture, enc, _ string) {
			if err := os.Remove(enc); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"the label alone", func(f *linkFixture, enc, _ string) {
			write(f.t, enc, `{"data":"ENC[x]","sops":{"age":[{"recipient":"`+f.gone.Recipient().String()+`"}]}}`)
		}},
		{"no mac", func(f *linkFixture, enc, _ string) {
			envelope(f.t, enc, func(_, meta map[string]any) { delete(meta, "mac") })
		}},
		{"an empty lastmodified", func(f *linkFixture, enc, _ string) {
			envelope(f.t, enc, func(_, meta map[string]any) { meta["lastmodified"] = "" })
		}},
		{"the entry of the key has no enc", func(f *linkFixture, enc, _ string) {
			envelope(f.t, enc, func(_, meta map[string]any) {
				delete(ageEntry(f.t, meta, f.gone.Recipient().String()), "enc")
			})
		}},
		{"an enc that is not armored", func(f *linkFixture, enc, _ string) {
			envelope(f.t, enc, func(_, meta map[string]any) {
				ageEntry(f.t, meta, f.gone.Recipient().String())["enc"] = "x"
			})
		}},
		{"an enc without the footer line", func(f *linkFixture, enc, _ string) {
			envelope(f.t, enc, func(_, meta map[string]any) {
				entry := ageEntry(f.t, meta, f.gone.Recipient().String())
				entry["enc"] = strings.TrimSuffix(entry["enc"].(string), armor.Footer+"\n")
			})
		}},
		{"the old recipients", func(f *linkFixture, enc, old string) {
			write(f.t, enc, old)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			gone := f.gone.Recipient().String()
			app, db := f.store+"/Secret.app.default.K.enc", f.store+"/Secret.db.default.K.enc"
			old := readFile(t, app)
			config := f.finished()
			id := configFile(t, f.c)
			tc.set(f, app, old)
			states := fileStates(t, db)

			if err := f.run(); err != nil {
				t.Fatalf("the rerun: %v\n%s", err, f.errOut.String())
			}
			want := "recipient already present in .sops.yaml, but 1 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key\n  " + gone + "\n" +
				"Encrypted 1 secret(s)\n" +
				"done — commit .sops.yaml and the updated .enc files together\n"
			if f.out.String() != want || f.errOut.String() != "" {
				t.Errorf("stdout:\n%q\nstderr %q\nwant stdout:\n%q", f.out.String(), f.errOut.String(), want)
			}
			f.rekeyed("app", "v")
			sameStates(t, states, fileStates(t, db))
			unchanged(t, f.c, config)
			sameConfigFile(t, f.c, id)

			if err := f.run(); err != nil {
				t.Fatalf("the last run: %v\n%s", err, f.errOut.String())
			}
			if want := "recipient already present in .sops.yaml — nothing to add\n  " + gone + "\n"; f.out.String() != want {
				t.Errorf("the last run: stdout %q", f.out.String())
			}
		})
	}
}

// .sops.yaml holds the new key in a comment only: the rule lists the old
// recipient. The rerun finds the key present and re-keys both files, but
// sops encrypts them to the rule, so they still do not list the key. The
// run reads the sweep again after the encrypt, names each file, says that
// the rule may not list the key, and fails, with no "done" line (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunFailsWhenTheRuleDoesNotListTheKey(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	gone := f.gone.Recipient().String()
	write(t, f.c.sopsConfigPath(), "# the next key: "+gone+"\n"+readFile(t, f.c.sopsConfigPath()))
	config := readFile(t, f.c.sopsConfigPath())

	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	wantOut := "recipient already present in .sops.yaml, but 2 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key\n  " + gone + "\n" +
		"Encrypted 2 secret(s)\n"
	rel := f.c.rel(f.store)
	wantErr := "[error] the sops metadata of " + rel + "/Secret.app.default.K.enc does not pass the recipient envelope check after the re-key\n" +
		"[error] the sops metadata of " + rel + "/Secret.db.default.K.enc does not pass the recipient envelope check after the re-key\n" +
		"[error]   the key is in .sops.yaml, but the creation rule of these files may not list it (for example, the key is only in a comment): fix .sops.yaml, then run this command again\n"
	if f.out.String() != wantOut || f.errOut.String() != wantErr {
		t.Errorf("stdout:\n%q\nstderr:\n%q\nwant stdout:\n%q\nwant stderr:\n%q", f.out.String(), f.errOut.String(), wantOut, wantErr)
	}
	unchanged(t, f.c, config)
}

// An orphan-only store (an .enc with no plaintext twin, and no plaintext):
// the first run writes .sops.yaml, then refuses the orphan. A rerun without
// --skip-orphans refuses it too, before it decides that nothing is left to
// do. An orphan whose envelope shows the recipient (here, a copy of an .enc
// that the run re-keyed) needs no re-key: the run with it changes nothing,
// without --skip-orphans and without a warning (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunRefusesAnOrphanOnlyStoreWithoutSkipOrphans(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	orphan := f.store + "/Secret.orphan.default.K.enc"
	finishedApp := f.store + "/Secret.app.default.K.enc"
	write(t, orphan, readFile(t, finishedApp))
	for _, p := range []string{"app", "db"} {
		for _, name := range []string{"Secret." + p + ".default.K", "Secret." + p + ".default.K.enc"} {
			if err := os.Remove(f.store + "/" + name); err != nil {
				t.Fatal(err)
			}
		}
	}
	refusal := "[warn] orphan (no decrypted twin, cannot re-key): " + f.c.rel(orphan) + "\n" +
		"[error] 1 encrypted file(s) have no decrypted twin — they would keep the OLD\n" +
		"[error]   recipient list while .sops.yaml claims the new key was added.\n" +
		"[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.\n"
	if err := f.run(); !errors.Is(err, ErrHandled) || f.errOut.String() != refusal {
		t.Fatalf("the first run: want ErrHandled and the refusal, got %v\n%s", err, f.errOut.String())
	}
	config := readFile(t, f.c.sopsConfigPath())
	if !strings.Contains(config, f.gone.Recipient().String()) {
		t.Fatal("fixture: the first run did not write .sops.yaml before it stopped")
	}
	states := fileStates(t, f.store)
	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("the rerun: want ErrHandled, got %v\n%s", err, f.out.String())
	}
	if f.out.String() != "" || f.errOut.String() != refusal {
		t.Errorf("the rerun: stdout %q\nstderr:\n%q\nwant stderr:\n%q", f.out.String(), f.errOut.String(), refusal)
	}
	sameStates(t, states, fileStates(t, f.store))
	unchanged(t, f.c, config)

	// The control: an orphan that shows the recipient.
	g := newLinkFixture(t, "add-key")
	g.finished()
	write(t, g.store+"/Secret.orphan.default.K.enc", readFile(t, g.store+"/Secret.app.default.K.enc"))
	if err := g.run(); err != nil {
		t.Fatalf("the control: %v\n%s", err, g.errOut.String())
	}
	if want := "recipient already present in .sops.yaml — nothing to add\n  " + g.gone.Recipient().String() + "\n"; g.out.String() != want || g.errOut.String() != "" {
		t.Errorf("the control: stdout %q\nstderr %q", g.out.String(), g.errOut.String())
	}
}

// A rerun after a finished run, with the .enc of app replaced by a symlink
// to the .enc of db (which shows the recipient) or by a directory: the run
// refuses it with the lines of the first run before it decides that
// nothing is left to do, and changes nothing (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunRefusesAnEncThatIsNotARegularFile(t *testing.T) {
	for _, kind := range []string{"a symlink", "a directory"} {
		t.Run(kind, func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			config := f.finished()
			app := f.store + "/Secret.app.default.K.enc"
			if err := os.Remove(app); err != nil {
				t.Fatal(err)
			}
			want := "[error] " + f.c.rel(app)
			if kind == "a symlink" {
				if err := os.Symlink("Secret.db.default.K.enc", app); err != nil {
					t.Fatal(err)
				}
				want += " is a symlink: the re-key does not write an .enc through a link\n" +
					"[error]   replace each link with the file it names, or remove it from the store, then run this command again; nothing was written\n"
			} else {
				mkdir(t, app)
				want += " is not a regular file (a directory, a FIFO, a socket or a device): the re-key does not write an .enc there\n" +
					"[error]   remove or fix each entry named above, then run this command again; nothing was written\n"
			}
			states := fileStates(t, f.store)
			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
			}
			if f.out.String() != "" || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, states, fileStates(t, f.store))
			unchanged(t, f.c, config)
		})
	}
}

// A rerun with nothing to do writes the .gitignore of the store when it is
// missing, as the first run does, before it says "nothing to add" (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunWritesAMissingGitignore(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	config := f.finished()
	if err := os.Remove(f.store + "/.gitignore"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	if want := "recipient already present in .sops.yaml — nothing to add\n  " + f.gone.Recipient().String() + "\n"; f.out.String() != want {
		t.Errorf("stdout %q", f.out.String())
	}
	if got := readFile(t, f.store+"/.gitignore"); got != litBlock {
		t.Errorf(".gitignore: %q", got)
	}
	unchanged(t, f.c, config)
}

// A store that it cannot list (no read permission: the lookup of the
// .gitignore in it still works) is refused before the first write, with the
// key in .sops.yaml and without it: an empty list would let the run report
// the re-key as done. (A store with no search permission is refused before,
// at the check of its .gitignore.) (bash:
// tests/unit/secrets_add_key_test.bats.)
func TestAddKeyRefusesAStoreThatItCannotList(t *testing.T) {
	skipUnlessPermissionsHold(t)
	for _, mode := range []fs.FileMode{0o300} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%#o/present=%v", mode, present), func(t *testing.T) {
				f := newLinkFixture(t, "add-key")
				if present {
					f.finished()
				}
				config := readFile(t, f.c.sopsConfigPath())
				states := fileStates(t, f.store)
				if err := os.Chmod(f.store, mode); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(f.store, 0o755) })
				err := f.run()
				if cerr := os.Chmod(f.store, 0o755); cerr != nil {
					t.Fatal(cerr)
				}
				if !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
				}
				want := "[error] cannot list the store " + f.c.rel(f.store) + " (not a directory, no read or search permission, or the read of the directory failed): add-key did not change .sops.yaml; fix the store, then run this command again\n"
				if f.out.String() != "" || f.errOut.String() != want {
					t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
				}
				sameStates(t, states, fileStates(t, f.store))
				unchanged(t, f.c, config)
			})
		}
	}
}

// An .enc larger than 16 MiB is refused on a rerun, with one line that
// names the file, before any change: the metadata read is bounded. The
// control: a file of exactly 16 MiB is read (it does not parse, so the
// rerun re-keys it) (bash: tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunRefusesAnEncLargerThanTheLimit(t *testing.T) {
	for _, size := range []int64{maxEncRead + 1, maxEncRead} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			config := f.finished()
			app := f.store + "/Secret.app.default.K.enc"
			if err := os.Truncate(app, size); err != nil {
				t.Fatal(err)
			}
			states := fileStates(t, f.store)
			err := f.run()
			if size == maxEncRead {
				if err != nil {
					t.Fatalf("the control: %v\n%s", err, f.errOut.String())
				}
				f.rekeyed("app", "v")
				return
			}
			if !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
			}
			want := "[error] " + f.c.rel(app) + " is larger than 16 MiB, the size limit of the metadata check: add-key does not read its sops metadata and stops here; restore or remove that file, then run this command again\n"
			if f.out.String() != "" || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, states, fileStates(t, f.store))
			unchanged(t, f.c, config)
		})
	}
}

// A write of the .gitignore that stops part way (the writeGitignore seam
// writes half the content, then fails): add-key stops before .sops.yaml, the
// old .gitignore stays, and no temp file is left. Only the line that names
// the file is printed, not the error of the write. The seam counts its
// calls, so the test knows that the write ran. The control: with the real
// write, the run passes and writes the .gitignore (bash:
// tests/unit/secrets_test.bats).
func TestAddKeyKeepsTheGitignoreOnAPartialWrite(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	write(t, f.store+"/.gitignore", "# old\n")
	before := readFile(t, f.c.sopsConfigPath())
	states := fileStates(t, f.store)
	orig := writeGitignore
	calls := 0
	writeGitignore = func(w io.Writer, want []byte) error {
		calls++
		if _, err := w.Write(want[:len(want)/2]); err != nil {
			return err
		}
		return errors.New("no space left on device")
	}
	t.Cleanup(func() { writeGitignore = orig })

	if err := f.run(); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
	}
	want := "[error] cannot write " + f.c.rel(f.store) + "/.gitignore: it was not changed\n" +
		"[error]   add-key did not change .sops.yaml or an .enc file: fix the cause above, then run this command again\n"
	if f.errOut.String() != want || f.out.String() != "" {
		t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
	}
	unchanged(t, f.c, before)
	if calls != 1 {
		t.Fatalf("the seam ran %d times, want 1", calls)
	}
	// The temp file was made and removed: the time of the store directory
	// changes, and each entry in it stays as it was.
	after := fileStates(t, f.store)
	delete(states, f.store)
	delete(after, f.store)
	sameStates(t, states, after)

	writeGitignore = orig
	if err := f.run(); err != nil {
		t.Fatalf("the control: %v\n%s", err, f.errOut.String())
	}
	// The old file has no block: the block goes on top of its line.
	if got := readFile(t, f.store+"/.gitignore"); got != litBlock+"# old\n" {
		t.Errorf("the control: .gitignore %q", got)
	}
	f.rekeyed("app", "v")
}

// The resolution of .sops.yaml fails closed: a link that names no file, or a
// directory, is refused with one line, and no path comes back to lock
// beside (bash: secrets::_config_dest).
func TestSopsConfigDestRefusesALinkThatNamesNoRegularFile(t *testing.T) {
	for _, target := range []string{"gone.yaml", "dir"} {
		t.Run(target, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			mkdir(t, c.Paths.Base+"/dir")
			if err := os.Symlink(target, c.sopsConfigPath()); err != nil {
				t.Fatal(err)
			}
			dest, err := c.sopsConfigDest()
			if !errors.Is(err, ErrHandled) || dest != "" {
				t.Fatalf("want ErrHandled and no path, got %q, %v", dest, err)
			}
			want := "[error] " + c.sopsConfigPath() + " does not resolve to a regular file — nothing was changed; run this command again\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
		})
	}
}

// The lock and the write name one file. A run resolves the .sops.yaml link
// once: when the link names another file while the run waits for the lock,
// the run still reads and writes the file beside the lock that it took, and
// the other file keeps its bytes. The re-key reads the creation rules
// through the link, so b.yaml holds the rules of the edit already (plus a
// comment that the edit of a.yaml does not have).
func TestKeyCommandsWriteTheFileThatTheyLocked(t *testing.T) {
	for _, cmd := range []string{"add-key", "remove-key"} {
		t.Run(cmd, func(t *testing.T) {
			f := newLinkFixture(t, cmd)
			dir := mkdir(t, f.c.Paths.Base+"/conf")
			a, b := dir+"/a.yaml", dir+"/b.yaml"
			if err := os.Rename(f.c.sopsConfigPath(), a); err != nil {
				t.Fatal(err)
			}
			rules := strings.Replace(readFile(t, a), ","+f.gone.Recipient().String(), "", 1)
			write(t, b, "# b.yaml\n"+rules)
			if err := os.Symlink("conf/a.yaml", f.c.sopsConfigPath()); err != nil {
				t.Fatal(err)
			}
			t.Setenv(lockWaitEnv, "5")
			held, err := os.OpenFile(a+".lock", os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = held.Close() })
			if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			// The first wait for the lock points the link at b.yaml, then
			// frees the lock beside a.yaml.
			waits := 0
			saved := lockSleep
			t.Cleanup(func() { lockSleep = saved })
			lockSleep = func(context.Context, time.Duration) error {
				waits++
				if waits == 1 {
					if err := os.Remove(f.c.sopsConfigPath()); err != nil {
						t.Error(err)
					}
					if err := os.Symlink("conf/b.yaml", f.c.sopsConfigPath()); err != nil {
						t.Error(err)
					}
					if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
						t.Error(err)
					}
				}
				return nil
			}
			wantB := readFile(t, b)

			if err := f.run(); err != nil {
				t.Fatalf("%v\n%s", err, f.errOut.String())
			}
			if waits == 0 {
				t.Fatal("the run did not wait for the lock")
			}
			if got := strings.Contains(readFile(t, a), f.gone.Recipient().String()); got != (cmd == "add-key") {
				t.Errorf("the file beside the lock was not edited:\n%s", readFile(t, a))
			}
			if got := readFile(t, b); got != wantB {
				t.Errorf("the file that the link names after the lock was written:\n%s", got)
			}
			if target, err := os.Readlink(f.c.sopsConfigPath()); err != nil || target != "conf/b.yaml" {
				t.Errorf("the link names %q (%v), want conf/b.yaml", target, err)
			}
			if _, err := os.Lstat(b + ".lock"); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("a lock beside b.yaml: %v", err)
			}
		})
	}
}

// failListing makes each read of the directory dir after the first after
// reads fail with EIO (the readDir seam), and returns the count of the reads
// of dir.
func failListing(t *testing.T, dir string, after int) *int {
	t.Helper()
	orig := readDir
	reads := 0
	readDir = func(name string) ([]os.DirEntry, error) {
		if name == dir {
			reads++
			if reads > after {
				return nil, &fs.PathError{Op: "readdirent", Path: name, Err: syscall.EIO}
			}
		}
		return orig(name)
	}
	t.Cleanup(func() { readDir = orig })
	return &reads
}

// unlistable is the refusal of a store that add-key cannot list.
func unlistable(c *Context, store, outcome string) string {
	return "[error] cannot list the store " + c.rel(store) + " (not a directory, no read or search permission, or the read of the directory failed): " + outcome + "; fix the store, then run this command again\n"
}

// A read of the store directory that fails (the readDir seam: EIO, with the
// mode and the search of the directory as they are) is refused before the
// first write, with the key in .sops.yaml and without it: an empty list
// would let the run report the re-key as done. Nothing changes (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyRefusesAStoreWhoseListingFails(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			if present {
				f.finished()
			}
			config := readFile(t, f.c.sopsConfigPath())
			id := configFile(t, f.c)
			states := fileStates(t, f.store)
			reads := failListing(t, f.store, 0)
			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
			}
			want := unlistable(f.c, f.store, "add-key did not change .sops.yaml")
			if f.out.String() != "" || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			if *reads != 1 {
				t.Errorf("the store was read %d times, want 1", *reads)
			}
			sameStates(t, states, fileStates(t, f.store))
			unchanged(t, f.c, config)
			sameConfigFile(t, f.c, id)
		})
	}
}

// add-key lists the store of the sweep once (the readDir seam counts the
// reads of the store, and fails each read after the first): the first run
// re-keys both files from that listing, and so does a rerun that finishes
// a re-key that an earlier run left. A second listing would find an empty
// store and report the re-key as done (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyListsTheStoreOnce(t *testing.T) {
	t.Run("the first run", func(t *testing.T) {
		f := newLinkFixture(t, "add-key")
		gone := f.gone.Recipient().String()
		reads := failListing(t, f.store, 1)
		if err := f.run(); err != nil {
			t.Fatalf("%v\n%s", err, f.errOut.String())
		}
		want := "added recipient to .sops.yaml (1 creation rule(s))\n  " + gone + "\n" +
			"re-keying 2 cache file(s) across 1 store(s)\u2026\n" +
			"Encrypted 2 secret(s)\n" +
			"done \u2014 commit .sops.yaml and the updated .enc files together\n"
		if f.out.String() != want || f.errOut.String() != "" {
			t.Errorf("stdout:\n%q\nstderr %q\nwant stdout:\n%q", f.out.String(), f.errOut.String(), want)
		}
		f.rekeyed("app", "v")
		f.rekeyed("db", "w")
		if *reads != 1 {
			t.Errorf("the store was read %d times, want 1", *reads)
		}
	})
	t.Run("a rerun", func(t *testing.T) {
		f := newLinkFixture(t, "add-key")
		gone := f.gone.Recipient().String()
		f.finished()
		if err := os.Remove(f.store + "/Secret.app.default.K.enc"); err != nil {
			t.Fatal(err)
		}
		reads := failListing(t, f.store, 1)
		if err := f.run(); err != nil {
			t.Fatalf("%v\n%s", err, f.errOut.String())
		}
		want := "recipient already present in .sops.yaml, but 1 .enc file(s) of the store are missing or not re-keyed for it: finishing the re-key\n  " + gone + "\n" +
			"Encrypted 1 secret(s)\n" +
			"done \u2014 commit .sops.yaml and the updated .enc files together\n"
		if f.out.String() != want || f.errOut.String() != "" {
			t.Errorf("stdout:\n%q\nstderr %q\nwant stdout:\n%q", f.out.String(), f.errOut.String(), want)
		}
		f.rekeyed("app", "v")
		if *reads != 1 {
			t.Errorf("the store was read %d times, want 1", *reads)
		}
	})
}

// With --all, each other store is listed once, with its error checked: a
// listing that fails is refused with a line that names that store. A rerun
// refuses it before any write. The first run refuses it after its write of
// .sops.yaml (the orphan check of the first run comes after that write),
// with no .enc re-keyed and no plaintext touched (bash:
// tests/unit/secrets_add_key_test.bats).
func TestAddKeyAllRefusesAnotherStoreWhoseListingFails(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			gone := f.gone.Recipient().String()
			other := f.c.Paths.Clusters + "/b.dev/secrets"
			write(t, other+"/Secret.x.default.K", "x")
			if present {
				f.finished()
			}
			config := readFile(t, f.c.sopsConfigPath())
			states := fileStates(t, f.store, other)
			reads := failListing(t, other, 0)
			f.out.Reset()
			f.errOut.Reset()
			if err := f.c.AddKey(gone, true, false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.errOut.String())
			}
			wantOut := "added recipient to .sops.yaml (1 creation rule(s))\n  " + gone + "\n"
			outcome := "add-key wrote .sops.yaml and stopped before the re-key"
			if present {
				wantOut, outcome = "", "add-key did not change .sops.yaml"
			}
			if want := unlistable(f.c, other, outcome); f.out.String() != wantOut || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant stdout %q\nwant stderr:\n%q", f.out.String(), f.errOut.String(), wantOut, want)
			}
			if *reads != 1 {
				t.Errorf("the other store was read %d times, want 1", *reads)
			}
			sameStates(t, states, fileStates(t, f.store, other))
			if present {
				unchanged(t, f.c, config)
			} else if !strings.Contains(readFile(t, f.c.sopsConfigPath()), gone) {
				t.Error("the first run did not write .sops.yaml before the orphan check")
			}
		})
	}
}

// An orphan .enc that is a symlink to a path that does not exist is in the
// store, and a re-key cannot rewrite it: add-key counts it as an orphan (it
// does not skip it). The first run and a rerun refuse it without
// --skip-orphans; with --skip-orphans the rerun finishes the re-key and
// warns about it (bash: tests/unit/secrets_add_key_test.bats).
func TestAddKeyCountsADanglingOrphanLink(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	gone := f.gone.Recipient().String()
	orphan := f.store + "/Secret.orphan.default.K.enc"
	if err := os.Symlink("Secret.missing.enc", orphan); err != nil {
		t.Fatal(err)
	}
	warning := "[warn] orphan (no decrypted twin, cannot re-key): " + f.c.rel(orphan) + "\n"
	refusal := warning +
		"[error] 1 encrypted file(s) have no decrypted twin \u2014 they would keep the OLD\n" +
		"[error]   recipient list while .sops.yaml claims the new key was added.\n" +
		"[error]   Run 'lo secrets decrypt' first, or re-run with --skip-orphans to accept it.\n"
	if err := f.run(); !errors.Is(err, ErrHandled) || f.errOut.String() != refusal {
		t.Fatalf("the first run: want ErrHandled and the refusal, got %v\n%q", err, f.errOut.String())
	}
	if !strings.Contains(readFile(t, f.c.sopsConfigPath()), gone) {
		t.Fatal("fixture: the first run did not write .sops.yaml before it stopped")
	}
	states := fileStates(t, f.store)
	if err := f.run(); !errors.Is(err, ErrHandled) || f.out.String() != "" || f.errOut.String() != refusal {
		t.Errorf("the rerun: want ErrHandled and the refusal, got %v\nstdout %q\nstderr:\n%q", err, f.out.String(), f.errOut.String())
	}
	sameStates(t, states, fileStates(t, f.store))

	f.out.Reset()
	f.errOut.Reset()
	if err := f.c.AddKey(gone, false, true); err != nil {
		t.Fatalf("the run with --skip-orphans: %v\n%s", err, f.errOut.String())
	}
	if f.errOut.String() != warning {
		t.Errorf("the run with --skip-orphans: stderr %q, want %q", f.errOut.String(), warning)
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
}

// The shared fixture of the envelope parser (bash: the same fixture in
// tests/unit/secrets_add_key_test.bats): after a finished run, the .enc of
// app holds the good entry of the recipient and, before it, an entry of
// another recipient whose enc is spelled "ENC", and the plaintext of app
// holds another value than the .enc. Only the entries of the recipient are
// read for their enc, so the rerun finds nothing to do: it does not re-key
// that .enc from the plaintext, and the .enc keeps its bytes.
func TestAddKeyRerunReadsOnlyTheEntriesOfTheRecipient(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	config := f.finished()
	id := configFile(t, f.c)
	app := f.store + "/Secret.app.default.K.enc"
	envelope(t, app, func(_, meta map[string]any) {
		// The entry of the other recipient comes first: a parser that reads
		// the enc of every entry meets it before the entry of the key.
		list, _ := meta["age"].([]any)
		meta["age"] = append([]any{map[string]any{"recipient": keyA, "ENC": armor.Header + "\nx\n" + armor.Footer + "\n"}}, list...)
	})
	write(t, f.store+"/Secret.app.default.K", "not the value of the .enc")
	states := fileStates(t, f.store)
	if err := f.run(); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	if want := "recipient already present in .sops.yaml \u2014 nothing to add\n  " + f.gone.Recipient().String() + "\n"; f.out.String() != want || f.errOut.String() != "" {
		t.Errorf("stdout %q\nstderr %q\nwant stdout %q", f.out.String(), f.errOut.String(), want)
	}
	sameStates(t, states, fileStates(t, f.store))
	unchanged(t, f.c, config)
	sameConfigFile(t, f.c, id)
}

// .sops.yaml is a link to the .gitignore of the store, which holds valid
// rule YAML (with the key, and without it). That .gitignore has no allow
// line, so its write would replace the file of .sops.yaml. add-key refuses
// before it changes a file: the file keeps its bytes, and .sops.yaml stays
// a link (bash: the same fixture in tests/unit/secrets_add_key_test.bats).
func TestAddKeyRefusesAConfigThatIsTheGitignore(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			if present {
				f.finished()
			}
			sops := f.c.sopsConfigPath()
			gitignore := f.store + "/.gitignore"
			write(t, gitignore, readFile(t, sops))
			if err := os.Remove(sops); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(gitignore, sops); err != nil {
				t.Fatal(err)
			}
			// The lock sits beside the file that .sops.yaml names.
			write(t, gitignore+".lock", "")
			dest, err := f.c.sopsConfigDest()
			if err != nil {
				t.Fatal(err)
			}
			config := readFile(t, gitignore)
			states := fileStates(t, f.store)
			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
			}
			want := "[error] .sops.yaml names " + f.c.rel(dest) + ", and add-key writes that file as " + f.c.rel(gitignore) + ": add-key stopped before it changed .sops.yaml, the .gitignore or an .enc file, but the lock file " + f.c.rel(dest) + ".lock can be new; give .sops.yaml a file of its own, then run this command again\n"
			if f.out.String() != "" || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, states, fileStates(t, f.store))
			if got := readFile(t, gitignore); got != config {
				t.Errorf("the file of .sops.yaml changed: %q", got)
			}
			if info, err := os.Lstat(sops); err != nil || info.Mode()&fs.ModeSymlink == 0 {
				t.Errorf(".sops.yaml is no longer a link: %v", err)
			}
		})
	}
}

// A read of the store that returns entries together with an error (the
// readDir seam: the entries of the directory and EIO, as a read that fails
// after a first batch) is refused as a read that returns no entry: a part
// of the store is not the store. Nothing changes, with the key in
// .sops.yaml and without it (bash: a find that prints a name and then
// fails, in tests/unit/secrets_add_key_test.bats).
func TestAddKeyRefusesAListingThatFailsAfterSomeEntries(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(fmt.Sprintf("present=%v", present), func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			if present {
				f.finished()
			}
			config := readFile(t, f.c.sopsConfigPath())
			id := configFile(t, f.c)
			states := fileStates(t, f.store)
			orig := readDir
			returned := 0
			readDir = func(name string) ([]os.DirEntry, error) {
				entries, err := orig(name)
				if name != f.store || err != nil {
					return entries, err
				}
				returned = len(entries)
				return entries, &fs.PathError{Op: "readdirent", Path: name, Err: syscall.EIO}
			}
			t.Cleanup(func() { readDir = orig })
			if err := f.run(); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
			}
			if returned == 0 {
				t.Fatal("fixture: the seam returned no entry of the store")
			}
			want := unlistable(f.c, f.store, "add-key did not change .sops.yaml")
			if f.out.String() != "" || f.errOut.String() != want {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, states, fileStates(t, f.store))
			unchanged(t, f.c, config)
			sameConfigFile(t, f.c, id)
		})
	}
}

// After a finished run, the .enc of app holds two entries of the recipient:
// the good one, and one whose enc is not an armored age block, first or
// last. Any good entry of the recipient passes the envelope check, so the
// rerun finds nothing to do in both orders, and the .enc keeps its bytes
// (bash: the same fixture in tests/unit/secrets_add_key_test.bats).
func TestAddKeyRerunPassesWithOneGoodEntryOfTheKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first bool
	}{{"the bad entry first", true}, {"the bad entry last", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkFixture(t, "add-key")
			config := f.finished()
			id := configFile(t, f.c)
			gone := f.gone.Recipient().String()
			envelope(t, f.store+"/Secret.app.default.K.enc", func(_, meta map[string]any) {
				list, _ := meta["age"].([]any)
				bad := map[string]any{"recipient": gone, "enc": "x"}
				if tc.first {
					meta["age"] = append([]any{bad}, list...)
				} else {
					meta["age"] = append(list, bad)
				}
			})
			write(t, f.store+"/Secret.app.default.K", "not the value of the .enc")
			states := fileStates(t, f.store)
			if err := f.run(); err != nil {
				t.Fatalf("%v\n%s", err, f.errOut.String())
			}
			if want := "recipient already present in .sops.yaml — nothing to add\n  " + gone + "\n"; f.out.String() != want || f.errOut.String() != "" {
				t.Errorf("stdout %q\nstderr %q\nwant stdout %q", f.out.String(), f.errOut.String(), want)
			}
			sameStates(t, states, fileStates(t, f.store))
			unchanged(t, f.c, config)
			sameConfigFile(t, f.c, id)
		})
	}
}

// .sops.yaml is a hard link to the .gitignore of the store, or to an .enc
// of the sweep, and that file holds valid rule YAML (with the key, and
// without it). os.SameFile compares the files, so a hard link counts, and
// an .enc of the sweep counts as the .gitignore does. add-key refuses
// before it changes .sops.yaml, the .gitignore or an .enc file: the files
// of the store keep their state, and .sops.yaml stays that file (bash: the
// same fixture in tests/unit/secrets_add_key_test.bats).
func TestAddKeyRefusesAConfigThatIsAHardLinkOfAFileThatItWrites(t *testing.T) {
	for _, target := range []string{".gitignore", "Secret.app.default.K.enc"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/present=%v", target, present), func(t *testing.T) {
				f := newLinkFixture(t, "add-key")
				if present {
					f.finished()
				}
				sops := f.c.sopsConfigPath()
				out := f.store + "/" + target
				write(t, out, readFile(t, sops))
				if err := os.Remove(sops); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(out, sops); err != nil {
					t.Fatal(err)
				}
				dest, err := f.c.sopsConfigDest()
				if err != nil {
					t.Fatal(err)
				}
				config := readFile(t, sops)
				states := fileStates(t, f.store)
				if err := f.run(); !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, f.out.String())
				}
				want := "[error] .sops.yaml names " + f.c.rel(dest) + ", and add-key writes that file as " + f.c.rel(out) + ": add-key stopped before it changed .sops.yaml, the .gitignore or an .enc file, but the lock file " + f.c.rel(dest) + ".lock can be new; give .sops.yaml a file of its own, then run this command again\n"
				if f.out.String() != "" || f.errOut.String() != want {
					t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", f.out.String(), f.errOut.String(), want)
				}
				sameStates(t, states, fileStates(t, f.store))
				if got := readFile(t, sops); got != config {
					t.Errorf(".sops.yaml changed: %q", got)
				}
				conf, err1 := os.Stat(sops)
				info, err2 := os.Stat(out)
				if err1 != nil || err2 != nil || !os.SameFile(conf, info) {
					t.Errorf(".sops.yaml is no longer the file of %s: %v %v", f.c.rel(out), err1, err2)
				}
			})
		}
	}
}
