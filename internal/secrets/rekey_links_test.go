package secrets

// rekey_links_test.go: add-key, remove-key and symlinks. The scan of the
// stores follows a symlinked .enc, but the re-key never writes through one
// (encryptFile refuses it), so a link in the files that a run re-keys is
// refused before the first write. A .sops.yaml that is a symlink stays a
// link: both commands write the edit to its target.
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
	"testing"
	"time"

	"filippo.io/age"
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
