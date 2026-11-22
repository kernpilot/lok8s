package secrets

// The tests of the plaintext writes of set and decrypt (bash:
// tests/unit/secrets_test.bats): a destination that is a symlink, a hard
// link or a directory, a name with a path part, the size limit of the
// .gitignore at the boundary, and the printed paths. The expected lines are
// literals.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kernpilot/lok8s/kustomize/pkg/storeguard"
)

// litNotRegular is the middle of each refusal of a destination that is not
// a regular file.
const litNotRegular = " is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: "

// spyReader fails the test when set reads the value from it.
type spyReader struct{ t *testing.T }

func (r spyReader) Read([]byte) (int, error) {
	r.t.Error("set read the value")
	return 0, errors.New("the spy reads nothing")
}

func TestSetPartProblem(t *testing.T) {
	for _, s := range []string{"app", "default", "kube-system", "tls.crt", "JWT_SECRET", "a.b-c_d", "...", "x..y", "", "ü"} {
		if why := storeguard.PartProblem(s); why != "" {
			t.Errorf("%q is refused: %s", s, why)
		}
	}
	for s, want := range map[string]string{
		"a/b":                  "it has a / or a \\, which makes the file name a path",
		"../x":                 "it has a / or a \\, which makes the file name a path",
		"anchor/../.gitignore": "it has a / or a \\, which makes the file name a path",
		"a\\b":                 "it has a / or a \\, which makes the file name a path",
		".":                    "it is . or .., a part of a path",
		"..":                   "it is . or .., a part of a path",
		"a\x00b":               "it has a control byte",
		"a\x1b[2Jb":            "it has a control byte",
		"a\nb":                 "it has a control byte",
		"a\x7fb":               "it has a control byte",
	} {
		if got := storeguard.PartProblem(s); got != want {
			t.Errorf("%q: got %q, want %q", s, got, want)
		}
	}
}

func TestSetDestination(t *testing.T) {
	for _, tc := range []struct {
		dir, base, dest string
		ok              bool
	}{
		{"/p/.secrets", "Secret.app.default.K", "/p/.secrets/Secret.app.default.K", true},
		{"/p/.secrets/", "Secret.app.default.K", "/p/.secrets//Secret.app.default.K", true},
		{"/p/.secrets", "Secret.app.default.anchor/../.gitignore", "/p/.secrets/Secret.app.default.anchor/../.gitignore", false},
		{"/p/.secrets", "Secret.app.default.a/b", "/p/.secrets/Secret.app.default.a/b", false},
		{"/p/.secrets", "Secret.app.default.K/", "/p/.secrets/Secret.app.default.K/", false},
	} {
		dest, ok := storeguard.Destination(tc.dir, tc.base)
		if dest != tc.dest || ok != tc.ok {
			t.Errorf("%q + %q: got %q %v, want %q %v", tc.dir, tc.base, dest, ok, tc.dest, tc.ok)
		}
	}
}

// set refuses a name, a namespace or a key that is not one part of a path,
// before it reads the value (a spy fails the test on a read) and before any
// write. The refusal names the field and its text with printable bytes
// only. With the directory Secret.app.default.anchor/ in the store, the key
// anchor/../.gitignore would write the value into the .gitignore.
func TestSetRefusesAPathPartBeforeItReadsTheValue(t *testing.T) {
	sep := "it has a / or a \\, which makes the file name a path"
	for _, tc := range []struct{ label, name, namespace, key, field, shown, why string }{
		{"key anchor/../.gitignore", "app", "default", "anchor/../.gitignore", "key", "anchor/../.gitignore", sep},
		{"key ../x", "app", "default", "../x", "key", "../x", sep},
		{"key a/b", "app", "default", "a/b", "key", "a/b", sep},
		{"key with a backslash", "app", "default", "a\\b", "key", "a\\b", sep},
		{"key .", "app", "default", ".", "key", ".", "it is . or .., a part of a path"},
		{"key ..", "app", "default", "..", "key", "..", "it is . or .., a part of a path"},
		{"key with a control byte", "app", "default", "a\x1b[2Jb", "key", "a?[2Jb", "it has a control byte"},
		{"key with a NUL", "app", "default", "a\x00b", "key", "a?b", "it has a control byte"},
		{"name a/b", "a/b", "default", "K", "name", "a/b", sep},
		{"namespace ..", "app", "..", "K", "namespace", "..", "it is . or .., a part of a path"},
	} {
		for _, tty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, tty %v", tc.label, tty), func(t *testing.T) {
				c, out, errOut := testEnv(t)
				dir := c.Paths.Base + "/.secrets"
				write(t, dir+"/.gitignore", litBlock)
				mkdir(t, dir+"/Secret.app.default.anchor")
				c.Stdin = spyReader{t}
				c.StdinIsTTY = func() bool { return tty }
				c.ReadPassword = func() (string, error) {
					t.Error("set read the value at the prompt")
					return "", errors.New("the spy reads nothing")
				}
				before := fileStates(t, c.Paths.Base)
				if err := c.Set(context.Background(), tc.name, tc.namespace, tc.key, "", false); !errors.Is(err, ErrHandled) {
					t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
				}
				want := "[error] the " + tc.field + " '" + tc.shown + "' is refused: " + tc.why +
					". Nothing was written: use a " + tc.field + " without it\n"
				if errOut.String() != want || out.String() != "" {
					t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
				}
				sameStates(t, before, fileStates(t, c.Paths.Base))
			})
		}
	}
}

