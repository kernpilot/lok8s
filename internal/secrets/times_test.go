package secrets

// times_test.go: the times that an encrypt and a decrypt give the two files
// of a pair. The kernel stamps a write with a clock that can be behind
// time.Now(), so nudgeNewer and touchPlaintexts set no time from
// time.Now(). times_linux_test.go reads the ctime of a file to show that its
// time came from the kernel. tests/unit/secrets_test.bats runs the bash twin
// of the warning.

import (
	"context"
	"os"
	"testing"
	"time"
)

// setTime sets the access and modification times of path to ts.
func setTime(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// modTime is the modification time of path.
func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.ModTime()
}

// An .enc that is older than its plaintext, with a plaintext that is not
// after now, is the state of a failed CI run: the re-key set the plaintext
// to time.Now(), and the .enc that the encrypt wrote after it got an older
// time from the kernel (20 µs older here). That is no time in the future:
// nudgeNewer gives no warning and sets no time, and the next encrypt writes
// the .enc again. A plaintext dated after now gives the warning, and the
// .enc keeps the time of its write. The test sets each time, one second or
// more from now, so the result does not depend on the clock.
func TestNudgeNewerWarnsOnlyForATimeAfterNow(t *testing.T) {
	dir := t.TempDir()
	plain, enc := dir+"/Secret.app.default.K", dir+"/Secret.app.default.K.enc"
	write(t, plain, "v")
	write(t, enc, "e")
	encTime := time.Now().Add(-time.Second)
	for _, c := range []struct {
		name  string
		plain time.Time
		want  bool
	}{
		{"an older .enc, a plaintext not after now", encTime.Add(20 * time.Microsecond), true},
		{"a plaintext after now", time.Now().Add(time.Hour), false},
	} {
		setTime(t, enc, encTime)
		setTime(t, plain, c.plain)
		if got := nudgeNewer(enc, plain); got != c.want {
			t.Errorf("%s: nudgeNewer = %v, want %v", c.name, got, c.want)
		}
		if e, p := modTime(t, enc), modTime(t, plain); !e.Equal(encTime) || !p.Equal(c.plain) {
			t.Errorf("%s: a time changed: .enc %v (was %v), plaintext %v (was %v)", c.name, e, encTime, p, c.plain)
		}
	}
}

// On an equal time nudgeNewer touches the .enc until it is newer than the
// plaintext, one try per millisecond. Here the first touch gives the .enc
// the time of the plaintext again (a touch in the same tick of the clock),
// and the second one is the touch of the kernel. When no touch makes the
// .enc newer, it stops after stampTries tries and the times stay equal.
func TestNudgeNewerTouchesAnEqualTimeUntilItIsNewer(t *testing.T) {
	dir := t.TempDir()
	plain, enc := dir+"/Secret.app.default.K", dir+"/Secret.app.default.K.enc"
	write(t, plain, "v")
	write(t, enc, "e")
	same := time.Now().Add(-time.Second)
	savedTouch, savedSleep := stampTouch, stampSleep
	t.Cleanup(func() { stampTouch, stampSleep = savedTouch, savedSleep })
	for _, c := range []struct {
		name            string
		sameTick, tries int
		wantEncIsNewer  bool
	}{
		{"the second touch", 1, 2, true},
		{"no touch", stampTries, stampTries, false},
	} {
		setTime(t, enc, same)
		setTime(t, plain, same)
		touches, sleeps := 0, 0
		stampTouch = func(path string) error {
			touches++
			if touches <= c.sameTick {
				return os.Chtimes(path, same, same)
			}
			return touchNow(path)
		}
		stampSleep = func(context.Context, time.Duration) error {
			sleeps++
			return nil
		}
		if !nudgeNewer(enc, plain) {
			t.Errorf("%s: nudgeNewer = false, want true", c.name)
		}
		if touches != c.tries || sleeps != c.tries-1 {
			t.Errorf("%s: %d touch(es) and %d wait(s), want %d and %d", c.name, touches, sleeps, c.tries, c.tries-1)
		}
		if newerThan(enc, plain) != c.wantEncIsNewer || !modTime(t, plain).Equal(same) {
			t.Errorf("%s: .enc %v, plaintext %v (was %v)", c.name, modTime(t, enc), modTime(t, plain), same)
		}
	}
}
