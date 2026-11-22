package cache

// The cache writes a store entry through storeguard, the one plaintext write
// of a store that `lo secrets set` and `lo secrets decrypt` use too. These
// tests run the cache API against the real store layout in a temp dir:
// clusters/<domain>/secrets, made by New.

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/kustomize/pkg/storeguard"
)

// block is the lok8s block of a store .gitignore, as a literal: a change of
// the block text must change this test too.
const block = "# BEGIN lok8s secrets. lo rewrites the lines up to the END line: add your own rules below it.\n" +
	"# Ignore everything except this file and the SOPS-encrypted secret files.\n" +
	"# A plaintext secret file must never be committed.\n" +
	"*\n" +
	"!.gitignore\n" +
	"!Secret.*.enc\n" +
	"*/\n" +
	"# END lok8s secrets\n"

// rel is the store path of a refusal: the per-domain store of newStore,
// relative to its project, as `lo secrets set` prints it.
const rel = "clusters/alpha.dev/secrets/"

// newStore makes the cache of the Secret app in namespace default, in a new
// per-domain store under a temp project, and returns it with its directory.
func newStore(t *testing.T) (*Cache, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	c, err := New(dir, "default", "app")
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// stopLines returns the lines of err, a *storeguard.Stop, or fails the test.
func stopLines(t *testing.T, err error) []string {
	t.Helper()
	s, ok := errors.AsType[*storeguard.Stop](err)
	if !ok {
		t.Fatalf("want a *storeguard.Stop, got %T: %v", err, err)
	}
	return s.Lines
}

// names returns the sorted names of the entries of dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func readString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeString(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

// A generator run into a store with no .gitignore writes the lok8s block
// before the plaintext, as `lo secrets set` does: when the write of the
// .gitignore fails, no plaintext is written. A .gitignore with rules of the
// user and no block gets the block on top, and each line of the user stays
// after it. A store whose .gitignore has the block already is not
// rewritten: the file keeps its inode and its time.
func TestPutWritesTheGitignoreBeforeThePlaintext(t *testing.T) {
	// The order: the .gitignore write fails part way, and the store gets
	// no plaintext and no temp file.
	cf, dirf := newStore(t)
	cf.fillGitignore = func(w io.Writer, want []byte) error {
		if _, err := w.Write(want[:10]); err != nil {
			return err
		}
		return errors.New("the disk is full")
	}
	_, err := cf.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("minted"), nil })
	if got, want := stopLines(t, err), []string{"cannot write " + rel + ".gitignore: it was not changed"}; !slices.Equal(got, want) {
		t.Errorf("failed .gitignore write:\n%q\nwant:\n%q", got, want)
	}
	if got := names(t, dirf); len(got) != 0 {
		t.Errorf("store %q: a plaintext or a temp file was written without the .gitignore", got)
	}

	c, dir := newStore(t)
	got, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("minted"), nil })
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "minted" {
		t.Errorf("value %q", got)
	}
	if g := readString(t, filepath.Join(dir, ".gitignore")); g != block {
		t.Errorf(".gitignore:\n%q\nwant:\n%q", g, block)
	}
	if m := mode(t, filepath.Join(dir, ".gitignore")); m != 0o644 {
		t.Errorf(".gitignore mode %v, want 0644", m)
	}
	if m := mode(t, filepath.Join(dir, "Secret.app.default.PASSWORD")); m != 0o600 {
		t.Errorf("entry mode %v, want 0600", m)
	}
	if want := []string{".gitignore", "Secret.app.default.PASSWORD"}; !slices.Equal(names(t, dir), want) {
		t.Errorf("store %q, want %q (no temp file)", names(t, dir), want)
	}

	// The block is there: a second write leaves the file as it is. A
	// rewrite is a new file (a temp file and a rename) with the time of now.
	gitignore := filepath.Join(dir, ".gitignore")
	past := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(gitignore, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put("USER", []byte("u")); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !after.ModTime().Equal(past) || after.Mode() != before.Mode() {
		t.Errorf("the second write rewrote the .gitignore: same file %v, time %v, mode %v", os.SameFile(before, after), after.ModTime(), after.Mode())
	}
	if g := readString(t, gitignore); g != block {
		t.Errorf("the second write changed the .gitignore: %q", g)
	}

	// The rules of the user stay, after the block.
	c2, dir2 := newStore(t)
	writeString(t, filepath.Join(dir2, ".gitignore"), "*.log\n# mine\n")
	if err := c2.Put("PASSWORD", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if g, want := readString(t, filepath.Join(dir2, ".gitignore")), block+"*.log\n# mine\n"; g != want {
		t.Errorf(".gitignore of the user:\n%q\nwant:\n%q", g, want)
	}
}

// A store whose .gitignore has a rule that can re-include a plaintext is
// refused before any write, with the lines of `lo secrets set`: the store
// gets no plaintext and no temp file, and the .gitignore keeps its bytes.
// A .gitignore that is a symlink is refused the same way.
func TestPutRefusesAnUnsafeGitignoreBeforeAnyWrite(t *testing.T) {
	c, dir := newStore(t)
	gitignore := filepath.Join(dir, ".gitignore")
	unsafe := block + "!Secret.app.default.PASSWORD\n"
	writeString(t, gitignore, unsafe)

	_, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("minted"), nil })
	want := []string{
		rel + ".gitignore:9: this negation can re-include a plaintext secret file (the last matching rule wins): remove the line, or keep the file outside the store",
		rel + ".gitignore was not changed, and lo stopped before the plaintext write: fix each cause above, then run this command again",
	}
	if got := stopLines(t, err); !slices.Equal(got, want) {
		t.Errorf("refusal:\n%q\nwant:\n%q", got, want)
	}
	if strings.Contains(err.Error(), "minted") {
		t.Errorf("the refusal prints the value: %v", err)
	}
	if got := names(t, dir); !slices.Equal(got, []string{".gitignore"}) {
		t.Errorf("store %q: a refused write left a file", got)
	}
	if g := readString(t, gitignore); g != unsafe {
		t.Errorf("the refused .gitignore changed: %q", g)
	}

	// A .gitignore that is a symlink: git does not read it, and a write
	// would go through it.
	c2, dir2 := newStore(t)
	other := filepath.Join(t.TempDir(), "rules")
	writeString(t, other, block)
	if err := os.Symlink(other, filepath.Join(dir2, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	err = c2.Put("PASSWORD", []byte("v"))
	wantLink := []string{rel + ".gitignore is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before the plaintext write — remove it, then run this command again"}
	if got := stopLines(t, err); !slices.Equal(got, wantLink) {
		t.Errorf("refusal:\n%q\nwant:\n%q", got, wantLink)
	}
	if got := names(t, dir2); !slices.Equal(got, []string{".gitignore"}) {
		t.Errorf("store %q: a refused write left a file", got)
	}
}

// encKeyRefusal is the refusal of `lo secrets set` for the key key of the
// Secret app in namespace default, whose entry name ends in .enc.
func encKeyRefusal(key string) []string {
	return []string{"the key '" + key + "' is refused: the plaintext file Secret.app.default." + key + " would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"}
}

// A key whose entry name ends in .enc, in any letter case, is refused with
// the text of `lo secrets set`: the rule !Secret.*.enc of the block
// re-includes the name, and git would commit the plaintext. GetOrCreate
// refuses before it generates, and nothing is written (no .gitignore
// either). A key that ends in ENC without the dot is written.
func TestPutRefusesAKeyThatEndsInEnc(t *testing.T) {
	for _, key := range []string{"enc", "ENC", "password.enc", "Password.Enc"} {
		t.Run(key, func(t *testing.T) {
			c, dir := newStore(t)
			called := false
			_, err := c.GetOrCreate(key, func() ([]byte, error) {
				called = true
				return []byte("minted"), nil
			})
			want := encKeyRefusal(key)
			if got := stopLines(t, err); !slices.Equal(got, want) {
				t.Errorf("GetOrCreate refusal:\n%q\nwant:\n%q", got, want)
			}
			if called {
				t.Error("GetOrCreate generated a value for a key that it cannot write")
			}
			if got := stopLines(t, c.Put(key, []byte("v"))); !slices.Equal(got, want) {
				t.Errorf("Put refusal:\n%q\nwant:\n%q", got, want)
			}
			if got := names(t, dir); len(got) != 0 {
				t.Errorf("store %q: a refused key wrote a file", got)
			}
		})
	}
	c, dir := newStore(t)
	if err := c.Put("NOT_ENC", []byte("v")); err != nil {
		t.Fatalf("the control: %v", err)
	}
	if got := readString(t, filepath.Join(dir, "Secret.app.default.NOT_ENC")); got != "v" {
		t.Errorf("the control: %q", got)
	}
}

// A name, a namespace or a key that makes the entry name a path is refused
// with the text of `lo secrets set`, and nothing is written.
func TestPutRefusesAPathPart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	for _, tc := range []struct {
		name, namespace, key, want string
	}{
		{"app", "default", "anchor/../.gitignore", "the key 'anchor/../.gitignore' is refused: it has a / or a \\, which makes the file name a path. Nothing was written: use a key without it"},
		{"app", "default", "..", "the key '..' is refused: it is . or .., a part of a path. Nothing was written: use a key without it"},
		{"a/b", "default", "K", "the name 'a/b' is refused: it has a / or a \\, which makes the file name a path. Nothing was written: use a name without it"},
		{"app", "ns\x1b", "K", "the namespace 'ns?' is refused: it has a control byte. Nothing was written: use a namespace without it"},
	} {
		c, err := New(dir, tc.namespace, tc.name)
		if err != nil {
			t.Fatal(err)
		}
		if got := stopLines(t, c.Put(tc.key, []byte("v"))); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%q/%q/%q:\n%q\nwant %q", tc.name, tc.namespace, tc.key, got, tc.want)
		}
	}
	if got := names(t, dir); len(got) != 0 {
		t.Errorf("store %q: a refused part wrote a file", got)
	}
}