// linkStore is a store with the .gitignore of lo, the file Secret.leak.enc
// (a name that git commits) and the plaintext name of set and decrypt as a
// symlink to target ("Secret.leak.enc" or ".gitignore").
func linkStore(t *testing.T, dir, target string) {
	t.Helper()
	write(t, dir+"/.gitignore", litBlock)
	write(t, dir+"/Secret.leak.enc", "ciphertext-bytes")
	if err := os.Symlink(target, dir+"/Secret.app.default.K"); err != nil {
		t.Fatal(err)
	}
}

// set refuses a plaintext file that is a symlink (to a file that git
// commits) or a directory: nothing is written, and the file that the link
// names keeps its bytes.
func TestSetRefusesALinkedPlaintextFile(t *testing.T) {
	for _, target := range []string{"Secret.leak.enc", ".gitignore", ""} {
		label := "a symlink to " + target
		if target == "" {
			label = "a directory"
		}
		t.Run(label, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			dir := c.Paths.Base + "/.secrets"
			if target == "" {
				write(t, dir+"/.gitignore", litBlock)
				mkdir(t, dir+"/Secret.app.default.K")
			} else {
				linkStore(t, dir, target)
			}
			before := fileStates(t, c.Paths.Base)
			if err := c.Set(context.Background(), "app", "default", "K", "the-value", false); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] .secrets/Secret.app.default.K" + litNotRegular + "lo stopped before the plaintext write — remove it, then run this command again\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// set replaces a hard link at the plaintext file as a name: the other name
// keeps its old content, the plaintext file has the value and mode 0600,
// and no temp file is left.
func TestSetReplacesAHardLinkedPlaintextFile(t *testing.T) {
	c, _, errOut := testEnv(t)
	dir := c.Paths.Base + "/.secrets"
	write(t, dir+"/.gitignore", litBlock)
	other := c.Paths.Base + "/elsewhere"
	write(t, other, "old")
	if err := os.Link(other, dir+"/Secret.app.default.K"); err != nil {
		t.Fatal(err)
	}
	if err := c.Set(context.Background(), "app", "default", "K", "new", false); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	hardLinkReplaced(t, dir, other, "new")
}

// hardLinkReplaced checks a plaintext file Secret.app.default.K of dir that
// was a hard link of other: it holds want with mode 0600, other holds old,
// the two are different files, and no temp file is left.
func hardLinkReplaced(t *testing.T, dir, other, want string) {
	t.Helper()
	dest := dir + "/Secret.app.default.K"
	if got := readFile(t, dest); got != want {
		t.Errorf("the plaintext file: %q, want %q", got, want)
	}
	if got := readFile(t, other); got != "old" {
		t.Errorf("the other name of the hard link: %q, want \"old\"", got)
	}
	a, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(a, b) {
		t.Error("the plaintext file is still a hard link of the other name")
	}
	if a.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", a.Mode().Perm())
	}
	if left, _ := filepath.Glob(dir + "/.Secret.app.default.K.tmp-*"); len(left) != 0 {
		t.Errorf("temp files stay: %q", left)
	}
}

// encryptedStore makes the store of a working decrypt: an age identity, the
// plaintext Secret.app.default.K with value "v" encrypted, then removed. The
// identity is in SOPS_AGE_KEY.
func encryptedStore(t *testing.T, c *Context) string {
	t.Helper()
	id := testAgeIdentity(t, c)
	dir := c.Paths.Base + "/.secrets"
	write(t, dir+"/Secret.app.default.K", "v")
	if err := c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir + "/Secret.app.default.K"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SOPS_AGE_KEY", id.String())
	return dir
}

// decrypt refuses a plaintext file that is a symlink (to a file that git
// commits) before its first write, with a key that decrypts the .enc:
// nothing is written, and the file that the link names keeps its bytes.
func TestDecryptRefusesALinkedPlaintextFile(t *testing.T) {
	for _, target := range []string{"Secret.leak.enc", ".gitignore"} {
		t.Run("a symlink to "+target, func(t *testing.T) {
			c, out, errOut := testEnv(t)
			dir := encryptedStore(t, c)
			write(t, dir+"/Secret.leak.enc", "ciphertext-bytes")
			if err := os.Symlink(target, dir+"/Secret.app.default.K"); err != nil {
				t.Fatal(err)
			}
			out.Reset()
			errOut.Reset()
			before := fileStates(t, c.Paths.Base)
			if err := c.Decrypt("/nonexistent-ssh-key"); !errors.Is(err, ErrHandled) {
				t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
			}
			want := "[error] .secrets/Secret.app.default.K" + litNotRegular + "decrypt would write the plaintext of Secret.app.default.K.enc there\n" +
				"[error] decrypt wrote no plaintext file: remove these entries, then run this command again\n"
			if errOut.String() != want || out.String() != "" {
				t.Errorf("stdout %q\nstderr:\n%q\nwant:\n%q", out.String(), errOut.String(), want)
			}
			sameStates(t, before, fileStates(t, c.Paths.Base))
		})
	}
}

// decrypt checks each entry again at its write, after its check of the
// whole store: a plaintext path that becomes a symlink after that check
// is refused, and nothing is written through the link or over it. The
// link is made while decrypt writes the .gitignore (the writeGitignore
// seam), which comes after the check. The link names a file older than
// the .enc, so decrypt does not skip the entry as newer.
func TestDecryptRefusesALinkMadeAfterItsCheck(t *testing.T) {
	c, _, errOut := testEnv(t)
	dir := encryptedStore(t, c)
	if err := os.Remove(dir + "/.gitignore"); err != nil {
		t.Fatal(err)
	}
	other := c.Paths.Base + "/elsewhere"
	write(t, other, "old")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}
	orig := writeGitignore
	calls := 0
	writeGitignore = func(w io.Writer, want []byte) error {
		calls++
		if err := os.Symlink(other, dir+"/Secret.app.default.K"); err != nil {
			return err
		}
		return orig(w, want)
	}
	t.Cleanup(func() { writeGitignore = orig })

	errOut.Reset()
	if err := c.Decrypt("/nonexistent-ssh-key"); !errors.Is(err, ErrHandled) {
		t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
	}
	if calls != 1 {
		t.Fatalf("the seam ran %d times, want 1: the link was not made after the check", calls)
	}
	want := "[error] .secrets/Secret.app.default.K" + litNotRegular + "lo stopped before the plaintext write — remove it, then run this command again\n"
	if errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}
	if got := readFile(t, other); got != "old" {
		t.Errorf("the write went through the link: %q", got)
	}
	if fi, err := os.Lstat(dir + "/Secret.app.default.K"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if left, _ := filepath.Glob(dir + "/.Secret.app.default.K.tmp-*"); len(left) != 0 {
		t.Errorf("temp files stay: %q", left)
	}
}

