//go:build linux

package secrets

// times_linux_test.go: a file that the kernel touches has a modification
// time equal to its change time (ctime): utimensat with no times sets both
// from one reading of the clock of the kernel. os.Chtimes with time.Now()
// sets the modification time from time.Now() and the ctime from the clock
// of the kernel, and the two differ in the nanoseconds. The tests read that
// sign, not the order of two writes, so they do not depend on when the
// clock ticks.

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"
)

// kernelStamped reports whether the modification time of path is its ctime.
func kernelStamped(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: no syscall.Stat_t", path)
	}
	return info.ModTime().Equal(time.Unix(st.Ctim.Unix()))
}

// The case of a failed CI run: add-key right after an encrypt. The re-key
// sets each plaintext to now and encrypts it at once. The plaintexts get the
// time of the kernel (as touch does in the bash twin), so each .enc, written
// after it, is the newer file: no warning, the lint check names no secret,
// and the next encrypt writes nothing. With a time from time.Now() an .enc
// was older than its plaintext in 218 of 2000 tries on one kernel; the
// check of the ctime fails on such a time in every run.
func TestAddKeyRightAfterAnEncryptLeavesEachEncTheNewerFile(t *testing.T) {
	f := newLinkFixture(t, "add-key")
	if err := f.run(); err != nil {
		t.Fatalf("%v\n%s", err, f.errOut.String())
	}
	if f.errOut.Len() != 0 {
		t.Errorf("stderr: %q", f.errOut.String())
	}
	for _, name := range []string{"app", "db"} {
		plain := f.store + "/Secret." + name + ".default.K"
		if !kernelStamped(t, plain) {
			t.Errorf("%s: the time of the plaintext is not a time of the kernel", f.c.rel(plain))
		}
		if !newerThan(plain+".enc", plain) {
			t.Errorf("%s: the .enc is not the newer file", f.c.rel(plain))
		}
	}
	f.rekeyed("app", "v")
	f.rekeyed("db", "w")
	var lint bytes.Buffer
	if !CheckUnencrypted(f.store, &lint) || lint.Len() != 0 {
		t.Errorf("the lint check names a secret: %q", lint.String())
	}
	f.out.Reset()
	f.errOut.Reset()
	if err := f.c.Encrypt(""); err != nil {
		t.Fatal(err)
	}
	if f.out.Len() != 0 || f.errOut.Len() != 0 {
		t.Errorf("the encrypt after add-key: stdout %q stderr %q", f.out.String(), f.errOut.String())
	}
}

// On an equal time nudgeNewer gives the .enc a time of the kernel after the
// plaintext: a later edit of the plaintext, stamped by the same clock, is
// never older than the .enc. A time of the plaintext plus 1 µs is a time
// that the kernel did not stamp.
func TestNudgeNewerGivesAnEqualTimeATimeOfTheKernel(t *testing.T) {
	dir := t.TempDir()
	plain, enc := dir+"/Secret.app.default.K", dir+"/Secret.app.default.K.enc"
	write(t, plain, "v")
	write(t, enc, "e")
	same := time.Now().Add(-time.Second)
	setTime(t, enc, same)
	setTime(t, plain, same)
	if !nudgeNewer(enc, plain) {
		t.Error("nudgeNewer = false, want true")
	}
	if !newerThan(enc, plain) || !kernelStamped(t, enc) || !modTime(t, plain).Equal(same) {
		t.Errorf(".enc %v (a time of the kernel: %v), plaintext %v (was %v)", modTime(t, enc), kernelStamped(t, enc), modTime(t, plain), same)
	}
}
