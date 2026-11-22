package storeguard

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func lines(t *testing.T, err error) []string {
	t.Helper()
	s, ok := errors.AsType[*Stop](err)
	if !ok {
		t.Fatalf("want a *Stop, got %T: %v", err, err)
	}
	return s.Lines
}

// WritePlaintext checks the name itself, also for a caller that has no key
// (decrypt writes the plaintext of each .enc of the store): a name that
// ends in .enc, in any letter case, and a name that is not an entry of the
// store are refused before any write, and the store stays empty.
// CheckPlaintext refuses them with the same text. The refusal of a
// reserved name has the one text of CheckName, with the write as its
// subject.
func TestWritePlaintextRefusesANameBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"Secret.app.default.x.enc", "the plaintext write is refused: the plaintext file Secret.app.default.x.enc would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"},
		{"Secret.app.default.ENC", "the plaintext write is refused: the plaintext file Secret.app.default.ENC would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"},
		{"sub/Secret.app.default.K", "the plaintext file sub/Secret.app.default.K is refused: it is not a file of the store directory itself. Nothing was written: use a name, a namespace and a key without a path part"},
	} {
		dir := t.TempDir()
		p := Plaintext{Dir: dir, Base: tc.base, Who: "decrypt"}
		if got := lines(t, Guard{}.WritePlaintext(p, []byte("v"))); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s:\n%q\nwant %q", tc.base, got, tc.want)
		}
		if got := lines(t, Guard{}.CheckPlaintext(p)); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("CheckPlaintext %s:\n%q\nwant %q", tc.base, got, tc.want)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%s: a refused name wrote %d files", tc.base, len(ents))
		}
	}
}

// CheckName first refuses each part that makes the file name a path, so
// the check that the file is an entry of the store refuses only an empty
// store dir: the file would be at the root. Each other dir passes. A
// reserved key gets the text of lo secrets set.
func TestCheckNameRefusesAFileOutsideTheStore(t *testing.T) {
	_, err := CheckName("", "app", "default", "K")
	want := []string{"the plaintext file Secret.app.default.K is refused: it is not a file of the store directory itself. Nothing was written: use a name, a namespace and a key without a path part"}
	if got := lines(t, err); !slices.Equal(got, want) {
		t.Errorf("empty dir:\n%q\nwant %q", got, want)
	}
	for _, dir := range []string{"/", ".", "store", "store/", "/a/b/../store"} {
		if base, err := CheckName(dir, "app", "default", "K"); err != nil || base != "Secret.app.default.K" {
			t.Errorf("CheckName(%q) = %q, %v", dir, base, err)
		}
	}
	_, err = CheckName("store", "app", "default", "x.Enc")
	want = []string{"the key 'x.Enc' is refused: the plaintext file Secret.app.default.x.Enc would end in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"}
	if got := lines(t, err); !slices.Equal(got, want) {
		t.Errorf("reserved key:\n%q\nwant %q", got, want)
	}
}

// A write that fails names the command and the path, through Rel, and not
// the error of the file system. The store has the .gitignore of the guard,
// and no temp file stays.
func TestWritePlaintextNamesAFailedWrite(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g := Guard{Rel: func(p string) string { return p[len(base)+1:] }}
	if _, err := g.EnsureGitignore(dir, StepPlaintext); err != nil {
		t.Fatal(err)
	}
	// A directory made read-only after the .gitignore: the temp file
	// cannot be made.
	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory, so the write cannot fail here: run this test as a user that is not root")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		t.Skip("this file system ignores the mode of a directory, so the write cannot fail here")
	}
	err := g.WritePlaintext(Plaintext{Dir: dir, Base: "Secret.app.default.K", Who: "set"}, []byte("the-value"))
	want := []string{"set cannot write secrets/Secret.app.default.K: it was not changed"}
	if got := lines(t, err); !slices.Equal(got, want) {
		t.Errorf("failed write:\n%q\nwant %q", got, want)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("%d entries, want the .gitignore only", len(ents))
	}
}