// decrypt replaces a hard link at the plaintext file as a name: the other
// name keeps its old content, and the plaintext file holds the decrypted
// value. The plaintext is older than its .enc, so decrypt writes it.
func TestDecryptReplacesAHardLinkedPlaintextFile(t *testing.T) {
	c, _, errOut := testEnv(t)
	dir := encryptedStore(t, c)
	other := c.Paths.Base + "/elsewhere"
	write(t, other, "old")
	if err := os.Link(other, dir+"/Secret.app.default.K"); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}
	if err := c.Decrypt("/nonexistent-ssh-key"); err != nil {
		t.Fatalf("%v\n%s", err, errOut.String())
	}
	hardLinkReplaced(t, dir, other, "v")
}

// A path of a refusal prints with printable bytes only: a store name and a
// file name with a control byte show it as ?.
func TestPlaintextRefusalsPrintAPathAsPrintable(t *testing.T) {
	t.Run("decrypt", func(t *testing.T) {
		c, _, errOut := testEnv(t)
		dir := c.Paths.Base + "/.secrets"
		write(t, dir+"/.gitignore", litBlock)
		write(t, dir+"/Secret.app.default.K\x1b[2J.enc", "ciphertext-bytes")
		if err := os.Symlink("Secret.leak.enc", dir+"/Secret.app.default.K\x1b[2J"); err != nil {
			t.Fatal(err)
		}
		if err := c.Decrypt("/nonexistent-ssh-key"); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
		}
		want := "[error] .secrets/Secret.app.default.K?[2J" + litNotRegular + "decrypt would write the plaintext of Secret.app.default.K?[2J.enc there\n" +
			"[error] decrypt wrote no plaintext file: remove these entries, then run this command again\n"
		if errOut.String() != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
		}
	})
	t.Run("set", func(t *testing.T) {
		c, _, errOut := testEnv(t)
		dir := c.Paths.Base + "/st\x1b[2Jore"
		c.Paths.SecretsEnv, c.Paths.SecretsEnvSet = dir, true
		write(t, dir+"/.gitignore", litBlock)
		mkdir(t, dir+"/Secret.app.default.K")
		if err := c.Set(context.Background(), "app", "default", "K", "the-value", false); !errors.Is(err, ErrHandled) {
			t.Fatalf("want ErrHandled, got %v\n%s", err, errOut.String())
		}
		want := "[error] st?[2Jore/Secret.app.default.K" + litNotRegular + "lo stopped before the plaintext write — remove it, then run this command again\n"
		if errOut.String() != want {
			t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
		}
	})
}