// A symlink at the entry is refused and not followed: the link stays, and
// the file that it names keeps its content. That file can be the
// ciphertext of the store or its .gitignore, which git commits. A dangling
// link, a FIFO and a directory are refused with the same text. The rename
// would replace a link and a FIFO, so only the check refuses them; the
// rename of a file onto a directory fails, and the text tells the check
// from that failure.
func TestPutDoesNotFollowALink(t *testing.T) {
	entry := "Secret.app.default.PASSWORD"
	for _, tc := range []struct {
		name   string
		target func(dir string) string
	}{
		{"to the ciphertext", func(dir string) string { return filepath.Join(dir, entry+".enc") }},
		{"to the .gitignore", func(dir string) string { return filepath.Join(dir, ".gitignore") }},
		{"outside the store", func(string) string { return filepath.Join(t.TempDir(), "outside") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := newStore(t)
			writeString(t, filepath.Join(dir, ".gitignore"), block)
			target := tc.target(dir)
			if target != filepath.Join(dir, ".gitignore") {
				writeString(t, target, "keep")
			}
			before := readString(t, target)
			link := filepath.Join(dir, entry)
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			err := c.Put("PASSWORD", []byte("the-value"))
			want := []string{rel + entry + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before the plaintext write — remove it, then run this command again"}
			if got := stopLines(t, err); !slices.Equal(got, want) {
				t.Errorf("refusal:\n%q\nwant:\n%q", got, want)
			}
			if got := readString(t, target); got != before {
				t.Errorf("the write went through the link: %q", got)
			}
			if mode(t, link)&os.ModeSymlink == 0 {
				t.Error("the link was replaced")
			}
		})
	}

	notRegular := func(base string) []string {
		return []string{rel + base + " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: lo stopped before the plaintext write — remove it, then run this command again"}
	}
	c, dir := newStore(t)
	dangling := filepath.Join(dir, entry)
	if err := os.Symlink(filepath.Join(dir, "gone"), dangling); err != nil {
		t.Fatal(err)
	}
	_, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("minted"), nil })
	if got := stopLines(t, err); !slices.Equal(got, notRegular(entry)) {
		t.Errorf("dangling link:\n%q\nwant:\n%q", got, notRegular(entry))
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the write made the target of a dangling link: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "Secret.app.default.FIFO"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stopLines(t, c.Put("FIFO", []byte("v"))); !slices.Equal(got, notRegular("Secret.app.default.FIFO")) {
		t.Errorf("FIFO:\n%q\nwant:\n%q", got, notRegular("Secret.app.default.FIFO"))
	}
	if mode(t, filepath.Join(dir, "Secret.app.default.FIFO"))&os.ModeNamedPipe == 0 {
		t.Error("the FIFO was replaced")
	}
	if err := os.Mkdir(filepath.Join(dir, "Secret.app.default.DIR"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := stopLines(t, c.Put("DIR", []byte("v"))); !slices.Equal(got, notRegular("Secret.app.default.DIR")) {
		t.Errorf("directory:\n%q\nwant:\n%q", got, notRegular("Secret.app.default.DIR"))
	}
}

// GetOrCreate runs the checks of the write before it generates a value: a
// generator (a bash: command has side effects) does not run for a write
// that Put refuses. The store keeps its files as they were.
func TestGetOrCreateRunsNoGeneratorForARefusedWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"a negation below the block", func(t *testing.T, dir string) {
			writeString(t, filepath.Join(dir, ".gitignore"), block+"!README.md\n")
		}},
		{"a .gitignore that is a symlink", func(t *testing.T, dir string) {
			other := filepath.Join(t.TempDir(), "rules")
			writeString(t, other, block)
			if err := os.Symlink(other, filepath.Join(dir, ".gitignore")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a symlink at the entry", func(t *testing.T, dir string) {
			writeString(t, filepath.Join(dir, ".gitignore"), block)
			if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, "Secret.app.default.PASSWORD")); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, dir := newStore(t)
			tc.setup(t, dir)
			before := dirListing(t, dir)
			calls := 0
			_, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) {
				calls++
				return []byte("minted"), nil
			})
			stopLines(t, err)
			if calls != 0 {
				t.Errorf("the generator ran %d times for a refused write", calls)
			}
			if after := dirListing(t, dir); after != before {
				t.Errorf("the store changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
	c, _ := newStore(t)
	calls := 0
	if _, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { calls++; return []byte("v"), nil }); err != nil || calls != 1 {
		t.Fatalf("the control: %v, %d calls", err, calls)
	}
}