// newStore makes an empty store dir under a temp project and returns it
// with a Guard whose Rel names a file relative to the project.
func newStore(t *testing.T) (string, Guard) {
	t.Helper()
	base := t.TempDir()
	dir := filepath.Join(base, "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir, Guard{Rel: func(p string) string { return strings.TrimPrefix(p, base+"/") }}
}

// entries returns the sorted names of the entries of dir.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// The store directory is synced after the rename of the .gitignore,
// before the first byte of the plaintext (the temp file of the plaintext
// does not exist yet), and again after the rename of the plaintext. A
// sync that fails after the .gitignore leaves no plaintext. A sync that
// fails after the plaintext says that the file was written.
func TestWritePlaintextSyncsTheStoreDirectory(t *testing.T) {
	dir, g := newStore(t)
	var seen [][]string
	g.syncDir = func(d string) error {
		if d != dir {
			t.Errorf("sync of %s, want the store %s", d, dir)
		}
		seen = append(seen, entries(t, dir))
		return nil
	}
	if err := g.WritePlaintext(Plaintext{Dir: dir, Base: "Secret.app.default.K"}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if want := [][]string{{".gitignore"}, {".gitignore", "Secret.app.default.K"}}; !slices.EqualFunc(seen, want, slices.Equal[[]string]) {
		t.Errorf("the store at each sync: %q, want %q", seen, want)
	}

	// The .gitignore is there: one sync, after the plaintext.
	seen = nil
	if err := g.WritePlaintext(Plaintext{Dir: dir, Base: "Secret.app.default.L"}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Errorf("%d syncs for a store with its .gitignore, want 1", len(seen))
	}

	// The sync after the .gitignore fails: no plaintext, no temp file.
	dir, g = newStore(t)
	g.syncDir = func(string) error { return errors.New("input/output error") }
	err := g.WritePlaintext(Plaintext{Dir: dir, Base: "Secret.app.default.K", Who: "set"}, []byte("v"))
	want := []string{"secrets/.gitignore was written, but the sync of its directory failed, so a power loss can lose it: lo stopped before the plaintext write — run this command again"}
	if got := lines(t, err); !slices.Equal(got, want) {
		t.Errorf("failed sync after the .gitignore:\n%q\nwant %q", got, want)
	}
	if got := entries(t, dir); !slices.Equal(got, []string{".gitignore"}) {
		t.Errorf("store %q: a plaintext was written before the .gitignore was synced", got)
	}

	// The sync after the plaintext fails: the error says that it was written.
	dir, g = newStore(t)
	calls := 0
	g.syncDir = func(string) error {
		if calls++; calls == 2 {
			return errors.New("input/output error")
		}
		return nil
	}
	err = g.WritePlaintext(Plaintext{Dir: dir, Base: "Secret.app.default.K", Who: "set"}, []byte("v"))
	want = []string{"secrets/Secret.app.default.K was written, but the sync of its directory failed, so a power loss can lose the new file"}
	if got := lines(t, err); !slices.Equal(got, want) {
		t.Errorf("failed sync after the plaintext:\n%q\nwant %q", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Secret.app.default.K")); err != nil || string(b) != "v" {
		t.Errorf("the plaintext: %q, %v", b, err)
	}
}

// MakeDir syncs the parent of each directory that it makes, from the top,
// and nothing for a directory that exists. A sync that fails names the
// new directory, and MakeDir removes each directory that it made, so that
// a run again makes them and syncs their parents. A directory that it
// cannot remove (here: a file came into it) stays and is named.
func TestMakeDirSyncsTheParentOfEachNewDirectory(t *testing.T) {
	base := t.TempDir()
	var synced []string
	g := Guard{
		Rel:     func(p string) string { return strings.TrimPrefix(p, base+"/") },
		syncDir: func(d string) error { synced = append(synced, d); return nil },
	}
	dir := filepath.Join(base, "clusters", "alpha.dev", "secrets")
	if err := g.MakeDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := []string{base, filepath.Join(base, "clusters"), filepath.Join(base, "clusters", "alpha.dev")}
	if !slices.Equal(synced, want) {
		t.Errorf("synced %q, want %q", synced, want)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("the store: %v", err)
	}
	synced = nil
	if err := g.MakeDir(dir, 0o700); err != nil || len(synced) != 0 {
		t.Errorf("a store that exists: %v, synced %q", err, synced)
	}

	// The second sync (the parent of other/a) fails: lo removes other/a/b,
	// other/a and other, and the next run syncs each parent again.
	calls := 0
	g.syncDir = func(string) error {
		if calls++; calls == 2 {
			return errors.New("input/output error")
		}
		return nil
	}
	other := filepath.Join(base, "other", "a", "b")
	err := g.MakeDir(other, 0o700)
	wantLines := []string{"the sync of the parent directory of other/a failed, so lo removed the directories that it made: lo wrote no file — run this command again"}
	if got := lines(t, err); !slices.Equal(got, wantLines) {
		t.Errorf("failed sync:\n%q\nwant %q", got, wantLines)
	}
	if _, err := os.Lstat(filepath.Join(base, "other")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("other stays after the failed sync: %v", err)
	}
	synced = nil
	g.syncDir = func(d string) error { synced = append(synced, d); return nil }
	if err := g.MakeDir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if want := []string{base, filepath.Join(base, "other"), filepath.Join(base, "other", "a")}; !slices.Equal(synced, want) {
		t.Errorf("the run again synced %q, want %q", synced, want)
	}

	// A file comes into the new directory before the sync fails: it stays.
	g.syncDir = func(string) error {
		if err := os.WriteFile(filepath.Join(base, "third", "theirs"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return errors.New("input/output error")
	}
	err = g.MakeDir(filepath.Join(base, "third"), 0o700)
	wantLines = []string{"the sync of the parent directory of third failed, and lo cannot remove third, which it made: a power loss can lose it. lo wrote no file in it — remove it, then run this command again"}
	if got := lines(t, err); !slices.Equal(got, wantLines) {
		t.Errorf("failed sync and remove:\n%q\nwant %q", got, wantLines)
	}
	if got := entries(t, filepath.Join(base, "third")); !slices.Equal(got, []string{"theirs"}) {
		t.Errorf("third holds %q, want the file that came into it", got)
	}
}

// SyncDir counts a directory sync that the file system does not support
// (EINVAL, ENOTSUP, wrapped as os wraps them) as synced, and returns each
// other error of the sync, and an error of the open. A Guard without its
// syncDir seam syncs with SyncDir: its writes sync the store, and a failed
// sync stops them.
func TestSyncDirCountsAnUnsupportedSyncAsSynced(t *testing.T) {
	var fail error
	var seen []string
	t.Cleanup(func() { syncFile = (*os.File).Sync })
	syncFile = func(f *os.File) error {
		seen = append(seen, f.Name())
		if fail == nil {
			return nil
		}
		return &fs.PathError{Op: "sync", Path: f.Name(), Err: fail}
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		fail error
		ok   bool
	}{{nil, true}, {syscall.EINVAL, true}, {syscall.ENOTSUP, true}, {syscall.EIO, false}, {syscall.EBADF, false}} {
		fail = tc.fail
		err := SyncDir(dir)
		if tc.ok && err != nil {
			t.Errorf("sync error %v: SyncDir = %v, want synced", tc.fail, err)
		}
		if !tc.ok && !errors.Is(err, tc.fail) {
			t.Errorf("sync error %v: SyncDir = %v, want that error", tc.fail, err)
		}
	}
	if err := SyncDir(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing directory: SyncDir = %v, want the error of the open", err)
	}

	// The default path: a Guard with no syncDir syncs through SyncDir.
	fail, seen = nil, nil
	store, g := newStore(t)
	if g.syncDir != nil {
		t.Fatal("newStore set the syncDir seam")
	}
	if err := g.WritePlaintext(Plaintext{Dir: store, Base: "Secret.app.default.K"}, []byte("v")); err != nil {
		t.Fatal(err)
	}
	if want := []string{store, store}; !slices.Equal(seen, want) {
		t.Errorf("the default path synced %q, want the store after the .gitignore and after the plaintext", seen)
	}
	fail = syscall.ENOTSUP
	if err := g.WritePlaintext(Plaintext{Dir: store, Base: "Secret.app.default.L"}, []byte("v")); err != nil {
		t.Errorf("an unsupported sync on the default path: %v", err)
	}
	fail = syscall.EIO
	err := g.WritePlaintext(Plaintext{Dir: store, Base: "Secret.app.default.M"}, []byte("v"))
	want := []string{"secrets/Secret.app.default.M was written, but the sync of its directory failed, so a power loss can lose the new file"}
	if got := lines(t, err); !slices.Equal(got, want) {
		t.Errorf("a failed sync on the default path:\n%q\nwant %q", got, want)
	}
}

// The plaintext boundary refuses the name .gitignore in any letter case,
// for a direct caller too: the plaintext would replace the rules of the
// store. The .gitignore keeps its bytes, and no file is written. A name
// that only starts with .gitignore is a plaintext like any other.
func TestWritePlaintextRefusesTheGitignoreName(t *testing.T) {
	for _, base := range []string{".gitignore", ".GITIGNORE", ".GitIgnore"} {
		for _, withRules := range []bool{false, true} {
			dir := t.TempDir()
			if withRules {
				if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(gitignoreContent), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			p := Plaintext{Dir: dir, Base: base}
			want := []string{"the plaintext write is refused: the plaintext file " + base + " would replace the .gitignore of the store (in any letter case), which keeps every plaintext out of git. Nothing was written: use the name of a store entry"}
			if got := lines(t, Guard{}.WritePlaintext(p, []byte("the-value"))); !slices.Equal(got, want) {
				t.Errorf("%s: %q, want %q", base, got, want)
			}
			if got := lines(t, Guard{}.CheckPlaintext(p)); !slices.Equal(got, want) {
				t.Errorf("CheckPlaintext %s: %q, want %q", base, got, want)
			}
			got := entries(t, dir)
			if !withRules && len(got) != 0 {
				t.Errorf("%s: store %q, want empty", base, got)
			}
			if withRules {
				if b, _ := os.ReadFile(filepath.Join(dir, ".gitignore")); string(b) != gitignoreContent || !slices.Equal(got, []string{".gitignore"}) {
					t.Errorf("%s: store %q, .gitignore %q", base, got, b)
				}
			}
		}
	}
	dir := t.TempDir()
	if err := (Guard{}).WritePlaintext(Plaintext{Dir: dir, Base: ".gitignore.bak"}, []byte("v")); err != nil {
		t.Errorf("the control: %v", err)
	}
}

// EnsureGitignore reads the file again after its write: a fill that writes
// other bytes and returns nil (here: nothing, the block with a negation, or
// the block with one more rule, a file that the plan keeps as it is) is
// refused, and no plaintext is written.
func TestEnsureGitignoreRefusesAFillThatWritesOtherBytes(t *testing.T) {
	for name, fill := range map[string]func(io.Writer, []byte) error{
		"nothing":       func(io.Writer, []byte) error { return nil },
		"a negation":    func(w io.Writer, want []byte) error { _, err := w.Write(append(want, "!x\n"...)); return err },
		"one more rule": func(w io.Writer, want []byte) error { _, err := w.Write(append(want, "x\n"...)); return err },
	} {
		dir, g := newStore(t)
		g.FillGitignore = fill
		err := g.WritePlaintext(Plaintext{Dir: dir, Base: "Secret.app.default.K"}, []byte("the-value"))
		want := []string{"secrets/.gitignore was written, but it does not have the bytes that lo planned for it: lo stopped before the plaintext write — fix the file, then run this command again"}
		if got := lines(t, err); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
		if got := entries(t, dir); !slices.Equal(got, []string{".gitignore"}) {
			t.Errorf("%s: store %q, want the .gitignore only", name, got)
		}
		wrote, err := g.EnsureGitignore(t.TempDir(), StepPlaintext)
		if !wrote || err == nil {
			t.Errorf("%s: EnsureGitignore = %v, %v, want true and the refusal", name, wrote, err)
		}
	}
}

// The plan reads a negation below the block in each form that git reads
// it: after a UTF-8 BOM, with CRLF line ends, and with a ** pattern. Each
// is refused as a plain negation is, on its own line. The same files
// without the negation pass.
func TestPlanGitignoreRefusesANegationInEachForm(t *testing.T) {
	const bom = "\xef\xbb\xbf"
	crlf := strings.ReplaceAll(gitignoreContent, "\n", "\r\n")
	want := []string{"s/.gitignore:9: " + gitignoreNegationText}
	for _, tc := range []struct{ name, rules, negation string }{
		{"plain", gitignoreContent, "!x\n"},
		{"a BOM", bom + gitignoreContent, "!x\n"},
		{"CRLF line ends", crlf, "!x\r\n"},
		{"a BOM and CRLF line ends", bom + crlf, "!x\r\n"},
		{"a ** negation", gitignoreContent, "!**/x\n"},
		{"a ** negation of a plaintext", crlf, "!**/Secret.app.default.K\r\n"},
	} {
		plan, problems := PlanGitignore([]byte(tc.rules+tc.negation), "s/.gitignore")
		if plan != nil || !slices.Equal(problems, want) {
			t.Errorf("%s: plan %q, problems %q, want %q", tc.name, plan, problems, want)
		}
		if _, problems := PlanGitignore([]byte(tc.rules), "s/.gitignore"); problems != nil {
			t.Errorf("%s without the negation: %q", tc.name, problems)
		}
	}
}