// At the size limit, a run writes a .gitignore that the next run reads: the
// plan of a file with no block is exactly 1048576 bytes, set writes it,
// and a second set passes and does not write it again. One byte more is
// refused before any write, also by the second run.
func TestGitignoreAtTheSizeLimitPassesTwice(t *testing.T) {
	c, _, errOut := testEnv(t)
	dir := c.Paths.Base + "/.secrets"
	user := strings.Repeat("#", 1048576-len(litBlock))
	write(t, dir+"/.gitignore", user)
	for run := 1; run <= 2; run++ {
		if err := c.Set(context.Background(), "app", "default", "K", "v", false); err != nil {
			t.Fatalf("run %d: %v\n%s", run, err, errOut.String())
		}
		if got := readFile(t, dir+"/.gitignore"); got != litBlock+user {
			t.Fatalf("run %d: the .gitignore has %d bytes", run, len(got))
		}
	}

	c, _, errOut = testEnv(t)
	dir = c.Paths.Base + "/.secrets"
	write(t, dir+"/.gitignore", user+"#")
	want := "[error] .secrets/.gitignore: with the lok8s block the file would have more than 1048576 bytes, and lo does not read a larger .gitignore: make it smaller\n" +
		"[error] " + litFinal(".secrets/.gitignore", "the plaintext write") + "\n"
	for run := 1; run <= 2; run++ {
		errOut.Reset()
		before := fileStates(t, c.Paths.Base)
		if err := c.Set(context.Background(), "app", "default", "K", "v", false); !errors.Is(err, ErrHandled) {
			t.Fatalf("one byte more, run %d: want ErrHandled, got %v", run, err)
		}
		if errOut.String() != want {
			t.Errorf("one byte more, run %d: stderr:\n%q\nwant:\n%q", run, errOut.String(), want)
		}
		sameStates(t, before, fileStates(t, c.Paths.Base))
	}
}

// A write that failed prints fixed lines only: the temp file that stays
// (with printable bytes only, relative to the project), then the final
// line. The text of the file system errors (it has the raw path) is not
// printed.
func TestFailedWritePrintsNoErrorText(t *testing.T) {
	c, _, errOut := testEnv(t)
	tmp := c.Paths.Base + "/.secrets/.Secret.app.default.K\x1b[2J.tmp-123"
	left := &tempLeftError{
		Err:  fmt.Errorf("write %s: no space left on device", tmp),
		Name: tmp,
		Rm:   fmt.Errorf("remove %s: permission denied", tmp),
	}
	_ = c.printStop(c.guard().FailedWrite(left, "set cannot write .secrets/Secret.app.default.K?[2J: it was not changed"))
	want := "[error] cannot remove the temp file .secrets/.Secret.app.default.K?[2J.tmp-123: remove it by hand\n" +
		"[error] set cannot write .secrets/Secret.app.default.K?[2J: it was not changed\n"
	if errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}

	errOut.Reset()
	_ = c.printStop(c.guard().FailedWrite(errors.New("rename "+tmp+": no such file or directory"), "decrypt cannot write .secrets/X: it was not changed"))
	if want := "[error] decrypt cannot write .secrets/X: it was not changed\n"; errOut.String() != want {
		t.Errorf("stderr:\n%q\nwant:\n%q", errOut.String(), want)
	}
}