// A dry run (the overlay) runs the checks of the real write and refuses
// what the real run refuses, with the same text: a key whose entry name
// ends in .enc and a store whose .gitignore cannot keep the plaintext out
// of git. It writes nothing: no store, no .gitignore, no file of the run
// store, no line of the report. A store with no .gitignore passes, and the
// dry run does not write the block.
func TestDryRunRefusesWhatTheRealRunRefuses(t *testing.T) {
	open := func(t *testing.T, store string) (Store, string, string) {
		t.Helper()
		root := t.TempDir()
		report, run := filepath.Join(root, "report"), filepath.Join(root, "run")
		writeString(t, report, "")
		s, err := Open(store, "default", "app", DryRunFromEnv(dryRunEnvStore(report, run)))
		if err != nil {
			t.Fatal(err)
		}
		return s, report, run
	}
	nothingKept := func(t *testing.T, report, run string) {
		t.Helper()
		if got := readString(t, report); got != "" {
			t.Errorf("report %q, want empty", got)
		}
		if _, err := os.Stat(run); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the run store has a value: %v", err)
		}
	}
	gen := func(calls *int) func() ([]byte, error) {
		return func() ([]byte, error) { *calls++; return []byte("minted"), nil }
	}

	// A reserved key, in a store that does not exist yet.
	store := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	s, report, run := open(t, store)
	for _, key := range []string{"x.enc", "x.ENC"} {
		want := []string{"the key '" + key + "' is refused: the plaintext file Secret.app.default." + key + " would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"}
		calls := 0
		_, err := s.GetOrCreate(key, gen(&calls))
		if got := stopLines(t, err); !slices.Equal(got, want) || calls != 0 {
			t.Errorf("GetOrCreate(%q): %q (generated %d times), want %q", key, got, calls, want)
		}
		if got := stopLines(t, s.Put(key, []byte("v"))); !slices.Equal(got, want) {
			t.Errorf("Put(%q): %q, want %q", key, got, want)
		}
	}
	if _, err := os.Stat(store); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the dry run made the store: %v", err)
	}
	nothingKept(t, report, run)

	// An unsafe rule below the block.
	store = filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	unsafe := block + "!Secret.app.default.PASSWORD\n"
	writeString(t, filepath.Join(store, ".gitignore"), unsafe)
	s, report, run = open(t, store)
	want := []string{
		rel + ".gitignore:9: this negation can re-include a plaintext secret file (the last matching rule wins): remove the line, or keep the file outside the store",
		rel + ".gitignore was not changed, and lo stopped before the plaintext write: fix each cause above, then run this command again",
	}
	calls := 0
	_, err := s.GetOrCreate("PASSWORD", gen(&calls))
	if got := stopLines(t, err); !slices.Equal(got, want) || calls != 0 {
		t.Errorf("GetOrCreate: %q (generated %d times), want %q", got, calls, want)
	}
	if got := stopLines(t, s.Put("PASSWORD", []byte("v"))); !slices.Equal(got, want) {
		t.Errorf("Put: %q, want %q", got, want)
	}
	if got := names(t, store); !slices.Equal(got, []string{".gitignore"}) {
		t.Errorf("store %q, want the .gitignore only", got)
	}
	if got := readString(t, filepath.Join(store, ".gitignore")); got != unsafe {
		t.Errorf("the dry run changed the .gitignore: %q", got)
	}
	nothingKept(t, report, run)

	// The control: an empty store passes, and the dry run writes no block.
	store = filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
	if err := os.MkdirAll(store, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _, _ = open(t, store)
	calls = 0
	if v, err := s.GetOrCreate("PASSWORD", gen(&calls)); err != nil || string(v) != "minted" || calls != 1 {
		t.Fatalf("the control: %q, %v, %d calls", v, err, calls)
	}
	if err := s.Put("FROM_ENV", []byte("v")); err != nil {
		t.Fatalf("the control Put: %v", err)
	}
	if got := names(t, store); len(got) != 0 {
		t.Errorf("store %q: the dry run wrote a file", got)
	}
}

