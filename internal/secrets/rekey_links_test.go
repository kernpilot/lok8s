package secrets

// rekey_links_test.go: add-key, remove-key and symlinks. The scan of the
// stores follows a symlinked .enc, but the re-key never writes through one
// (encryptFile refuses it), so a link in the files that a run re-keys is
// refused before the first write. add-key refuses every other .enc that the
// encrypt refuses too (a directory, a FIFO, a socket, a path that it cannot
// look up), and its encrypt reads the list that it checked. A .sops.yaml
// that is a symlink stays a link: both commands write the edit to its
// target.
// tests/unit/secrets_add_key_test.bats and
// tests/unit/secrets_remove_key_test.bats run the same cases through the bash
// twin.

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"

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
			write(t, f.c.sopsConfigLock(), "")
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
	write(t, f.c.sopsConfigLock(), "")
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
	write(t, f.c.sopsConfigLock(), "")
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
			write(t, f.c.sopsConfigLock(), "")
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
	write(t, f.c.sopsConfigLock(), "")
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
			if entries, _ := os.ReadDir(filepath.Dir(conf)); len(entries) != 1 {
				t.Errorf("a temp file was left: %d entries", len(entries))
			}
			f.rekeyed("app", "v")
		})
	}
}
