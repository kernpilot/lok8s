package secrets

// lock.go: the lock that add-key and remove-key hold for their whole run,
// from the read of .sops.yaml to the last re-encrypted file (bash:
// secrets::_lock). Both twins take flock(2) on the same file,
// <project>/.sops.yaml.lock (or the lock beside the file at the end of the
// links of a .sops.yaml symlink), so a Go lo and a bash lo exclude each
// other too. Without
// it two runs can read the same .sops.yaml, and the second write puts back
// the recipient the first one removed. The pattern is the one of
// internal/driver/lo/lock.go, with one difference: a wait that runs out
// refuses the command instead of going on unlocked.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/kernpilot/lok8s/internal/clock"
	"github.com/kernpilot/lok8s/internal/ui"
)

// lockWaitEnv sets how many seconds add-key and remove-key wait for the
// lock (default 60, 0 = do not wait).
const lockWaitEnv = "LO_SECRETS_LOCK_WAIT"

const defaultLockWait = 60

var lockWaitRe = regexp.MustCompile(`^[0-9]+$`)

// lockWait is the wait in seconds: LO_SECRETS_LOCK_WAIT when it is a
// whole number, else the default.
func lockWait() int {
	v := os.Getenv(lockWaitEnv)
	if !lockWaitRe.MatchString(v) {
		return defaultLockWait
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultLockWait
	}
	return n
}

// lockSleep is the wait between two tries of the lock (the clock seam).
var lockSleep clock.SleepFunc = clock.Sleep

// lockChecked runs after the check of the lock path and before its open (a
// seam: a test puts a link at the path there).
var lockChecked = func(string) {}

// lockOpened runs after the open of the lock and before the test of the
// opened file (a seam: a test puts another file at the path there).
var lockOpened = func(string) {}

// sopsConfigDest resolves the project's .sops.yaml once for a run of add-key
// or remove-key (bash: secrets::_config_dest): the path itself, or, when it
// is a symlink, the file at the end of all its links. The lock sits beside
// that file, the run reads that file, and replaceFile writes it, so the lock
// and the write cannot name two files, and two links to one file take one
// lock. A path that does not resolve to a regular file is refused with one
// line and ErrHandled: the check before it refuses such a path, so only a
// change during the run gets there. The lock never falls back to the side of
// the link.
func (c *Context) sopsConfigDest() (string, error) {
	path := c.sopsConfigPath()
	dest, err := resolveConfig(path)
	if err != nil {
		ui.ErrorTo(c.ErrOut, "%s does not resolve to a regular file — nothing was changed; run this command again", path)
		return "", ErrHandled
	}
	return dest, nil
}

// resolveConfig returns path, or for a symlink the file at the end of all
// its links, when that is a regular file.
func resolveConfig(path string) (string, error) {
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&fs.ModeSymlink != 0 {
		if path, err = filepath.EvalSymlinks(path); err == nil {
			info, err = os.Lstat(path)
		}
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New(path + " is not a regular file")
	}
	return path, nil
}

// lockSopsConfig takes the lock beside dest (sopsConfigDest) and returns its
// release. It prints its own [error] line and returns ErrHandled when the
// lock path is not a regular file, when the file cannot be opened, or when
// another run holds the lock past the wait. Nothing is changed then.
//
// The lock can sit in the directory of the file that a .sops.yaml link
// names, and another user can write there. So the open never truncates,
// never follows a link and never waits. A lock path that is not a regular
// file or absent (encDestinationOK) is refused before the open, with the
// line of the bash twin. The open has O_NOFOLLOW, O_NONBLOCK and no
// O_TRUNC, and the opened file must be a regular file that the path still
// names (openedPathOK). An entry put there after the check is refused with
// the line of the check: a link (the open fails with ELOOP), a FIFO (with no
// reader the open fails with ENXIO and does not wait; with a reader fstat
// refuses it), or a directory. Another regular file at the path after the
// open is refused with a line that says that the file at the path changed
// after its check.
//
// The boundary: the directory of the lock must be trusted. The checks find
// an entry that another user puts there during the run, but a user who can
// change that directory can still replace the lock file after the checks,
// and two runs then lock two files. The lock belongs to the .sops.yaml file,
// not to a store: every store that uses that file shares it.
func (c *Context) lockSopsConfig(dest string) (func(), error) {
	path := dest + ".lock"
	if !encDestinationOK(path) {
		c.refuseLockPath(path)
		return nil, ErrHandled
	}
	lockChecked(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o644) // #nosec G302 G304 -- an empty lock file next to .sops.yaml
	if err != nil && (errors.Is(err, syscall.ELOOP) || !encDestinationOK(path)) {
		c.refuseLockPath(path)
		return nil, ErrHandled
	}
	if err != nil {
		ui.ErrorTo(c.ErrOut, "cannot open the lock file %s — the lock goes beside the file that .sops.yaml names, so lo needs write access to %s; nothing was changed", path, filepath.Dir(path))
		return nil, ErrHandled
	}
	lockOpened(path)
	switch ok, swapped := openedPathOK(f, path); {
	case swapped:
		_ = f.Close()
		ui.ErrorTo(c.ErrOut, "%s changed after its check (another file is there now): the command stopped before it changed .sops.yaml, a .gitignore or an .enc file, but its open can have made the lock file; run this command again", path)
		return nil, ErrHandled
	case !ok:
		_ = f.Close()
		c.refuseLockPath(path)
		return nil, ErrHandled
	}
	deadline := time.Now().Add(time.Duration(lockWait()) * time.Second)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			ui.ErrorTo(c.ErrOut, "cannot lock %s", path)
			return nil, ErrHandled
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			ui.ErrorTo(c.ErrOut, "%s is held by another lo secrets add-key or remove-key — nothing was changed; run it again when that one ends", path)
			return nil, ErrHandled
		}
		_ = lockSleep(context.Background(), 200*time.Millisecond)
	}
}

// refuseLockPath prints the refusal of a lock path that is not a regular
// file (bash: the same line in secrets::_lock).
func (c *Context) refuseLockPath(path string) {
	ui.ErrorTo(c.ErrOut, "%s is not a regular file (a symlink, a directory, a FIFO, a socket or a device), or its lookup failed: nothing was changed — remove it, then run this command again", path)
}

// openedPathOK reports whether f, opened from path, is a regular file and
// path, read without following a link, still names it (bash: the /dev/fd
// test of secrets::_lock). swapped is true when f and the file at path are
// two regular files that are not the same file: the file at the path
// changed after its check.
func openedPathOK(f *os.File, path string) (ok, swapped bool) {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return false, false
	}
	li, err := os.Lstat(path)
	if err != nil || !li.Mode().IsRegular() {
		return false, false
	}
	if os.SameFile(fi, li) {
		return true, false
	}
	return false, true
}