// A refusal names a file as `lo secrets set` names it: relative to the
// project for a per-domain store, relative to the parent of any other store
// (the flat .secrets of a project).
func TestARefusalNamesTheFileAsSetDoes(t *testing.T) {
	for _, tc := range []struct{ dir, path, want string }{
		{"/p/clusters/alpha.dev/secrets", "/p/clusters/alpha.dev/secrets/.gitignore", "clusters/alpha.dev/secrets/.gitignore"},
		{"/p/clusters/alpha.dev/secrets/", "/p/clusters/alpha.dev/secrets/.gitignore", "clusters/alpha.dev/secrets/.gitignore"},
		{"/p/.secrets", "/p/.secrets/Secret.app.default.K", ".secrets/Secret.app.default.K"},
		{"/p/other/secrets", "/p/other/secrets/.gitignore", "secrets/.gitignore"},
		{"clusters/alpha.dev/secrets", "clusters/alpha.dev/secrets/.gitignore", "clusters/alpha.dev/secrets/.gitignore"},
		{"/secrets", "/secrets/.gitignore", "/secrets/.gitignore"},
	} {
		if got := storeRel(tc.dir)(tc.path); got != tc.want {
			t.Errorf("storeRel(%q)(%q) = %q, want %q", tc.dir, tc.path, got, tc.want)
		}
	}

	dir := filepath.Join(t.TempDir(), ".secrets")
	c, err := New(dir, "default", "app")
	if err != nil {
		t.Fatal(err)
	}
	writeString(t, filepath.Join(dir, ".gitignore"), block+"!x\n")
	want := []string{
		".secrets/.gitignore:9: this negation can re-include a plaintext secret file (the last matching rule wins): remove the line, or keep the file outside the store",
		".secrets/.gitignore was not changed, and lo stopped before the plaintext write: fix each cause above, then run this command again",
	}
	if got := stopLines(t, c.Put("K", []byte("v"))); !slices.Equal(got, want) {
		t.Errorf("flat store:\n%q\nwant:\n%q", got, want)
	}
}

