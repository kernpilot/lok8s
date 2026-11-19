package secrets

// removekey_reads_test.go: how remove-key writes each .enc: through a temp
// file and a rename. tests/unit/secrets_remove_key_test.bats runs the same
// cases through the bash twin.

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The write helper of every encrypt, called directly (in process, no seam):
// a write that stops half way leaves the old file whole and no temp file; a
// write that ends replaces the file (mode 0600) and leaves a hard link to the
// old one with the old content.
func TestWriteReplacingKeepsTheOldFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/Secret.app.default.K.enc"
	write(t, path, "old\n")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, dir+"/hard"); err != nil {
		t.Fatal(err)
	}
	entries := func() int {
		list, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	stop := errors.New("stopped half way")
	err := writeReplacing(path, func(w io.Writer) error {
		if _, err := io.WriteString(w, "ne"); err != nil {
			return err
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("want the write error, got %v", err)
	}
	if got := readFile(t, path); got != "old\n" {
		t.Errorf("after a failed write the file is %q, want the old content", got)
	}
	if n := entries(); n != 2 {
		t.Errorf("a failed write left %d entries, want 2 (no temp file)", n)
	}

	if err := writeReplacing(path, func(w io.Writer) error {
		_, err := io.WriteString(w, "new\n")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, path); got != "new\n" {
		t.Errorf("file: %q", got)
	}
	if got := readFile(t, dir+"/hard"); got != "old\n" {
		t.Errorf("the hard link sees %q: the file was written in place", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Error(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	if n := entries(); n != 2 {
		t.Errorf("%d entries, want 2 (no temp file)", n)
	}
}

// The same through remove-key and the real encrypt: the re-key replaces each
// .enc (a hard link keeps the old bytes), mode 0600, and leaves no temp file.
func TestRemoveKeyReplacesEachEnc(t *testing.T) {
	c, out, errOut := testEnv(t)
	c.Domain = "a.dev"
	keep, gone := newAgeIdentity(t), newAgeIdentity(t)
	k, g := keep.Recipient().String(), gone.Recipient().String()
	sopsRules(t, c, k+","+g)
	a := c.Paths.Clusters + "/a.dev/secrets"
	write(t, a+"/Secret.app.default.K", "v")
	encryptStore(t, c, "a.dev", out, errOut)
	freshen(t, a+"/Secret.app.default.K")
	enc := a + "/Secret.app.default.K.enc"
	old := readFile(t, enc)
	link := c.Paths.Base + "/old.enc"
	if err := os.Link(enc, link); err != nil {
		t.Fatal(err)
	}

	if err := c.RemoveKey(g, false, false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	if readFile(t, link) != old {
		t.Error("the .enc was written in place, not replaced")
	}
	if !opens(t, enc, keep, "v") {
		t.Error("the remaining identity cannot open the re-keyed .enc")
	}
	if info, err := os.Stat(enc); err != nil {
		t.Error(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
	list, err := os.ReadDir(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range list {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("a temp file was left: %s", e.Name())
		}
	}
}
