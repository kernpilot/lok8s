package storeguard

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
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