// A hard link at the entry is a regular file: the write replaces the name,
// and the other name of the link keeps its old content.
func TestPutReplacesAHardLinkAsAName(t *testing.T) {
	c, dir := newStore(t)
	other := filepath.Join(dir, "..", "other")
	writeString(t, other, "old")
	if err := os.Link(other, filepath.Join(dir, "Secret.app.default.PASSWORD")); err != nil {
		t.Skipf("no hard link here: %v", err)
	}
	if err := c.Put("PASSWORD", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if got := readString(t, filepath.Join(dir, "Secret.app.default.PASSWORD")); got != "new" {
		t.Errorf("entry %q", got)
	}
	if got := readString(t, other); got != "old" {
		t.Errorf("the other name of the hard link reads %q: the write went in place", got)
	}
}

// A store can hold an entry whose name ends in .enc from before the check
// (an earlier generator wrote it). A read writes nothing, so the cache keeps
// reading it: GetOrCreate returns it and generates nothing. A write to it
// is refused with the refusal of the key (as `lo secrets set` refuses it),
// and the file keeps its bytes.
func TestReadKeepsAnEntryThatPutRefuses(t *testing.T) {
	c, dir := newStore(t)
	path := filepath.Join(dir, "Secret.app.default.enc")
	writeString(t, path, "old")
	called := false
	got, err := c.GetOrCreate("enc", func() ([]byte, error) {
		called = true
		return []byte("minted"), nil
	})
	if err != nil || string(got) != "old" || called {
		t.Fatalf("GetOrCreate = %q, %v (generated: %v), want the stored value", got, err, called)
	}
	if got, want := stopLines(t, c.Put("enc", []byte("new"))), encKeyRefusal("enc"); !slices.Equal(got, want) {
		t.Errorf("Put refusal:\n%q\nwant:\n%q", got, want)
	}
	if got := readString(t, path); got != "old" {
		t.Errorf("entry %q", got)
	}
}

// The writer and the readers of an entry build its name in one place
// (storeguard.EntryName): the file that Put writes, under the name that
// storeguard.CheckName checks, is the file that Get, ReadByName and a
// reference (FormatName) read. An empty namespace is "default" on both
// sides.
func TestPutWritesTheNameThatTheReadersRead(t *testing.T) {
	for _, ns := range []string{"", "default", "ns"} {
		dir := filepath.Join(t.TempDir(), "clusters", "alpha.dev", "secrets")
		c, err := New(dir, ns, "app")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put("tls.crt", []byte("v")); err != nil {
			t.Fatal(err)
		}
		name := FormatName("app", ns, "tls.crt")
		if got := names(t, dir); !slices.Equal(got, []string{".gitignore", name}) {
			t.Errorf("namespace %q: the store holds %q, want the .gitignore and %s", ns, got, name)
		}
		if base, err := storeguard.CheckName(dir, "app", defaultNamespace(ns), "tls.crt"); err != nil || base != name {
			t.Errorf("namespace %q: CheckName = %q, %v, want %s", ns, base, err, name)
		}
		if v, err := c.Get("tls.crt"); err != nil || string(v) != "v" {
			t.Errorf("namespace %q: Get = %q, %v", ns, v, err)
		}
		if v, err := c.ReadByName(name); err != nil || string(v) != "v" {
			t.Errorf("namespace %q: ReadByName(%s) = %q, %v", ns, name, v, err)
		}
	}
}

// Git is the oracle: after generator runs into a store in a git repository
// (no .gitignore first, one with rules of the user, or the legacy lines),
// git would commit only the .gitignore and the ciphertext, with
// core.ignorecase off and on. The keys that the cache refuses leave no
// file. The store also holds a directory that the rule !Secret.*.enc
// matches, with a .gitignore of its own that re-includes its files: only
// the */ rule of the block keeps git out of it, so the case of the legacy
// lines fails when the plan leaves those lines as they are. Git must be
// installed: the CI job of this module runs on the runner, with its git.
func TestGeneratedPlaintextStaysOutOfGit(t *testing.T) {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("this test needs git: %v", err)
	}
	for _, tc := range []struct{ name, gitignore string }{
		{"no .gitignore", ""},
		{"rules of the user", "*.log\n# notes\n"},
		{"the legacy lines", "# Auto-generated by lok8s — ignore everything except SOPS-encrypted\n" +
			"# secret cache files. Plaintext cache entries must never be committed.\n*\n!.gitignore\n!Secret.*.enc\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, home := t.TempDir(), t.TempDir()
			dir := filepath.Join(repo, "clusters", "alpha.dev", "secrets")
			if tc.gitignore != "" {
				writeString(t, filepath.Join(dir, ".gitignore"), tc.gitignore)
			}
			c, err := New(dir, "default", "app")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetOrCreate("PASSWORD", func() ([]byte, error) { return []byte("minted"), nil }); err != nil {
				t.Fatal(err)
			}
			if err := c.Put("tls.key", []byte("key")); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"enc", "x.ENC", "y.Enc"} {
				if err := c.Put(key, []byte("plain")); err == nil {
					t.Fatalf("Put(%q) wrote a plaintext that git commits", key)
				}
			}
			// The ciphertext that `lo secrets encrypt` writes next to it.
			writeString(t, filepath.Join(dir, "Secret.app.default.PASSWORD.enc"), "cipher")
			// A directory that !Secret.*.enc re-includes, whose .gitignore
			// re-includes each file in it.
			writeString(t, filepath.Join(dir, "Secret.app.default.d.enc", ".gitignore"), "!*\n")
			writeString(t, filepath.Join(dir, "Secret.app.default.d.enc", "leak"), "plain")

			env := []string{
				"PATH=" + os.Getenv("PATH"), "HOME=" + home, "LC_ALL=C",
				"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull,
			}
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command(gitBin, args...)
				cmd.Dir, cmd.Env = repo, env
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, stderr.String())
				}
				return stdout.String()
			}
			git("init", "-q")
			want := []string{"clusters/alpha.dev/secrets/.gitignore", "clusters/alpha.dev/secrets/Secret.app.default.PASSWORD.enc"}
			for _, ic := range []string{"core.ignorecase=false", "core.ignorecase=true"} {
				var added []string
				for l := range strings.SplitSeq(strings.TrimSpace(git("-c", ic, "-c", "core.quotepath=off", "add", "--dry-run", "-A")), "\n") {
					if l != "" {
						added = append(added, strings.TrimSuffix(strings.TrimPrefix(l, "add '"), "'"))
					}
				}
				sort.Strings(added)
				if !slices.Equal(added, want) {
					t.Errorf("%s: git would add %q, want %q", ic, added, want)
				}
			}
		})
	}
}
