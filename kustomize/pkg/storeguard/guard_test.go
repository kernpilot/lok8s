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
func TestWritePlaintextRefusesANameBeforeAnyWrite(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"Secret.app.default.x.enc", "the plaintext file Secret.app.default.x.enc is refused: its name ends in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"},
		{"Secret.app.default.ENC", "the plaintext file Secret.app.default.ENC is refused: its name ends in .enc (in any letter case), and the .gitignore rule !Secret.*.enc lets git commit it. Nothing was written: use a key name that does not end in .enc"},
		{"sub/Secret.app.default.K", "the plaintext file sub/Secret.app.default.K is refused: it is not a file of the store directory itself. Nothing was written: use a name, a namespace and a key without a path part"},
	} {
		dir := t.TempDir()
		err := Guard{}.WritePlaintext(Plaintext{Dir: dir, Base: tc.base, Who: "decrypt"}, []byte("v"))
		if got := lines(t, err); !slices.Equal(got, []string{tc.want}) {
			t.Errorf("%s:\n%q\nwant %q", tc.base, got, tc.want)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("%s: a refused name wrote %d files", tc.base, len(ents))
		}
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
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil {
		_ = f.Close()
		t.Skip("the permissions do not hold here (root)")
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
